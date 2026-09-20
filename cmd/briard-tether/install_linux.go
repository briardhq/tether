//go:build linux

package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"briard.io/tether/internal/family"
	"briard.io/tether/internal/management"
)

// The install verb: the packaging layer, and the only place in this repo that may know what
// systemd is. It writes the unit ARCHITECTURE.md "Packaging, and the service" describes —
// OS-supervised, outside whatever started tether, so that an agent restarting itself cannot
// take the radio down — and the udev rule that arms the one guard an unprivileged tether
// cannot arm for itself.
//
// **Hand-written rather than a service library.** The candidate was `kardianos/service`, and it
// costs more than it saves here: what it generates is a *generic* unit, and every line below
// that matters — `RuntimeDirectory`, `SupplementaryGroups`, `DynamicUser`, the start limit — is
// one it does not model, so the unit would have to be hand-finished anyway on top of a
// dependency. The argument it would win is Windows, where a service needs the SCM rather than
// a file, and the Windows half makes the same call for the same reason.
const (
	unitName      = "briard-tether.service"
	systemUnitDir = "/etc/systemd/system"
	// **99, and the number is the point.** udev sorts every rules file lexicographically across
	// all of its directories — measured against the shipped man page, systemd 260: "All rules
	// files are collectively sorted and processed in lexicographic order, regardless of the
	// directories in which they live." Two rules that set the same attribute therefore resolve
	// by filename, last writer wins, and the tools this rule exists to counter ship their own:
	// TLP's is `85-tlp.rules`. At 60 this rule would lose to it at every plug, quietly, which
	// is the one case it exists for. 99 also leaves an administrator's own `99-local.rules`
	// after it, which is the right way round.
	udevRulePath = "/etc/udev/rules.d/99-briard-tether.rules"
)

// installVerb installs the unit for this binary and starts it, and returns the exit code for
// whoever asked. Like `status`, it is a different program from serving: it reads no config,
// opens no port, and exits.
func installVerb(out, errOut io.Writer, in io.Reader, args []string) int {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(errOut)
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if rest := flags.Args(); len(rest) > 0 {
		fmt.Fprintf(errOut, "briard-tether install: unexpected argument %q; install takes none — "+
			"run it as yourself for a user service, under sudo for a system one\n", rest[0])
		return 1
	}
	if err := install(out, in); err != nil {
		fmt.Fprintf(errOut, "briard-tether install: %v\n", err)
		return 1
	}
	return 0
}

// install does the whole of it: decide the scope, write the files, tell systemd, start it.
//
// **The scope is not a flag — it is who is asking**, because a flag whose absence has a
// sensible default is not a flag. Root can write `/etc/systemd/system` and nobody else can, so
// `sudo briard-tether install` means the machine's tether and a plain one means this user's.
// Both are real answers: the machine's is what an unattended install wants and what a NUC in a
// cupboard wants, and a user service is the whole of what somebody trying this on their laptop
// needs.
func install(out io.Writer, in io.Reader) error {
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		return fmt.Errorf("this installs a systemd unit and there is no systemctl on PATH. " +
			"Run briard-tether directly, or supervise it with whatever this machine uses")
	}

	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding this binary, which is what the unit has to point at: %w", err)
	}
	// The unit outlives the shell that ran this, so it records where the binary actually is
	// rather than how it was reached — a relative path or a symlink into a build tree is a unit
	// that works until something moves.
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
	}

	system := os.Geteuid() == 0
	dir := systemUnitDir
	if !system {
		if dir, err = userUnitDir(); err != nil {
			return err
		}
	}
	group, gid := ttyGroup()

	// The system service runs its own copy, under a directory only root can write. ⚠️ This is a
	// privilege boundary and not tidiness: a unit whose ExecStart somebody else can replace lets
	// them choose what root runs at the next start, and the file somebody types `install` from
	// is often in their home directory. The user scope needs none of this — a service running as
	// you, from a binary you own, is not a boundary at all — so it keeps pointing where it is.
	source, copied := binary, false
	if system {
		if copied, err = installBinary(binary, installedPath()); err != nil {
			return err
		}
		binary = installedPath()
	}

	unitPath := filepath.Join(dir, unitName)
	if !system {
		// **A user service is the lesser install, and the operator hears that before it is
		// written, not after.** Somebody who types `briard-tether install` without sudo has
		// usually not chosen the user scope; they have chosen not to think about scopes. The
		// three limits below are not edge cases, they are what that choice costs, and two of
		// them are silent — an adapter that never opens and a service that is not there after
		// a reboot both look like tether being broken.
		if !confirm(out, in, unitPath, group, gid) {
			fmt.Fprintf(out, "nothing written. `sudo briard-tether install` installs the system service\n")
			return nil
		}
	}
	replaced, err := writeFile(unitPath, unitText(binary, system))
	if err != nil {
		return err
	}

	if system {
		if _, err := writeFile(udevRulePath, udevRuleText()); err != nil {
			return err
		}
		// Best-effort, and said out loud when it fails: the rule is already on disk and applies
		// at the next plug whatever udev has been told, so a reload that did not happen is
		// worth a line rather than an aborted install.
		if err := run("udevadm", "control", "--reload-rules"); err != nil {
			fmt.Fprintf(out, "note        udev did not reload its rules (%v); the rule still applies at the next plug\n", err)
		}
	}

	ctl := func(args ...string) error {
		if system {
			return run(systemctl, args...)
		}
		return run(systemctl, append([]string{"--user"}, args...)...)
	}
	if err := ctl("daemon-reload"); err != nil {
		return err
	}
	if err := ctl("enable", "--now", unitName); err != nil {
		return fmt.Errorf("%w\nThe unit is written; `systemctl %sstatus %s` says why it would not start",
			err, userFlag(system), unitName)
	}

	report(out, system, unitPath, source, binary, copied, replaced)
	return nil
}

