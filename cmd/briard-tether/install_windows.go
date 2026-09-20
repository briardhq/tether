//go:build windows

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"briard.io/tether/internal/device"
)

// The install verb on Windows: the packaging layer, and the only place in this repo that may
// know what the SCM is. It registers the service — supervised by the OS, outside whatever
// started tether — opens the firewall for this binary, and fetches the adapter's driver if
// Windows has none, which is the one thing a stranger with a new stick cannot get past on
// their own.
//
// **Hand-written rather than a service library**, the same call as on Linux and for the same
// reason: `kardianos/service` generates a generic registration, and what matters here — the
// recovery actions, LocalSystem, the event source, the firewall rule, the driver — is not
// modelled by it, so it would be hand-finished on top of a dependency.
//
// **There is one scope, unlike Linux.** A systemd user unit is a real answer for somebody trying
// tether on their laptop; Windows has no user-scope service worth the name, so `install` here is
// always the machine's tether and always needs administrator. The unprivileged alternative is
// running `briard-tether run` in a console, which works and is what the report says.
const (
	firewallRule = "briard-tether"
	// The service is LocalSystem, which is this platform's `User=root`, and it is the same
	// single reason: USB selective suspend lives under HKLM and nothing less can write it.
	// Everything else tether does — serving TCP, advertising, reading descriptors, opening a
	// COM port — needs no privilege at all.
	serviceAccount = "" // empty means LocalSystem
)

// in is unused here: unlike the Linux half there is no scope to choose, and replacing is
// not a question (see install).
func installVerb(out, errOut io.Writer, in io.Reader, args []string) int {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(errOut)
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if err := install(out); err != nil {
		fmt.Fprintf(errOut, "briard-tether install: %v\n", err)
		return 1
	}
	return 0
}

// install registers the service and everything it needs to be useful, then starts it.
func install(out io.Writer) error {
	if !elevated() {
		return fmt.Errorf("this needs administrator: registering a service, opening the " +
			"firewall and installing a driver each do.\n" +
			"    Start Menu, type Terminal, right-click it, Run as administrator — then " +
			"briard-tether install again.\n" +
			"    To try tether without installing anything, run `briard-tether run` in this " +
			"window instead; it serves an adapter that already has a driver")
	}
	source, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding this binary: %w", err)
	}
	if source, err = filepath.Abs(source); err != nil {
		return fmt.Errorf("resolving %s: %w", source, err)
	}
	binary := installedPath()

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("opening the service manager: %w", err)
	}
	defer m.Disconnect()

	// An existing service is replaced rather than refused, and without asking — the same as the
	// Linux half, and for the same reason. Reinstalling is how somebody updates the binary, it
	// is idempotent, and a prompt here is the kind that trains the yes-reflex that makes the
	// prompts worth having elsewhere. The report says "replaced" so it is not silent.
	replaced := false
	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		replaced = true
		if err := removeService(m); err != nil {
			return err
		}
	}

	// Copied into Program Files before the service points at it — after the old service is
	// stopped, because Windows will not overwrite a running image.
	copied, err := installBinary(source, binary)
	if err != nil {
		return err
	}

	s, err := m.CreateService(serviceName, binary, mgr.Config{
		DisplayName:      serviceDisplayName,
		Description:      serviceDescription,
		StartType:        mgr.StartAutomatic,
		ServiceStartName: serviceAccount,
		// Delayed so the network stack and USB enumeration are up first. tether survives
		// either way — it waits for an adapter and says so — but a service that spends its
		// first minute reporting an absence that resolves itself is noise in the event log.
		DelayedAutoStart: true,
	}, "run")
	if err != nil {
		return fmt.Errorf("creating the %s service: %w", serviceName, err)
	}
	defer s.Close()

	// The counterpart of the unit's Restart=on-failure / RestartSec=2s / StartLimitIntervalSec=60.
	// ⚠️ The SCM has no burst limit: where systemd gives up after five starts in a minute, this
	// restarts twice and then stops trying until the reset period passes. The shape of the
	// guard is the same — a config it cannot parse or an address that will not bind must not
	// become a restart loop — and the third action is deliberately none.
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 2 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.NoAction},
	}, 60); err != nil {
		fmt.Fprintf(out, "note        the service was created but its restart policy was not set (%v)\n", err)
	}

	// Registered before the service starts, because the first thing it will try to do is log.
	eventSource := true
	if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Info|eventlog.Warning|eventlog.Error); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		eventSource = false
		fmt.Fprintf(out, "note        the event source was not registered (%v); the service still runs, "+
			"but its log lines will be harder to read\n", err)
	}

	opened := openFirewall(out, binary)
	driver := fetchDrivers(out)

	// No arguments: the SCM passes none at boot, and a first start that differed from every
	// later one is exactly the difference that hides a fault until the first reboot. The verb
	// is in the registered command line instead, where it applies to both.
	if err := s.Start(); err != nil {
		return fmt.Errorf("the service was created but would not start: %w", err)
	}
	status, _ := s.Query()

	report(out, source, binary, copied, replaced, eventSource, opened, driver, serviceStatusText(status))
	return nil
}

