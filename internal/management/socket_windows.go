//go:build windows

package management

import (
	"os"
	"path/filepath"
)

// SocketDirs is where a tether answers `tether status`. Windows has had AF_UNIX since
// Windows 10 and Go speaks it, so the shape of the verb is the same one; only the place
// changes. ProgramData is the machine-wide, service-writable equivalent of /run — with the
// difference that it is not cleared on reboot, so a socket left by a killed tether is likelier
// here than on Linux.
//
// One directory and not two: the per-user runtime directory Linux has is systemd's, and a
// Windows service and a Windows console session already share this one.
func SocketDirs() []string {
	dir := os.Getenv("ProgramData")
	if dir == "" {
		dir = `C:\ProgramData`
	}
	return []string{filepath.Join(dir, "tether")}
}