// installedPath is where the system service's binary lives. ⚠️ Not wherever the file was when
// somebody typed `install`: see the boundary described at the call site.
//
// **`/opt/briard/` rather than `/opt/briard-tether/`**, and the Windows half uses
// `%ProgramFiles%\briard\` for the same reason: briard installs more than this binary, and a
// per-product directory would mean either moving this later or leaving briard's files
// scattered. FHS reserves /opt/<provider> for exactly this.
func installedPath() string { return "/opt/briard/briard-tether" }

// installBinary puts the binary where the unit will point, and reports whether it had to. Running
// `install` from the installed copy — an upgrade in place, then a re-install — copies nothing and
// is not an error.
func installBinary(source, target string) (bool, error) {
	if source == target {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return false, fmt.Errorf("making %s: %w", filepath.Dir(target), err)
	}
	b, err := os.ReadFile(source)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", source, err)
	}
	// Written to a temporary name and renamed, because the target may be the binary a running
	// service is executing: replacing the file wholesale gives that process its old inode until
	// it restarts, where writing through it would be ETXTBSY or, worse, a half-written binary.
	tmp := target + ".new"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return false, fmt.Errorf("writing %s: %w", tmp, err)
	}
	// WriteFile does not chmod a file that already exists, and this one may from a previous run.
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("setting the mode on %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("moving %s into place: %w", target, err)
	}
	return true, nil
}

// replaceable reports why a root service's binary is writable by somebody who is not root, or ""
// when it is not: whoever can write the image chooses what root runs at the next start.
//
// **It is silent on a normal install, because the system scope copies into /opt/briard**, and
// that is what it is for — the copy is the fix and this is the check that the fix landed
// somewhere that is actually safe. A machine whose /opt somebody has made world-writable is rare
// and is exactly the case a copy would otherwise paper over.
//
// Both the file and its directory are checked, because deleting and recreating a root-owned file
// needs only the directory.
func replaceable(binary string) string {
	if fi, err := os.Stat(binary); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 {
			return fmt.Sprintf("%s is owned by uid %d rather than root", binary, st.Uid)
		}
	}
	dir := filepath.Dir(binary)
	fi, err := os.Stat(dir)
	if err != nil {
		return ""
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	switch {
	case st.Uid != 0:
		return fmt.Sprintf("%s is owned by uid %d rather than root", dir, st.Uid)
	// The sticky bit is what makes /tmp survivable: it stops one user removing another's file.
	case fi.Mode()&0o022 != 0 && fi.Mode()&os.ModeSticky == 0:
		return fmt.Sprintf("%s is writable by others (%s)", dir, fi.Mode().Perm())
	}
	return ""
}

