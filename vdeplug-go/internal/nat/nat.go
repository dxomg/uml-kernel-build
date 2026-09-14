// Package nat wires gVisor netstack as the userspace NAT for one UML guest,
// replacing libslirp:
//
//   - TCP: per-connection Forwarder with a goroutine pair pumping 64 KiB
//     buffers to/from the host socket (the aggregation libslirp lacks).
//   - UDP: per-flow forwarders; DNS is relayed to the host resolver.
//   - ICMP echo to the gateway is answered by netstack itself.
//
// Address mapping matches libslirp: the gateway address (10.0.2.2) maps to
// host loopback, the nameserver address (10.0.2.3) is a DNS proxy.
package nat

import (
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"uml-kernel-build/vdeplug-go/internal/link"
)

const (
	nicID       = 1
	copyBufSize = 64 * 1024
	udpIdle     = 30 * time.Second
)

// Options for building the NAT stack.
type Options struct {
	GatewayIP  string // 10.0.2.2
	Nameserver string // 10.0.2.3
	Network    string // 10.0.2.0/24
	IPv6       bool
}

// NAT owns the netstack instance and its host-side plumbing.
type NAT struct {
	Stack *stack.Stack
	EP    *link.EtherEndpoint

	gatewayIP net.IP
	nsIP      net.IP
	resolvers []string
	wg        sync.WaitGroup
}

// New builds the netstack stack and attaches ep as the Ethernet device.
func New(ep *link.EtherEndpoint, opts Options) (*NAT, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{arp.NewProtocol, ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{udp.NewProtocol, tcp.NewProtocol},
	})

	if err := s.CreateNIC(nicID, ep); err != nil {
		return nil, fmt.Errorf("CreateNIC: %s", err)
	}

	n := &NAT{
		Stack:     s,
		EP:        ep,
		gatewayIP: net.ParseIP(opts.GatewayIP),
		nsIP:      net.ParseIP(opts.Nameserver),
		resolvers: resolvers(),
	}

	// The gateway address answers ARP/ICMP/TCP on the segment. The DNS
	// address is a second local address so 10.0.2.3:53 reaches us too.
	for _, addr := range []string{opts.GatewayIP, opts.Nameserver} {
		ip := net.ParseIP(addr)
		a4 := tcpip.AddrFrom4([4]byte{ip[12], ip[13], ip[14], ip[15]})
		if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
			Protocol:          ipv4.ProtocolNumber,
			AddressWithPrefix: a4.WithPrefix(),
		}, stack.AddressProperties{}); err != nil {
			return nil, fmt.Errorf("AddProtocolAddress(%s): %s", addr, err)
		}
	}

	// Connected route for the guest subnet. A default route via our own
	// address would hairpin.
	sub, err := subnetOf(opts.Network)
	if err != nil {
		return nil, err
	}
	s.SetRouteTable([]tcpip.Route{{Destination: sub, NIC: nicID}})

	tcpFwd := tcp.NewForwarder(s, 0, 1024, n.tcpHandler)
	s.SetTransportProtocolHandler(header.TCPProtocolNumber, tcpFwd.HandlePacket)
	udpFwd := udp.NewForwarder(s, func(r *udp.ForwarderRequest) bool {
		return n.udpHandler(r)
	})
	s.SetTransportProtocolHandler(header.UDPProtocolNumber, udpFwd.HandlePacket)

	return n, nil
}

func subnetOf(cidr string) (tcpip.Subnet, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return tcpip.Subnet{}, fmt.Errorf("network %q: %w", cidr, err)
	}
	ip4 := ipnet.IP.To4()
	if ip4 == nil {
		return tcpip.Subnet{}, fmt.Errorf("network %q: not IPv4", cidr)
	}
	mask := ipnet.Mask
	return tcpip.NewSubnet(
		tcpip.AddrFrom4([4]byte{ip4[0], ip4[1], ip4[2], ip4[3]}),
		tcpip.MaskFromBytes(append([]byte(nil), mask...)),
	)
}

