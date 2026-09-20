package management

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// socketPrefix and socketSuffix bracket the process id in a status socket's name.
const (
	socketPrefix = "status."
	socketSuffix = ".sock"
)

// SocketPath is where this process answers `tether status`.
//
// **The process id is in the name, and that is what makes a second tether on one host safe.**
// One host runs two tethers whenever it has two dongles — two processes, not a scope change —
// and a single shared path made that silently destructive: ServeStatus removes a stale socket
// before binding, so the second tether would unlink the first's and the first would be left
// listening on an inode nothing could reach. With the id in the name there is nothing to
// collide over, and the removal becomes provably safe rather than merely usual: no live
// process shares our id, so anything already at this path was left by a dead one.
//
// The cost is that the path is not a constant another program can hardcode, which is why
// Peers exists — finding a tether is a directory read rather than a guess, and briard reads the
// card the same way `tether status` does.
func SocketPath() string { return SocketPathIn(SocketDir()) }

// SocketDir is where this process serves, which is the first of the places a reader looks.
//
// Serving in the directory searched first is what makes the two sides one decision rather than
// two that have to be kept in step: SocketDirs orders them by what this process may create, and
// the head of that list is therefore both "where I can bind" and "where I would look first".
func SocketDir() string { return SocketDirs()[0] }

// SocketPathIn is SocketPath with the directory said out loud, for a caller that has its own —
// which in practice means a test, since the real directory is not writable by an ordinary user.
func SocketPathIn(dir string) string {
	return filepath.Join(dir, socketPrefix+strconv.Itoa(os.Getpid())+socketSuffix)
}

// Peer is one tether running on this host, and the card it answered with.
type Peer struct {
	PID  int
	Path string
	Card Card
}

// Peers reads every tether answering in dir, in process-id order.
//
// **A socket nothing answers on is skipped rather than reported**, because a killed tether
// leaves its socket file behind — on Windows especially, where the directory is not cleared at
// boot — and a dead tether must not be offered to someone choosing which one to ask. It is also
// not deleted: removing a path another process may be a moment away from binding is a race, and
// the only path a tether may safely remove is its own (see SocketPath).
//
// An unreadable directory is no tethers rather than an error. The commonest reason for one is
// the ordinary reason there are none — nothing has ever created it.
func Peers(dir string) []Peer {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var peers []Peer
	for _, e := range entries {
		pid, ok := pidOf(e.Name())
		if !ok {
			continue
		}
		path := filepath.Join(dir, e.Name())
		card, err := ReadStatus(path)
		if err != nil {
			// Left by a tether that is gone. Nothing to say that anyone can act on.
			continue
		}
		peers = append(peers, Peer{PID: pid, Path: path, Card: card})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].PID < peers[j].PID })
	return peers
}

// pidOf reads the process id back out of a socket's name, and reports whether the name was one
// of ours at all — the directory is ours, but a stray file in it is not a reason to fail.
func pidOf(name string) (int, bool) {
	// The length test is not belt-and-braces: a stray "status.sock" satisfies both the prefix
	// and the suffix, which overlap in it, and slicing between them inverts the bounds — it
	// would crash the reader that found it.
	if len(name) <= len(socketPrefix)+len(socketSuffix) ||
		!strings.HasPrefix(name, socketPrefix) || !strings.HasSuffix(name, socketSuffix) {
		return 0, false
	}
	pid, err := strconv.Atoi(name[len(socketPrefix) : len(name)-len(socketSuffix)])
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// FindPeer answers "which tether did you mean" for a reader that has to pick one.
//
// The three answers are deliberately different, because each sends the reader somewhere else:
// none running is a service to start, one is the answer, and several is a question only they
// can settle — so it names them rather than choosing, the same refusal tether makes about two
// attached adapters.
//
// It takes every directory a tether might be serving in rather than one (SocketDirs), because
// on Linux there are two scopes and the reader is rarely in the same one as the tether: a
// system unit serves in /run/tether while the operator asking has a runtime directory of their
// own. Two live tethers cannot collide across the lists — the name carries a process id — and a
// socket nothing answers on is skipped wherever it is found, so the merge needs no more care
// than the single-directory read did.
func FindPeer(dirs []string, pid int) (Peer, error) {
	var peers []Peer
	for _, dir := range dirs {
		peers = append(peers, Peers(dir)...)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].PID < peers[j].PID })
	where := strings.Join(dirs, " or ")
	if pid != 0 {
		for _, p := range peers {
			if p.PID == pid {
				return p, nil
			}
		}
		return Peer{}, fmt.Errorf("no tether with process id %d is answering in %s", pid, where)
	}
	switch len(peers) {
	case 0:
		return Peer{}, fmt.Errorf("no tether is answering in %s", where)
	case 1:
		return peers[0], nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d tethers are running on this host; say which one with -pid:", len(peers))
	for _, p := range peers {
		// What distinguishes them, in the order somebody reads it: who to ask for, then what
		// that one is, then where it serves. A process id alone is not recognisable.
		fmt.Fprintf(&b, "\n  -pid %d", p.PID)
		if p.Card.Instance != "" {
			fmt.Fprintf(&b, "  %s", p.Card.Instance)
		}
		if p.Card.Listen != "" {
			fmt.Fprintf(&b, "  serving %s", p.Card.Listen)
		}
		if p.Card.RadioType != "" {
			fmt.Fprintf(&b, "  (%s)", p.Card.RadioType)
		}
	}
	return Peer{}, fmt.Errorf("%s", b.String())
}
