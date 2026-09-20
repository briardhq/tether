package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"briard.io/tether/internal/family"
	"briard.io/tether/internal/management"
)

// write puts a config on disk and gives back its path.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The zero-config install is the normal one: no file, no keys, and every answer derived. This
// is the case that has to work without anybody having read any documentation.
func TestNoConfigIsAValidConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there.json")
	for _, tc := range []struct {
		what string
		path string
	}{
		{what: "no file at all", path: missing},
		{what: "an empty object", path: write(t, `{}`)},
	} {
		opts, err := loadOptions(tc.path, false)
		if err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		if opts.Listen != defaultListen {
			t.Errorf("%s: listen = %q, want the default %q", tc.what, opts.Listen, defaultListen)
		}
		if opts.StatusSocket != management.SocketPath() {
			t.Errorf("%s: status socket = %q, want the derived %q",
				tc.what, opts.StatusSocket, management.SocketPath())
		}
		// Every one of these means "derive it", never "off". An empty device path is the whole
		// of the zero-config story and must not read as "no device".
		if opts.DevicePath != "" || opts.Radio != "" || opts.Baud != 0 || opts.Instance != "" {
			t.Errorf("%s: something was defaulted to a value instead of being left to derive: %+v",
				tc.what, opts)
		}
	}
}

// Naming the file is a different claim from leaving it alone: someone said where it is, and a
// silent fallback to every default would run tether on settings nobody chose.
func TestANamedConfigMustExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there.json")
	if _, err := loadOptions(missing, true); err == nil {
		t.Error("loadOptions accepted a config path that does not exist")
	}
	if _, err := loadOptions(missing, false); err != nil {
		t.Errorf("loadOptions refused an absent config at the default path: %v", err)
	}
}

func TestConfigIsAppliedOverTheDefaults(t *testing.T) {
	path := write(t, `{
		"device": "/dev/serial/by-id/usb-ITead-if00-port0",
		"listen": "127.0.0.1:9999",
		"radio": "ezsp",
		"baud": 57600,
		"instance": "the attic radio",
		"advertise_address": "192.168.1.5"
	}`)
	opts, err := loadOptions(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if opts.AdvertiseAddress != "192.168.1.5" {
		t.Errorf("advertise_address = %q", opts.AdvertiseAddress)
	}
	if opts.DevicePath != "/dev/serial/by-id/usb-ITead-if00-port0" {
		t.Errorf("device = %q", opts.DevicePath)
	}
	if opts.Listen != "127.0.0.1:9999" {
		t.Errorf("listen = %q", opts.Listen)
	}
	if opts.Radio != family.EZSP {
		t.Errorf("radio = %q", opts.Radio)
	}
	if opts.Baud != 57600 {
		t.Errorf("baud = %d", opts.Baud)
	}
	if opts.Instance != "the attic radio" {
		t.Errorf("instance = %q", opts.Instance)
	}
}

// Everything a config can be wrong about, refused at startup rather than where it is used.
// The family name is the one that matters most: a typo there would otherwise surface inside
// the open-retry loop, where every failure is worth retrying — so `znpp` would look exactly
// like a dongle nobody has plugged in yet, and retry forever saying so.
func TestABadConfigIsRefusedAtStartup(t *testing.T) {
	for _, tc := range []struct {
		what string
		body string
		says string
	}{
		{what: "a misspelled family", body: `{"radio": "znpp"}`, says: "znpp"},
		{what: "a family that is not one", body: `{"radio": "zigbee"}`, says: "zigbee"},
		{what: "an address with no port", body: `{"listen": "nuc"}`, says: "listen"},
		{what: "a port that is not a number", body: `{"listen": ":six"}`, says: "listen"},
		{what: "a negative baud", body: `{"baud": -1}`, says: "baud"},
		// An address is an IP here, never a name to resolve later on some other machine, and
		// never host:port — the port is listen's.
		{what: "an advertised address that is a name", body: `{"advertise_address": "nuc.local"}`, says: "advertise_address"},
		{what: "an advertised address with a port", body: `{"advertise_address": "192.168.1.5:6638"}`, says: "advertise_address"},
		// A key nobody reads is worse than a key that fails: tether would run on the default
		// the operator was trying to change, and nothing anywhere would say so.
		{what: "a misspelled key", body: `{"devise": "/dev/ttyUSB0"}`, says: "devise"},
		{what: "a key from some other program", body: `{"flow_control": "rtscts"}`, says: "flow_control"},
		// The value has a type, and a string where a number goes is a mistake worth naming.
		{what: "a baud in quotes", body: `{"baud": "115200"}`, says: "baud"},
		{what: "not JSON at all", body: `device = /dev/ttyUSB0`, says: "config"},
		{what: "a truncated file", body: `{"listen": ":6638"`, says: "config"},
		// Two objects usually means a second copy somebody meant to replace the first with.
		{what: "more than one object", body: `{"listen": ":6638"} {"listen": ":6639"}`, says: "more than one"},
	} {
		_, err := loadOptions(write(t, tc.body), true)
		if err == nil {
			t.Errorf("%s: accepted %s", tc.what, tc.body)
			continue
		}
		if !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: the error does not name %q: %v", tc.what, tc.says, err)
		}
	}
}

