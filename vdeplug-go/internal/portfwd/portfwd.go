// Package portfwd listens on the host side and relays connections into the
// netstack toward the guest — the reverse direction of the NAT forwarders.
//
// Rules come from config.yaml "ports:". Fallback rules ("2222:22") target
// the fallback IP (today: the static guest / DHCP start address); pinned
// rules ("10.0.2.15:2222:22" or ".15:2222:22") name the guest explicitly.
package portfwd

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"uml-kernel-build/vdeplug-go/internal/config"
)

// Start launches one host listener per rule (TCP listeners + UDP relays).
// It returns immediately; services run until the process exits.
func Start(s *stack.Stack, cfg *config.Config, fallbackIP net.IP) (*Server, error) {
	rules, err := cfg.ParsePorts()
	if err != nil {
		return nil, fmt.Errorf("ports: %w", err)
	}
	srv := &Server{}
	for _, r := range rules {
		guestIP := r.GuestIP
		if guestIP == "" {
			guestIP = fallbackIP.String()
		}
		if r.LastOctet > 0 {
			ip4 := fallbackIP.To4()
			if ip4 == nil {
				return nil, fmt.Errorf("ports: fallback %s not IPv4", fallbackIP)
			}
			ip := make(net.IP, 4)
			copy(ip, ip4)
			ip[3] = byte(r.LastOctet)
			guestIP = ip.String()
		}
		hostAddr := net.JoinHostPort(r.HostIP, strconv.Itoa(int(r.HostPort)))
		if r.UDP {
			udpAddr, err := net.ResolveUDPAddr("udp", hostAddr)
			if err != nil {
				return nil, fmt.Errorf("listen %s: %w", hostAddr, err)
			}
			srv.wg.Add(1)
			go srv.serveUDP(s, udpAddr, guestIP, r.GuestPort)
			continue
		}
		ln, err := net.Listen("tcp", hostAddr)
		if err != nil {
			return nil, fmt.Errorf("listen %s: %w", hostAddr, err)
		}
		srv.wg.Add(1)
		go srv.serveTCP(s, ln, guestIP, r.GuestPort)
	}
	return srv, nil
}

// Server holds the active port forwards.
type Server struct {
	wg sync.WaitGroup
}

// Wait blocks until all accept loops have ended (only on shutdown).
func (s *Server) Wait() { s.wg.Wait() }

func (s *Server) serveTCP(sck *stack.Stack, ln net.Listener, guestIP string, guestPort uint16) {
	defer s.wg.Done()
	guest4 := net.ParseIP(guestIP).To4()
	dst := tcpip.FullAddress{
		Addr: tcpip.AddrFrom4([4]byte{guest4[0], guest4[1], guest4[2], guest4[3]}),
		Port: guestPort,
	}
	for {
		hostConn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer hostConn.Close()
			guestConn, dialErr := gonet.DialContextTCP(context.Background(), sck, dst, ipv4.ProtocolNumber)
			if dialErr != nil {
				return // guest refused/unreachable: close like a rejected port
			}
			defer guestConn.Close()
			pipe(hostConn, guestConn)
		}()
	}
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// serveUDP relays host datagrams into the guest. Each remote host sender
// gets its own netstack socket so guest replies return to the right peer;
// idle paths die after udpIdle.
func (s *Server) serveUDP(sck *stack.Stack, hostAddr *net.UDPAddr, guestIP string, guestPort uint16) {
	defer s.wg.Done()
	host, err := net.ListenUDP("udp", hostAddr)
	if err != nil {
		return
	}
	guest4 := net.ParseIP(guestIP).To4()
	dst := tcpip.FullAddress{
		Addr: tcpip.AddrFrom4([4]byte{guest4[0], guest4[1], guest4[2], guest4[3]}),
		Port: guestPort,
	}
	type path struct {
		guest net.Conn // netstack-side, connected to the guest
		src   *net.UDPAddr
		last  time.Time
	}
	var mu sync.Mutex
	var paths []*path

	// Reaper for idle paths.
	go func() {
		for {
			time.Sleep(time.Minute)
			mu.Lock()
			keep := paths[:0]
			for _, p := range paths {
				if time.Since(p.last) > 30*time.Second {
					p.guest.Close()
				} else {
					keep = append(keep, p)
				}
			}
			paths = keep
			mu.Unlock()
		}
	}()

	buf := make([]byte, 65536)
	for {
		n, src, err := host.ReadFromUDP(buf)
		if err != nil {
			return
		}
		mu.Lock()
		var p *path
		for _, cand := range paths {
			if cand.src.IP.Equal(src.IP) && cand.src.Port == src.Port {
				p = cand
				break
			}
		}
		if p == nil {
			var wq waiter.Queue
			ep, epErr := sck.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
			if epErr != nil {
				mu.Unlock()
				continue
			}
			if bindErr := ep.Bind(tcpip.FullAddress{NIC: 1}); bindErr != nil {
				ep.Close()
				mu.Unlock()
				continue
			}
			if connectErr := ep.Connect(dst); connectErr != nil {
				ep.Close()
				mu.Unlock()
				continue
			}
			gconn := gonet.NewUDPConn(&wq, ep)
			p = &path{guest: gconn, src: src}
			paths = append(paths, p)
			go func() {
				rbuf := make([]byte, 65536)
				for {
					gconn.SetReadDeadline(time.Now().Add(30 * time.Second))
					n, rErr := gconn.Read(rbuf)
					if rErr != nil {
						mu.Lock()
						for i, c := range paths {
							if c == p {
								paths = append(paths[:i], paths[i+1:]...)
							}
						}
						mu.Unlock()
						gconn.Close()
						return
					}
					host.SetWriteDeadline(time.Now().Add(5 * time.Second))
					if _, wErr := host.WriteToUDP(rbuf[:n], src); wErr != nil {
						gconn.Close()
						return
					}
				}
			}()
		}
		p.last = time.Now()
		mu.Unlock()

		guest := p.guest
		guest.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, wErr := guest.Write(buf[:n]); wErr != nil {
			guest.Close()
		}
	}
}
