//go:build !linux && !windows

package device

import (
	"errors"

	"briard.io/tether/internal/family"
)

// This file is what is left once Linux and Windows have their own: macOS, and anything else Go
// will build for. The device layer is where platform-specific lines live, and these platforms
// are kept compiling rather than kept working — none of them is a target, and each of these
// becomes real work on the day one is.
var goneErrors []error

func openControl(path string) (int, error) { return -1, nil }

func applyFlowControl(fd int, on bool) error {
	if on {
		return errors.New("RTS/CTS flow control is implemented for Linux only")
	}
	return nil
}

func closeControl(fd int) {}

func disableAutosuspend(path string) {}

func adviseUnstablePath(path string) {}

// DescribeUSB has no portable form: identifying a USB adapter means reading descriptors, and
// every platform keeps them somewhere of its own — a filesystem here, SetupAPI on Windows. So
// this says so instead of pretending, because the family table's whole point is not guessing.
func DescribeUSB(devPath string) (family.Device, error) {
	return family.Device{}, errors.New("identifying USB adapters is not implemented on this platform; name the family explicitly in the config")
}

// Detect has no portable form for the same reason DescribeUSB does not. It says so rather than
// returning an empty list, because "none attached" and "cannot look" are opposite answers and
// the caller is about to tell a human which one it got.
func Detect() ([]Adapter, error) {
	return nil, errors.New("detecting attached adapters is not implemented on this platform; name the device path in the config")
}
