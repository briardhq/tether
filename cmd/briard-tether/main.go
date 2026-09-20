// Command tether serves a USB Zigbee coordinator on a TCP port and advertises it over mDNS,
// so that ZHA or zigbee2mqtt can drive a dongle plugged into a different machine. The layers
// below speak of a *serial radio* rather than of Zigbee, because three radio families already
// share one parameter table — but Zigbee is the only thing this has been measured against, and
// it says so. ARCHITECTURE.md is the account of the design.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"briard.io/tether/internal/build"
	"briard.io/tether/internal/management"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	// **Every way in is a verb, serving included.** A bare `briard-tether` prints the help
	// page, so the daemon is never the thing you get by accident — by a typo, by a forgotten
	// argument, by asking for help. Serving a radio is the one action here with a side effect
	// on somebody's house, so it is asked for by name.
	//
	// The verbs are matched before any flag is parsed because each is a different program: one
	// serves, one reads a socket, two write files, and only the first has any use for a config.
	if len(os.Args) < 2 {
		// The same page as `help`, to stderr and exit 1 rather than stdout and 0. The
		// difference is not for the person — they get the same words either way — it is for
		// whatever supervises tether: a unit whose ExecStart lost its verb must *fail*, loudly
		// and into the start limit, rather than exit 0 and leave a service that is quietly not
		// running.
		usage(os.Stderr)
		os.Exit(1)
	}

	switch verb := os.Args[1]; verb {
	case "run":
		os.Exit(runVerb(os.Args[2:]))
	case "status":
		os.Exit(statusVerb(os.Stdout, os.Stderr, management.SocketDirs(), os.Args[2:]))
	case "install":
		os.Exit(installVerb(os.Stdout, os.Stderr, os.Stdin, os.Args[2:]))
	case "uninstall":
		os.Exit(uninstallVerb(os.Stdout, os.Stderr, os.Args[2:]))
	case "version", "-version", "--version":
		fmt.Println(build.String())
		os.Exit(0)
	case "help", "-h", "-help", "--help":
		usage(os.Stdout)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "briard-tether: %s\n\n", misdirected(verb))
		usage(os.Stderr)
		os.Exit(1)
	}
}

// runVerb serves the radio until it is asked to stop, and is the only verb that is a daemon.
//
// It returns an exit code rather than calling log.Fatalf, so that the three one-shot verbs and
// this one answer their caller the same way. The messages are unchanged: whatever supervises
// tether reads them, and the errors that reach here are the ones no retry can fix.
func runVerb(args []string) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "",
		"path to the config file; absent is normal — tether reads the first of "+
			"~/.config/tether/config.json and /etc/tether/config.json that exists, and every "+
			"default applies when neither does")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	// An argument we do not understand is refused rather than ignored. On a host where the
	// listen address happens to be free, ignoring it would start a second tether on the radio.
	if rest := flags.Args(); len(rest) > 0 {
		log.Printf("tether: unexpected argument %q; `run` takes no arguments beyond -config", rest[0])
		return 1
	}

	// First, so that any log somebody pastes into an issue opens with which build wrote it.
	log.Printf("%s starting", build.String())

	// Given by name is a different claim from left alone: someone said where the file is, and
	// not finding it there is a mistake rather than an empty config.
	named := false
	flags.Visit(func(f *flag.Flag) { named = named || f.Name == "config" })

	// Which file is in effect is a question worth answering out loud now that there is more
	// than one place it could be — and "none, and that is normal" is the answer a zero-config
	// install needs to see, since the alternative is an operator wondering whether tether found
	// the file they did not write.
	path := *configPath
	if !named {
		paths := defaultConfigPaths()
		if path = firstExisting(paths); path == "" {
			log.Printf("config: no file at %s — every default applies, which is the normal install",
				strings.Join(paths, " or "))
			path = paths[len(paths)-1]
		} else {
			log.Printf("config: %s", path)
		}
	}

	opts, err := loadOptions(path, named)
	if err != nil {
		log.Printf("tether: %v", err)
		return 1
	}

	// Started by a service manager that talks rather than signals? Then it owns the shutdown,
	// and `run` is the same verb either way — the SCM is the one supervisor that hands a
	// process control messages instead of signals, so it is the one that needs a seam.
	if handled, code := serviceRun(opts); handled {
		return code
	}

	// SIGINT and SIGTERM are how a service manager asks for the advert to be withdrawn and the
	// client closed before the process goes — not a kill to be survived, so the second one is
	// left to the default handler in case the first gets stuck.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx, opts); err != nil {
		// Whatever supervises tether reads this. The errors that reach here are the ones no
		// retry can fix, so restarting blindly into them is the thing to avoid.
		log.Printf("tether: %v", err)
		return 1
	}
	return 0
}

