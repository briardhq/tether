//go:build linux

package server

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// The contracted socket options, read back out of the kernel rather than trusted because we
// called a setter: "configured" is not "working". Reading them back is inherently
// platform-specific, which is why this test is, even though tune() is not.
func TestAcceptedConnectionsGetTheContractedSocketOptions(t *testing.T) {
	addr, _, _ := rig(t)

	// The server side of an accepted connection is not reachable from the test, so assert on
	// what tune() does by running it against a connection we hold.
	probe := dial(t, addr)
	tune(probe)

	raw, err := probe.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var nodelay, keepalive, idle int
	var opterr error
	if err := raw.Control(func(fd uintptr) {
		if nodelay, opterr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NODELAY); opterr != nil {
			return
		}
		if keepalive, opterr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_KEEPALIVE); opterr != nil {
			return
		}
		idle, opterr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_KEEPIDLE)
	}); err != nil {
		t.Fatalf("Control: %v", err)
	}
	if opterr != nil {
		t.Fatalf("getsockopt: %v", opterr)
	}
	if nodelay == 0 {
		t.Error("TCP_NODELAY is off")
	}
	if keepalive == 0 {
		t.Error("SO_KEEPALIVE is off")
	}
	if want := int(keepaliveIdle.Seconds()); idle != want {
		t.Errorf("TCP_KEEPIDLE is %ds, want %ds — the clients use a 15 s keepalive", idle, want)
	}
}
