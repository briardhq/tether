package management

import (
	"errors"
	"fmt"
	"log"
	"time"

	"briard.io/tether/internal/family"
	"briard.io/tether/internal/pipe"
)

// restartTimeout is how long a reset radio has to announce that it booted. The measured stick
// takes about two seconds from the reset line's release; this is the margin over that, and it
// is also how long a client connecting mid-restart is kept waiting (see pipe.Act).
const restartTimeout = 5 * time.Second

// busyRetry is how long a restart waits before asking a second time when the device is busy
// with no client on it — which means a probe is in flight, and a probe is over in well under a
// second.
const busyRetry = time.Second

// RestartResult is what a restart came to. It is a published shape, like the card: the
// restart verb prints it and briard reads it, so a field may be added but not repurposed.
type RestartResult struct {
	OK bool `json:"ok"`

	// ResetReason is what the radio said about its own boot, in the census's words. A pin
	// reset reads as power-up on the measured stick, so expect that rather than "external".
	ResetReason string `json:"reset_reason,omitempty"`

	// BootMs is from the reset line's release to the radio announcing that it booted.
	BootMs float64 `json:"boot_ms,omitempty"`

	Error string `json:"error,omitempty"`
}

// Restart resets the radio and waits for it to say it has booted. It is wedge recovery: a radio
// that has stopped answering while still enumerated, which nothing but a reset or a replug
// cures. Normal operation never needs it — clients recover in band.
//
// It refuses while a client is attached rather than resetting the radio under it. The client
// is the one thing that would notice, and the person who asked can stop it first; what tether
// cannot do is know whether the client is wedged too or in the middle of something.
//
// The restart is confirmed rather than assumed. A radio that took the reset announces its boot
// with a SYS ResetInd, and only that counts: a pulse with no announcement after it is reported
// as a failure, because it is indistinguishable from a reset line wired somewhere else.
func (m *Monitor) Restart() RestartResult {
	m.mu.Lock()
	act, radio, present := m.restart, m.radio, m.devicePresent
	m.mu.Unlock()

	switch {
	case !present:
		return failed("no adapter is open")
	case act == nil:
		return failed("no restart is known for this adapter; unplugging it and plugging it back " +
			"in is the restart that always works")
	case radio != family.ZNP:
		// The restart is confirmed by a ZNP frame. Every measured entry is a ZNP stick; this
		// is a stick named as another family in the config, which nothing here can confirm.
		return failed(fmt.Sprintf("a restart is confirmed by a ZNP reset indication, and this "+
			"adapter is configured as %s", radio))
	}

	log.Printf("management: restarting the radio, as asked")
	result := m.restartOnce(act)
	if result.Error == errBusyNoClient {
		time.Sleep(busyRetry)
		result = m.restartOnce(act)
	}
	if !result.OK {
		log.Printf("management: the radio restart failed: %s", result.Error)
		return result
	}
	log.Printf("management: the radio restarted — it announced a %s boot %.0f ms after the "+
		"reset line was released", result.ResetReason, result.BootMs)

	// A fresh answer on the card, so a reader does not see the wedge that prompted this as the
	// latest word. Busy means a client attached the moment the restart let go, and that client
	// is proving the radio alive instead.
	if _, err := m.Probe(); err != nil && !errors.Is(err, pipe.ErrBusy) {
		result.OK = false
		result.Error = "the radio announced its boot but did not answer a ping after it: " + err.Error()
		log.Printf("management: %s", result.Error)
	}
	return result
}

// errBusyNoClient is the one refusal worth a second try: the device is busy and no client
// holds it, which is the probe.
const errBusyNoClient = "the device is busy with another out-of-band exchange"

func (m *Monitor) restartOnce(act func() error) RestartResult {
	var released time.Time
	reply, err := m.pipe.Act(func() error {
		if err := act(); err != nil {
			return err
		}
		released = time.Now()
		return nil
	}, func(b []byte) bool {
		_, ok := findFrame(b, typeAREQ, subsystemSYS, commandResetInd)
		return ok
	}, restartTimeout)

	switch {
	case errors.Is(err, pipe.ErrBusy):
		if client := m.pipe.Stats().Client; client != "" {
			return failed(fmt.Sprintf("a client is attached (%s), and tether does not reset a "+
				"radio under its client; stop the client first", client))
		}
		return failed(errBusyNoClient)
	case err != nil:
		return failed(err.Error())
	}

	payload, ok := findFrame(reply, typeAREQ, subsystemSYS, commandResetInd)
	if !ok || len(payload) == 0 {
		return failed(fmt.Sprintf("the reset line was pulsed but the radio did not announce a "+
			"boot within %v", restartTimeout))
	}
	return RestartResult{
		OK:          true,
		ResetReason: resetReason(payload[0]),
		BootMs:      float64(time.Since(released).Microseconds()) / 1000,
	}
}

func failed(why string) RestartResult { return RestartResult{Error: why} }
