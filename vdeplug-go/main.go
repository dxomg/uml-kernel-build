// vde_plug — UML vector vde transport helper, rewritten on gVisor netstack.
//
// Contract (unchanged from the C binary): UML execs this program as
//
//	vde_plug --descr <name> seqpacket://FD <vnl>
//
// where FD is one end of a SOCK_SEQPACKET socketpair. Ethernet frames travel
// one per message. This program bridges the guest into a gVisor netstack
// doing NAT (uplink slirp), a host tap device (uplink tap:NAME), or nothing
// (uplink none).
//
// Switch mode (config `switch: true`, default): the first instance to take
// the hub seat becomes the hub and owns the uplink; the rest are plain peer
// wires. When the hub dies, its peers contend for the seat (flock referee)
// and one promotes — see the fleet state machine below.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"uml-kernel-build/vdeplug-go/internal/config"
	"uml-kernel-build/vdeplug-go/internal/dhcp"
	"uml-kernel-build/vdeplug-go/internal/elect"
	"uml-kernel-build/vdeplug-go/internal/link"
	"uml-kernel-build/vdeplug-go/internal/nat"
	"uml-kernel-build/vdeplug-go/internal/portfwd"
	"uml-kernel-build/vdeplug-go/internal/tap"
	"uml-kernel-build/vdeplug-go/internal/unixseq"
	"uml-kernel-build/vdeplug-go/internal/vswitch"

	"gvisor.dev/gvisor/pkg/tcpip"
)

func logf(f string, a ...any)  { fmt.Fprintf(os.Stderr, "[vde_plug-go] "+f+"\n", a...) }
func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "[vde_plug-go] fatal: "+f+"\n", a...)
	os.Exit(1)
}

func main() {
	descr, fd, vnl := parseArgs(os.Args[1:])
	if fd < 0 {
		fatalf("no seqpacket fd, nothing to do")
	}
	params := ""
	if rest, ok := strings.CutPrefix(vnl, "slirp://"); ok {
		params = rest
	}

	// Config lives next to the binary, like the C version resolves it.
	self, err := os.Executable()
	if err != nil {
		fatalf("cannot resolve self: %v", err)
	}
	cfg, err := config.Load(filepath.Join(filepath.Dir(self), "config.yaml"))
	if err != nil {
		fatalf("%v", err)
	}
	cfg.ApplyVNL(params)

	if cfg.Uplink != "" && cfg.Uplink != "slirp" {
		logf("uplink %q", cfg.Uplink)
	}

	ep := link.New(fd, link.FrameMax-18, vdeMAC)
	sw := vswitch.New()

	sockPath := resolveSwitchSocket(cfg, filepath.Dir(self))
	f := &fleet{
		ep:       ep,
		sw:       sw,
		cfg:      cfg,
		descr:    descr,
		sockPath: sockPath,
		mode:     cfg.SocketMode,
		rxErr:    make(chan error, 1),
	}
	if !cfg.SwitchEnabled() {
		f.runInstance(nil, nil) // one guest, one uplink; exits the process
		return
	}
	f.loop() // DISCOVER → PEER ⇄ HUB; only os.Exit returns
}

// resolveSwitchSocket follows the C binary's cascade: config `socket:` →
// $VDE_SWITCH_SOCKET → /tmp/vde.socket → $TMPDIR/vde.socket → the binary's
// directory → cwd.
func resolveSwitchSocket(cfg *config.Config, selfDir string) string {
	if cfg.Socket != "" {
		return cfg.Socket
	}
	if env := os.Getenv("VDE_SWITCH_SOCKET"); env != "" {
		return env
	}
	if dirWritable("/tmp") {
		return "/tmp/vde.socket"
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" && dirWritable(tmp) {
		return filepath.Join(tmp, "vde.socket")
	}
	if dirWritable(selfDir) {
		return filepath.Join(selfDir, "vde.socket")
	}
	return "vde.socket"
}

