// The fleet e2e without UML: fake guests speak raw Ethernet over
// SOCK_SEQPACKET pairs to real vdeplug-go processes, so the switch core,
// peer wires, heartbeats and the failover election run exactly as in
// production. uplink: none keeps everything hermetic — no netstack, no
// host listeners.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"uml-kernel-build/vdeplug-go/internal/elect"
	"uml-kernel-build/vdeplug-go/internal/unixseq"
)

var binSrc string // built once by TestMain

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "vdeplug-e2e-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	binSrc = filepath.Join(dir, "vdeplug-go")
	out, err := exec.Command("go", "build", "-o", binSrc, ".").CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("go build: %v\n%s", err, out))
	}
	os.Exit(m.Run())
}

// ── fake guest plumbing ────────────────────────────────────────────────

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type guest struct {
	name   string
	cmd    *exec.Cmd
	conn   *os.File
	logs   *syncBuffer
	frames chan []byte
	closed chan struct{}
	mac    [6]byte
	ip     [4]byte
}

// pump reads the wire: ARPs for our address get answered like a real
// NIC, everything else lands in the frames channel for the assertions.
func (g *guest) pump() {
	buf := make([]byte, 9234)
	for {
		n, err := g.conn.Read(buf)
		if err != nil {
			close(g.closed)
			return
		}
		if n < 14 {
			continue
		}
		frame := make([]byte, n)
		copy(frame, buf[:n])
		if binary.BigEndian.Uint16(frame[12:14]) == 0x0806 && n >= 42 &&
			binary.BigEndian.Uint16(frame[20:22]) == 1 &&
			bytes.Equal(frame[38:42], g.ip[:]) {
			var requesterMAC [6]byte
			var requesterIP [4]byte
			copy(requesterMAC[:], frame[22:28])
			copy(requesterIP[:], frame[28:32])
			reply := arpFrame(2, g.mac, g.ip, requesterMAC, requesterIP)
			g.conn.Write(reply)
			// fall through: the test also asserts the request was seen
		}
		select {
		case g.frames <- frame:
		default: // reader not keeping up: fine, assertions use small volumes
		}
	}
}

func (g *guest) kill(t *testing.T) {
	t.Helper()
	g.cmd.Process.Kill()
	g.cmd.Wait()
}

func (g *guest) send(t *testing.T, frame []byte) {
	t.Helper()
	g.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := g.conn.Write(frame); err != nil {
		t.Fatalf("guest %s send: %v\nlogs:\n%s", g.name, err, g.logs.String())
	}
}

var allGuests []*guest

// dumpFleet returns every instance's log — failures rarely make sense
// from one node's point of view.
func dumpFleet() string {
	var w strings.Builder
	for _, g := range allGuests {
		fmt.Fprintf(&w, "── guest %s ──\n%s\n", g.name, g.logs.String())
	}
	return w.String()
}

// recvFrame waits for a frame of the wanted ethertype; hub beacons
// (0x88B5) and any other control chatter are skipped.
func (g *guest) recvFrame(t *testing.T, ethertype uint16, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case frame := <-g.frames:
			if len(frame) >= 14 && binary.BigEndian.Uint16(frame[12:14]) == ethertype {
				return frame
			}
			continue
		case <-g.closed:
			t.Fatalf("guest %s link died\n%s", g.name, dumpFleet())
		case <-deadline:
			t.Fatalf("guest %s: no %04x frame in %v\n%s", g.name, ethertype, timeout, dumpFleet())
		}
	}
}

func macOf(b byte) [6]byte { return [6]byte{0x02, 0xaa, 0x00, 0x00, 0x00, b} }
func ipOf(b byte) [4]byte  { return [4]byte{10, 0, 9, b} }

