//go:build linux

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"briard.io/tether/internal/family"
)

// The unit is the whole of what "ship it" means on Linux, and the two scopes differ in exactly
// one idea: a system service is given an identity and a group, a user service already has both.
// Asserting the difference rather than the text keeps this from being a copy of the template.
func TestTheTwoUnitsDifferOnlyInTheIdentityTheyAskFor(t *testing.T) {
	system := unitText("/usr/local/bin/briard-tether", true)
	for _, want := range []string{
		// The verb, without which the unit starts the help page and exits.
		"ExecStart=/usr/local/bin/briard-tether run",
		// Root, and explicitly: it is the one privileged thing about this service, it is there
		// for the autosuspend guard alone, and a reader should not have to know systemd's
		// default to see it.
		"User=root",
		// The card's directory, which the binary derives to the same answer.
		"RuntimeDirectory=tether",
		// A config it cannot parse must not become a restart loop that hides it.
		"Restart=on-failure",
		"StartLimitBurst=5",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("the system unit is missing %q:\n%s", want, system)
		}
	}

	user := unitText("/home/someone/bin/briard-tether", false)
	// A user manager cannot change user or add groups, so asking for either is a unit that
	// fails to start rather than one that is careful.
	for _, unwanted := range []string{"DynamicUser", "SupplementaryGroups", "User="} {
		if strings.Contains(user, unwanted) {
			t.Errorf("the user unit asks for %q, which a user manager cannot grant:\n%s", unwanted, user)
		}
	}
	for _, want := range []string{"RuntimeDirectory=tether", "WantedBy=default.target"} {
		if !strings.Contains(user, want) {
			t.Errorf("the user unit is missing %q:\n%s", want, user)
		}
	}
}

// The user scope is the lesser install and says so before it writes anything, so the gate is
// part of the product rather than a nicety: a no has to leave the machine untouched. Asserted
// against the filesystem rather than the printed text, because "it printed a warning" and "it
// wrote nothing" are different claims and only the second one matters here.
func TestAUserInstallWritesNothingUntilItIsConfirmed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this is the unprivileged path; as root there is nothing to confirm")
	}
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	unit := filepath.Join(home, "systemd", "user", unitName)

	for _, answer := range []string{"n\n", "\n", "", "no\n", "sudo\n"} {
		var out, errOut bytes.Buffer
		if code := installVerb(&out, &errOut, strings.NewReader(answer), nil); code != 0 {
			t.Errorf("declining with %q exited %d, which reads as a failure rather than a choice: %s",
				answer, code, errOut.String())
		}
		if _, err := os.Stat(unit); !os.IsNotExist(err) {
			t.Fatalf("answering %q wrote a unit at %s", answer, unit)
		}
	}

	// And the three limits are named before the question, since the question is unanswerable
	// without them.
	var out, errOut bytes.Buffer
	installVerb(&out, &errOut, strings.NewReader("n\n"), nil)
	for _, want := range []string{"tty group", "autosuspend", "at boot", "sudo briard-tether install"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the confirmation does not mention %q:\n%s", want, out.String())
		}
	}
}

// The unit is generated, so nothing but systemd itself can say whether it is well-formed — a
// misspelled directive is ignored by the parser and the service just quietly behaves
// differently, which is the failure this whole repo exists to stop being possible.
//
// ⚠️ **The assertion is the output and not the exit code**, and that is measured rather than
// assumed: `systemd-analyze verify` prints `Unknown key 'RuntimeDirectoryy' in section
// [Service], ignoring.` and **exits 0**. Only a structural fault — no ExecStart at all — makes
// it exit nonzero, and an unknown *group* passes silently too. A unit it is happy with produces
// no output whatever, so "said nothing" is the check with something to bite on.
func TestSystemdAcceptsBothUnits(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("no systemd-analyze on this machine; the unit's grammar is checked where there is one")
	}
	// A real executable, because verify resolves ExecStart and an absent binary is an error in
	// its own right — this test is about the directives.
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []struct {
		what   string
		system bool
	}{{"system", true}, {"user", false}} {
		dir := t.TempDir()
		path := filepath.Join(dir, unitName)
		if err := os.WriteFile(path, []byte(unitText(binary, scope.system)), 0o644); err != nil {
			t.Fatal(err)
		}
		args := []string{"verify"}
		if !scope.system {
			args = append(args, "--user")
		}
		out, err := exec.Command(analyze, append(args, path)...).CombinedOutput()
		if err != nil || len(out) > 0 {
			t.Errorf("systemd has something to say about the %s unit (%v):\n%s\n%s",
				scope.what, err, out, unitText(binary, scope.system))
		}
	}
}

