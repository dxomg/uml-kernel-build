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

	// 2. Nobody answered: clear the stale file and take the socket.
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

	return &Listener{fd: hubFD, path: path}, nil, nil
}

// Accept returns the next peer connection fd.
func (l *Listener) Accept() (*Conn, error) {
	fd, _, err := unix.Accept(l.fd)
	if err != nil {
		return nil, err
	}
	return &Conn{fd: fd}, nil
}

// Close stops listening and removes the socket file.
func (l *Listener) Close() {
	unix.Close(l.fd)
	_ = os.Remove(l.path)
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
