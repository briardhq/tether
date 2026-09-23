package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"briard.io/tether/internal/family"
	"briard.io/tether/internal/management"
	"briard.io/tether/internal/pipe"
)

// socketDir is `t.TempDir()` with a short name, because a unix socket's path has to fit in
// `sun_path` — 108 bytes — and `t.TempDir()` spends that budget on the test's own name. Under
// CI's `TMPDIR`, which `nix develop` sets to `/home/runner/work/_temp/nix-shell.XXXXXX/`, the
// longer-named tests in this package go past it and `dial` answers `invalid argument`.
// `internal/management` carries the same four lines and the longer account of it. Go has no way
// to share a test helper across two packages without making it shipped code, and four lines are
// not worth a package.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "t")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// The verb against a tether that is actually listening: the point of `tether status` is that a
// second process can ask a running one, so the test starts one and asks it.
func TestStatusVerbReadsARunningTether(t *testing.T) {
	radio := &scriptedRadio{out: make(chan []byte, 4)}
	defer close(radio.out)
	p := pipe.New(nil)
	p.Serve(radio)

	m := management.NewMonitor(p, nil)
	m.DeviceOpened(family.ZNP)
	if _, err := m.Probe(); err != nil {
		t.Fatalf("probing: %v", err)
	}

	// Bound where a reader would look for it: the directory is the contract now, and the name
	// inside it carries the process id.
	dir := socketDir(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = m.ServeStatus(ctx, management.SocketPathIn(dir)) }()

	var out, errOut bytes.Buffer
	deadline := time.Now().Add(3 * time.Second)
	code := 1
	for time.Now().Before(deadline) {
		out.Reset()
		errOut.Reset()
		if code = statusVerb(&out, &errOut, []string{dir}, nil); code == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code != 0 {
		t.Fatalf("status exited %d: %s", code, errOut.String())
	}

	printed := out.String()
	for _, want := range []string{"up ", "radio       znp", "client      none attached", "probe       answered"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the status output does not contain %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "FAILED") || strings.Contains(printed, "not probed") {
		t.Errorf("a healthy radio was rendered as unhealthy:\n%s", printed)
	}
}

// Nothing to talk to has to be an exit code, not a cheerful zero with an empty card: whatever
// supervises tether reads that number.
func TestStatusVerbFailsWhenNothingIsListening(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := statusVerb(&out, &errOut, []string{t.TempDir()}, nil); code == 0 {
		t.Error("status succeeded with no tether running")
	}
	if out.Len() != 0 {
		t.Errorf("status printed a card it never received:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "no tether is answering") {
		t.Errorf("the failure does not say what was wrong: %s", errOut.String())
	}
}

// A first argument that is not a verb is refused, and the message has to say which mistake it
// was. The slip it exists for is `tether -config … status`, written by somebody who reasonably
// expected flags and verbs to compose. Every way in is a verb, so that slip lands here rather
// than starting anything, and the fix is one line of the message.
func TestAFirstArgumentThatIsNotAVerbSaysWhichMistakeItWas(t *testing.T) {
	// A flag in front: the order is the thing to say, because whoever wrote it knows what they
	// wanted.
	flagFirst := misdirected("-config")
	for _, want := range []string{"-config", "after the verb", "briard-tether run -config"} {
		if !strings.Contains(flagFirst, want) {
			t.Errorf("the refusal does not contain %q, so it does not say how to fix it: %s",
				want, flagFirst)
		}
	}
	// A word that is not a verb is a different mistake, and must not be answered with advice
	// about flag order.
	unknown := misdirected("serve")
	if !strings.Contains(unknown, `"serve"`) || strings.Contains(unknown, "after the verb") {
		t.Errorf("an unknown verb was answered with the wrong advice: %s", unknown)
	}
}

// The help page is what somebody meets first, and it is generated from this machine rather than
// written out, so the paths in it are the paths that will actually be used. That is the half
// worth asserting: a help page naming a config location tether does not read is worse than none.
func TestTheHelpPageNamesThisMachinesOwnPaths(t *testing.T) {
	var out bytes.Buffer
	usage(&out)
	page := out.String()

	for _, verb := range []string{"run", "status", "install", "uninstall", "version", "help"} {
		if !strings.Contains(page, "briard-tether "+verb) {
			t.Errorf("the help page does not name the %q verb:\n%s", verb, page)
		}
	}
	for _, path := range defaultConfigPaths() {
		if !strings.Contains(page, path) {
			t.Errorf("the help page does not name the config path %q it will read:\n%s", path, page)
		}
	}
	for _, dir := range management.SocketDirs() {
		if !strings.Contains(page, dir) {
			t.Errorf("the help page does not name the socket directory %q it will use:\n%s", dir, page)
		}
	}
	if !strings.Contains(page, defaultListen) {
		t.Errorf("the help page does not say what port a default install serves on:\n%s", page)
	}
}

// The contention line is absent from a healthy card and names both machines on an unhealthy
// one. Absent matters as much as present: a card is read by somebody who already has a
// problem, and a line that says "no contention" on every healthy tether is one more thing
// between them and the line that matters.
func TestRenderNamesContendersOnlyWhenThereIsAFight(t *testing.T) {
	now := time.Now()
	base := management.Card{StartedAt: now.Add(-time.Minute), Now: now, RadioType: "znp"}

	var healthy bytes.Buffer
	render(&healthy, base)
	if strings.Contains(healthy.String(), "contention") {
		t.Errorf("a healthy tether mentions contention:\n%s", healthy.String())
	}

	// One host taking the radio repeatedly is a client that keeps dying, not a fight, and the
	// card must not send its reader hunting for a second machine.
	restarting := base
	restarting.Takeovers, restarting.RecentTakeovers = 4, 4
	var alone bytes.Buffer
	render(&alone, restarting)
	if strings.Contains(alone.String(), "contention") {
		t.Errorf("one client reconnecting rendered as contention:\n%s", alone.String())
	}

	fighting := restarting
	fighting.Contenders = []string{"10.0.0.5", "10.0.0.9"}
	var contended bytes.Buffer
	render(&contended, fighting)
	printed := contended.String()
	for _, want := range []string{"contention", "10.0.0.5", "10.0.0.9", "stop one of them"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the contention line does not contain %q:\n%s", want, printed)
		}
	}
}

// An unprobed radio and a failed probe must not read the same — that distinction is the reason
// the field is a pointer rather than a zero value.
func TestRenderDistinguishesUnprobedFromFailed(t *testing.T) {
	now := time.Now()
	base := management.Card{StartedAt: now.Add(-time.Minute), Now: now, RadioType: "ezsp"}

	var unprobed bytes.Buffer
	render(&unprobed, base)
	if !strings.Contains(unprobed.String(), "not probed") {
		t.Errorf("an unprobed radio rendered as:\n%s", unprobed.String())
	}

	failed := base
	failed.Probe = &management.ProbeReport{At: now.Add(-10 * time.Second), OK: false, Error: "no answer"}
	var broken bytes.Buffer
	render(&broken, failed)
	if !strings.Contains(broken.String(), "FAILED") || !strings.Contains(broken.String(), "no answer") {
		t.Errorf("a failed probe rendered as:\n%s", broken.String())
	}
	if strings.Contains(broken.String(), "not probed") {
		t.Errorf("a failed probe rendered as never having been probed:\n%s", broken.String())
	}
}

// The frames line names the radio type when it says why nothing is counted, and before the
// adapter is identified there is no type to name — so that case needs its own words rather than
// a sentence with a hole in it.
func TestRenderFramesBeforeAndAfterTheRadioIsIdentified(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct{ radio, want string }{
		{"", "frames      not counted until the radio is identified\n"},
		{"ezsp", "frames      not counted for ezsp radios\n"},
	} {
		var out bytes.Buffer
		render(&out, management.Card{StartedAt: now, Now: now, RadioType: tc.radio})
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("radio %q: want the line %q, the card was:\n%s", tc.radio, tc.want, out.String())
		}
		if strings.Contains(out.String(), "for  radios") {
			t.Errorf("radio %q: the frames line has an empty radio type in it:\n%s", tc.radio, out.String())
		}
	}
}

