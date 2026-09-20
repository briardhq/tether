// Package build says which build of tether this is: the release version stamped in at link time
// and the platform it was compiled for. It is its own package because two places report it —
// the `version` verb and the report card — and a bug report is only as useful as its answer to
// "which binary was this".
package build

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version is the release, set by the release build with
// `-ldflags "-X briard.io/tether/internal/build.Version=v0.1.0"`. A build nothing stamped says
// "dev" rather than guessing at a number.
var Version = "dev"

// Platform is the target this binary was compiled for, as the release names it:
// `linux/arm64`, `windows/amd64`, and `linux/armv7` rather than a bare `linux/arm`, because the
// ARM level is the one part of the target runtime does not carry and the one a Pi owner needs.
func Platform() string {
	arch := runtime.GOARCH
	if arch == "arm" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				if s.Key == "GOARM" {
					arch = "armv" + s.Value
				}
			}
		}
	}
	return runtime.GOOS + "/" + arch
}

// String is the one-line answer: `briard-tether v0.1.0 linux/amd64`.
func String() string {
	return fmt.Sprintf("briard-tether %s %s", Version, Platform())
}
