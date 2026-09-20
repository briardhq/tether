package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"briard.io/tether/internal/family"
	"briard.io/tether/internal/management"
)

// defaultListen is where tether serves when the config does not say. It is not a number we
// picked: 6638 is ZHA's own `LEGACY_ZEROCONF_PORT`, the port it assumes for a coordinator
// advertised over zeroconf without one, so it is the number already in the muscle memory of
// everyone who has wired a network coordinator to Home Assistant.
const defaultListen = ":6638"

// config is the file, and only the file. It is deployment wiring — which device, which port,
// which family override — and deliberately not a mirror of Options: the status socket is
// derived rather than configured, and putting it in this struct would make it a setting by
// accident.
//
// **Its shape is a published contract.** Another program writes this file and so does a
// stranger with a text editor, so keys may be added but never renamed or repurposed.
// ARCHITECTURE.md "Configuration" carries the key table and the rules behind it.
type config struct {
	// Device is the serial port. Absent is the normal case and means "find it": the family
	// table knows every supported stick, so the zero-config install names nothing at all. Set
	// it to settle the one case tether refuses to guess at — two compatible adapters attached
	// at once — or to point at a stick the table does not carry.
	Device string `json:"device,omitempty"`

	// Listen is the TCP address for clients, host:port. Absent means defaultListen.
	Listen string `json:"listen,omitempty"`

	// Radio names the family instead of reading it off the adapter, which is the way out of a
	// stick the table does not carry and of a stick that has been reflashed into another
	// family. It overrides the parameters, never the identity — tether still reads the
	// descriptors to say what the adapter is.
	Radio string `json:"radio,omitempty"`

	// Baud overrides the family's. Absent means the table's, which is what everything but a
	// bench experiment wants.
	Baud int `json:"baud,omitempty"`

	// Instance overrides the advertised identity. Absent derives it.
	Instance string `json:"instance,omitempty"`

	// Advertise turns the mDNS advert off. Absent means on, which is the whole zero-config
	// story, so this is the one key whose *presence* is the unusual case.
	//
	// It exists because **one mDNS coordinator per LAN is the supported configuration**:
	// zigbee2mqtt's `mdns://` takes the first responder at every start, so a second
	// advertised coordinator is a coin flip it cannot be talked out of. Turning the advert
	// off on the second tether is the remedy, and without a key there would be none.
	// A tether that does not advertise still serves — `socket://host:port` and `tcp://host:port`
	// never depended on discovery.
	//
	// A pointer because absent and false are different claims and `omitempty` cannot tell a
	// false apart from a missing key.
	Advertise *bool `json:"advertise,omitempty"`

	// AdvertiseAddress is the one IP address the advert carries, instead of the addresses of
	// the interfaces it goes out on. Absent means derive it: the address `listen` bound when
	// that is a specific one, every non-loopback interface's addresses when it is the
	// wildcard. Set it when neither is what a client should connect to — a host behind NAT or
	// a container with an address the LAN cannot reach, or a machine whose interfaces the
	// enumeration gets wrong, the measured case being Windows announcing 127.0.0.1. An IP
	// address, not host:port: the port is `listen`'s and the advert already carries it.
	AdvertiseAddress string `json:"advertise_address,omitempty"`
}

// defaultConfigPaths is where tether looks when nothing points it elsewhere, in order.
//
// **Two places, and the rule is the status socket's** (`management.SocketDirs`): a user's own
// place first, the system's always, and root has no user place. That is what lets somebody
// without write access to `/etc` configure the tether they started — the user-scope install's
// whole story — while a system service, which is the main mode, keeps reading only what an
// administrator put there. A dotfile in root's home quietly outranking the system file would
// be a trap, and skipping the user place for root is how it is closed rather than warned about.
//
// **Neither is created.** An absent file is the normal install and means every default, so a
// file written to hold defaults would be a copy of the code that goes stale.
//
// A runtime switch rather than the build tags the device and management layers use: those
// carry syscalls that do not compile off their platform, and this is a string. Splitting it
// across three files would spend a file per path to say the same sentence. Windows keeps one
// place until its service exists to have a second opinion about.
func defaultConfigPaths() []string {
	if runtime.GOOS == "windows" {
		dir := os.Getenv("ProgramData")
		if dir == "" {
			dir = `C:\ProgramData`
		}
		return []string{filepath.Join(dir, "tether", "config.json")}
	}
	const system = "/etc/tether/config.json"
	if os.Geteuid() == 0 {
		return []string{system}
	}
	// UserConfigDir is $XDG_CONFIG_HOME, or ~/.config — the standard place, asked for by its
	// standard name so that a machine which puts it elsewhere is followed rather than guessed at.
	dir, err := os.UserConfigDir()
	if err != nil {
		return []string{system}
	}
	return []string{filepath.Join(dir, "tether", "config.json"), system}
}