func dirWritable(dir string) bool {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return false
	}
	f, err := os.OpenFile(filepath.Join(dir, ".vde-probe"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(filepath.Join(dir, ".vde-probe"))
	return true
}

// runInstance wires one guest to its uplink; in switch mode (ln != nil)
// it also owns the switch socket, DHCP, portfwd and the beacons. Returns
// only on shutdown.
func (f *fleet) runInstance(ln *unixseq.Listener, release func()) {
	ep, sw, cfg, descr := f.ep, f.sw, f.cfg, f.descr

	// The uplink port. slirp = netstack NAT (the default); tap:NAME = a
	// raw host tap; none = an isolated guest.
	var theNAT *nat.NAT
	var leasePool *dhcp.Pool
	var dhcpSrv *dhcp.Server
	var upl vswitch.Sink
	var shutdownUplink func()
	txErr := make(chan error, 1)

	switch {
	case cfg.Uplink == "none":
		upl = blackhole{}
		shutdownUplink = func() {}
		fmt.Fprintf(os.Stderr, "[vde_plug-go] %s: no uplink (isolated)\n", descr)

	case strings.HasPrefix(cfg.Uplink, "tap"):
		name := "tap0"
		if rest, ok := strings.CutPrefix(cfg.Uplink, "tap:"); ok && rest != "" {
			name = rest
		}
		dev, err := tap.Open(name)
		if err != nil {
			fatalf("uplink tap: %v", err)
		}
		upl = dev
		shutdownUplink = func() { dev.Close() }
		// Host frames into the switch, like any uplink arrival.
		go func() {
			buf := make([]byte, link.FrameMax)
			for {
				n, err := dev.RecvFrame(buf)
				if err != nil {
					logf("tap %s gone: %v", name, err)
					return
				}
				if n >= 14 {
					frame := make([]byte, n)
					copy(frame, buf[:n])
					sw.Forward(vswitch.Uplink, frame)
				}
			}
		}()
		fmt.Fprintf(os.Stderr, "[vde_plug-go] %s: tap uplink %s\n", descr, dev.Name())

	default: // slirp
		if cfg.Uplink != "slirp" && cfg.Uplink != "" {
			fatalf("unknown uplink %q (want slirp | tap[:NAME] | none)", cfg.Uplink)
		}
		var err error
		theNAT, err = nat.New(ep, nat.Options{
			GatewayIP:  cfg.Gateway,
			Nameserver: cfg.Nameserver,
			Network:    cfg.Network,
			IPv6:       cfg.IPv6,
			MTU:        cfg.MTU,
		})
		if err != nil {
			fatalf("nat: %v", err)
		}

		// DHCP at the frame layer (netstack sheds limited-broadcast
		// DISCOVERs before the transport demuxer would see them). A
		// promoted hub seeds the pool with the lease table the dead hub
		// gossiped, so guests keep their addresses.
		if start := net.ParseIP(cfg.DHCPStart); start != nil && cfg.DHCPStart != "" {
			leasePool = dhcp.NewPool(start, 256)
			f.mu.Lock()
			seed := f.leases
			f.leases = nil
			f.mu.Unlock()
			if len(seed) > 0 {
				leasePool.Restore(seed)
				logf("%s: promoted to hub, %d leases inherited", descr, len(seed))
			}
			dhcpSrv = dhcp.New(dhcp.Options{
				Network:    cfg.Network,
				GatewayIP:  cfg.Gateway,
				Nameserver: cfg.Nameserver,
				MTU:        cfg.MTU,
			}, leasePool, vdeMAC)
		}

		upl = uplinkSink{ep: ep, dhcp: dhcpSrv, sw: sw, own: ownAddrs(cfg)}
		shutdownUplink = theNAT.Close
		// Netstack TX pump: pops packets netstack emits and switches them.
		go func() { txErr <- ep.Start() }()
		fmt.Fprintf(os.Stderr, "[vde_plug-go] %s: slirp nat on %s gw %s dns %s\n",
			descr, cfg.Network, cfg.Gateway, cfg.Nameserver)
	}

	ep.SetSink(func(frame []byte) { sw.Forward(vswitch.Uplink, frame) })
	sw.AddPort(vswitch.Uplink, upl)
	sw.AddPort(vswitch.Local, guestSink{ep})
	f.wireOnce.Do(func() { go f.wire() }) // single guest-wire reader, forever

	if ln != nil {
		go f.acceptPeers(ln)
		// Heartbeats: flood our state through the switch so peers notice
		// a wedged hub (three missed beats) and a promoted peer inherits
		// the lease table.
		go f.sendBeacons(leasePool)
	}

	if theNAT != nil && cfg.PortFwd {
		fallback := net.ParseIP(cfg.DHCPStart)
		if leasePool != nil {
			if first := leasePool.First(); first != nil {
				fallback = first // follow the first client, like the C binary
			}
		}
		pf, err := portfwd.Start(theNAT.Stack, cfg, fallback)
		if err != nil {
			logf("portfwd: %v", err)
		} else {
			_ = pf
		}
	}

	waitShutdown(theNAT, sw, leasePool, f.rxErr, txErr)
	shutdownUplink()
	if ln != nil {
		ln.Close()
		release() // drop the hub seat
	}
	os.Exit(0)
}

// acceptPeers runs the hub's listener: each connection becomes a switch
// port with its own reader and outbound queue.
func (f *fleet) acceptPeers(ln *unixseq.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed: shutting down
		}
		port, ok := f.sw.AddPeer(conn)
		if !ok {
			logf("switch: peer limit reached, refusing")
			conn.Close()
			continue
		}
		logf("switch: peer %d joined", port)
		go peerReader(f.sw, port, conn)
	}
}

