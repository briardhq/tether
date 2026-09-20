// Package management is part 5 of ARCHITECTURE.md "The shape": the out-of-band verbs that do
// not ride the byte stream. Today that is the liveness probe; the reset and bootloader verbs
// join it.
//
// It is the only package permitted to know what a byte on the UART means, and the liveness
// probe is the only reason it may. Every vendor who built one of these started with a dumb
// bridge and ended up parsing frames inside it; this is the one landing spot where that earns
// its place, because "the TCP port is open" is not a health signal — it stays true
// with the radio wedged, and stays true with the radio gone until something writes to it.
//
// The probe answers a narrow question: is the coordinator answering. It is not a stack check,
// it does not look at the network, and it is issued only while no client is attached — a radio
// with a client on it is being proved alive by that client, continuously and for free.
package management

import (
	"encoding/binary"
	"fmt"
	"time"

	"briard.io/tether/internal/pipe"
)

// The ZNP transport frame, from zigpy-znp's frames.py and types/commands.py rather than from
// memory: SOF, a one-byte length of the data only, a two-byte header, the data, and an XOR
// checksum over everything between the SOF and itself. The header's low byte packs the command
// type in its top three bits and the subsystem in its low five; its high byte is the command
// id. So SYS Ping — an SREQ to the SYS subsystem, command 1, with no arguments — is
// FE 00 21 01 20 on the wire, and the reply is an SRSP carrying a 16-bit capability bitmap.
const (
	sof          = 0xFE
	typeSREQ     = 1
	typeSRSP     = 3
	subsystemSYS = 0x01
	commandPing  = 0x01

	// The two indications the census counts, and the reasons they carry, read from
	// zigpy-znp: SYS ResetInd is AREQ 0x80 carrying a ResetReason first, and RPCError
	// CommandNotRecognized is an SRSP on subsystem 0 carrying an ErrorCode first.
	typeAREQ             = 2
	commandResetInd      = 0x80
	subsystemRPCError    = 0x00
	commandNotRecognized = 0x00

	resetPowerUp  = 0x00
	resetExternal = 0x01
	resetWatchdog = 0x02

	// headerLen is SOF + length + the two header bytes; overhead adds the trailing checksum.
	headerLen = 4
	overhead  = headerLen + 1
)

// probeTimeout is how long the radio has to answer. The link is 115200 baud and the reply is
// seven bytes, so a healthy coordinator answers in single-digit milliseconds; this is orders of
// magnitude of slack, and short enough that a client connecting mid-probe waits imperceptibly
// (pipe.Attach waits rather than cutting in).
const probeTimeout = 500 * time.Millisecond

// DefaultInterval is how often an idle radio is asked. Idle is exactly when nobody else would
// notice it had died, and the cost is twelve bytes a minute.
const DefaultInterval = 60 * time.Second

// Result is what a coordinator said about itself.
type Result struct {
	// Capabilities is the MT capability bitmap. It is not interpreted here — tether has no
	// business caring which subsystems a coordinator compiled in. It is logged so that a
	// support conversation can compare two dongles.
	Capabilities uint16

	// RTT is the round trip through the device layer, the UART, and the radio's firmware.
	RTT time.Duration
}

// pingRequest is the frame, built once.
var pingRequest = znpFrame(typeSREQ, subsystemSYS, commandPing, nil)

// ping asks a ZNP coordinator whether it is there, and returns what it said. Monitor.Probe is
// the way to it: an answer nothing remembers is half a health signal, so asking and recording
// are not separable in the shape this package offers.
//
// It returns pipe.ErrBusy when a client holds the device, which is not a fault and callers
// must not report it as one.
func ping(p *pipe.Pipe) (Result, error) {
	start := time.Now()
	reply, err := p.Exchange(pingRequest, func(b []byte) bool {
		_, ok := pingResponse(b)
		return ok
	}, probeTimeout)
	if err != nil {
		return Result{}, err
	}
	rtt := time.Since(start)

	payload, ok := pingResponse(reply)
	if !ok {
		if len(reply) == 0 {
			return Result{}, fmt.Errorf("the radio did not answer a SYS ping within %v", probeTimeout)
		}
		// Bytes but not an answer is a different fault from silence, and worth the hex: it is
		// what a wrong baud rate, a half-open frame, or the wrong radio family looks like.
		return Result{}, fmt.Errorf("the radio answered a SYS ping with %d bytes that are not "+
			"a response to it (% x)", len(reply), reply)
	}
	if len(payload) != 2 {
		return Result{}, fmt.Errorf("the radio's SYS ping response carries %d bytes of "+
			"capabilities, want 2 (% x)", len(payload), payload)
	}
	return Result{Capabilities: binary.LittleEndian.Uint16(payload), RTT: rtt}, nil
}

// znpFrame builds a ZNP transport frame.
func znpFrame(typ, subsystem, id byte, data []byte) []byte {
	f := make([]byte, 0, len(data)+overhead)
	f = append(f, sof, byte(len(data)), typ<<5|subsystem, id)
	f = append(f, data...)
	return append(f, checksum(f))
}

// checksum is the XOR of everything after the SOF and before the FCS itself, which is what a
// frame carries as its FCS. Callers pass a frame without its checksum byte — building one it
// does not have yet, or verifying one that has been sliced off.
func checksum(f []byte) byte {
	var fcs byte
	for _, b := range f[1:] {
		fcs ^= b
	}
	return fcs
}

// pingResponse finds the SYS ping response in what the radio said and returns its payload.
//
// It scans rather than assuming the reply starts at byte zero, because an idle coordinator is
// not a silent one: it emits unsolicited AREQs of its own, and one of them can arrive between
// the request and the answer. Anything that does not check out — a bad checksum, a frame that
// is not this response — is stepped over rather than treated as a failure, which is also what
// makes this usable as the "is it complete yet" test while bytes are still arriving.
func pingResponse(b []byte) (payload []byte, ok bool) {
	for i := 0; i+1 < len(b); i++ {
		if b[i] != sof {
			continue
		}
		length := int(b[i+1])
		end := i + length + overhead
		if end > len(b) {
			// A frame that has not finished arriving. Nothing later can be a complete frame
			// either, so stop and let more bytes come.
			return nil, false
		}
		frame := b[i:end]
		if checksum(frame[:len(frame)-1]) != frame[len(frame)-1] {
			continue // not a frame boundary after all; keep looking from the next byte
		}
		if frame[2] == typeSRSP<<5|subsystemSYS && frame[3] == commandPing {
			return frame[headerLen : headerLen+length], true
		}
		i = end - 1 // a real frame, but not ours: step over it whole
	}
	return nil, false
}