// firstExisting is the file tether will actually read, or "" when there is none — which is not
// an error but the normal install, and is why this is a separate question from opening it.
func firstExisting(paths []string) string {
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// loadOptions reads the config file and fills in everything it did not say.
//
// **A missing file is not an error unless it was asked for by name.** The normal install writes
// no config at all — the adapter is found, the port is the default, the identity is derived —
// so an absent file at the default path means "all defaults" and must not look like a failure.
// A path given on the command line is a different claim: someone said where the file is, and
// not finding it there is a mistake worth stopping for rather than quietly ignoring.
func loadOptions(path string, required bool) (Options, error) {
	opts := Options{
		Listen:       defaultListen,
		Advertise:    true,
		StatusSocket: management.SocketPath(),
	}

	f, err := os.Open(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && !required:
		return opts, nil
	case err != nil:
		return Options{}, fmt.Errorf("reading the config: %w", err)
	}
	defer f.Close()

	cfg, err := decode(f)
	if err != nil {
		return Options{}, fmt.Errorf("reading the config %s: %w", path, err)
	}
	return cfg.options(opts)
}

// decode parses the file. Unknown keys are refused rather than ignored, because the failure
// they cause otherwise is the worst kind: a typo in a key name leaves tether running happily
// on the default the operator was trying to change, and nothing anywhere says so.
func decode(r io.Reader) (config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var cfg config
	if err := dec.Decode(&cfg); err != nil {
		return config{}, err
	}
	// One object, not a stream: trailing content is a mistake and is usually a second copy of
	// the config somebody meant to replace.
	if err := dec.Decode(new(struct{})); err != io.EOF {
		return config{}, errors.New("there is more than one object in it")
	}
	return cfg, nil
}

// options applies the file over the defaults, validating as it goes.
//
// Everything here is checked at startup rather than where it is used, and that is the point of
// the function. A bad family name would otherwise surface inside the open-retry loop, where
// tether treats every failure as worth retrying — so a typo would look exactly like a dongle
// that has not been plugged in yet, and retry forever saying so.
func (c config) options(opts Options) (Options, error) {
	if c.Device != "" {
		opts.DevicePath = c.Device
	}
	if c.Listen != "" {
		if err := checkListen(c.Listen); err != nil {
			return Options{}, fmt.Errorf("listen: %w", err)
		}
		opts.Listen = c.Listen
	}
	if c.Radio != "" {
		if _, err := family.Defaults(family.Radio(c.Radio)); err != nil {
			return Options{}, fmt.Errorf("radio: %w", err)
		}
		opts.Radio = family.Radio(c.Radio)
	}
	if c.Baud != 0 {
		if c.Baud < 0 {
			return Options{}, fmt.Errorf("baud: %d is not a baud rate", c.Baud)
		}
		opts.Baud = c.Baud
	}
	opts.Instance = c.Instance
	if c.Advertise != nil {
		opts.Advertise = *c.Advertise
	}
	if c.AdvertiseAddress != "" {
		// An IP and nothing else. A name would have to be resolved, on a machine and at a
		// time this file does not control; a host:port would carry a port that is not the
		// listener's. Both are the kind of value that means different things on different
		// machines, which is the trap checkListen exists to keep out of this file.
		if net.ParseIP(c.AdvertiseAddress) == nil {
			return Options{}, fmt.Errorf("advertise_address: %q is not an IP address", c.AdvertiseAddress)
		}
		opts.AdvertiseAddress = c.AdvertiseAddress
	}
	return opts, nil
}

// checkListen refuses an address that will not bind, at startup rather than at the first
// generation — a listener that cannot open stops tether, and stopping with "unknown port" an
// unpredictable number of seconds after start is a worse account of a typo than refusing it.
//
// It is stricter than net.Listen on purpose: the port must be a number. net.Listen would also
// take a service name out of /etc/services, and a config file that means different things on
// two machines — or on Windows, where there may be no such file — is a trap in a contract
// another program writes.
func checkListen(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not a host:port address: %w", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("%q is not a port number in %q; a name out of /etc/services is not "+
			"accepted here, because it would mean different things on different machines", port, addr)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("%d is not a TCP port", n)
	}
	return nil
}
