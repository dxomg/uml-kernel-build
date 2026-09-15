// vdews bridges the vde switch socket across the internet when neither
// end has a routable address: a WebSocket in the middle, TLS terminated
// by the reverse proxy in front of it.
//
//	serve:   vdews -mode serve -listen :5001 -upstream /tmp/vde.socket -token S
//	         (one WebSocket connection per peer wire; frames are one
//	          binary message each, so boundaries survive end-to-end)
//	connect: vdews -mode connect -downstream /tmp/vde-remote.sock \
//	            -url wss://quangdz.exe.xyz/vde -token S
//	         (exposes the remote switch as a LOCAL unix socket; each
//	          accepted local connection opens its own WebSocket, so
//	          vde_plug's own retry loop drives reconnects)
//
// Token auth: the client appends ?token=...; compared in constant time.
package main

import (
	"crypto/subtle"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"golang.org/x/net/websocket"
	"uml-kernel-build/vdeplug-go/internal/unixseq"
)

var (
	mode       = flag.String("mode", "", "serve | connect")
	listen     = flag.String("listen", ":5001", "serve: HTTP listen address")
	path       = flag.String("path", "/vde", "serve: WebSocket path")
	upstream   = flag.String("upstream", "/tmp/vde.socket", "serve: local switch socket")
	downstream = flag.String("downstream", "/tmp/vde-remote.sock", "connect: local socket to expose the remote switch on")
	url        = flag.String("url", "", "connect: WebSocket URL of the remote bridge")
	token      = flag.String("token", "", "shared auth token")
)

// pump copies frames between a WebSocket and a seqpacket unix connection
// until either side dies.
func pump(ws *websocket.Conn, file *os.File) {
	errc := make(chan error, 2)
	go func() { // ws -> guest wire
		for {
			var frame []byte
			if err := websocket.Message.Receive(ws, &frame); err != nil {
				errc <- err
				return
			}
			if len(frame) > 0 {
				if _, err := file.Write(frame); err != nil {
					errc <- err
					return
				}
			}
		}
	}()
	go func() { // guest wire -> ws
		buf := make([]byte, 9254)
		for {
			n, err := file.Read(buf)
			if err != nil {
				errc <- err
				return
			}
			if n > 0 {
				if err := websocket.Message.Send(ws, buf[:n]); err != nil {
					errc <- err
					return
				}
			}
		}
	}()
	<-errc // first failure tears the pair down
	ws.Close()
	file.Close()
	<-errc
}

func serveWS(ws *websocket.Conn) {
	up, err := unixseq.TryConnect(*upstream)
	if err != nil {
		log.Printf("upstream %s: %v", *upstream, err)
		return
	}
	file := os.NewFile(uintptr(up.Fd()), "upstream")
	pump(ws, file)
}

func acceptLoop(ln *unixseq.Listener, wsURL string, done chan struct{}) {
	defer close(done)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed: rebinding or shutting down
		}
		go func() {
			file := os.NewFile(uintptr(conn.Fd()), "peer")
			ws, err := websocket.Dial(wsURL, "", "https://localhost")
			if err != nil {
				log.Printf("dial %s: %v", *url, err)
				file.Close()
				return
			}
			log.Printf("bridge up: %s", *url)
			pump(ws, file)
			log.Printf("bridge down: %s", *url)
		}()
	}
}

// watchListener blocks until the socket file no longer refers to this
// listener's inode (someone rebound or unlinked the path), then closes
// the listener so the outer loop rebinds.
func watchListener(ln *unixseq.Listener, done <-chan struct{}) {
	own, err := ln.Stat()
	if err != nil {
		return
	}
	for {
		select {
		case <-done:
			return
		case <-time.After(2 * time.Second):
		}
		cur, err := unixseq.Stat(*downstream)
		if err != nil || cur.Ino != own.Ino || cur.Dev != own.Dev {
			ln.Close()
			return
		}
	}
}

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags)

	switch *mode {
	case "serve":
		mux := http.NewServeMux()
		mux.HandleFunc(*path, func(w http.ResponseWriter, r *http.Request) {
			if *token != "" &&
				subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(*token)) != 1 {
				http.Error(w, "forbidden", http.StatusForbidden)
				log.Printf("reject %s: bad token", r.RemoteAddr)
				return
			}
			websocket.Handler(serveWS).ServeHTTP(w, r)
		})
		log.Printf("vdews serve %s%s -> %s", *listen, *path, *upstream)
		log.Fatal(http.ListenAndServe(*listen, mux))

	case "connect":
		wsURL := *url
		if *token != "" {
			sep := "?"
			for i := 0; i < len(wsURL); i++ {
				if wsURL[i] == '?' {
					sep = "&"
					break
				}
			}
			wsURL += sep + "token=" + *token
		}
		// A helper that declares our socket file stale unlinks it; when
		// the path no longer resolves to our listener's inode, rebind a
		// fresh listener so the bridge stays reachable.
		for {
			ln, _, err := unixseq.BindOrConnect(*downstream, 0o600)
			if err != nil {
				log.Fatal(err)
			}
			if ln == nil {
				log.Fatalf("%s: someone is already listening", *downstream)
			}
			log.Printf("listening on %s", *downstream)
			done := make(chan struct{})
			go acceptLoop(ln, wsURL, done)
			watchListener(ln, done)
			log.Printf("socket file replaced under us; rebinding")
		}

	default:
		log.Fatalf("unknown -mode %q (serve|connect)", *mode)
	}
}