// report prints what just happened and the two or three things the operator has to know that
// the install could not do for them. It is the first thing a stranger sees this tool say, so it
// is a card and not a paragraph: what was written, what runs it, what is still missing.
func report(out io.Writer, system bool, unitPath, source, binary string, copied, replaced bool) {
	verb := "installed"
	if replaced {
		verb = "replaced "
	}
	fmt.Fprintf(out, "%s   %s\n", verb, unitPath)
	if copied {
		// **What the source file is now** is the question this answers, and it answers it
		// without advising a deletion. "You can delete it" is wrong in three ordinary cases —
		// a build tree, a read-only /nix/store path, a packaged /usr/bin — so what is said
		// instead is that it was left alone and no longer matters. The consequence is the part
		// people actually trip on: editing the source changes nothing, because the service
		// runs the copy.
		fmt.Fprintf(out, "copied      from %s, left as it is\n", source)
		fmt.Fprintf(out, "            the service runs the copy, so a rebuild wants `install` again\n")
	}
	fmt.Fprintf(out, "runs        %s run\n", binary)

	if system {
		fmt.Fprintf(out, "as          root — the one thing on this machine that needs it is keeping the\n")
		fmt.Fprintf(out, "            adapter out of USB runtime suspend, and the card is 0666 regardless\n")
		fmt.Fprintf(out, "rule        %s — %d adapter ids kept out of USB runtime suspend\n",
			udevRulePath, len(family.IDs()))
		fmt.Fprintf(out, "            it applies when a device appears, so replug an adapter that is already\n")
		fmt.Fprintf(out, "            in, or reboot — udev replays the same event for what is attached at boot\n")
		if why := replaceable(binary); why != "" {
			fmt.Fprintf(out, "⚠ warning   %s runs as root and %s.\n", unitName, why)
			fmt.Fprintf(out, "            Whoever can replace that file chooses what root runs at the next\n")
			fmt.Fprintf(out, "            start. Move it somewhere only root can write — /usr/local/bin is\n")
			fmt.Fprintf(out, "            the usual answer — and run this again\n")
		}
	} else {
		// Everything a user service cannot do was said before it was written, and saying it
		// again here would teach an operator to skim the part that mattered.
		fmt.Fprintf(out, "as          you, so nothing on this machine changed outside your home directory\n")
	}

	fmt.Fprintf(out, "card        %s\n", management.SocketDir())
	fmt.Fprintf(out, "started     %s — `briard-tether status` reads it\n", unitName)
	// The verb rather than the commands: undoing this by hand means a stop, a disable, a file
	// and — as root — a udev rule, and three of those four are easy to forget.
	if system {
		fmt.Fprintf(out, "undo        sudo briard-tether uninstall\n")
	} else {
		fmt.Fprintf(out, "undo        briard-tether uninstall\n")
	}
}

// uninstallVerb removes what install wrote, for the scope that is asking.
//
// It exists because an installer that cannot be undone is a worse thing to hand a stranger than
// no installer at all: this one writes a unit, a udev rule, an enable symlink and a running
// service, and "rm the file" leaves three of those four behind. The same rule decides the scope
// as on the way in — root removes the machine's, anyone else removes their own — so a user can
// never be told they have nothing installed because they forgot a sudo.
func uninstallVerb(out, errOut io.Writer, args []string) int {
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	flags.SetOutput(errOut)
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if rest := flags.Args(); len(rest) > 0 {
		fmt.Fprintf(errOut, "briard-tether uninstall: unexpected argument %q; uninstall takes none\n", rest[0])
		return 1
	}
	if err := uninstall(out); err != nil {
		fmt.Fprintf(errOut, "briard-tether uninstall: %v\n", err)
		return 1
	}
	return 0
}