// blackhole is the "uplink: none" port: frames vanish.
type blackhole struct{}

func (blackhole) SendFrame([]byte) error { return nil }

// ── failover state machine ─────────────────────────────────────────────

const (
	beaconInterval = time.Second
	// A hub that has beaconed before and then goes silent for three
	// beats is treated as dead even though its socket may still accept:
	// it is wedged (SIGSTOP, deadlock). Hubs that never beaconed (older
	// binaries) are trusted until EOF — no false takeovers in mixed
	// fleets.
	beaconDeadline = 3 * beaconInterval
)

var errHubSilent = errors.New("no heartbeat for " + beaconDeadline.String())

// fleet runs switch mode's DISCOVER → PEER ⇄ HUB loop:
//
//   - DISCOVER: join a listening hub as a peer; nobody there, contend
//     for the hub seat (flock on <socket>.lock).
//   - PEER: wire frames guest↔hub and watch heartbeats; hub loss
//     returns to DISCOVER.
//   - HUB: own the socket, the uplink, DHCP and portfwd until shutdown.
//
// The same loop bootstraps a cold fleet: the first instance up takes
// the seat, the rest peer it. Split-brain is structurally impossible —
// the lock is exclusive and the socket is only bound while holding it.
type fleet struct {
	ep       *link.EtherEndpoint
	sw       *vswitch.Switch
	cfg      *config.Config
	descr    string
	sockPath string
	mode     uint32

	mu     sync.Mutex
	leases map[[6]byte][4]byte // DHCP state from hub gossip

	// The guest wire has exactly one reader for the process lifetime:
	// frames route to the hub connection in peer mode, into the switch
	// otherwise. Role changes are atomic pointer swaps, so a promotion
	// can never leave a second reader racing for the fd (each seqpacket
	// message is delivered to exactly one reader; a leaked pump would
	// silently eat guest frames).
	peerConn atomic.Pointer[unixseq.Conn]
	wireOnce sync.Once
	rxErr    chan error
}

// wire reads the guest socket forever, forwarding per current role.
func (f *fleet) wire() {
	buf := make([]byte, link.FrameMax)
	for {
		n, err := f.ep.RecvFrame(buf)
		if err != nil {
			f.rxErr <- err // guest side gone: shut the instance down
			return
		}
		if n < 14 {
			continue
		}
		frame := make([]byte, n)
		copy(frame, buf[:n])
		if peer := f.peerConn.Load(); peer != nil {
			if err := peer.SendFrame(frame); err == nil {
				continue
			} else {
				logf("guest frame to hub: %v", err)
			}
			// The hub died mid-flight; the switch takes it from here
			// (a no-op until runInstance adds the ports).
		}
		f.sw.Forward(vswitch.Local, frame)
	}
}

func (f *fleet) loop() {
	f.wireOnce.Do(func() { go f.wire() }) // peers never reach runInstance
	backoff := 250 * time.Millisecond
	for {
		// Join an existing hub?
		if conn, err := unixseq.TryConnect(f.sockPath); err == nil {
			backoff = 250 * time.Millisecond
			reason := f.runPeer(conn)
			logf("%s: hub lost (%v), rediscovering", f.descr, reason)
			time.Sleep(time.Duration(rand.Intn(250)) * time.Millisecond)
			continue
		}
		// Nobody answered: contend for the seat.
		release, lockErr := elect.TryLock(f.sockPath)
		if lockErr != nil {
			// Another instance is promoting (or a wedged hub still
			// holds the seat). Retry with jittered backoff.
			time.Sleep(backoff + time.Duration(rand.Intn(int(backoff/2))+1))
			if backoff < 2*time.Second {
				backoff *= 2
			}
			continue
		}
		// Under the seat: bind — or join a hub that appeared without
		// taking the seat (an older binary). We do not fight it.
		ln, conn, err := unixseq.BindOrConnect(f.sockPath, f.mode)
		if err != nil {
			release()
			fatalf("switch socket %s: %v", f.sockPath, err)
		}
		if conn != nil {
			release()
			continue
		}
		f.runInstance(ln, release) // returns only on shutdown
	}
}