// openFirewall lets clients reach this binary. The rule is per-program rather than per-port on
// purpose: the port is a config key somebody may change, and a rule naming a port would quietly
// stop matching when they did — where a rule naming the program keeps working.
func openFirewall(out io.Writer, binary string) bool {
	// Removed first so that reinstalling to a new path does not leave the old rule behind
	// pointing at a binary that is gone.
	_ = exec.Command("netsh", "advfirewall", "firewall", "delete", "rule",
		"name="+firewallRule).Run()
	cmd := exec.Command("netsh", "advfirewall", "firewall", "add", "rule",
		"name="+firewallRule, "dir=in", "action=allow",
		"program="+binary, "enable=yes", "profile=any")
	if outText, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(out, "note        the firewall was not opened (%v: %s); clients on the LAN will "+
			"hang rather than be refused until it is\n", err, strings.TrimSpace(string(outText)))
		return false
	}
	return true
}

// fetchDrivers installs the driver for any attached adapter Windows cannot open, and reports what
// it did in one line for the card. Nothing to do is the normal case and says so.
//
// **Why this is here and not in the daemon**: it is a one-shot change to the machine that
// somebody asked for by typing `install`, which is a different thing from a serving process
// reaching out to Windows Update on its own. Installing the driver is the installer's job, and
// on this platform tether is its own installer.
func fetchDrivers(out io.Writer) string {
	ids := device.DriverlessIDs()
	if len(ids) == 0 {
		return "nothing needed it"
	}
	fmt.Fprintf(out, "            asking Windows Update for a driver for %s — this takes about a minute\n",
		strings.Join(ids, ", "))
	if err := device.InstallDrivers(ids); err != nil {
		fmt.Fprintf(out, "note        the driver did not install (%v)\n", err)
		return "not installed — see the note above, and `briard-tether run` prints the manual steps"
	}
	return "installed for " + strings.Join(ids, ", ")
}

// installedPath is where the service's binary lives, and it is not wherever the file happened to
// be when somebody typed `install`.
//
// ⚠️ **This is a privilege boundary, not tidiness.** The service runs as LocalSystem, so whoever
// can write its image can run code as SYSTEM at the next start — and the ordinary way to arrive
// here is a browser download sitting in a profile directory the user, and anything running as
// them, can overwrite. Registering that path would be this installer creating the hole. Program
// Files is administrator-write by default, which is the whole reason Windows installers copy
// into it rather than registering a download.
//
// The Linux half does not do this and does not need to, because nothing there moves the binary
// for you: `install` records where a package or a person already put it. Windows has no such
// step, so the installer is it.
func installedPath() string {
	dir := os.Getenv("ProgramFiles")
	if dir == "" {
		dir = `C:\Program Files`
	}
	// `briard\` rather than `briard-tether\`, matching /opt/briard on Linux: briard installs
	// more than this binary, and a per-product directory would mean either moving this later
	// or leaving briard's files scattered beside it.
	return filepath.Join(dir, "briard", serviceName+".exe")
}