// uninstall stops the service and removes the files, in that order, and says what it did.
//
// **It asks nothing.** Somebody who typed `uninstall` has already said it; a prompt there is the
// kind that trains people to answer yes without reading, which is exactly what makes the one on
// the way in worth having.
//
// **It keeps going after a failure it can explain.** A unit systemd will not stop is still a
// unit file to remove — leaving the file because the stop failed would produce a machine that
// has neither a working service nor a clean one.
func uninstall(out io.Writer) error {
	system := os.Geteuid() == 0
	dir := systemUnitDir
	if !system {
		var err error
		if dir, err = userUnitDir(); err != nil {
			return err
		}
	}
	unitPath := filepath.Join(dir, unitName)
	systemctl, ctlErr := exec.LookPath("systemctl")
	ctl := func(args ...string) error {
		if ctlErr != nil {
			return ctlErr
		}
		if system {
			return run(systemctl, args...)
		}
		return run(systemctl, append([]string{"--user"}, args...)...)
	}

	removed := false
	if _, err := os.Stat(unitPath); err == nil {
		// Before the file goes, because this is what drops the enable symlink and stops the
		// process; afterwards systemd no longer knows what it is being asked about.
		if err := ctl("disable", "--now", unitName); err != nil {
			fmt.Fprintf(out, "note        systemd would not stop it (%v); removing the unit anyway\n", err)
		} else {
			fmt.Fprintf(out, "stopped     %s\n", unitName)
		}
		if err := os.Remove(unitPath); err != nil {
			return fmt.Errorf("removing %s: %w", unitPath, err)
		}
		fmt.Fprintf(out, "removed     %s\n", unitPath)
		_ = ctl("daemon-reload")
		removed = true
	}

	if system {
		// Unlike the Windows half this can remove the binary even when it is the one running:
		// unlinking an executing file is ordinary on Unix, and the process keeps the inode it
		// already opened. So there is no case here that has to be reported instead of done.
		switch err := os.Remove(installedPath()); {
		case err == nil:
			fmt.Fprintf(out, "removed     %s\n", installedPath())
			// Only if empty, and that is the point of the shared directory: briard installs
			// beside this, and removing tether must not take its neighbours with it.
			if err := os.Remove(filepath.Dir(installedPath())); err == nil {
				fmt.Fprintf(out, "removed     %s\n", filepath.Dir(installedPath()))
			}
		case !errors.Is(err, os.ErrNotExist):
			fmt.Fprintf(out, "note        %s could not be removed (%v)\n", installedPath(), err)
		}

		switch err := os.Remove(udevRulePath); {
		case err == nil:
			fmt.Fprintf(out, "removed     %s\n", udevRulePath)
			// The attribute it set stays as it is until the adapter is replugged, which is the
			// honest thing to say: removing a rule does not undo a plug that already happened.
			fmt.Fprintf(out, "            an adapter already plugged in keeps the setting until it is replugged\n")
			fmt.Fprintf(out, "            or the machine reboots; removing a rule does not undo a plug\n")
			if err := run("udevadm", "control", "--reload-rules"); err != nil {
				fmt.Fprintf(out, "note        udev did not reload its rules (%v)\n", err)
			}
			removed = true
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("removing %s: %w", udevRulePath, err)
		}
	}

	if !removed {
		scope := "you"
		if system {
			scope = "this machine"
		}
		fmt.Fprintf(out, "nothing to remove: no briard-tether service is installed for %s\n", scope)
	}
	// The other scope is a real thing to have forgotten, and only this direction can be checked
	// — root cannot know which users have one.
	if !system {
		if _, err := os.Stat(filepath.Join(systemUnitDir, unitName)); err == nil {
			fmt.Fprintf(out, "note        a system service is installed too: sudo briard-tether uninstall removes it\n")
		}
	}
	return nil
}