// An absent key means "derive it", never "zero" — so a config that sets one thing must leave
// every other answer exactly where it was.
func TestOneKeyDoesNotDisturbTheRest(t *testing.T) {
	opts, err := loadOptions(write(t, `{"device": "/dev/ttyUSB0"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Listen != defaultListen {
		t.Errorf("listen = %q, want the default %q", opts.Listen, defaultListen)
	}
	if opts.Baud != 0 {
		t.Errorf("baud = %d, want 0 so the family table decides", opts.Baud)
	}
	if opts.Radio != "" {
		t.Errorf("radio = %q, want none so the adapter decides", opts.Radio)
	}
}

// The status socket is derived, not configured: it is a path two programs have to agree on,
// and briard is the other one. A key for it would make that agreement a setting.
func TestTheStatusSocketIsNotAConfigKey(t *testing.T) {
	if _, err := loadOptions(write(t, `{"status_socket": "/tmp/elsewhere.sock"}`), true); err == nil {
		t.Error("the config accepted a status socket path")
	}
}

// Every default is baked in and documented here, because these are the numbers a stranger
// inherits without reading anything. 6638 is ZHA's own LEGACY_ZEROCONF_PORT.
func TestTheDefaultsAreTheDocumentedOnes(t *testing.T) {
	if defaultListen != ":6638" {
		t.Errorf("the default listen address is %q; ZHA assumes 6638 for a coordinator "+
			"advertised without a port", defaultListen)
	}
	paths := defaultConfigPaths()
	if len(paths) == 0 {
		t.Fatal("tether looks for a config in no places at all")
	}
	for _, path := range paths {
		if !strings.HasSuffix(path, "config.json") {
			t.Errorf("the default config path %q is not a config.json", path)
		}
	}
	// The system path is always searched and is always last: a user's own place is the more
	// specific claim, and root has no user place at all (defaultConfigPaths).
	if runtime.GOOS != "windows" {
		if paths[len(paths)-1] != "/etc/tether/config.json" {
			t.Errorf("the last place tether looks is %q, want the system config", paths[len(paths)-1])
		}
		switch root := os.Geteuid() == 0; {
		case root && len(paths) != 1:
			t.Errorf("as root tether looks in %v; a dotfile in root's home must not outrank /etc", paths)
		case !root && len(paths) != 2:
			t.Errorf("as an ordinary user tether looks in %v, want its own place and the system's", paths)
		}
	}
}

// The user's own config is the more specific claim and wins, and a place that has no file is
// one place that did not have it rather than an answer. This is the mechanism behind the order
// above: without it, adding a second place would have meant the system file shadowing the one a
// user can actually write.
func TestTheFirstConfigThatExistsIsTheOneRead(t *testing.T) {
	dir := t.TempDir()
	mine := filepath.Join(dir, "mine.json")
	theirs := filepath.Join(dir, "theirs.json")
	absent := filepath.Join(dir, "never-written.json")

	if got := firstExisting([]string{absent, mine, theirs}); got != "" {
		t.Errorf("firstExisting found %q with nothing written yet", got)
	}
	if err := os.WriteFile(theirs, []byte(`{"listen": ":1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := firstExisting([]string{absent, mine, theirs}); got != theirs {
		t.Errorf("firstExisting chose %q, want the only file that exists", got)
	}
	if err := os.WriteFile(mine, []byte(`{"listen": ":2"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := firstExisting([]string{absent, mine, theirs}); got != mine {
		t.Errorf("firstExisting chose %q, want the earlier of two that exist", got)
	}

	// And the file it chose is the one whose contents take effect, which is the half a path
	// test cannot show.
	opts, err := loadOptions(firstExisting([]string{absent, mine, theirs}), false)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Listen != ":2" {
		t.Errorf("tether listened on %q, so it read the wrong file", opts.Listen)
	}
}

// The one key whose presence is the unusual case. Absent means advertise, which is the whole
// zero-config story, so "absent" and "false" must not be the same thing to the parser — which
// is why the field is a pointer.
func TestAdvertiseDefaultsOnAndCanBeTurnedOff(t *testing.T) {
	for _, tc := range []struct {
		what string
		body string
		want bool
	}{
		{what: "no config at all", body: `{}`, want: true},
		{what: "some other key set", body: `{"listen": ":6639"}`, want: true},
		{what: "turned off", body: `{"advertise": false}`, want: false},
		{what: "turned on explicitly", body: `{"advertise": true}`, want: true},
	} {
		opts, err := loadOptions(write(t, tc.body), true)
		if err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		if opts.Advertise != tc.want {
			t.Errorf("%s: advertise = %v, want %v", tc.what, opts.Advertise, tc.want)
		}
	}
	// It is a boolean, and a string that looks like one is a mistake worth naming.
	if _, err := loadOptions(write(t, `{"advertise": "false"}`), true); err == nil {
		t.Error(`the config accepted "false" as a string`)
	}
}
