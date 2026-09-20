//go:build !linux && !windows

package main

import (
	"flag"
	"fmt"
	"io"
)

// installVerb exists off Linux and Windows so that the verb is refused with a sentence rather
// than being silently absent — a verb the README names and the binary does not have is worse
// than one that says what this platform has instead.
func installVerb(out, errOut io.Writer, in io.Reader, args []string) int {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(errOut)
	if err := flags.Parse(args); err != nil {
		return 1
	}
	fmt.Fprintf(errOut, "briard-tether install: only Linux and Windows have a service to install — "+
		"a systemd unit there, a registered service there. Run briard-tether run directly on this "+
		"machine, or supervise it with whatever it uses\n")
	return 1
}

// uninstallVerb is the same sentence from the other side: there is nothing this platform could
// have installed, so there is nothing here to remove.
func uninstallVerb(out, errOut io.Writer, args []string) int {
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	flags.SetOutput(errOut)
	if err := flags.Parse(args); err != nil {
		return 1
	}
	fmt.Fprintf(errOut, "briard-tether uninstall: only Linux and Windows have a service to install, so "+
		"this platform has none to remove\n")
	return 1
}