// ── TCP ────────────────────────────────────────────────────────────────

func (n *NAT) tcpHandler(r *tcp.ForwarderRequest) {
	id := r.ID()
	// The guest's destination is the LOCAL side of the 4-tuple: we are the
	// endpoint 10.0.2.2:port.
	dst := id.LocalAddress.String()
	if dst == n.gatewayIP.String() {
		dst = "127.0.0.1"
	}
	hostConn, err := net.Dial("tcp", net.JoinHostPort(dst, strconv.Itoa(int(id.LocalPort))))
	if err != nil {
		r.Complete(true) // RST: connection-refused semantics
		return
	}

	go func() {
		var wq waiter.Queue
		ep, err := r.CreateEndpoint(&wq)
		if err != nil {
			hostConn.Close()
			return
		}
		guest := gonet.NewTCPConn(&wq, ep)
		done := func() { guest.Close(); hostConn.Close() }
		go pump(hostConn, guest, done)
		pump(guest, hostConn, done)
	}()
}

func pump(dst io.Writer, src io.Reader, done func()) {
	buf := make([]byte, copyBufSize)
	io.CopyBuffer(dst, src, buf)
	done()
}

// ── UDP ────────────────────────────────────────────────────────────────

type udpFlow struct {
	conn *gonet.UDPConn
	host net.Conn
}

// udpHandler forwards one guest UDP flow to the host. DNS traffic to the
// nameserver address goes to the host resolver; flows to the gateway go to
// host loopback; everything else is dialed literally.
func (n *NAT) udpHandler(r *udp.ForwarderRequest) bool {
	id := r.ID()
	dstIP := id.LocalAddress.String()
	dstPort := id.LocalPort

	go func() {
		host := n.dialUDP(dstIP, dstPort)
		if host == nil {
			return // let netstack send port-unreachable
		}
		var wq waiter.Queue
		ep, err := r.CreateEndpoint(&wq)
		if err != nil {
			host.Close()
			return
		}
		guest := gonet.NewUDPConn(&wq, ep)
		n.relayUDP(guest, host)
	}()
	return true
}

func (n *NAT) dialUDP(dstIP string, dstPort uint16) net.Conn {
	if dstIP == n.nsIP.String() && dstPort == 53 {
		for _, server := range n.resolvers {
			c, err := net.Dial("udp", net.JoinHostPort(server, "53"))
			if err == nil {
				return c
			}
		}
		return nil
	}
	if dstIP == n.gatewayIP.String() {
		dstIP = "127.0.0.1"
	}
	c, err := net.Dial("udp", net.JoinHostPort(dstIP, strconv.Itoa(int(dstPort))))
	if err != nil {
		return nil
	}
	return c
}

// relayUDP copies guest datagrams to the host socket and replies back.
// One request/response per flow iteration; idle flows die after udpIdle.
func (n *NAT) relayUDP(guest *gonet.UDPConn, host net.Conn) {
	defer guest.Close()
	defer host.Close()
	buf := make([]byte, 65536)
	for {
		guest.SetReadDeadline(time.Now().Add(udpIdle))
		nread, _, err := guest.ReadFrom(buf)
		if err != nil {
			return
		}
		host.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := host.Write(buf[:nread]); err != nil {
			return
		}
		host.SetReadDeadline(time.Now().Add(5 * time.Second))
		nwrote, err := host.Read(buf)
		if err != nil {
			return
		}
		guest.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := guest.WriteTo(buf[:nwrote], nil); err != nil {
			return
		}
	}
}

// resolvers reads the host's /etc/resolv.conf nameservers.
func resolvers() []string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "nameserver" {
			out = append(out, f[1])
		}
	}
	return out
}

// Close releases netstack resources on shutdown.
func (n *NAT) Close() {
	n.Stack.Close()
}
