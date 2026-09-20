// Package server accepts client connections and hands them to the pipe. It is part 2 of
// ARCHITECTURE.md "The shape": one listener, one client at a time, and the socket options the
// clients expect.
package server

import (
	"fmt"
	"log"
	"net"
	"time"

	"briard.io/tether/internal/pipe"
)

// The clients open a plain socket with TCP_NODELAY and a 15 s keepalive and then just talk —
// no handshake, no banner. Mirroring their keepalive is what makes a dead peer
// surface as a closed connection on our side too, in the same window they use.
const keepaliveIdle = 15 * time.Second

// Listen opens the listener. It is a separate call from Serve so that startup order is the
// caller's to get right: the device must be open, configured and drained before this runs,
// because a client's first byte must never race port setup (INV 2).
func Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}
	log.Printf("server: listening on %s", ln.Addr())
	return ln, nil
}

// Serve accepts connections and attaches each to the pipe, forever. It returns when the
// listener stops accepting.
//
// Accepting is a single goroutine on purpose: it serialises Attach, which is what lets
// takeover be a simple close-then-wait rather than an arbitration problem (INV 4).
func Serve(ln net.Listener, p *pipe.Pipe) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("accepting on %s: %w", ln.Addr(), err)
		}
		tune(conn)
		p.Attach(conn)
	}
}

// tune applies the contracted socket options. Failures are logged rather than fatal: a
// connection with default options still carries bytes correctly, and refusing to serve it
// would turn a latency question into an outage.
func tune(conn net.Conn) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	// Go enables TCP_NODELAY by default; set it anyway, because it is a named term of the
	// contract and a default is not a decision.
	if err := tc.SetNoDelay(true); err != nil {
		log.Printf("server: could not set TCP_NODELAY on %s: %v", tc.RemoteAddr(), err)
	}
	if err := tc.SetKeepAliveConfig(net.KeepAliveConfig{
		Enable:   true,
		Idle:     keepaliveIdle,
		Interval: keepaliveIdle,
	}); err != nil {
		log.Printf("server: could not set keepalive on %s: %v", tc.RemoteAddr(), err)
	}
}
