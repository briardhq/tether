//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

// What the SCM knows this service as. The name is the key for every later operation — start,
// stop, delete, the event source — so it is written once here and never spelled again.
const (
	serviceName        = "briard-tether"
	serviceDisplayName = "briard-tether"
	serviceDescription = "briard-tether — a USB Zigbee coordinator, served over the network"
)

// serviceRun runs tether under the SCM when the SCM is what started it, and reports whether it
// did. An interactive run falls through to the ordinary path and keeps its console, which is the
// unprivileged "try it on this laptop" case — Windows has no user-scope service, so running the
// binary is the whole of that alternative.
func serviceRun(opts Options) (bool, int) {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return false, 0
	}

	// A service has no console, so log output would go nowhere at all — which for a program
	// whose contract is to be diagnosable from outside is not a degraded mode but a broken
	// one. The event log is where an operator on this platform looks, and it is the
	// counterpart of the journal a systemd unit writes to.
	if elog, err := eventlog.Open(serviceName); err == nil {
		defer elog.Close()
		log.SetOutput(eventWriter{elog})
		log.SetFlags(0) // the event log timestamps every entry itself
	}

	code := 0
	if err := svc.Run(serviceName, &tether{opts: opts}); err != nil {
		log.Printf("tether: the service stopped: %v", err)
		code = 1
	}
	return true, code
}

// eventWriter sends each log line to the event log as one entry. The id is constant because
// these are lines for a human to read rather than events for a rule to match on; giving each
// message its own id would be a second numbering to keep in step with the text.
type eventWriter struct{ elog *eventlog.Log }

const eventID = 1

func (w eventWriter) Write(p []byte) (int, error) {
	if err := w.elog.Info(eventID, strings.TrimRight(string(p), "\r\n")); err != nil {
		return 0, err
	}
	return len(p), nil
}

// tether is the service itself. Everything it does is serve; this type exists only to translate
// between the SCM's control messages and the context that the rest of the program already stops
// on, so that a service stop and a Ctrl-C are the same shutdown — the advert withdrawn and the
// client closed before the process goes.
type tether struct{ opts Options }

func (t *tether) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	// Shutdown as well as Stop: a machine going down is the case where an unwithdrawn advert
	// outlives the coordinator and a client keeps trying to reach it.
	const accepts = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- serve(ctx, t.opts) }()
	changes <- svc.Status{State: svc.Running, Accepts: accepts}

	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				// StopPending before the cancel, not after: withdrawing the advert and
				// closing the client takes long enough for the SCM to wonder, and a service
				// that stops answering without saying it is stopping is one the SCM kills.
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				err := <-done
				return false, exitCode(err)
			default:
				// Nothing else is accepted, so nothing else should arrive; say so rather
				// than drop it silently, because the alternative is a control that looks
				// like it worked.
				log.Printf("tether: ignoring an unexpected service control (%d)", c.Cmd)
			}
		case err := <-done:
			// serve returned without being asked to. These are the errors no retry can fix
			// — an address that will not bind, a dongle swapped for another family — so the
			// exit code is what tells the SCM whether to restart into it.
			changes <- svc.Status{State: svc.StopPending}
			return false, exitCode(err)
		}
	}
}

func exitCode(err error) uint32 {
	if err == nil {
		return 0
	}
	log.Printf("tether: %v", err)
	return 1
}

// serviceStatusText renders what the SCM says about the service, for the install report.
func serviceStatusText(s svc.Status) string {
	switch s.State {
	case svc.Running:
		return "running"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Stopped:
		return "stopped"
	}
	return fmt.Sprintf("state %d", s.State)
}
