// fakeguest is a stand-in for a UML guest: it spawns the real
// vde_plug helper with one end of a SOCK_SEQPACKET socketpair as the
// guest fd, and behaves like a minimal NIC on the other end —
// answering ARP for its IP and ICMP echo requests, optionally pinging
// another host. Built for testing a fleet without a UML kernel (CI,
// remote peers).
//
//	fakeguest -bin ./vde_plug -ip 10.0.2.42 -mac 02:de:ad:be:ef:42
//	fakeguest -bin ./vde_plug -ip 10.0.2.42 -ping 10.0.2.16 -n 4
//
// The helper's own arguments are fixed: it runs in switch mode with the
// switch socket from the config.yaml next to -bin, uplink none (the
// remote end has no uplink; the hub's NAT serves the fleet).
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

var (
	bin     = flag.String("bin", "./vde_plug", "helper binary to spawn")
	ip      = flag.String("ip", "10.0.2.42", "this fake NIC's IPv4")
	mac     = flag.String("mac", "02:de:ad:be:ef:42", "this fake NIC's MAC")
	sock    = flag.String("socket", "", "switch socket override (passed as socket= in the vnl)")
	ping    = flag.String("ping", "", "ARP+ICMP ping this IP, then exit")
	nPings  = flag.Int("n", 4, "number of echo requests in -ping mode")
	logArgs = flag.Bool("logs", true, "pass the helper's stderr through")
)

func mustMAC(s string) [6]byte {
	var m [6]byte
	hw, err := net.ParseMAC(s)
	if err != nil || len(hw) != 6 {
		log.Fatalf("bad -mac %q", s)
	}
	copy(m[:], hw)
	return m
}

func mustIP(s string) [4]byte {
	var out [4]byte
	v := net.ParseIP(s).To4()
	if v == nil {
		log.Fatalf("bad -ip %q", s)
	}
	copy(out[:], v)
	return out
}

func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum>>16 + sum&0xffff
	}
	return ^uint16(sum)
}

// arpFrame builds an Ethernet+ARP frame. op: 1=request, 2=reply.
func arpFrame(op uint16, sha [6]byte, spa [4]byte, tha [6]byte, tpa [4]byte) []byte {
	f := make([]byte, 42)
	copy(f[0:6], tha[:])
	copy(f[6:12], sha[:])
	binary.BigEndian.PutUint16(f[12:14], 0x0806)
	binary.BigEndian.PutUint16(f[14:16], 1) // ethernet
	binary.BigEndian.PutUint16(f[16:18], 0x0800)
	f[18] = 6
	f[19] = 4
	binary.BigEndian.PutUint16(f[20:22], op)
	copy(f[22:28], sha[:])
	copy(f[28:32], spa[:])
	copy(f[32:38], tha[:])
	copy(f[38:42], tpa[:])
	return f
}

// icmpEcho builds an Ethernet+IPv4+ICMP echo request (id=1, seq=seq).
func icmpEcho(srcMAC [6]byte, dstMAC [6]byte, srcIP, dstIP [4]byte, seq uint16) []byte {
	f := make([]byte, 14+20+8+56) // eth + ip + icmp hdr + 56B payload
	copy(f[0:6], dstMAC[:])
	copy(f[6:12], srcMAC[:])
	binary.BigEndian.PutUint16(f[12:14], 0x0800)
	ip := f[14:34]
	ip[0] = 0x45
	ip[1] = 0
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+8+56))
	ip[8] = 64
	ip[9] = 1 // ICMP
	copy(ip[12:16], srcIP[:])
	copy(ip[16:20], dstIP[:])
	binary.BigEndian.PutUint16(ip[10:12], checksum(ip))
	icmp := f[34:]
	icmp[0] = 8 // echo request
	binary.BigEndian.PutUint16(icmp[4:6], 1)
	binary.BigEndian.PutUint16(icmp[6:8], seq)
	for i := 8; i < len(icmp); i++ {
		icmp[i] = byte('a' + i%23)
	}
	binary.BigEndian.PutUint16(icmp[2:4], checksum(icmp))
	return f
}

func main() {
	flag.Parse()
	myMAC, myIP := mustMAC(*mac), mustIP(*ip)

	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		log.Fatal(err)
	}
	a, b := pair[0], pair[1]
	wire := os.NewFile(uintptr(b), "wire")
	defer wire.Close()

	vnl := "slirp://"
	if *sock != "" {
		vnl += "socket=" + *sock
	}
	cmd := exec.Command(*bin, "--descr", "fakeguest", "seqpacket://3", vnl)
	cmd.ExtraFiles = []*os.File{wire} // becomes fd 3 in the child
	cmd.Env = append(os.Environ(), "VDE_DEBUG=1")
	if *logArgs {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	wire.Close() // the child owns it now
	defer cmd.Process.Kill()

	conn := os.NewFile(uintptr(a), "nic")
	go func() { cmd.Wait(); os.Exit(0) }()

	if *ping == "" {
		serveNIC(conn, myMAC, myIP)
		return
	}
	runPing(conn, myMAC, myIP)
}

