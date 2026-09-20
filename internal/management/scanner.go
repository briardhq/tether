package management

// scanner finds ZNP transport frames in a byte stream that arrives in arbitrary chunks. It is
// the only stateful thing in the census, and the state is exactly one partial frame.
//
// It resynchronises rather than trusting: a stream can be joined mid-frame, and a byte that
// looks like a SOF can appear inside a payload. So a candidate frame is accepted only if its
// checksum agrees, and a candidate that does not is stepped over one byte at a time. Bytes
// discarded that way are counted — a stream that keeps producing them is one where something
// below is corrupting bytes, which is worth knowing and is not something tether can do.
type scanner struct {
	buf []byte
}

// frame is one parsed frame, borrowed for the duration of the callback. Nothing keeps it.
type frame struct {
	kind      byte // command type: SREQ, AREQ, SRSP
	subsystem byte
	id        byte
	payload   []byte
}

// feed consumes b, calling onFrame for each complete frame and onSkip for runs of bytes that
// are not part of one. It never blocks, never allocates unboundedly — the buffer cannot exceed
// one maximum-length frame plus a byte — and has no path that panics on any input, which
// matters because it runs on the data path's goroutine.
func (s *scanner) feed(b []byte, onFrame func(frame), onSkip func(int)) {
	s.buf = append(s.buf, b...)

	for {
		// Drop anything before the first plausible start of frame.
		start := 0
		for start < len(s.buf) && s.buf[start] != sof {
			start++
		}
		if start > 0 {
			onSkip(start)
			s.buf = s.buf[start:]
		}
		if len(s.buf) < headerLen {
			return // not enough yet to know how long this frame claims to be
		}

		length := int(s.buf[1])
		total := length + overhead
		if len(s.buf) < total {
			return // a real frame still arriving, or junk that will be stepped over once it is
		}

		candidate := s.buf[:total]
		if checksum(candidate[:total-1]) != candidate[total-1] {
			// Not a frame boundary after all. Step over this byte only: the real SOF may be
			// the very next one, and skipping the claimed length would eat it.
			onSkip(1)
			s.buf = s.buf[1:]
			continue
		}

		onFrame(frame{
			kind:      candidate[2] >> 5,
			subsystem: candidate[2] & 0x1F,
			id:        candidate[3],
			payload:   candidate[headerLen : headerLen+length],
		})
		s.buf = s.buf[total:]
	}
}
