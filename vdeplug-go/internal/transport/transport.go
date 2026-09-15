// Package transport abstracts the switch socket: a unix SOCK_SEQPACKET
// socket on one machine, or a length-prefixed TCP socket across machines.
// The address grammar (shared with config socket_file_location):
//
//	/tmp/vde.socket     unix socket file
//	203.0.113.7:9100    TCP host:port (remote raw socket)
package transport

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"uml-kernel-build/vdeplug-go/internal/unixseq"
)

// FrameMax mirrors link.FrameMax (kept local: transport must not depend
// on the link layer).
const FrameMax = 9216 + 14 + 4

// Conn is one peer wire (or the guest-side view of a hub wire).
type Conn interface {
	SendFrame([]byte) error
	RecvFrame([]byte) (int, error)
	Close()
}

// Listener accepts peer wires; the unix variant removes its socket file
// on Close.
type Listener interface {
	Accept() (Conn, error)
	Close()
}

// IsTCP reports whether addr names a TCP socket rather than a file path.
func IsTCP(addr string) bool {
	if strings.Contains(addr, "/") {
		return false
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	_, err = strconv.Atoi(port)
	return err == nil
}

// TryConnect joins an existing switch at addr without binding anything.
func TryConnect(addr string) (Conn, error) {
	if IsTCP(addr) {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			return nil, err
		}
		return tcpConn{c.(*net.TCPConn)}, nil
	}
	c, err := unixseq.TryConnect(addr)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// BindOrConnect joins an existing switch at addr, or binds it (becoming
// the hub) when nobody answers. Returns either ln != nil (hub side) or
// conn != nil (peer side).
func BindOrConnect(addr string, mode uint32) (Listener, Conn, error) {
	if IsTCP(addr) {
		if c, err := TryConnect(addr); err == nil {
			return nil, c, nil
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, nil, fmt.Errorf("listen %s: %w", addr, err)
		}
		return tcpListener{l.(*net.TCPListener)}, nil, nil
	}
	ln, conn, err := unixseq.BindOrConnect(addr, mode)
	if err != nil {
		return nil, nil, err
	}
	if conn != nil {
		return nil, conn, nil
	}
	return unixListener{ln}, nil, nil
}

// --- unix adapters (the concrete types already have the right shapes) ---

type unixConn struct{ *unixseq.Conn }

type unixListener struct{ l *unixseq.Listener }

func (w unixListener) Accept() (Conn, error) {
	c, err := w.l.Accept()
	if err != nil {
		return nil, err
	}
	return unixConn{c}, nil
}

func (w unixListener) Close() { w.l.Close() }

// --- TCP: 2-byte big-endian length prefix per frame ---

type tcpConn struct{ c *net.TCPConn }

func (t tcpConn) SendFrame(b []byte) error {
	if len(b) > FrameMax {
		return fmt.Errorf("frame %d exceeds %d", len(b), FrameMax)
	}
	head := make([]byte, 2, 2+len(b))
	binary.BigEndian.PutUint16(head, uint16(len(b)))
	head = append(head, b...)
	_, err := t.c.Write(head)
	return err
}

func (t tcpConn) RecvFrame(buf []byte) (int, error) {
	var head [2]byte
	if _, err := io.ReadFull(t.c, head[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(head[:]))
	if n == 0 {
		return 0, io.EOF
	}
	if n > FrameMax || n > len(buf) {
		return 0, fmt.Errorf("peer frame %d exceeds buffer", n)
	}
	if _, err := io.ReadFull(t.c, buf[:n]); err != nil {
		return 0, err
	}
	return n, nil
}

func (t tcpConn) Close() { t.c.Close() }

type tcpListener struct{ l *net.TCPListener }

func (t tcpListener) Accept() (Conn, error) {
	c, err := t.l.Accept()
	if err != nil {
		return nil, err
	}
	return tcpConn{c.(*net.TCPConn)}, nil
}

func (t tcpListener) Close() { t.l.Close() }
