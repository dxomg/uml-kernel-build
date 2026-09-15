// Package unixseq is the SOCK_SEQPACKET unix switch socket: the first
// vde_plug on a machine binds it and becomes the hub; later ones connect
// and become plain peer wires. Framing is one Ethernet frame per message,
// no handshake — same protocol as the C binary's hub.
package unixseq

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Listener is a bound, listening SEQPACKET socket.
type Listener struct {
	fd   int
	path string
	// fileIno/fileDev = the socket file's identity captured at bind
	// time (fstat on the fd would report the sockfs inode, not the
	// filesystem file's).
	fileIno uint64
	fileDev uint64
}

// TryConnect joins an existing switch at path, or returns an error when
// nobody is listening. Unlike BindOrConnect it never binds, so the
// failover state machine can probe without taking the hub seat.
func TryConnect(path string) (*Conn, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &Conn{fd: fd}, nil
}

// BindOrConnect tries to join an existing switch at path; when nobody
// answers it binds the socket and returns a Listener (hub side).
//
// A stale socket file blocks bind(), so it is unlinked once connect has
// failed — safe, same as the C binary. Returns either ln != nil (hub) or
// conn != nil (peer).
func BindOrConnect(path string, mode uint32) (*Listener, *Conn, error) {
	addr := &unix.SockaddrUnix{Name: path}

	// 1. Join an existing switch?
	connFD, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socket: %w", err)
	}
	if err := unix.Connect(connFD, addr); err == nil {
		return nil, &Conn{fd: connFD}, nil
	}
	unix.Close(connFD)

	// 2. Nobody answered. One retry before declaring the file stale —
	// a transient refusal against a live listener must not end with us
	// unlinking someone else's socket.
	connFD2, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socket: %w", err)
	}
	if err := unix.Connect(connFD2, addr); err == nil {
		return nil, &Conn{fd: connFD2}, nil
	}
	unix.Close(connFD2)
	_ = os.Remove(path)

	hubFD, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socket: %w", err)
	}
	if err := unix.Bind(hubFD, addr); err != nil {
		unix.Close(hubFD)
		// Lost the race: another instance bound between our calls.
		connFD, err2 := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
		if err2 != nil {
			return nil, nil, fmt.Errorf("socket: %w", err2)
		}
		if err2 := unix.Connect(connFD, addr); err2 == nil {
			return nil, &Conn{fd: connFD}, nil
		}
		unix.Close(connFD)
		return nil, nil, fmt.Errorf("bind %s: %w", path, err)
	}
	if err := unix.Listen(hubFD, 32); err != nil {
		unix.Close(hubFD)
		_ = os.Remove(path)
		return nil, nil, fmt.Errorf("listen %s: %w", path, err)
	}
	socketMode := mode
	if socketMode == 0 {
		socketMode = 0o600
	}
	_ = os.Chmod(path, os.FileMode(socketMode))

	ln := &Listener{fd: hubFD, path: path}
	if st, err := Stat(path); err == nil {
		ln.fileIno, ln.fileDev = st.Ino, st.Dev
	}
	return ln, nil, nil
}

// Accept returns the next peer connection fd.
func (l *Listener) Accept() (*Conn, error) {
	fd, _, err := unix.Accept(l.fd)
	if err != nil {
		return nil, err
	}
	return &Conn{fd: fd}, nil
}

// Stat returns the socket FILE's inode identity (captured at bind
// time), for callers to verify the path still resolves to this
// listener.
func (l *Listener) Stat() (Stat_t, error) {
	return Stat_t{Ino: l.fileIno, Dev: l.fileDev}, nil
}

// Close stops listening and removes the socket file — only when the
// path still refers to the file created at bind time. A rogue rebinder
// (or another helper that cleared a "stale" file) must not have its
// live socket unlinked from under it.
func (l *Listener) Close() {
	unix.Close(l.fd)
	if l.fileIno == 0 {
		return // never captured: be conservative, leave the file alone
	}
	pathSt, err := Stat(l.path)
	if err != nil {
		return // file already gone
	}
	if pathSt.Ino == l.fileIno && pathSt.Dev == l.fileDev {
		_ = os.Remove(l.path)
	}
}

// Stat_t and Stat re-export the unix stat plumbing so callers can
// verify socket-file identity without pulling x/sys/unix in directly.
type Stat_t = unix.Stat_t

func Stat(path string) (Stat_t, error) {
	var st Stat_t
	err := unix.Stat(path, &st)
	return st, err
}

// Conn is one SEQPACKET connection (a peer wire).
type Conn struct {
	fd int
}

// SendFrame writes one frame, retrying EINTR.
func (c *Conn) SendFrame(b []byte) error {
	for {
		err := unix.Sendmsg(c.fd, b, nil, nil, 0)
		if err == unix.EINTR {
			continue
		}
		return err
	}
}

// RecvFrame reads one frame, retrying EINTR. A peer close surfaces as
// io.EOF instead of a silent (0, nil), so readers cannot spin on it —
// this is also how a peer learns its hub died.
func (c *Conn) RecvFrame(buf []byte) (int, error) {
	for {
		n, _, _, _, err := unix.Recvmsg(c.fd, buf, nil, 0)
		if err == unix.EINTR {
			continue
		}
		if n == 0 && err == nil {
			return 0, io.EOF
		}
		return n, err
	}
}

// Close drops the connection.
func (c *Conn) Close() { unix.Close(c.fd) }

// Fd exposes the raw socket fd (for wrapping in an os.File).
func (c *Conn) Fd() int { return c.fd }
