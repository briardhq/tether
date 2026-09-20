//go:build !linux && !windows

package management

import (
	"os"
	"path/filepath"
)

// SocketDirs keeps the two targets that matter compiling everywhere else. Anything that is not
// Linux or Windows gets a plausible place rather than a build failure — Linux is the first
// target and Windows stays possible, and nothing is claimed at all about the rest.
func SocketDirs() []string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return []string{filepath.Join(dir, "tether")}
}