// runPeer wires the local guest to the hub and watches its heartbeat.
// Returns when the hub is gone; the caller then redisCOVERs — possibly
// promoting this instance.
func (f *fleet) runPeer(hub *unixseq.Conn) error {
	defer func() {
		f.peerConn.Store(nil) // route the wire back to the switch
		hub.Close()
	}()
	f.peerConn.Store(hub)
	logf("peer of %s", f.sockPath)
	done := make(chan error, 1)

	// hub → guest, snooping heartbeats (control frames never reach the
	// guest) and caching the gossiped lease table for a later promotion
	var sawBeacon atomic.Bool
	var lastBeacon atomic.Int64
	go func() {
		buf := make([]byte, link.FrameMax)
		for {
			n, err := hub.RecvFrame(buf)
			if err != nil {
				done <- err
				return
			}
			if n >= 14 && elect.IsBeacon(buf[:n]) {
				lastBeacon.Store(time.Now().UnixNano())
				sawBeacon.Store(true)
				if _, leases, err := elect.ParseBeacon(buf[:n]); err == nil {
					f.mu.Lock()
					f.leases = leaseMap(leases)
					f.mu.Unlock()
				}
				continue
			}
			if n >= 14 {
				if err := f.ep.SendFrame(buf[:n]); err != nil {
					done <- err
					return
				}
			}
		}
	}()

	// heartbeat watch: armed only after the first beat proves the hub
	// speaks the beacon protocol
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(beaconInterval):
			}
			if !sawBeacon.Load() {
				continue
			}
			if time.Since(time.Unix(0, lastBeacon.Load())) > beaconDeadline {
				done <- errHubSilent
				return
			}
		}
	}()

	err := <-done
	close(stop)
	return err
}

// sendBeacons floods the switch with the hub's state once per interval.
// Runs until the process exits.
func (f *fleet) sendBeacons(pool *dhcp.Pool) {
	var seq uint32
	var leases []elect.Lease
	for {
		if pool != nil {
			leases = poolSnapshot(pool)
		}
		f.sw.Forward(vswitch.Uplink, elect.EncodeBeacon([]byte(vdeMAC), seq, leases))
		seq++
		time.Sleep(beaconInterval)
	}
}

func poolSnapshot(p *dhcp.Pool) []elect.Lease {
	snap := p.Leases()
	out := make([]elect.Lease, 0, len(snap))
	for macStr, ipStr := range snap {
		var l elect.Lease
		hw, err := net.ParseMAC(macStr)
		if err != nil {
			continue
		}
		copy(l.MAC[:], hw)
		ip := net.ParseIP(ipStr).To4()
		if ip == nil {
			continue
		}
		copy(l.IP[:], ip)
		out = append(out, l)
	}
	return out
}

func leaseMap(ls []elect.Lease) map[[6]byte][4]byte {
	m := make(map[[6]byte][4]byte, len(ls))
	for _, l := range ls {
		m[l.MAC] = l.IP
	}
	return m
}

// peerReader pumps one peer's frames into the switch; the peer port is
// removed when the pipe breaks.
func peerReader(sw *vswitch.Switch, port vswitch.Port, conn *unixseq.Conn) {
	reason := error(nil)
	defer func() {
		sw.RemovePort(port)
		conn.Close()
		logf("switch: peer %d gone (%v)", port, reason)
	}()
	buf := make([]byte, link.FrameMax)
	for {
		n, err := conn.RecvFrame(buf)
		if err != nil || n == 0 {
			reason = err
			return
		}
		if n < 14 {
			continue
		}
		frame := make([]byte, n)
		copy(frame, buf[:n])
		sw.Forward(port, frame)
	}
}

func waitShutdown(theNAT *nat.NAT, sw *vswitch.Switch, leasePool *dhcp.Pool, rxErr <-chan error, txErr <-chan error) {	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
loop:
	for {
		select {
		case err := <-rxErr:
			logf("guest link gone: %v", err)
			break loop
		case err := <-txErr:
			logf("netstack pump gone: %v", err)
			break loop
		case s := <-sigs:
			if s == syscall.SIGUSR1 {
				dumpStats(theNAT, sw, leasePool)
				continue
			}
			logf("received %v, exiting", s)
			break loop
		}
	}
	if theNAT != nil {
		theNAT.Close()
	}
}

