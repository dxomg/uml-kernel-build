// Package dhcp serves DHCPv4 leases to the guest, replacing libslirp's
// BOOTP backend (and its NB_BOOTP_CLIENTS ceiling).
//
// Unlike gvisor-tap-vsock (a socket bound to netstack's :67), this handles
// DHCP at the frame layer: netstack's IPv4 layer sheds the limited-
// broadcast DISCOVERs before the transport demuxer sees them, so the
// handler hooks the uplink path instead — same special-casing libslirp
// does. Replies go back as raw frames, no sockets involved.
package dhcp

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/rfc1035label"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func (s *Server) debugf(f string, a ...any) {
	if os.Getenv("VDE_DEBUG") != "" {
		log.Printf("[vde_plug-go] dhcp: "+f, a...)
	}
}

// Pool assigns stable leases per client MAC.
type Pool struct {
	mu       sync.Mutex
	base     [4]byte
	size     int
	next     int
	assigned map[[6]byte][4]byte
	order    [][6]byte // assignment order, for "first client" semantics
}

// NewPool serves size addresses beginning at start (the first lease is
// start itself).
func NewPool(start net.IP, size int) *Pool {
	ip4 := start.To4()
	p := &Pool{
		size:     size,
		assigned: make(map[[6]byte][4]byte, size),
	}
	copy(p.base[:], ip4)
	return p
}

// GetOrAssign returns the stable lease for a MAC, assigning the next free
// address in the pool. Fully occupied pools return an error (the C binary
// just ran out of its 20 BOOTP clients; same semantics, no surprises).
func (p *Pool) GetOrAssign(mac net.HardwareAddr) (net.IP, error) {
	var key [6]byte
	copy(key[:], mac)
	p.mu.Lock()
	defer p.mu.Unlock()
	if ip, ok := p.assigned[key]; ok {
		return net.IP(append([]byte(nil), ip[:]...)), nil
	}
	if p.next >= p.size {
		return nil, fmt.Errorf("dhcp pool exhausted (%d leases)", p.size)
	}
	var ip [4]byte
	copy(ip[:], p.base[:])
	ip[3] += byte(p.next)
	p.next++
	p.assigned[key] = ip
	p.order = append(p.order, key)
	return net.IP(append([]byte(nil), ip[:]...)), nil
}

// Restore seeds the pool with leases gossiped by a previous hub, so a
// promoted peer hands out the same addresses. Entries outside the pool
// range are dropped; the rest are re-registered in address order, so
// First() again names the lowest leased address.
func (p *Pool) Restore(leases map[[6]byte][4]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for off := 0; off < p.size; off++ {
		var ip [4]byte
		copy(ip[:], p.base[:])
		ip[3] += byte(off)
		for mac, a := range leases {
			if a != ip {
				continue
			}
			if _, ok := p.assigned[mac]; !ok {
				p.assigned[mac] = a
				p.order = append(p.order, mac)
			}
			break
		}
	}
	p.next = len(p.assigned)
}

// First returns the first assigned lease, or nil when nobody has asked yet.
func (p *Pool) First() net.IP {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.order) == 0 {
		return nil
	}
	ip := p.assigned[p.order[0]]
	return net.IP(append([]byte(nil), ip[:]...))
}

// Leases snapshots MAC→IP for the SIGUSR1 dump.
func (p *Pool) Leases() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]string, len(p.assigned))
	for mac, ip := range p.assigned {
		out[net.HardwareAddr(mac[:]).String()] = net.IP(ip[:]).String()
	}
	return out
}

// Options for DHCP replies.
type Options struct {
	Network       string // 10.0.2.0/24
	GatewayIP     string // 10.0.2.2
	Nameserver    string // 10.0.2.3
	MTU           int
	LeaseTime     time.Duration
	SearchDomains []string
}

// Server handles DHCP at the frame layer.
type Server struct {
	opts Options
	pool *Pool
	mac  tcpip.LinkAddress // server's MAC on the segment
}

// New creates the DHCP service.
func New(opts Options, pool *Pool, serverMAC tcpip.LinkAddress) *Server {
	if opts.LeaseTime == 0 {
		opts.LeaseTime = time.Hour
	}
	return &Server{opts: opts, pool: pool, mac: serverMAC}
}

const serverPort = 67

// HandleFrame checks a raw guest frame for a DHCP request and returns the
// reply frame to switch back to the guest, or nil when the frame is not a
// DHCP request for us.
func (s *Server) HandleFrame(frame []byte) []byte {
	if len(frame) < header.EthernetMinimumSize+header.IPv4MinimumSize+header.UDPMinimumSize {
		return nil
	}
	if binary.BigEndian.Uint16(frame[12:14]) != 0x0800 {
		return nil
	}
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	if ip.Protocol() != uint8(header.UDPProtocolNumber) {
		return nil
	}
	udp := header.UDP(ip.Payload())
	if len(udp) < header.UDPMinimumSize || udp.DestinationPort() != serverPort {
		return nil
	}
	var guestMAC [6]byte
	copy(guestMAC[:], frame[6:12])
	return s.HandlePacket(udp.Payload(), guestMAC)
}