// confirm lays out what a user service cannot do and waits for a yes. It returns false on
// anything else, on end of input, and on a read error — a confirmation that defaults to yes when
// nobody is there to answer is not one.
//
// It is deliberately not a `-yes` flag as well. The scripted case is the *system* install,
// which automation drives and which asks nothing; a user install is by definition somebody at
// a keyboard, and `yes | briard-tether install` is there for anyone who disagrees.
func confirm(out io.Writer, in io.Reader, unitPath, group string, gid int) bool {
	fmt.Fprintf(out, "This installs a service for you alone, which is limited in three ways:\n\n")

	if group == "" {
		fmt.Fprintf(out, "  tty group     this host has neither a dialout nor a uucp group, so nothing here can\n"+
			"                open a serial port until one exists and you are in it\n")
	} else if inGroup(gid) {
		fmt.Fprintf(out, "  tty group     you are in %s, so opening an adapter is fine\n", group)
	} else {
		fmt.Fprintf(out, "  tty group     you are not in %s, so tether cannot open a USB adapter at all:\n"+
			"                  sudo usermod -aG %s %s   (then log out and back in)\n", group, group, username())
	}
	fmt.Fprintf(out, "  autosuspend   only root can keep an adapter out of USB runtime suspend, which is the\n"+
		"                \"works fine, then dies after N minutes\" failure. A user service cannot\n"+
		"                arm that guard, and nothing will tell you it is missing\n")
	if lingering(username()) {
		fmt.Fprintf(out, "  at boot       it starts when you log in; lingering is already on for you, so a\n"+
			"                reboot brings it back without one\n")
	} else {
		fmt.Fprintf(out, "  at boot       it starts when you log in, not when the machine boots, unless\n"+
			"                lingering is on:\n"+
			"                  sudo loginctl enable-linger %s\n", username())
	}
	fmt.Fprintf(out, "\n  sudo briard-tether install has none of these: it runs at boot as root, writes the\n"+
		"  udev rule that arms autosuspend, and needs no group.\n\n")

	fmt.Fprintf(out, "Write the user unit at %s? [y/N] ", unitPath)
	line, err := bufio.NewReader(in).ReadString('\n')
	// The answer is echoed by a terminal and not by a pipe, so the newline has to come from
	// here; without it a scripted install runs the prompt and the outcome together on one line.
	fmt.Fprintln(out)
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// unitText is the unit, and it is a function rather than a file in the repo so that the two
// scopes cannot drift apart and so that the binary's own path is in it. Kept deliberately
// short: every line here is one an operator may have to read at 3am, and a directive nobody
// can explain is worse than an absent one.
func unitText(binary string, system bool) string {
	var b strings.Builder
	b.WriteString("# Written by `briard-tether install`. Installing again overwrites it.\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=briard-tether — a USB Zigbee coordinator, served over the network\n")
	if system {
		b.WriteString("After=network-online.target\n")
		b.WriteString("Wants=network-online.target\n")
	}
	// tether retries on its own everything that a retry can fix — an adapter nobody has plugged
	// in yet, a port that went away — so a process that *exits* has met something else: a
	// config it cannot parse, an address that will not bind. Restarting into those forever
	// hides exactly the errors the status verb exists to surface, so the unit gives up and
	// stays failed where `systemctl status` says why.
	b.WriteString("StartLimitIntervalSec=60\n")
	b.WriteString("StartLimitBurst=5\n\n")

	b.WriteString("[Service]\n")
	// `run`, because serving is a verb: a unit whose ExecStart lost it would print the help
	// page and exit, which is why that exit is a failure rather than a 0 (main.go).
	fmt.Fprintf(&b, "ExecStart=%s run\n", binary)
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=2s\n")
	// The status socket's directory, which the binary derives to the same answer without being
	// told — /run/tether under the system manager, $XDG_RUNTIME_DIR/tether under a user one.
	b.WriteString("RuntimeDirectory=tether\n")
	if system {
		// **Root, and said out loud rather than left to the default.** A system unit is root
		// unless told otherwise, so this line changes nothing mechanically and everything for
		// a reader: it is the one privileged thing about this service and it should not be
		// invisible.
		//
		// The reason is USB autosuspend and only that. Everything else tether does needs no
		// privilege — serving TCP, advertising, reading the descriptors, and the tty, which is
		// a group — but `power/control` is `root root`, so a non-root service cannot write it
		// and the guard against the load-dependent failure class could only ever be armed out
		// of band by the udev rule. That would bound an autosuspend strategy to a privilege
		// the service can simply have, which is a limit better left undrawn than discovered
		// from inside. The status socket does not pay for this: it is 0666 whoever binds it,
		// so a root service's card is still readable by the operator who asks.
		// **And no hardening directives**, which is the same decision rather than a second one.
		// DynamicUser brings ProtectSystem=strict and ProtectHome=read-only along; each would
		// be another limit to discover from inside while designing the autosuspend strategy,
		// and none of them buys much from a service that reads a config, opens a tty and
		// writes one socket. They are cheap to add later, against a strategy that exists, and
		// expensive to debug at 3am against one that does not.
		b.WriteString("User=root\n")
	}
	b.WriteString("\n[Install]\n")
	if system {
		b.WriteString("WantedBy=multi-user.target\n")
	} else {
		b.WriteString("WantedBy=default.target\n")
	}
	return b.String()
}

// udevRuleText keeps the adapters this binary knows about out of USB runtime suspend.
//
// It is the packaged half of the load-dependent failure class — "works fine, then dies after N
// minutes" — and it exists in the package because the unit that runs unprivileged is the one that
// cannot write `power/control` itself. A rule beats the write in any case: it applies at *plug*
// time, so it covers the window before tether has opened the port and every replug while it is
// retrying.
//
// ⚠️ It is a policy statement and not a lock: `powertop --auto-tune`, TLP and their kind set the
// same attribute and may run after this.
//
// Scoped to the ids in the family table rather than to the bus, because a blanket
// `SUBSYSTEM=="usb"` rule would be this tool making a power decision about every device on
// somebody else's machine.
func udevRuleText() string {
	var b strings.Builder
	b.WriteString("# Written by `briard-tether install`. Installing again overwrites it.\n")
	b.WriteString("# Keep the radio coordinator out of USB runtime suspend: a suspended adapter\n")
	b.WriteString("# looks exactly like a dongle that died after an hour. Applies when the device\n")
	b.WriteString("# appears — at plug, and at boot for what is already attached.\n")
	b.WriteString("#\n")
	b.WriteString("# The 99 is deliberate: udev sorts every rules file lexicographically across all\n")
	b.WriteString("# of its directories, so a power tool's own rule (TLP ships 85-tlp.rules) would\n")
	b.WriteString("# otherwise run after this one and win. It cannot stop `powertop --auto-tune`\n")
	b.WriteString("# run by hand afterwards; nothing in udev can.\n")
	for _, d := range family.IDs() {
		fmt.Fprintf(&b, "ACTION==\"add\", SUBSYSTEM==\"usb\", ATTR{idVendor}==\"%s\", ATTR{idProduct}==\"%s\", ATTR{power/control}=\"on\"\n",
			d.Vendor, d.Product)
	}
	return b.String()
}

// ttyGroup is the group that owns serial ports on this host, and its gid. It returns "" and -1
// when the host has neither of the two, which is a thing to say rather than to guess at.
//
// **Asked of the host rather than answered from a list**, because the answer differs by
// distribution — `dialout` on Debian/Ubuntu/Fedora/NixOS, `uucp` on Arch — and a unit naming
// the wrong one is a service that starts and then cannot open the radio.
func ttyGroup() (string, int) {
	// An adapter that is already plugged in answers the question directly, and outranks any
	// list: it is the group that owns the very device tether will open.
	for _, pattern := range []string{"/dev/ttyUSB*", "/dev/ttyACM*"} {
		matches, _ := filepath.Glob(pattern)
		for _, path := range matches {
			var st syscall.Stat_t
			if err := syscall.Stat(path, &st); err != nil {
				continue
			}
			if g, err := user.LookupGroupId(strconv.FormatUint(uint64(st.Gid), 10)); err == nil {
				return g.Name, int(st.Gid)
			}
		}
	}
	// Nothing plugged in yet, which is the ordinary case at install time — so ask which of the
	// two this host has.
	for _, name := range []string{"dialout", "uucp"} {
		if g, err := user.LookupGroup(name); err == nil {
			if gid, err := strconv.Atoi(g.Gid); err == nil {
				return g.Name, gid
			}
		}
	}
	return "", -1
}

// inGroup reports whether the user running this is in that group right now — which is the
// question, rather than what /etc/group says, because a membership added since login is not one
// this process has.
func inGroup(gid int) bool {
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g == gid {
			return true
		}
	}
	return false
}

