package management

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"time"

	"briard.io/tether/internal/pipe"
)

// socketDir is `t.TempDir()` with a short name, and the shortness is the whole point: a unix
// socket's path has to fit in `sun_path` — 108 bytes, on Windows as much as on Linux — and
// `t.TempDir()` spends that budget on the test's own name. Measured:
// `TestServeStatusTakesOverItsOwnStaleSocket` under a hosted runner's
// `C:\Users\RUNNER~1\AppData\Local\Temp\` comes to 109 bytes and `bind` answers `invalid
// argument`, which the test then reports as a tether that could not rebind its socket.
// Reproduced here with a long `TMPDIR`: 107 binds, 108 does not.
//
// Nothing shipped is near the limit — `/run/tether` and `C:\ProgramData\tether` leave seventy-odd
// bytes for a name that spends eighteen — so this is the harness's problem, and it belongs here
// rather than in an assertion that has something else to say.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "t")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// answerAt stands in for a running tether: something listening on a status socket that replies
// with a card. Two tethers on one host is two processes, which a test cannot be, so the socket
// name is written rather than derived — which is also the only way to test the two-tether case
// at all.
func answerAt(t *testing.T, dir string, pid int, card Card) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("%s%d%s", socketPrefix, pid, socketSuffix))
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	m := NewMonitor(pipe.New(nil), nil)
	m.radio = "znp"
	m.Serving(card.Instance, card.Listen)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			m.answer(conn)
		}
	}()
	return path
}

// The case the process id exists for: two tethers on one host, both discoverable, neither
// having taken the other's socket. Before this the second unlinked the first's path and the
// first was left listening on an inode nothing could reach.
func TestTwoTethersOnOneHostAreBothFound(t *testing.T) {
	dir := socketDir(t)
	answerAt(t, dir, 4242, Card{Instance: "Sonoff ZBDongle-P at nuc (tether)", Listen: ":6638"})
	answerAt(t, dir, 1111, Card{Instance: "ConBee II at nuc (tether)", Listen: ":6639"})

	peers := Peers(dir)
	if len(peers) != 2 {
		t.Fatalf("found %d tethers, want 2: %+v", len(peers), peers)
	}
	// Process-id order, so two runs of the same command list them the same way round.
	if peers[0].PID != 1111 || peers[1].PID != 4242 {
		t.Errorf("found %d then %d, want them in process-id order", peers[0].PID, peers[1].PID)
	}
	if peers[0].Card.Instance != "ConBee II at nuc (tether)" {
		t.Errorf("the card does not belong to the socket it came from: %+v", peers[0])
	}
}

// A killed tether leaves its socket behind — always on Windows, and on Linux for anything
// within one boot. Offering a dead one to somebody choosing which tether to ask would be
// offering them a thing that cannot answer.
func TestASocketNothingAnswersOnIsNotATether(t *testing.T) {
	dir := socketDir(t)
	answerAt(t, dir, 4242, Card{Instance: "the live one"})
	stale := filepath.Join(dir, socketPrefix+"9999"+socketSuffix)
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Not one of ours at all, and not a reason to fail either.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	peers := Peers(dir)
	if len(peers) != 1 || peers[0].PID != 4242 {
		t.Fatalf("found %+v, want only the live tether", peers)
	}
	// Left alone rather than cleaned up: another process may be a moment from binding it, and
	// the only socket a tether may remove is its own.
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("the stale socket was deleted by a reader: %v", err)
	}
}