// misdirected says what to do about a first argument that is not a verb. The two ways to get
// one are worth telling apart: a flag in front means somebody expected flags and verbs to
// compose, and anything else is a name this binary does not have.
func misdirected(verb string) string {
	if strings.HasPrefix(verb, "-") {
		return fmt.Sprintf("flags come after the verb, so %q is in the wrong place — "+
			"`briard-tether run -config <path>`", verb)
	}
	return fmt.Sprintf("there is no %q verb", verb)
}

// usage is the help page, and it prints this machine's own paths rather than the ones a manual
// would have: where tether looks for a config and where it answers `status` both depend on who
// is asking, so a page that named the general case would be wrong for half its readers.
func usage(w io.Writer) {
	fmt.Fprintf(w, "briard-tether — a USB Zigbee coordinator, served over the network\n\n")
	fmt.Fprintf(w, "usage:\n")
	fmt.Fprintf(w, "  briard-tether run [-config <path>]   serve the adapter, and advertise it\n")
	fmt.Fprintf(w, "  briard-tether status [-pid <id>]     what a running tether says about itself\n")
	fmt.Fprintf(w, "  briard-tether install                install the service and start it\n")
	fmt.Fprintf(w, "  briard-tether uninstall              stop it and remove what install wrote\n")
	fmt.Fprintf(w, "  briard-tether version                which build this is\n")
	fmt.Fprintf(w, "  briard-tether help                   this page\n\n")

	fmt.Fprintf(w, "config, the first of these that exists — and none is the normal install,\n")
	fmt.Fprintf(w, "which means: find the adapter, listen on %s, advertise it\n", defaultListen)
	for _, path := range defaultConfigPaths() {
		fmt.Fprintf(w, "  %s\n", path)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "a running tether answers `status` on a socket in\n")
	for _, dir := range management.SocketDirs() {
		fmt.Fprintf(w, "  %s\n", dir)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "install as root installs a system service that starts at boot and can keep the\n")
	fmt.Fprintf(w, "adapter out of USB suspend; as yourself it installs one for your session, and\n")
	fmt.Fprintf(w, "says what that cannot do before it writes anything.\n")
}

// statusVerb prints what a running tether says about itself, and returns the exit code for it.
// Being observable from outside is a promise this repo makes to whatever packages it, and the
// exit code is half of that promise: 0 answered, 1 did not.
//
// It finds the tether rather than being told where one is. A host may be running two of them —
// two dongles is two processes — so the socket carries the process id and this reads the
// directory; -pid says which when more than one answers. Directories plural because a tether
// under a user unit serves in that user's runtime directory and a system one in /run/tether,
// and an operator asking about either is standing in the same shell.
func statusVerb(out, errOut io.Writer, dirs []string, args []string) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(errOut)
	pid := flags.Int("pid", 0, "which tether to ask, when more than one is running on this host")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	peer, err := management.FindPeer(dirs, *pid)
	if err != nil {
		fmt.Fprintf(errOut, "briard-tether status: %v\n", err)
		return 1
	}
	render(out, peer.Card)
	return 0
}

