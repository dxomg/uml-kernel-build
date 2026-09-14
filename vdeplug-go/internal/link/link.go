// Package link bridges one SOCK_SEQPACKET fd (the UML vector vde transport)
// and a gVisor netstack, one Ethernet frame per socket message.
//
// It wraps channel.Endpoint because that type is raw-IP style: it reports
// ARPHardwareNone (so netstack would never answer ARP), a zero
// MaxHeaderLength (no space reserved for the Ethernet header) and a no-op
// AddHeader. This wrapper turns it into a proper Ethernet device.
package link

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func debugEnabled() bool { return os.Getenv("VDE_DEBUG") != "" }

// netAddr renders a raw MAC byte slice in colon notation for logs.
func netAddr(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = fmt.Sprintf("%02x", c)
	}
	return strings.Join(parts, ":")
}

const (
	// FrameMax fits a jumbo frame the way the C binary sized its buffers.
	FrameMax = 9216 + 14 + 4
	// MinFrame is the smallest frame worth handing to netstack.
	MinFrame = header.EthernetMinimumSize
)

// Stats counters, exposed for the SIGUSR1 dump.
type Stats struct {
	RxPackets atomic.Uint64
	TxPackets atomic.Uint64
	RxBytes   atomic.Uint64
	TxBytes   atomic.Uint64
	Dropped   atomic.Uint64
}

// EtherEndpoint implements stack.LinkEndpoint over a seqpacket fd.
type EtherEndpoint struct {
	*channel.Endpoint

	fd       int
	linkAddr tcpip.LinkAddress
	Stats    Stats

	guestMAC atomic.Pointer[tcpip.LinkAddress]
	sink     atomic.Pointer[func(frame []byte)] // netstack TX -> switch
}

// New wraps fd (a connected SOCK_SEQPACKET from UML) as an Ethernet device.
func New(fd int, mtu uint32, linkAddr tcpip.LinkAddress) *EtherEndpoint {
	return &EtherEndpoint{
		Endpoint: channel.New(512, mtu, linkAddr),
		fd:       fd,
		linkAddr: linkAddr,
	}
}

// SetSink registers the function receiving netstack's outbound frames.
// Must be called before Start.
func (e *EtherEndpoint) SetSink(sink func(frame []byte)) {
	e.sink.Store(&sink)
}

// Start runs the netstack-TX pump until the link dies.
func (e *EtherEndpoint) Start() error {
	for {
		pkt := e.Endpoint.ReadContext(context.Background())
		if pkt == nil {
			return fmt.Errorf("link closed")
		}
		b := pkt.ToView().AsSlice()
		// Sink must finish with b before this returns: the switch copies
		// synchronously into its outbound queues.
		sink := e.sink.Load()
		if sink != nil {
			(*sink)(b)
			e.Stats.TxPackets.Add(1)
			e.Stats.TxBytes.Add(uint64(len(b)))
		} else {
			e.Stats.Dropped.Add(1)
		}
		pkt.DecRef() // only after the frame bytes are fully consumed
	}
}

// ARPHardwareType makes netstack attach its ARP endpoint to the NIC.
func (*EtherEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareEther
}

// MaxHeaderLength reserves room for the Ethernet header on outbound packets.
func (*EtherEndpoint) MaxHeaderLength() uint16 { return header.EthernetMinimumSize }

// AddHeader writes the Ethernet header for an outbound packet.
func (e *EtherEndpoint) AddHeader(pkt *stack.PacketBuffer) {
	dst := pkt.EgressRoute.RemoteLinkAddress
	if len(dst) == 0 {
		// The only neighbor on this link is the guest.
		if m := e.guestMAC.Load(); m != nil {
			dst = *m
		} else {
			dst = header.EthernetBroadcastAddress
		}
	}
	eth := header.Ethernet(pkt.LinkHeader().Push(header.EthernetMinimumSize))
	eth.Encode(&header.EthernetFields{
		SrcAddr: e.linkAddr,
		DstAddr: dst,
		Type:    pkt.NetworkProtocolNumber,
	})
}

// GuestMAC returns the MAC most recently seen on inbound frames.
func (e *EtherEndpoint) GuestMAC() (tcpip.LinkAddress, bool) {
	if m := e.guestMAC.Load(); m != nil {
		return *m, true
	}
	return "", false
}

// Inject delivers a raw Ethernet frame into netstack. Safe from any
// goroutine; called by the switch for uplink-bound frames.
func (e *EtherEndpoint) Inject(frame []byte) {
	if len(frame) < MinFrame {
		e.Stats.Dropped.Add(1)
		return
	}
	mac := tcpip.LinkAddress(frame[6:12])
	e.guestMAC.Store(&mac)

	var proto tcpip.NetworkProtocolNumber
	switch binary.BigEndian.Uint16(frame[12:14]) {
	case 0x0800:
		proto = ipv4.ProtocolNumber
	case 0x86DD:
		proto = ipv6.ProtocolNumber
	case 0x0806:
		proto = arp.ProtocolNumber
	default:
		e.Stats.Dropped.Add(1)
		return
	}

	// Canonical inbound construction (mirrors link/fdbased): the whole frame
	// in as payload, then consume the link header from the front.
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(frame),
	})
	if _, ok := pkt.LinkHeader().Consume(MinFrame); !ok {
		pkt.DecRef()
		e.Stats.Dropped.Add(1)
		return
	}
	pkt.NetworkProtocolNumber = proto
	if debugEnabled() {
		log.Printf("[vde_plug-go] inject: len=%d proto=%04x src=%s dst=%s",
			len(frame), proto, netAddr(frame[6:12]), netAddr(frame[0:6]))
	}
	e.InjectInbound(proto, pkt)

	e.Stats.RxPackets.Add(1)
	e.Stats.RxBytes.Add(uint64(len(frame)))
}

// SendFrame writes one frame to the guest (blocking; the seqpacket applies
// natural backpressure).
func (e *EtherEndpoint) SendFrame(b []byte) error {
	for {
		err := unix.Sendmsg(e.fd, b, nil, nil, 0)
		if err == unix.EINTR {
			continue
		}
		return err
	}
}

// RecvFrame reads one frame from the guest. Blocking; retries EINTR.
// Returns error when the guest side is gone — including the (0, nil) EOF
// a SEQPACKET peer close delivers, so no consumer can spin on it.
func (e *EtherEndpoint) RecvFrame(buf []byte) (int, error) {
	for {
		n, _, _, _, err := unix.Recvmsg(e.fd, buf, nil, 0)
		if err == unix.EINTR {
			continue
		}
		if n == 0 && err == nil {
			return 0, io.EOF
		}
		if n >= 12 {
			mac := tcpip.LinkAddress(buf[6:12])
			e.guestMAC.Store(&mac)
		}
		return n, err
	}
}

func linkClosed(err error) error {
	if err == nil {
		return fmt.Errorf("link closed by peer")
	}
	return fmt.Errorf("link closed: %w", err)
}