func TestFindPeer(t *testing.T) {
	empty := t.TempDir()
	if _, err := FindPeer([]string{empty}, 0); err == nil || !strings.Contains(err.Error(), "no tether") {
		t.Errorf("an empty directory gave %v, want a plain \"no tether\"", err)
	}
	// A directory that was never created is the ordinary reason there is no tether, and must
	// read the same way rather than as a filesystem error.
	if _, err := FindPeer([]string{filepath.Join(empty, "never-made")}, 0); err == nil ||
		!strings.Contains(err.Error(), "no tether") {
		t.Errorf("a missing directory gave %v, want a plain \"no tether\"", err)
	}

	one := socketDir(t)
	answerAt(t, one, 4242, Card{Instance: "the only one"})
	peer, err := FindPeer([]string{one}, 0)
	if err != nil {
		t.Fatalf("one tether running and it was not found: %v", err)
	}
	if peer.Card.Instance != "the only one" {
		t.Errorf("found %+v", peer)
	}

	two := socketDir(t)
	answerAt(t, two, 4242, Card{Instance: "Sonoff ZBDongle-P at nuc (tether)", Listen: ":6638"})
	answerAt(t, two, 1111, Card{Instance: "ConBee II at nuc (tether)", Listen: ":6639"})

	// Two is a question only the reader can settle, so it is named rather than guessed at —
	// the same refusal tether makes about two attached adapters.
	_, err = FindPeer([]string{two}, 0)
	if err == nil {
		t.Fatal("FindPeer chose between two tethers")
	}
	for _, want := range []string{"-pid 1111", "-pid 4242", "ConBee II at nuc", "serving :6638"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q, so the reader cannot act on it:\n%v", want, err)
		}
	}

	// And -pid settles it.
	if peer, err = FindPeer([]string{two}, 1111); err != nil {
		t.Fatalf("-pid 1111 did not find it: %v", err)
	}
	if peer.Card.Instance != "ConBee II at nuc (tether)" {
		t.Errorf("-pid 1111 found %+v", peer)
	}
	if _, err = FindPeer([]string{two}, 7777); err == nil {
		t.Error("-pid found a tether that is not running")
	}

	// A reader searches every scope a tether can serve in, because it is rarely in the same one
	// (SocketDirs): the operator asking has a runtime directory of their own and the tether
	// they mean is usually the system's. Asked about both, it finds the one that exists — and
	// a directory that has nothing in it is not an answer, it is one place that did not have it.
	peer, err = FindPeer([]string{empty, one}, 0)
	if err != nil {
		t.Fatalf("a tether in the second directory was not found: %v", err)
	}
	if peer.Card.Instance != "the only one" {
		t.Errorf("searching two directories found %+v", peer)
	}
	// And one in each scope is still two tethers, so it still refuses to choose between them.
	other := socketDir(t)
	answerAt(t, other, 5555, Card{Instance: "a user's own", Listen: ":6640"})
	_, err = FindPeer([]string{one, other}, 0)
	if err == nil {
		t.Fatal("FindPeer chose between two tethers in different directories")
	}
	for _, want := range []string{"-pid 4242", "-pid 5555", "a user's own"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q from both directories:\n%v", want, err)
		}
	}
}

// The name is the contract between a tether and whatever reads its card, so the two halves of
// it — writing and parsing — are asserted against each other rather than separately.
func TestSocketPathCarriesTheProcessID(t *testing.T) {
	path := SocketPathIn("/run/tether")
	pid, ok := pidOf(filepath.Base(path))
	if !ok {
		t.Fatalf("SocketPathIn produced %q, which pidOf does not recognise", path)
	}
	if pid != os.Getpid() {
		t.Errorf("the path carries process id %d, want this process's %d", pid, os.Getpid())
	}
	for _, name := range []string{"status.sock", "status..sock", "status.-1.sock", "status.abc.sock", "other.1.sock", ""} {
		if _, ok := pidOf(name); ok {
			t.Errorf("pidOf accepted %q", name)
		}
	}
}

// Removing a stale socket before binding is what makes a restart work, and with the process id
// in the name it is provably safe rather than merely usual — no live process shares our id, so
// anything already there was left by a dead one.
func TestServeStatusTakesOverItsOwnStaleSocket(t *testing.T) {
	dir := socketDir(t)
	path := SocketPathIn(dir)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewMonitor(pipe.New(nil), nil)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() { served <- m.ServeStatus(ctx, path) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := ReadStatus(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	// What ServeStatus said, if it said anything, rather than this test's theory of why the
	// socket is not there. The theory was wrong the one time this fired: the path was too long
	// to bind at all, and the message blamed the takeover it never reached.
	select {
	case err := <-served:
		t.Errorf("ServeStatus gave up rather than taking over the stale socket: %v", err)
	default:
		t.Error("a tether could not rebind its own socket after being killed")
	}
}
