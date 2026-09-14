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

	ep := link.New(fd, link.FrameMax-18, tcpip.LinkAddress(vdeMAC))
	sw := vswitch.New()

	// The uplink: netstack NAT. Its TX frames are switched like any port.
	theNAT, err := nat.New(ep, nat.Options{
		GatewayIP:  cfg.Gateway,
		Nameserver: cfg.Nameserver,
		Network:    cfg.Network,
	})
	if err != nil {
		fatalf("nat: %v", err)
	}
	ep.SetSink(func(frame []byte) { sw.Forward(vswitch.Uplink, frame) })
	sw.AddPort(vswitch.Uplink, uplinkSink{ep})

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
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-rxErr:
		logf("guest link gone: %v", err)
	case err := <-txErr:
		logf("netstack pump gone: %v", err)
	case s := <-sigs:
		logf("received %v, exiting", s)
	}
	theNAT.Close()
	os.Exit(0)
}

// uplinkSink sends switched frames into netstack.
type uplinkSink struct{ ep *link.EtherEndpoint }

func (u uplinkSink) SendFrame(frame []byte) error { u.ep.Inject(frame); return nil }

// guestSink sends switched frames out the guest socket.
type guestSink struct{ ep *link.EtherEndpoint }

func (g guestSink) SendFrame(frame []byte) error { return g.ep.SendFrame(frame) }

// vdeMAC is the link address netstack uses on the wire (libslirp's SLIRP
// ARP sender MAC is arbitrary; keep the one the prototype used).
const vdeMAC = "7a:72:6d:3c:d7:23"

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