// The rule has to name the devices it applies to. A blanket rule over the bus would be this
// tool making a power decision about every USB device on somebody else's machine, and a rule
// transcribing the ids by hand would drift from the table the first time a row was added.
func TestTheUdevRuleCoversTheTableAndNothingWider(t *testing.T) {
	rule := udevRuleText()
	for _, d := range family.IDs() {
		want := `ATTR{idVendor}=="` + d.Vendor + `", ATTR{idProduct}=="` + d.Product + `"`
		if !strings.Contains(rule, want) {
			t.Errorf("the udev rule does not cover %s:\n%s", d, rule)
		}
	}
	for _, line := range strings.Split(rule, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !regexp.MustCompile(`ATTR\{idVendor\}=="[0-9a-f]{4}", ATTR\{idProduct\}=="[0-9a-f]{4}"`).MatchString(line) {
			t.Errorf("a rule line matches more than one adapter id: %q", line)
		}
		if !strings.Contains(line, `ATTR{power/control}="on"`) {
			t.Errorf("a rule line does not do the one thing the rule is for: %q", line)
		}
	}
}

// A root service running a binary somebody else can replace is that somebody choosing what root
// runs. The warning is the only thing standing between a stranger and that, so each way of being
// replaceable is asserted rather than sampled — and so is the case that must stay quiet, because
// a warning that cries wolf on a correct install is one people learn to skip.
func TestReplaceableBinaryIsNoticed(t *testing.T) {
	root := t.TempDir()
	// t.TempDir() is 0700 under a directory owned by whoever runs the test, so on a normal
	// unprivileged run every case below is already "owned by uid != 0". That makes the uid
	// branch the one this can prove here, and the mode branch the one it can prove when it
	// runs as root — so assert against what the caller actually is rather than skipping.
	asRoot := os.Geteuid() == 0

	binary := filepath.Join(root, "briard-tether")
	if err := os.WriteFile(binary, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	switch why := replaceable(binary); {
	case !asRoot && why == "":
		t.Error("a binary under a directory this user owns was not reported as replaceable")
	case asRoot && why != "":
		t.Errorf("a root-owned binary in a root-owned 0700 directory was reported as %q", why)
	}

	// World-writable and not sticky is the other way in, and it does not depend on who owns it.
	loose := filepath.Join(root, "loose")
	if err := os.Mkdir(loose, 0o777); err != nil {
		t.Fatal(err)
	}
	// Mkdir is masked by umask, so the mode has to be set explicitly to mean it.
	if err := os.Chmod(loose, 0o777); err != nil {
		t.Fatal(err)
	}
	inLoose := filepath.Join(loose, "briard-tether")
	if err := os.WriteFile(inLoose, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if why := replaceable(inLoose); why == "" {
		t.Error("a binary in a world-writable directory was not reported as replaceable")
	}

	// The sticky bit is what makes /tmp survivable, and must not be reported as a hole when
	// the uid that owns it is otherwise right.
	sticky := filepath.Join(root, "sticky")
	if err := os.Mkdir(sticky, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	inSticky := filepath.Join(sticky, "briard-tether")
	if err := os.WriteFile(inSticky, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if why := replaceable(inSticky); asRoot && why != "" {
		t.Errorf("a sticky directory was reported as replaceable: %q", why)
	}
}

// The system service runs a copy, and the copy is the privilege boundary — so what matters is
// that it lands with a mode nobody but root can write, and that replacing one that is already
// there does not go through the file a running service is executing.
func TestInstallBinaryLandsExecutableAndReplacesInPlace(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "src", "briard-tether")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("first"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "opt", "briard", "briard-tether")

	copied, err := installBinary(source, target)
	if err != nil || !copied {
		t.Fatalf("installBinary: copied=%v, err=%v", copied, err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("the copy is not there: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("the copy is mode %s, want 0755 — it has to be executable and not writable by others", fi.Mode().Perm())
	}

	// A second install over the first: the content must be the new one, and no leftover .new.
	if err := os.WriteFile(source, []byte("second"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := installBinary(source, target); err != nil {
		t.Fatalf("replacing the copy: %v", err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "second" {
		t.Errorf("after replacing, the copy reads %q (%v), want \"second\"", b, err)
	}
	if _, err := os.Stat(target + ".new"); !os.IsNotExist(err) {
		t.Errorf("%s.new was left behind", target)
	}

	// Installing from the copy is what re-running install after an upgrade in place looks like.
	// It must not be an error, and must not rewrite the file it is reading.
	if copied, err := installBinary(target, target); err != nil || copied {
		t.Errorf("installing from the installed copy: copied=%v, err=%v; want false, nil", copied, err)
	}
}