// serveNIC behaves like a real NIC: ARP replies for our IP, ICMP echo
// replies to anything.
func serveNIC(conn *os.File, myMAC [6]byte, myIP [4]byte) {
	buf := make([]byte, 9254)
	log.Printf("fakeguest up: mac=%s ip=%s", *mac, *ip)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			log.Fatalf("wire: %v", err)
		}
		if n < 14 {
			continue
		}
		switch binary.BigEndian.Uint16(buf[12:14]) {
		case 0x0806: // ARP
			if n >= 42 && binary.BigEndian.Uint16(buf[20:22]) == 1 &&
				string(buf[38:42]) == string(myIP[:]) {
				var sha [6]byte
				var spa, tpa [4]byte
				copy(sha[:], buf[22:28])
				copy(spa[:], buf[28:32])
				copy(tpa[:], buf[38:42])
				if _, err := conn.Write(arpFrame(2, myMAC, myIP, sha, spa)); err == nil {
					log.Printf("arp: answered %s -> %s", net.IP(spa[:]).String(), *ip)
				}
			}
		case 0x0800: // IPv4
			if n >= 38 && buf[23] == 1 { // ICMP
				icmp := buf[14:34]
				_ = icmp
				ihl := int(buf[14]&0xf) * 4
				if n >= 14+ihl+8 && buf[14+ihl] == 8 { // echo request
					reply := make([]byte, n)
					copy(reply, buf[:n])
					copy(reply[0:6], buf[6:12]) // swap eth addrs
					copy(reply[6:12], buf[0:6])
					copy(reply[12:14], buf[12:14])
					copy(reply[26:30], buf[30:34]) // swap IP addrs
					copy(reply[30:34], buf[26:30])
					reply[14+ihl] = 0 // echo reply
					// zero + recompute the IP checksum
					reply[24], reply[25] = 0, 0
					binary.BigEndian.PutUint16(reply[24:26], ipChecksum(reply[14:14+ihl]))
					icmpPart := reply[14+ihl:]
					icmpPart[0] = 0
					binary.BigEndian.PutUint16(icmpPart[2:4], icmpChecksum(icmpPart))
					if _, err := conn.Write(reply); err == nil {
						id := binary.BigEndian.Uint16(icmpPart[4:6])
						seq := binary.BigEndian.Uint16(icmpPart[6:8])
						log.Printf("icmp: echoed id=%d seq=%d", id, seq)
					}
				}
			}
		}
	}
}

func ipChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum>>16 + sum&0xffff
	}
	return ^uint16(sum)
}

func icmpChecksum(b []byte) uint16 {
	b[2], b[3] = 0, 0
	return ipChecksum(b)
}

// runPing ARPs the target, then sends ICMP echoes and prints the RTTs.
func runPing(conn *os.File, myMAC [6]byte, myIP [4]byte) {
	target := mustIP(*ping)

	// ARP: who has target?
	entries := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 9254)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				log.Fatalf("wire: %v", err)
			}
			if n > 0 {
				frame := make([]byte, n)
				copy(frame, buf[:n])
				entries <- frame
			}
		}
	}()

	var dstMAC [6]byte
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn.Write(arpFrame(1, myMAC, myIP, [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, target))
		select {
		case f := <-entries:
			if len(f) >= 42 && binary.BigEndian.Uint16(f[12:14]) == 0x0806 &&
				binary.BigEndian.Uint16(f[20:22]) == 2 &&
				string(f[28:32]) == string(target[:]) {
				copy(dstMAC[:], f[6:12])
			}
		case <-time.After(300 * time.Millisecond):
		}
		if dstMAC != ([6]byte{}) {
			break
		}
	}
	if dstMAC == ([6]byte{}) {
		log.Fatalf("no ARP reply from %s in 3s", *ping)
	}
	fmt.Printf("PING %s (%s): %d data bytes\n", *ping, macStr(dstMAC), 56)

	lost := 0
	for seq := 1; seq <= *nPings; seq++ {
		start := time.Now()
		conn.Write(icmpEcho(myMAC, dstMAC, myIP, target, uint16(seq)))
		got := false
		wait := time.NewTimer(2 * time.Second)
		for !got {
			select {
			case f := <-entries:
				if len(f) >= 38 && binary.BigEndian.Uint16(f[12:14]) == 0x0800 &&
					string(f[26:30]) == string(target[:]) && f[14+(int(f[14]&0xf)*4)] == 0 {
					fmt.Printf("64 bytes from %s: icmp_seq=%d time=%.3f ms\n",
						*ping, seq, float64(time.Since(start).Microseconds())/1000)
					got = true
				}
			case <-wait.C:
				fmt.Printf("request timeout for icmp_seq %d\n", seq)
				lost++
				got = true
			}
		}
		wait.Stop()
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("--- %s ping statistics --- %d packets transmitted, %d received, %d%% packet loss\n",
		*ping, *nPings, *nPings-lost, lost*100/max(*nPings, 1))
}

func ipStr(b []byte) string { return net.IP(b).String() }

func macStr(m [6]byte) string {
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", m[0], m[1], m[2], m[3], m[4], m[5])
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