// installBinary puts the binary where the service will look for it, and reports whether it had
// to. Running `install` from the installed copy — which is what re-running it after an upgrade in
// place looks like — copies nothing and is not an error.
func installBinary(source, target string) (bool, error) {
	if strings.EqualFold(source, target) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return false, fmt.Errorf("making %s: %w", filepath.Dir(target), err)
	}
	b, err := os.ReadFile(source)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", source, err)
	}
	// The directory's inherited ACL is what makes this safe, so the file is written into it
	// rather than having permissions set on it: under Program Files that grants administrators
	// and SYSTEM write and everyone else read.
	if err := os.WriteFile(target, b, 0o755); err != nil {
		return false, fmt.Errorf("writing %s: %w", target, err)
	}
	return true, nil
}

// report prints what just happened and the one or two things the install could not do. It is the
// first thing a stranger sees this tool say, so it is a card and not a paragraph.
func report(out io.Writer, source, binary string, copied, replaced, eventSource, firewall bool, driver, state string) {
	verb := "installed"
	if replaced {
		verb = "replaced"
	}
	fmt.Fprintf(out, "\n%-11s %s\n", verb, serviceName+" (Windows service, LocalSystem, automatic)")
	if copied {
		// The same line as the Linux half, and it does not tell anybody to delete the file it
		// copied from: on this platform it could not have deleted it anyway — install runs
		// from that binary, and Windows does not delete a running image.
		fmt.Fprintf(out, "%-11s from %s, left as it is\n", "copied", source)
		fmt.Fprintf(out, "%-11s the service runs the copy, so a new build wants `install` again\n", "")
	}
	fmt.Fprintf(out, "%-11s %s run\n", "runs", binary)
	fmt.Fprintf(out, "%-11s %s\n", "state", state)
	fmt.Fprintf(out, "%-11s %s\n", "driver", driver)
	if firewall {
		fmt.Fprintf(out, "%-11s inbound allowed for this program, every profile\n", "firewall")
	}
	if eventSource {
		fmt.Fprintf(out, "%-11s Event Viewer, Windows Logs, Application, source %s\n", "log", serviceName)
	}
	fmt.Fprintf(out, "%-11s briard-tether status\n", "card")
	fmt.Fprintf(out, "\n  LocalSystem is the one privileged thing here, and it is for USB selective\n"+
		"  suspend: nothing less can write the driver's power settings, and that is the\n"+
		"  \"works fine, then dies after N minutes\" failure. Everything else tether does\n"+
		"  needs no privilege.\n\n")
}

// uninstallVerb removes what install wrote.
//
// It exists because an installer that cannot be undone is a worse thing to hand a stranger than
// no installer at all: this one leaves a service, an event source and a firewall rule behind, and
// "delete the exe" leaves all three. ⚠️ It does not remove a driver — that is a change to the
// machine's hardware support that outlives tether and that somebody else may now be relying on.
func uninstallVerb(out, errOut io.Writer, args []string) int {
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	flags.SetOutput(errOut)
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if err := uninstall(out); err != nil {
		fmt.Fprintf(errOut, "briard-tether uninstall: %v\n", err)
		return 1
	}
	return 0
}

