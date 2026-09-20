//go:build !windows

package main

// serviceRun is the Windows seam. Only Windows has a service manager that starts a process and
// then talks to it; everywhere else the supervisor is systemd or a person, and both ask tether
// to stop with a signal that `run` already waits on.
func serviceRun(opts Options) (bool, int) { return false, 0 }
