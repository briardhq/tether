//go:build windows

// leash runs a program on a Windows machine on behalf of a harness on another one, and stops
// it — properly — when the harness lets go.
//
//	leash.exe <config-path> <exe> [args...]
//
// It exists because of two measured things about running tether over ssh on Windows:
//
//   - Nothing outside a console program's own console can ask it to stop. Closing the ssh
//     connection leaves it running, `taskkill` without /F refuses (no window), and `taskkill /F`
//     kills it without its shutdown path — so the mDNS goodbye, the client close and the status
//     socket's removal were unreachable from the harness, and every stop was a third ssh session
//     costing half a second.
//   - What *does* reach a command over ssh is EOF on its stdin, and an ssh session has a console.
//
// So: the first line of stdin is the program's config, written to <config-path> (a file, so a
// human can open it after a failed run — the harness's reason for a fixed path). The program is
// started in its own process group with this process's stdout and stderr, so its log streams
// back over the same ssh session. Then leash waits for stdin to close — which is the harness
// closing its end — and raises CTRL_BREAK on the program's group, which Go delivers as SIGINT:
// the same orderly shutdown a service manager's stop would trigger. A program that has not
// exited ten seconds later is terminated. leash exits when the program does, with its code, so
// the harness's wait on the ssh client is a wait on the program.
//
// Before starting, any other process with the program's name is killed, because between tests
// what matters is that *nothing* holds the serial port — including a tether an interrupted run
// left behind.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const grace = 10 * time.Second

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: leash.exe <config-path> <exe> [args...]  (config on the first line of stdin)")
		os.Exit(2)
	}
	configPath, exe, args := os.Args[1], os.Args[2], os.Args[3:]

	in := bufio.NewReader(os.Stdin)
	config, err := in.ReadBytes('\n')
	if err != nil && err != io.EOF {
		fatal("reading the config from stdin: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		fatal("%v", err)
	}
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		fatal("%v", err)
	}

	// The sweep. taskkill's own exit code says nothing useful (1 = nothing matched), so it is
	// not checked; the program's "Serial port busy" retry is what would report a survivor.
	_ = exec.Command("taskkill", "/F", "/IM", filepath.Base(exe),
		"/FI", "PID ne "+strconv.Itoa(os.Getpid())).Run()

	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		fatal("starting %s: %v", exe, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	go func() {
		// EOF is the harness letting go; anything else on stdin is ignored.
		io.Copy(io.Discard, in)
		pid := uint32(cmd.Process.Pid)
		if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, pid); err != nil {
			fmt.Fprintf(os.Stderr, "leash: CTRL_BREAK to %d failed (%v); terminating\n", pid, err)
			cmd.Process.Kill()
			return
		}
		select {
		case <-done:
			done <- nil // hand the result back to main's receive
		case <-time.After(grace):
			fmt.Fprintf(os.Stderr, "leash: %s still running %s after CTRL_BREAK; terminating\n", exe, grace)
			cmd.Process.Kill()
		}
	}()

	<-done
	os.Exit(cmd.ProcessState.ExitCode())
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "leash: "+format+"\n", args...)
	os.Exit(1)
}
