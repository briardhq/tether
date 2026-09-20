//go:build linux

package management

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"briard.io/tether/internal/pipe"
)

// "Observable from outside" means from another user's shell, not only from the service user's.
// Connecting to a unix socket wants the write bit, and net.Listen leaves the file at
// 0777 &^ umask — 0755 under a service manager — which is connectable by the user tether runs
// as and by nobody else: an operator at a shell, or briard's agent, gets permission denied
// whichever user that is. The card is read-only and every field on it is already in the log,
// so the socket is made 0666 after the bind, and this is what pins it.
//
// The umask is forced restrictive first, so the assertion cannot pass by inheriting a lenient
// one — the point is that tether sets the mode itself rather than leaving it to whoever started
// it. Linux-only because a mode is a POSIX notion; on Windows the file's ACL decides.
func TestTheStatusSocketIsConnectableByAnyLocalUser(t *testing.T) {
	old := unix.Umask(0o077)
	defer unix.Umask(old)

	path := filepath.Join(socketDir(t), "status.sock")
	m := NewMonitor(pipe.New(nil), nil)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = m.ServeStatus(ctx, path) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := ReadStatus(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing answered on the status socket")
		}
		time.Sleep(10 * time.Millisecond)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o666 {
		t.Errorf("the status socket is %v, want 0666 — a reader that is not the user tether "+
			"runs as cannot connect to it", perm)
	}
}
