package management

import (
	"sync"
	"sync/atomic"
	"time"

	"briard.io/tether/internal/family"
)

// A Census counts what goes past, and nothing else. It is the second of the two places
// permitted to know what a byte means, and the fence around it is drawn in three places:
//
//   - It is **passive.** It sees a copy after the bytes have been forwarded and can neither
//     alter, delay, reframe nor drop anything. INV 6 is not negotiable, and the way to keep it
//     is to make it structurally impossible here rather than to be careful.
//   - It sees the **client path only.** The pipe never shows it an out-of-band exchange, so the
//     liveness probe's ping and the radio's answer to it are not in these counts (pipe.Observer
//     says why: the imbalance below is between a client's questions and the radio's answers,
//     and tether's own would put a thumb on that scale).
//   - It is **stateless per frame.** It never matches a request to a response, tracks a
//     transaction, or decodes a payload beyond a single leading enum byte. The moment it would
//     need to remember one frame to understand another, it has become a protocol implementation
//     and inherits somebody's firmware version — which is the creep this fence exists to refuse.
//   - What it counts is **an exhaustive list**, written down here and in ARCHITECTURE.md
//     "Protocol awareness, and the two places it is allowed", not "frames".
//
// The counts earn their place by answering questions an operator otherwise argues about:
// unsolicited frames from the radio mean the mesh is alive; a client's requests outnumbering the
// radio's answers means the link is losing or stalling; a rejection means bytes reached the
// radio corrupted; and a reset means the coordinator restarted, with the reason saying whose
// fault that was.
type Census struct {
	// Atomic because Observe reads it on the data path and Adopt writes it from the
	// supervision loop, which are different goroutines and must not meet over a mutex the
	// radio would wait behind.
	enabled atomic.Bool

	mu     sync.Mutex
	radio  scanner
	client scanner
	counts FrameCounts
}

// FrameCounts is the exhaustive set. Published shape, like the rest of the card.
type FrameCounts struct {
	RadioFrames   uint64 `json:"radio_frames"`
	RadioAREQ     uint64 `json:"radio_unsolicited"`
	RadioSRSP     uint64 `json:"radio_answers"`
	ClientFrames  uint64 `json:"client_frames"`
	ClientSREQ    uint64 `json:"client_requests"`
	UnframedBytes uint64 `json:"unframed_bytes"`

	// Resets are the radio telling us it restarted, split by the reason it gave. External is
	// the one that matters most: it means something pulled the reset line, which is the thing
	// INV 1 and INV 7 claim cannot happen here. A nonzero count without a reset verb having
	// been issued is tether being wrong in production, and it is the only way we would find
	// that out short of somebody noticing their network re-formed.
	ResetsPowerUp   uint64    `json:"resets_power_up"`
	ResetsExternal  uint64    `json:"resets_external"`
	ResetsWatchdog  uint64    `json:"resets_watchdog"`
	LastResetAt     time.Time `json:"last_reset_at,omitzero"`
	LastResetReason string    `json:"last_reset_reason,omitempty"`

	// Rejections are the radio saying it could not parse what it was sent. Bytes reached it
	// corrupted, and it noticed — which is the failure a telnet-mode ser2net produces by
	// eating 0xFF, and which tether can now show as zero rather than assert.
	Rejections        uint64    `json:"rejections"`
	LastRejectionAt   time.Time `json:"last_rejection_at,omitzero"`
	LastRejectionCode string    `json:"last_rejection_code,omitempty"`
}

// NewCensus returns a census that counts nothing yet. It has to exist before the radio is
// known, because tether is detecting its adapter rather than being told about it and may
// be waiting for one to be plugged in — and the pipe's observer is fixed when the pipe is made.
func NewCensus() *Census {
	return &Census{}
}

// Adopt tells the census which radio it is counting for, and is what turns it on: only ZNP
// frames can be read without becoming a protocol stack. For every other family it
// stays a no-op that says so on the card by being absent from it, because a census of zero and
// a silent radio must not look alike.
func (c *Census) Adopt(radio family.Radio) {
	if c == nil {
		return
	}
	c.enabled.Store(radio == family.ZNP)
}

// Enabled reports whether this census counts anything.
func (c *Census) Enabled() bool { return c != nil && c.enabled.Load() }

// Observe is handed a copy of bytes that have already been forwarded. It is the pipe's observer
// (see pipe.Observer) and must stay cheap and total: it runs on the data path's own goroutine,
// so anything it does slowly, the radio waits for.
func (c *Census) Observe(fromRadio bool, b []byte) {
	if !c.Enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if fromRadio {
		c.radio.feed(b, c.countFromRadio, c.countUnframed)
		return
	}
	c.client.feed(b, c.countFromClient, c.countUnframed)
}

// Counts returns a snapshot.
func (c *Census) Counts() FrameCounts {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts
}

func (c *Census) countUnframed(n int) { c.counts.UnframedBytes += uint64(n) }

func (c *Census) countFromRadio(f frame) {
	c.counts.RadioFrames++
	switch f.kind {
	case typeAREQ:
		c.counts.RadioAREQ++
		// SYS ResetInd: the radio volunteering that it restarted, with the cause in one byte.
		if f.subsystem == subsystemSYS && f.id == commandResetInd && len(f.payload) > 0 {
			c.counts.LastResetAt = time.Now()
			c.counts.LastResetReason = resetReason(f.payload[0])
			switch f.payload[0] {
			case resetPowerUp:
				c.counts.ResetsPowerUp++
			case resetExternal:
				c.counts.ResetsExternal++
			case resetWatchdog:
				c.counts.ResetsWatchdog++
			}
		}
	case typeSRSP:
		c.counts.RadioSRSP++
		// RPCError CommandNotRecognized: it could not parse what it was sent.
		if f.subsystem == subsystemRPCError && f.id == commandNotRecognized && len(f.payload) > 0 {
			c.counts.Rejections++
			c.counts.LastRejectionAt = time.Now()
			c.counts.LastRejectionCode = rejectionCode(f.payload[0])
		}
	}
}

func (c *Census) countFromClient(f frame) {
	c.counts.ClientFrames++
	if f.kind == typeSREQ {
		c.counts.ClientSREQ++
	}
}

// resetReason names a ZNP ResetReason. An unknown value is reported as itself rather than
// guessed at or dropped — a reason we do not recognise is still evidence.
func resetReason(b byte) string {
	switch b {
	case resetPowerUp:
		return "power-up"
	case resetExternal:
		return "external (something pulled the reset line)"
	case resetWatchdog:
		return "watchdog (the radio's firmware restarted itself)"
	}
	return "unknown reason " + hex(b)
}

func rejectionCode(b byte) string {
	switch b {
	case 0x01:
		return "invalid subsystem"
	case 0x02:
		return "invalid command id"
	case 0x03:
		return "invalid parameter"
	case 0x04:
		return "invalid length"
	}
	return "unknown code " + hex(b)
}

func hex(b byte) string {
	const digits = "0123456789abcdef"
	return "0x" + string([]byte{digits[b>>4], digits[b&0x0f]})
}
