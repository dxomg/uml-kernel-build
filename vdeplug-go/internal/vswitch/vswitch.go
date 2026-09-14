// Package vswitch implements the L2 learning switch shared by standalone
// mode (guest + uplink only) and switch mode (hub with peer wires).
//
// Semantics mirror the C binary:
//   - MAC→port learning (300s TTL), never learning multicast sources
//   - broadcast/multicast and unknown-unicast flood everywhere except the
//     source port; hairpins dropped
//   - each port has a bounded outbound queue drained by one goroutine, so
//     frames to a given port leave in order and a slow port never stalls the
//     switch (drops are correct bridge behaviour; TCP retransmits recover)
package vswitch

import (
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Port identifies a switch port.
type Port int

const (
	// Uplink is the NAT/tap port (injects into netstack or a tap fd).
	Uplink Port = -1
	// Local is the UML guest's seqpacket fd port.
	Local Port = -2
	// MaxPeers caps peer sockets on the hub.
	MaxPeers = 32
)

// Peer ports are 0..MaxPeers-1.
const outQueue = 1024

// framePool recycles outbound frame buffers (one alloc per frame is hot at
// ~80k pps).
var framePool = sync.Pool{
	New: func() any { return make([]byte, 0, linkFrameMax) },
}

const linkFrameMax = 9234

func getFrame(n int) []byte {
	b := framePool.Get().([]byte)
	return b[:n]
}

func putFrame(b []byte) {
	if cap(b) >= linkFrameMax {
		framePool.Put(b[:0])
	}
}

// Sink is a frame destination (uplink or a peer socket).
type Sink interface {
	SendFrame(frame []byte) error
}

type port struct {
	sink Sink
	out  chan []byte
	done chan struct{} // closed by RemovePort; writer exits after drain
}

// Switch is the learning switch core.
type Switch struct {
	mu    sync.Mutex
	macs  map[string]macEntry
	ports map[Port]*port
	drops atomic.Uint64 // frames shed under backpressure
}

type macEntry struct {
	port Port
	seen time.Time
}

// New creates the switch.
func New() *Switch {
	return &Switch{
		macs:  make(map[string]macEntry, 256),
		ports: make(map[Port]*port, MaxPeers+2),
	}
}

// AddPort registers a frame destination and starts its writer. The writer
// returns buffers to the pool after each send.
func (s *Switch) AddPort(p Port, sink Sink) {
	pt := &port{sink: sink, out: make(chan []byte, outQueue), done: make(chan struct{})}
	s.mu.Lock()
	s.ports[p] = pt
	s.mu.Unlock()
	go func() {
		for {
			select {
			case frame := <-pt.out:
				err := pt.sink.SendFrame(frame)
				putFrame(frame)
				if err != nil {
					s.RemovePort(p)
					return
				}
			case <-pt.done:
				return
			}
		}
	}()
}

// RemovePort drops a port (peer disconnect); its learned MACs are forgotten.
// The outbound queue is abandoned, not closed — Forward never races a close.
func (s *Switch) RemovePort(p Port) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pt, ok := s.ports[p]; ok {
		close(pt.done)
		delete(s.ports, p)
	}
	for mac, e := range s.macs {
		if e.port == p {
			delete(s.macs, mac)
		}
	}
}

// Dropped reports how many frames were shed under backpressure.
func (s *Switch) Dropped() uint64 { return s.drops.Load() }

// Forward handles one frame that arrived on from. It takes a copy of the
// frame; the caller's buffer is free to reuse once Forward returns.
func (s *Switch) Forward(from Port, frame []byte) {
	if len(frame) < 14 {
		return
	}
	if debug() {
		log.Printf("[vde_plug-go] sw: from=%d len=%d dst=%s type=%02x%02x",
			from, len(frame), tcpipMAC(frame[0:6]), frame[12], frame[13])
	}
	s.learn(frame[6:12], from)

	broadcast := frame[0]&0x01 != 0
	if broadcast {
		s.flood(from, frame)
		return
	}
	ok, dst := s.lookup(frame[0:6])
	if !ok {
		s.flood(from, frame)
		return
	}
	if dst == from {
		return // hairpin
	}
	s.emit(dst, frame)
}

func (s *Switch) emit(p Port, frame []byte) {
	s.mu.Lock()
	pt, ok := s.ports[p]
	s.mu.Unlock()
	if !ok {
		return
	}
	buf := getFrame(len(frame))
	copy(buf, frame)
	select {
	case pt.out <- buf:
	default:
		s.drops.Add(1) // slow port; bridge semantics say drop
		putFrame(buf)
	}
}

func (s *Switch) flood(from Port, frame []byte) {
	s.mu.Lock()
	targets := make([]*port, 0, len(s.ports))
	for p, pt := range s.ports {
		if p != from {
			targets = append(targets, pt)
		}
	}
	s.mu.Unlock()
	for _, pt := range targets {
		buf := getFrame(len(frame))
		copy(buf, frame)
		select {
		case pt.out <- buf:
		default:
			s.drops.Add(1)
			putFrame(buf)
		}
	}
}

func debug() bool {
	return os.Getenv("VDE_DEBUG") != ""
}

func tcpipMAC(b []byte) string {
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

func (s *Switch) learn(mac []byte, from Port) {
	if mac[0]&0x01 != 0 {
		return // never learn multicast sources
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := string(mac)
	if e, ok := s.macs[key]; ok && e.port == from {
		e.seen = time.Now()
		s.macs[key] = e
		return
	}
	s.macs[key] = macEntry{port: from, seen: time.Now()}
}

func (s *Switch) lookup(mac []byte) (bool, Port) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.macs[string(mac)]
	if !ok {
		return false, 0
	}
	if time.Since(e.seen) > 300*time.Second {
		delete(s.macs, string(mac))
		return false, 0
	}
	return true, e.port
}