// arpFrame builds a 42-byte ARP request (op=1) or reply (op=2).
func arpFrame(op uint16, srcMAC [6]byte, srcIP [4]byte, dstMAC [6]byte, dstIP [4]byte) []byte {
	f := make([]byte, 42)
	if op == 1 {
		copy(f[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	} else {
		copy(f[0:6], dstMAC[:])
	}
	copy(f[6:12], srcMAC[:])
	binary.BigEndian.PutUint16(f[12:14], 0x0806)
	binary.BigEndian.PutUint16(f[14:16], 1)      // ethernet
	binary.BigEndian.PutUint16(f[16:18], 0x0800) // IPv4
	f[18], f[19] = 6, 4
	binary.BigEndian.PutUint16(f[20:22], op)
	copy(f[22:28], srcMAC[:])
	copy(f[28:32], srcIP[:])
	copy(f[32:38], dstMAC[:])
	copy(f[38:42], dstIP[:])
	return f
}

func spawnGuest(t *testing.T, dir, name, sockPath string, id byte) *guest {
	t.Helper()
	bin := filepath.Join(dir, "vdeplug-go")
	if _, err := os.Stat(bin); err != nil {
		src, err := os.ReadFile(binSrc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, src, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	hub, wire := pair[0], pair[1]
	if err != nil {
		t.Fatal(err)
	}
	wireFile := os.NewFile(uintptr(wire), "wire")
	logs := &syncBuffer{}
	cmd := exec.Command(bin, "--descr", name, "seqpacket://3", "slirp://")
	cmd.ExtraFiles = []*os.File{wireFile} // becomes fd 3 in the child
	cmd.Stderr = logs
	cmd.Env = append(os.Environ(), "VDE_DEBUG=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wireFile.Close() // the child owns it now
	conn := os.NewFile(uintptr(hub), "guest-end")
	g := &guest{
		name:   name,
		cmd:    cmd,
		conn:   conn,
		logs:   logs,
		frames: make(chan []byte, 256),
		closed: make(chan struct{}),
		mac:    macOf(id),
		ip:     ipOf(id),
	}
	go g.pump()
	allGuests = append(allGuests, g)
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		conn.Close()
	})
	return g
}

func writeConfig(t *testing.T, dir, sockPath string) {
	t.Helper()
	cfg := fmt.Sprintf("switch: true\nuplink: none\nsocket: %s\n", sockPath)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ── the tests ──────────────────────────────────────────────────────────

// TestFleetBootSwitchingFailover boots a three-node fleet, proves frames
// cross it, kills the hub, proves a peer promotes and traffic re-flows,
// then proves the old hub returns as a plain peer (sticky failback).
func TestFleetBootSwitchingFailover(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "vde.socket")
	writeConfig(t, dir, sockPath)

	// First node up must take the hub seat.
	a := spawnGuest(t, dir, "A", sockPath, 1)
	waitFor(t, func() bool {
		_, err := os.Stat(sockPath)
		return err == nil
	}, 5*time.Second, "hub socket")

	b := spawnGuest(t, dir, "B", sockPath, 2)
	c := spawnGuest(t, dir, "C", sockPath, 3)
	time.Sleep(500 * time.Millisecond) // peers settle

	// The seat is exclusively held.
	if _, err := elect.TryLock(sockPath); err == nil {
		t.Fatal("hub seat is not held")
	}

	// A ARPs for B; the request must flood to B and the reply return.
	a.send(t, arpFrame(1, macOf(1), ipOf(1), macOf(2), ipOf(2)))
	b.recvFrame(t, 0x0806, 3*time.Second)
	reply := a.recvFrame(t, 0x0806, 3*time.Second)
	bmac, bip := macOf(2), ipOf(2)
	if !bytes.Equal(reply[6:12], bmac[:]) || !bytes.Equal(reply[28:32], bip[:]) {
		t.Fatalf("ARP reply not from B: % x", reply[:34])
	}

	// Kill the hub. Survivors re-elect; the winner rebinds the socket.
	a.cmd.Process.Kill()
	a.cmd.Wait()
	waitFor(t, func() bool {
		conn, err := unixseq.TryConnect(sockPath)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, 8*time.Second, "promoted hub socket")
	time.Sleep(500 * time.Millisecond) // the loser finishes reconnecting

	// Traffic re-flows through whoever promoted: B ARPs for C, C answers.
	b.send(t, arpFrame(1, macOf(2), ipOf(2), macOf(3), ipOf(3)))
	c.recvFrame(t, 0x0806, 3*time.Second)
	reply = b.recvFrame(t, 0x0806, 3*time.Second)
	cmac, cip := macOf(3), ipOf(3)
	if !bytes.Equal(reply[6:12], cmac[:]) || !bytes.Equal(reply[28:32], cip[:]) {
		t.Fatalf("post-failover ARP reply not from C: % x", reply[:34])
	}

	// Sticky failback: the old hub returns as a plain peer, the seat
	// stays with its new owner.
	a2 := spawnGuest(t, dir, "A2", sockPath, 1)
	waitFor(t, func() bool {
		return strings.Contains(a2.logs.String(), "peer of")
	}, 5*time.Second, "A2 joining as peer")
	if _, err := elect.TryLock(sockPath); err == nil {
		t.Fatal("returned hub stole the seat")
	}
	if a2logs := a2.logs.String(); strings.Contains(a2logs, "no uplink") {
		t.Fatalf("A2 became hub instead of peer:\n%s", a2logs)
	}
}
