// Package elect referees hub failover for the switch socket.
//
// The hub seat is a flock on <socket>.lock: the kernel releases it when
// the holder dies (even SIGKILL), so a seat fight needs no election
// protocol. Peers noticing a dead hub (EOF, or heartbeats gone silent)
// contend for the lock; the winner binds the socket and becomes the hub,
// losers reconnect as peers. Split-brain is structurally impossible: the
// lock is exclusive and the socket is only ever bound while holding it.
//
// Heartbeats are broadcast frames (ethertype 0x88B5) the hub floods
// through the switch. They carry the DHCP lease table, so a promoted
// peer hands out the same addresses the old hub did.
package elect

import (
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// ErrSeatHeld means another instance holds the hub seat.
var ErrSeatHeld = errors.New("hub seat already held")

// TryLock takes the hub seat for the switch at socketPath, or fails with
// ErrSeatHeld. The returned release func drops the lock; the kernel
// drops it anyway when the process dies. The lock file itself is never
// removed.
func TryLock(socketPath string) (release func(), err error) {
	lockPath := socketPath + ".lock"
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", lockPath, err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(fd)
		return nil, ErrSeatHeld
	}
	return func() { unix.Close(fd) }, nil
}

// BeaconEthertype is an IEEE local-experimental ethertype; guest kernels
// silently ignore frames carrying it.
const BeaconEthertype = 0x88B5

const beaconMagic = "VDHB"

// MaxLeases caps the gossiped table well under one seqpacket message.
const MaxLeases = 32

// Lease is one gossiped DHCP assignment.
type Lease struct {
	MAC [6]byte
	IP  [4]byte
}

// EncodeBeacon builds a full Ethernet heartbeat frame. srcMAC is the
// hub's link address as raw bytes.
func EncodeBeacon(srcMAC []byte, seq uint32, leases []Lease) []byte {
	if len(leases) > MaxLeases {
		leases = leases[:MaxLeases]
	}
	frame := make([]byte, 14+11+10*len(leases))
	copy(frame[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(frame[6:12], srcMAC)
	binary.BigEndian.PutUint16(frame[12:14], BeaconEthertype)
	copy(frame[14:], beaconMagic)
	frame[18] = 1 // protocol version
	binary.BigEndian.PutUint32(frame[19:23], seq)
	binary.BigEndian.PutUint16(frame[23:25], uint16(len(leases)))
	for i, l := range leases {
		e := frame[25+10*i:]
		copy(e[0:6], l.MAC[:])
		copy(e[6:10], l.IP[:])
	}
	return frame
}

// IsBeacon reports whether a frame is a hub heartbeat.
func IsBeacon(frame []byte) bool {
	if len(frame) < 25 || frame[0]&0x01 == 0 {
		return false // heartbeats are broadcast
	}
	return binary.BigEndian.Uint16(frame[12:14]) == BeaconEthertype &&
		string(frame[14:18]) == beaconMagic
}

// ParseBeacon decodes a heartbeat's sequence and lease table.
func ParseBeacon(frame []byte) (seq uint32, leases []Lease, err error) {
	if !IsBeacon(frame) {
		return 0, nil, errors.New("not a beacon")
	}
	if frame[18] != 1 {
		return 0, nil, fmt.Errorf("beacon version %d unsupported", frame[18])
	}
	seq = binary.BigEndian.Uint32(frame[19:23])
	n := int(binary.BigEndian.Uint16(frame[23:25]))
	if n > MaxLeases || len(frame) < 25+10*n {
		return 0, nil, errors.New("beacon truncated")
	}
	leases = make([]Lease, n)
	for i := range leases {
		e := frame[25+10*i:]
		copy(leases[i].MAC[:], e[0:6])
		copy(leases[i].IP[:], e[6:10])
	}
	return seq, leases, nil
}
