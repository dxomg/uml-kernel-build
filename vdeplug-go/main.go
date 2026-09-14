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
// Switch mode (config `switch: true`, default): the first instance to bind
// the switch socket becomes the hub and owns the uplink; later instances are
// plain peer wires into the hub. M3 wires that path; today every instance
// runs standalone with its own NAT.
package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"uml-kernel-build/vdeplug-go/internal/config"
	"uml-kernel-build/vdeplug-go/internal/dhcp"
	"uml-kernel-build/vdeplug-go/internal/link"
	"uml-kernel-build/vdeplug-go/internal/nat"
	"uml-kernel-build/vdeplug-go/internal/portfwd"
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
		logf("uplink %q not supported yet (M4); using slirp", cfg.Uplink)
	}

	ep := link.New(fd, link.FrameMax-18, vdeMAC)
	sw := vswitch.New()

	// The uplink: netstack NAT. Its TX frames are switched like any port.
	theNAT, err := nat.New(ep, nat.Options{
		GatewayIP:  cfg.Gateway,
		Nameserver: cfg.Nameserver,
		Network:    cfg.Network,
		IPv6:       cfg.IPv6,
		MTU:        cfg.MTU,
	})
	if err != nil {
		fatalf("nat: %v", err)
	}

	// DHCP leases for the guest (replaces libslirp's BOOTP backend).
	// Handled at the frame layer: netstack sheds limited-broadcast
	// DISCOVERs before the transport demuxer sees them.
	var leasePool *dhcp.Pool
	var dhcpSrv *dhcp.Server
	if start := net.ParseIP(cfg.DHCPStart); start != nil && cfg.DHCPStart != "" {
		leasePool = dhcp.NewPool(start, 256)
		dhcpSrv = dhcp.New(dhcp.Options{
			Network:    cfg.Network,
			GatewayIP:  cfg.Gateway,
			Nameserver: cfg.Nameserver,
			MTU:        cfg.MTU,
		}, leasePool, vdeMAC)
	}

	ep.SetSink(func(frame []byte) { sw.Forward(vswitch.Uplink, frame) })
	sw.AddPort(vswitch.Uplink, uplinkSink{ep: ep, dhcp: dhcpSrv, sw: sw})

	// Netstack TX pump: pops packets netstack emits and switches them.
	txErr := make(chan error, 1)
	go func() { txErr <- ep.Start() }()

	// The guest port: reader goroutine pushes frames into the switch.
	sw.AddPort(vswitch.Local, guestSink{ep})
	rxErr := make(chan error, 1)
	go func() {
		buf := make([]byte, link.FrameMax)
		for {
			n, err := ep.RecvFrame(buf)
			if err != nil {
				rxErr <- err
				return
			}
			if n < 14 {
				continue
			}
			frame := make([]byte, n)
			copy(frame, buf[:n])
			sw.Forward(vswitch.Local, frame)
		}
	}()

	// Status line, same shape as the C binary's output.
	fmt.Fprintf(os.Stderr, "[vde_plug-go] %s: standalone slirp nat on %s gw %s dns %s\n",
		descr, cfg.Network, cfg.Gateway, cfg.Nameserver)

	// DHCP leases for the guest: created above (before the uplink wiring).

	// Host-side listeners relaying into the guest (reverse NAT).
	if cfg.PortFwd {
		fallback := net.ParseIP(cfg.DHCPStart)
		pf, err := portfwd.Start(theNAT.Stack, cfg, fallback)
		if err != nil {
			logf("portfwd: %v", err)
		} else {
			_ = pf
		}
	}

	sigs := make(chan os.Signal, 2)
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
	theNAT.Close()
	os.Exit(0)
}

// uplinkSink sends switched frames into netstack, except DHCP requests,
// which the frame-layer DHCP service answers directly.
type uplinkSink struct {
	ep   *link.EtherEndpoint
	dhcp *dhcp.Server
	sw   *vswitch.Switch
}

func (u uplinkSink) SendFrame(frame []byte) error {
	if u.dhcp != nil {
		if reply := u.dhcp.HandleFrame(frame); reply != nil {
			u.sw.Forward(vswitch.Uplink, reply) // flood to the guest port
			return nil
		}
	}
	u.ep.Inject(frame)
	return nil
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
	st := n.EP.Stats
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