// HandlePacket processes a DHCP request carried in a UDP payload and
// returns the reply frame (full Ethernet+IP+UDP) to send to the guest, or
// nil when the payload is not a DHCP request we answer.
func (s *Server) HandlePacket(payload []byte, guestMAC [6]byte) []byte {
	m, err := dhcpv4.FromBytes(payload)
	if err != nil {
		s.debugf("parse failed: %v (payload len %d)", err, len(payload))
		if len(payload) > 244 {
			s.debugf("payload[0:16]=% x", payload[0:16])
			s.debugf("payload[230:246]=% x", payload[230:246])
		}
		return nil
	}
	s.debugf("request: type=%s chaddr=%s", m.MessageType(), m.ClientHWAddr)
	var msgType dhcpv4.MessageType
	switch m.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		msgType = dhcpv4.MessageTypeOffer
	case dhcpv4.MessageTypeRequest:
		msgType = dhcpv4.MessageTypeAck
	default:
		s.debugf("ignoring %s", m.MessageType())
		return nil // release/inform/decline: ignore like the C binary
	}

	reply, err := dhcpv4.NewReplyFromRequest(m)
	if err != nil {
		s.debugf("reply build failed: %v", err)
		return nil
	}
	ip, err := s.pool.GetOrAssign(m.ClientHWAddr)
	if err != nil {
		s.debugf("pool: %v", err)
		return nil
	}

	_, ipnet, err := net.ParseCIDR(s.opts.Network)
	if err != nil {
		return nil
	}
	reply.YourIPAddr = ip
	reply.UpdateOption(dhcpv4.OptServerIdentifier(net.ParseIP(s.opts.GatewayIP)))
	reply.UpdateOption(dhcpv4.OptIPAddressLeaseTime(s.opts.LeaseTime))
	reply.UpdateOption(dhcpv4.Option{Code: dhcpv4.OptionSubnetMask, Value: dhcpv4.IP(ipnet.Mask)})
	reply.UpdateOption(dhcpv4.Option{Code: dhcpv4.OptionRouter, Value: dhcpv4.IP(net.ParseIP(s.opts.GatewayIP))})
	reply.UpdateOption(dhcpv4.Option{Code: dhcpv4.OptionDomainNameServer, Value: dhcpv4.IPs([]net.IP{net.ParseIP(s.opts.Nameserver)})})
	if s.opts.MTU > 0 && s.opts.MTU <= 65535 {
		reply.UpdateOption(dhcpv4.Option{Code: dhcpv4.OptionInterfaceMTU, Value: dhcpv4.Uint16(uint16(s.opts.MTU))})
	}
	if len(s.opts.SearchDomains) > 0 {
		reply.UpdateOption(dhcpv4.Option{
			Code:  dhcpv4.OptionDNSDomainSearchList,
			Value: &rfc1035label.Labels{Labels: s.opts.SearchDomains},
		})
	}
	reply.UpdateOption(dhcpv4.OptMessageType(msgType))

	return s.buildFrame(reply.ToBytes(), guestMAC)
}

// buildFrame wraps a DHCP payload in Ethernet/IP/UDP from the server
// address to the broadcast address (RFC 2131 replies go to :68).
func (s *Server) buildFrame(payload []byte, dstMAC [6]byte) []byte {
	const (
		srcPort = 67
		dstPort = 68
	)
	ip4 := tcpip.AddrFrom4(s.gatewayBytes())
	bcast := tcpip.AddrFrom4([4]byte{255, 255, 255, 255})

	udpLen := header.UDPMinimumSize + len(payload)
	udp := header.UDP(make([]byte, udpLen))
	udp.Encode(&header.UDPFields{
		SrcPort: srcPort,
		DstPort: dstPort,
		Length:  uint16(udpLen),
	})
	copy(udp.Payload(), payload)
	pseudo := header.PseudoHeaderChecksum(header.UDPProtocolNumber, ip4, bcast, uint16(udpLen))
	withPayload := checksum.Combine(pseudo, checksum.Checksum(payload, 0))
	udp.SetChecksum(^udp.CalculateChecksum(withPayload))

	ipLen := header.IPv4MinimumSize + udpLen
	ip := header.IPv4(make([]byte, ipLen))
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(ipLen),
		TTL:         64,
		Protocol:    uint8(header.UDPProtocolNumber),
		SrcAddr:     ip4,
		DstAddr:     bcast,
	})
	copy(ip[header.IPv4MinimumSize:], udp)
	ip.SetChecksum(^ip.CalculateChecksum())

	frame := make([]byte, header.EthernetMinimumSize+ipLen)
	eth := header.Ethernet(frame)
	eth.Encode(&header.EthernetFields{
		SrcAddr: s.mac,
		DstAddr: header.EthernetBroadcastAddress,
		Type:    0x0800,
	})
	copy(frame[header.EthernetMinimumSize:], ip)

	return frame
}

// gatewayBytes parses the configured gateway into a 4-byte array.
func (s *Server) gatewayBytes() [4]byte {
	ip := net.ParseIP(s.opts.GatewayIP).To4()
	if ip == nil {
		return [4]byte{10, 0, 2, 2}
	}
	return [4]byte{ip[0], ip[1], ip[2], ip[3]}
}
