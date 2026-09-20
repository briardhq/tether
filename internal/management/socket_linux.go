//go:build linux

package management

import (
	"os"
	"path/filepath"
)

// SocketDirs is where a tether answers `tether status`, in the order to look for one.
//
// It is derived, not configured: a status socket whose location is a setting is a setting two
// programs have to agree on, and briard is one of them. What is derived is a directory rather
// than a path, because the socket carries a process id (see SocketPath).
//
// **What it derives is the directory `RuntimeDirectory=tether` would have made** in the scope
// tether is running in, which is the whole reason there are two of them. A system unit's
// manager runs as root and makes `/run/tether`; a user manager makes `$XDG_RUNTIME_DIR/tether`.
// So the unit says `RuntimeDirectory=tether` once, both scopes agree with the binary without
// either being told, and a tether started by hand from a shell — no unit at all — lands in the
// same place its own `tether status` looks. That last case is what the user directory is for:
// /run/tether is root's to create, so an ordinary user's tether needs a directory it can write
// rather than a privilege it should not need.
//
// **The reader searches both, because the reader and the writer are not the same process.** An
// operator at a shell has XDG_RUNTIME_DIR set, and the tether they are asking about is usually
// the system one; a reader that looked only at its own scope would say "no tether is answering"
// with one plainly running.
//
// Root is the exception to the user directory, in both directions: only root can create
// /run/tether, so a root tether has no reason to serve anywhere else, and a root reader has no
// way to guess which user's runtime directory to look in. `sudo tether status` therefore finds
// the system tether and not a colleague's.
func SocketDirs() []string {
	const system = "/run/tether"
	// The euid and not the name, because this is a question about what this process may
	// create. XDG_RUNTIME_DIR is checked rather than assembled from the uid: it is the user
	// manager's own variable, and a machine that puts it somewhere other than /run/user/<uid>
	// is a machine whose user units are there too.
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" && os.Geteuid() != 0 {
		return []string{filepath.Join(dir, "tether"), system}
	}
	return []string{system}
}