// render writes the card for a human. The distinctions it draws are the point of it: a radio
// nobody has asked is not a radio that failed to answer, and no client attached is not an
// absent number.
func render(out io.Writer, card management.Card) {
	if card.Version != "" {
		fmt.Fprintf(out, "version     %s\n", card.Version)
	}
	fmt.Fprintf(out, "up         %v (since %s)\n",
		card.Now.Sub(card.StartedAt).Round(time.Second), card.StartedAt.Format(time.RFC3339))
	if card.RadioType == "" {
		// tether finds its adapter rather than being told about one, so "which radio" can
		// genuinely be unanswered — and reads very differently from a radio it has met.
		fmt.Fprintf(out, "radio       not identified yet\n")
	} else {
		fmt.Fprintf(out, "radio       %s\n", card.RadioType)
	}
	if card.Client != "" {
		fmt.Fprintf(out, "client      %s\n", card.Client)
	} else {
		fmt.Fprintf(out, "client      none attached\n")
	}
	fmt.Fprintf(out, "bytes       %d to client, %d from client\n", card.BytesToClient, card.BytesFromClient)
	fmt.Fprintf(out, "clients     %d connects, %d takeovers, %d disconnects\n",
		card.Connects, card.Takeovers, card.Disconnects)
	// Printed only when there is a fight, because on a healthy tether it would be a line
	// saying nothing, and a card is read by somebody who already has a problem. Two hosts
	// displacing each other is the one way kick-old (INV 4) goes wrong, and the fix is to stop
	// one of them — so the line names them rather than reporting a rate.
	if len(card.Contenders) > 1 {
		fmt.Fprintf(out, "contention  %s are both using this coordinator — %d takeovers in the "+
			"last minute; stop one of them\n",
			strings.Join(card.Contenders, " and "), card.RecentTakeovers)
	}
	// Whether the adapter is there at all comes before how it has been behaving: a card read
	// during an outage — which is when one gets read — must not start by describing traffic.
	if card.DevicePresent {
		fmt.Fprintf(out, "device      present, %d errors, %d open attempts\n",
			card.DeviceErrors, card.OpenAttempts)
	} else {
		fmt.Fprintf(out, "device      ABSENT — %d errors, %d open attempts so far\n",
			card.DeviceErrors, card.OpenAttempts)
		if card.DeviceAbsentReason != "" {
			// Indented under the ABSENT line rather than appended to it: the reason can be a
			// sentence naming two adapters and their paths, and it is the part being read.
			fmt.Fprintf(out, "            %s\n", card.DeviceAbsentReason)
		}
	}

	// Ages, not timestamps: the question being asked here is "is it still happening", and a
	// reader should not have to subtract two clocks to answer it.
	fmt.Fprintf(out, "radio spoke %s\n", ago(card.Now, card.LastDeviceByteAt))
	fmt.Fprintf(out, "client sent %s\n", ago(card.Now, card.LastClientByteAt))

	renderFrames(out, card)

	switch {
	case card.Probe == nil:
		// Not the same as a failed probe, and must not read like one.
		fmt.Fprintf(out, "probe       not probed\n")
	case card.Probe.OK:
		fmt.Fprintf(out, "probe       answered %v ago in %.2fms (capabilities 0x%04x)\n",
			card.Now.Sub(card.Probe.At).Round(time.Second), card.Probe.RTTms, card.Probe.Capabilities)
	default:
		fmt.Fprintf(out, "probe       FAILED %v ago: %s\n",
			card.Now.Sub(card.Probe.At).Round(time.Second), card.Probe.Error)
	}
	if card.LastEvent != "" {
		fmt.Fprintf(out, "last event  %s (%v ago)\n",
			card.LastEvent, card.Now.Sub(card.LastEventAt).Round(time.Second))
	}
}

// ago renders how long ago something happened, and says so plainly when it never has — "never"
// and "0s ago" are opposite answers and must not look alike.
func ago(now, then time.Time) string {
	if then.IsZero() {
		return "never"
	}
	return now.Sub(then).Round(time.Second).String() + " ago"
}

// renderFrames prints the ZNP frame census. Resets and rejections get a line each even at
// zero, because "zero resets since boot" is the claim tether most wants to be able to make —
// INV 1 says the radio is never reset by anything we do, and this is the evidence.
func renderFrames(out io.Writer, card management.Card) {
	f := card.Frames
	if f == nil {
		// Not the same as all-zero, and must not read like it.
		fmt.Fprintf(out, "frames      not counted for %s radios\n", card.RadioType)
		return
	}
	// Protocol terms, not descriptions: whoever reads this line is debugging ZNP and AREQ/SRSP
	// are the words in front of them in every other tool. The JSON keeps the descriptive names,
	// because the program reading that is not a ZNP expert.
	fmt.Fprintf(out, "frames      radio %d (%d AREQ, %d SRSP), client %d (%d SREQ)\n",
		f.RadioFrames, f.RadioAREQ, f.RadioSRSP, f.ClientFrames, f.ClientSREQ)

	resets := f.ResetsPowerUp + f.ResetsExternal + f.ResetsWatchdog
	fmt.Fprintf(out, "resets      %d (%d external, %d watchdog, %d power-up)",
		resets, f.ResetsExternal, f.ResetsWatchdog, f.ResetsPowerUp)
	if resets > 0 {
		fmt.Fprintf(out, " — last %s, %s", f.LastResetReason, ago(card.Now, f.LastResetAt))
	}
	fmt.Fprintln(out)

	fmt.Fprintf(out, "rejections  %d", f.Rejections)
	if f.Rejections > 0 {
		fmt.Fprintf(out, " — last %q, %s", f.LastRejectionCode, ago(card.Now, f.LastRejectionAt))
	}
	fmt.Fprintln(out)

	if f.UnframedBytes > 0 {
		// ⚠️ Unframed bytes are not corruption on their own: a healthy ZNP session produces
		// them by design, measured on real hardware. zigpy writes 256 raw 0xEF bootloader-skip
		// bytes when its first ping times out — which is what it does over a socket, having no
		// DTR/RTS to toggle — and a real radio clocks out a single 0x00 as its line settles on
		// each reset. Both land here and neither is a fault.
		//
		// What is worth reading is the *shape*: a number climbing without connects or resets
		// to account for it is the corruption this counter was put here to catch.
		fmt.Fprintf(out, "unframed    %d bytes outside any frame — skip-bytes and reset nulls "+
			"are normal here\n", f.UnframedBytes)
	}
}
