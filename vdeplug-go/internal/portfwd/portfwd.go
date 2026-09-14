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

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"uml-kernel-build/vdeplug-go/internal/config"
)

// Start launches one host listener per TCP rule. It returns immediately;
// listeners run until the process exits.
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
		if r.UDP {
			continue // UDP relay lands with the M2 DHCP/lease work
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(r.HostIP, strconv.Itoa(int(r.HostPort))))
		if err != nil {
			return nil, fmt.Errorf("listen %s: %w", net.JoinHostPort(r.HostIP, strconv.Itoa(int(r.HostPort))), err)
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
