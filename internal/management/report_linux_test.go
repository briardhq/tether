//go:build linux

package management

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"briard.io/tether/internal/pipe"
)

// The socket takes the restart verb, so who can connect to it is who can reset the radio, and
// that is the user tether runs as and root. Connecting to a unix socket wants the write bit, and
// net.Listen leaves the file at 0777 &^ umask — so a lenient umask would hand the radio to every
// local user. The socket is made 0600 after the bind, and this is what pins it.
//
// The umask is forced lenient first, so the assertion cannot pass by inheriting a restrictive
// one — the point is that tether sets the mode itself rather than leaving it to whoever started
// it. Linux-only because a mode is a POSIX notion; on Windows the file's ACL decides.
func TestTheStatusSocketIsConnectableOnlyByItsOwner(t *testing.T) {
	old := unix.Umask(0)
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
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("the status socket is %v, want 0600 — any other mode lets a user who is not "+
			"tether's own reset its radio", perm)
	}
}

// The cost of a socket only its owner can open: an ordinary user asking about a tether that
// runs as root is refused by the kernel. That must read as "ask with sudo", never as "no tether
// is running" — the second sends them looking for a fault that is not there.
func TestATetherThatIsNotOursToAskSaysSudo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root connects to any socket, so there is no refusal to observe")
	}
	dir := socketDir(t)
	path := answerAt(t, dir, 4242, Card{Instance: "root's"})
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}

	for _, pid := range []int{0, 4242} {
		_, err := FindPeer([]string{dir}, pid)
		if err == nil || !strings.Contains(err.Error(), "sudo") {
			t.Errorf("FindPeer(pid %d) = %v, want the advice to ask with sudo", pid, err)
		}
	}
}
