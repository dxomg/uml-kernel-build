// Package tap attaches the switch to a host tap device (uplink
// "tap:NAME"): the guest joins the host's L2 as-is — no NAT, no DHCP
// from us; the tap side owns addressing. Creating the device needs
// CAP_NET_ADMIN (setcap the binary or pre-create persistent taps).
package tap

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	iffTap    = 0x0002
	iffNoPI   = 0x1000
	tunSetIFF = 0x400454ca // TUNSETIFF
)

// Device is an open tap interface.
type Device struct {
	f    *os.File
	name string
}

// ifreq mirrors struct ifreq for TUNSETIFF.
type ifreq struct {
	Name  [16]byte
	Flags uint16
	_     [22]byte
}

// Open attaches to the tap device name, creating it for the lifetime of
// the handle (non-persistent taps vanish on Close).
func Open(name string) (*Device, error) {
	if len(name) > 15 {
		return nil, fmt.Errorf("tap name %q longer than 15 chars", name)
	}
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}
	req := ifreq{Flags: iffTap | iffNoPI}
	copy(req.Name[:], name)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), tunSetIFF, uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("TUNSETIFF %s: %w", name, errno)
	}
	actual := strings.TrimRight(string(req.Name[:]), "\x00")
	return &Device{
		f:    os.NewFile(uintptr(fd), "tap:"+actual),
		name: actual,
	}, nil
}

// Name returns the interface name the kernel chose.
func (d *Device) Name() string { return d.name }

// SendFrame writes one frame to the host (implements vswitch.Sink).
func (d *Device) SendFrame(frame []byte) error {
	_, err := d.f.Write(frame)
	return err
}

// RecvFrame reads one frame from the host. Blocking; retries EINTR.
func (d *Device) RecvFrame(buf []byte) (int, error) {
	for {
		n, err := d.f.Read(buf)
		if err == unix.EINTR {
			continue
		}
		return n, err
	}
}

// Close drops the device.
func (d *Device) Close() error { return d.f.Close() }