// uplinkSink sends switched frames into netstack, except frames the
// netstack must not see: DHCP requests (answered at the frame layer) and
// ARP requests for addresses we don't own. With spoofing enabled,
// netstack's ARP responder claims EVERY requested address, hijacking
// guest-to-guest resolution on the switch — the real owner must answer.
type uplinkSink struct {
	ep   *link.EtherEndpoint
	dhcp *dhcp.Server
	sw   *vswitch.Switch
	own  map[[4]byte]bool // netstack's addresses on the segment
}

func (u uplinkSink) SendFrame(frame []byte) error {
	if u.dhcp != nil {
		if reply := u.dhcp.HandleFrame(frame); reply != nil {
			u.sw.Forward(vswitch.Uplink, reply) // flood to the guest port
			return nil
		}
	}
	if !arpForOwnAddr(frame, u.own) {
		return nil // foreign ARP request: shed, the owner answers via the switch
	}
	u.ep.Inject(frame)
	return nil
}

// arpForOwnAddr reports whether netstack should receive this frame:
// everything but ARP, plus ARP whose target protocol address is ours
// (requests we must answer, replies confirming our resolutions).
func arpForOwnAddr(frame []byte, own map[[4]byte]bool) bool {
	if len(frame) < 14 || binary.BigEndian.Uint16(frame[12:14]) != 0x0806 {
		return true
	}
	if len(frame) < 14+28 {
		return false // truncated ARP
	}
	arp := frame[14:]
	if binary.BigEndian.Uint16(arp[6:8]) != 1 { // not a request
		return true
	}
	var tpa [4]byte
	copy(tpa[:], arp[24:28])
	return own[tpa]
}

// ownAddrs collects the netstack's addresses on the segment.
func ownAddrs(cfg *config.Config) map[[4]byte]bool {
	own := map[[4]byte]bool{}
	for _, s := range []string{cfg.Gateway, cfg.Nameserver} {
		if ip := net.ParseIP(s).To4(); ip != nil {
			own[[4]byte{ip[0], ip[1], ip[2], ip[3]}] = true
		}
	}
	return own
}

// guestSink sends switched frames out the guest socket.
type guestSink struct{ ep *link.EtherEndpoint }

func (g guestSink) SendFrame(frame []byte) error { return g.ep.SendFrame(frame) }

// vdeMAC is the link address netstack uses on the wire (libslirp's SLIRP
// ARP sender MAC is arbitrary; keep the one the prototype used).
// tcpip.LinkAddress is raw bytes-as-string, NOT colon notation.
var vdeMAC = tcpip.LinkAddress(mustMACBytes("7a:72:6d:3c:d7:23"))

func mustMACBytes(s string) string {
	parts := strings.Split(s, ":")
	out := make([]byte, 6)
	for i, p := range parts {
		v, _ := strconv.ParseUint(p, 16, 8)
		out[i] = byte(v)
	}
	return string(out)
}

// dumpStats prints link, switch and netstack counters (SIGUSR1).
func dumpStats(n *nat.NAT, sw *vswitch.Switch, pool *dhcp.Pool) {
	if n == nil { // tap/none uplinks: no netstack to report
		logf("stats: switch_drops=%d", sw.Dropped())
		return
	}
	st := &n.EP.Stats
	logf("stats: rx=%d tx=%d rxbytes=%d txbytes=%d dropped=%d switch_drops=%d",
		st.RxPackets.Load(), st.TxPackets.Load(), st.RxBytes.Load(), st.TxBytes.Load(),
		st.Dropped.Load(), sw.Dropped())
	s := n.Stack
	logf("udp: in=%d unknownport=%d malformed=%d",
		s.Stats().UDP.PacketsReceived.Value(), s.Stats().UDP.UnknownPortErrors.Value(), s.Stats().UDP.MalformedPacketsReceived.Value())
	logf("ip: in=%d outerr=%d malformed=%d",
		s.Stats().IP.PacketsReceived.Value(), s.Stats().IP.OutgoingPacketErrors.Value(),
		s.Stats().IP.MalformedPacketsReceived.Value())
	if pool != nil {
		logf("dhcp leases: %v", pool.Leases())
	}
}

func parseArgs(argv []string) (descr string, fd int, vnl string) {
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if rest, ok := strings.CutPrefix(arg, "seqpacket://"); ok {
			fd, _ = strconv.Atoi(rest)
			continue
		}
		if rest, ok := strings.CutPrefix(arg, "--descr"); ok {
			if strings.HasPrefix(rest, "=") {
				descr = rest[1:]
			} else if rest == "" && i+1 < len(argv) {
				i++
				descr = argv[i]
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if descr == "" {
			descr = arg
		} else {
			vnl = arg
		}
	}
	return descr, fd, vnl
}