// uninstall stops the service and removes what was added, in that order, and says what it did.
//
// **It asks nothing.** Somebody who typed `uninstall` has already said it. **And it keeps going
// after a failure it can explain**: a service the SCM will not stop is still a registration to
// remove, and stopping there would leave a machine that has neither a working tether nor a clean
// one.
func uninstall(out io.Writer) error {
	if !elevated() {
		return fmt.Errorf("this needs administrator, the same as install did.\n" +
			"    Start Menu, type Terminal, right-click it, Run as administrator")
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("opening the service manager: %w", err)
	}
	defer m.Disconnect()

	switch err := removeService(m); {
	case err == nil:
		fmt.Fprintf(out, "removed     the %s service\n", serviceName)
	case strings.Contains(err.Error(), "does not exist"):
		fmt.Fprintf(out, "note        no %s service was registered\n", serviceName)
	default:
		fmt.Fprintf(out, "note        %v\n", err)
	}

	if err := eventlog.Remove(serviceName); err == nil {
		fmt.Fprintf(out, "removed     the %s event source\n", serviceName)
	}
	if err := exec.Command("netsh", "advfirewall", "firewall", "delete", "rule",
		"name="+firewallRule).Run(); err == nil {
		fmt.Fprintf(out, "removed     the firewall rule\n")
	}
	removeInstalledBinary(out)
	fmt.Fprintf(out, "kept        any driver that was installed — other software may be using the\n"+
		"            adapter now, and removing it is Device Manager's job, not this one's\n")
	return nil
}

// removeInstalledBinary deletes the copy install made, unless it is the copy doing the asking.
//
// ⚠️ **A running image cannot be deleted on Windows**, and `briard-tether uninstall` run from the
// installed path is the obvious way to uninstall — so that case is reported rather than attempted
// and failed. Scheduling the delete for the next reboot is the other answer and is not taken: it
// leaves somebody with a machine that says it is uninstalled and has the binary back until they
// restart, which is a worse thing to be told than one line saying where the file is.
func removeInstalledBinary(out io.Writer) {
	target := installedPath()
	self, err := os.Executable()
	if err == nil {
		if self, err = filepath.Abs(self); err == nil && strings.EqualFold(self, target) {
			fmt.Fprintf(out, "kept        %s — it is the binary running this; delete it, and\n"+
				"            %s, whenever you like\n", target, filepath.Dir(target))
			return
		}
	}
	if _, err := os.Stat(target); err != nil {
		return // nothing was copied there, which is the case where install found it in place
	}
	if err := os.Remove(target); err != nil {
		fmt.Fprintf(out, "note        %s could not be removed (%v)\n", target, err)
		return
	}
	// Only if it is ours and empty: somebody may have put a config or a log beside it.
	if err := os.Remove(filepath.Dir(target)); err == nil {
		fmt.Fprintf(out, "removed     %s\n", filepath.Dir(target))
	} else {
		fmt.Fprintf(out, "removed     %s\n", target)
	}
}

// removeService stops the service if it is running and deletes the registration. The stop is
// waited for: the SCM refuses to delete a service that is still stopping, and a delete that
// races the stop leaves a registration marked for deletion until the next reboot.
func removeService(m *mgr.Mgr) error {
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("the %s service does not exist: %w", serviceName, err)
	}
	defer s.Close()

	if status, err := s.Query(); err == nil && status.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil {
			return fmt.Errorf("stopping %s: %w", serviceName, err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(300 * time.Millisecond)
			if status, err := s.Query(); err == nil && status.State == svc.Stopped {
				break
			}
		}
	}
	return s.Delete()
}

// elevated reports whether this process can do the three privileged things install needs.
func elevated() bool {
	var sid *windows.SID
	if err := windows.AllocateAndInitializeSid(&windows.SECURITY_NT_AUTHORITY, 2,
		windows.SECURITY_BUILTIN_DOMAIN_RID, windows.DOMAIN_ALIAS_RID_ADMINS,
		0, 0, 0, 0, 0, 0, &sid); err != nil {
		return false
	}
	defer windows.FreeSid(sid)
	member, err := windows.Token(0).IsMember(sid)
	return err == nil && member
}