// userUnitDir is where a user manager reads units from.
func userUnitDir() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("finding your config directory, which is where a user unit goes: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user"), nil
}

// writeFile writes one of the two files, and reports whether it replaced one — which is the
// difference between a first install and an upgrade, and is worth saying out loud.
func writeFile(path, content string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("making %s: %w", filepath.Dir(path), err)
	}
	_, err := os.Stat(path)
	replaced := err == nil
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	return replaced, nil
}

// run is a command whose failure has to be readable: systemctl says why on stderr and the exit
// status alone says nothing anybody can act on.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s %s: %w: %s", filepath.Base(name), strings.Join(args, " "), err, msg)
		}
		return fmt.Errorf("%s %s: %w", filepath.Base(name), strings.Join(args, " "), err)
	}
	return nil
}

func username() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "$USER"
}

// userFlag is the "--user" that separates the two scopes everywhere a systemctl command is
// printed or run, so that a message about one scope cannot be produced for the other.
func userFlag(system bool) string {
	if system {
		return ""
	}
	return "--user "
}

// lingering reports whether this user's services already survive their last session. Anything
// it cannot answer counts as "no", because the warning it suppresses is cheap and the silence
// would not be.
func lingering(name string) bool {
	out, err := exec.Command("loginctl", "show-user", name, "--property=Linger", "--value").Output()
	return err == nil && strings.TrimSpace(string(out)) == "yes"
}