// A missing adapter has to be the loudest thing on the card, because an outage is when somebody
// runs this. The failure guarded against is the quiet one: a card that reports traffic and probe
// results as usual while there is no dongle plugged in at all.
func TestRenderSaysWhenTheDeviceIsMissing(t *testing.T) {
	now := time.Now()
	base := management.Card{StartedAt: now.Add(-time.Hour), Now: now, RadioType: "znp"}

	held := base
	held.DevicePresent = true
	held.OpenAttempts = 1
	var present bytes.Buffer
	render(&present, held)
	if strings.Contains(present.String(), "ABSENT") {
		t.Errorf("a held device rendered as absent:\n%s", present.String())
	}

	lost := base
	lost.OpenAttempts = 37
	lost.DeviceErrors = 1
	var gone bytes.Buffer
	render(&gone, lost)
	if !strings.Contains(gone.String(), "ABSENT") {
		t.Errorf("an unplugged adapter rendered as:\n%s", gone.String())
	}
	if !strings.Contains(gone.String(), "37 open attempts") {
		t.Errorf("the reopen attempts are not shown:\n%s", gone.String())
	}
}

// scriptedRadio answers every write with a ZNP ping response.
type scriptedRadio struct {
	out chan []byte
	buf []byte
}

func (r *scriptedRadio) Write(b []byte) (int, error) {
	r.out <- []byte{0xFE, 0x02, 0x61, 0x01, 0x79, 0x01, 0x1A}
	return len(b), nil
}

func (r *scriptedRadio) Read(b []byte) (int, error) {
	if len(r.buf) == 0 {
		answer, ok := <-r.out
		if !ok {
			return 0, io.EOF
		}
		r.buf = answer
	}
	n := copy(b, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
