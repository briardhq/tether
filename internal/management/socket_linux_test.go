//go:build linux

package management

import (
	"os"
	"strings"
	"testing"
)

// The whole claim of SocketDirs is that the binary derives what `RuntimeDirectory=tether`
// creates, in whichever scope started it — so a unit names the directory once and the two never
// drift. That makes this a test about the rule and not about a path: a user manager has
// XDG_RUNTIME_DIR and a system one does not, and root is neither.
func TestSocketDirsFollowTheScopeTetherRunsIn(t *testing.T) {
	const system = "/run/tether"

	t.Setenv("XDG_RUNTIME_DIR", "/run/user/4242")
	got := SocketDirs()
	if os.Geteuid() == 0 {
		// Root has no use for a user runtime directory in either direction: it can create the
		// system one, and it cannot know whose the other would be. Asserted rather than
		// skipped, because a suite run under sudo must not quietly stop checking anything.
		if len(got) != 1 || got[0] != system {
			t.Fatalf("as root SocketDirs is %v, want just %s", got, system)
		}
	} else {
		want := []string{"/run/user/4242/tether", system}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("SocketDirs is %v, want %v — the user's own scope first, the system's still searched", got, want)
		}
	}

	// A system unit's environment carries no XDG_RUNTIME_DIR, and this is the case where
	// inventing one would put the card somewhere nothing looks.
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := SocketDirs(); len(got) != 1 || got[0] != system {
		t.Errorf("with no user manager SocketDirs is %v, want just %s", got, system)
	}

	// Serving and looking are one decision rather than two kept in step.
	if SocketDir() != SocketDirs()[0] {
		t.Errorf("SocketDir is %s but a reader starts at %s", SocketDir(), SocketDirs()[0])
	}
}
