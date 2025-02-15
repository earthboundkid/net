// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"context"
	"errors"
	"io"
)

type Stream struct {
	id   streamID
	conn *Conn

	// ingate's lock guards all receive-related state.
	//
	// The gate condition is set if a read from the stream will not block,
	// either because the stream has available data or because the read will fail.
	ingate    gate
	in        pipe            // received data
	inwin     int64           // last MAX_STREAM_DATA sent to the peer
	insendmax sentVal         // set when we should send MAX_STREAM_DATA to the peer
	inmaxbuf  int64           // maximum amount of data we will buffer
	insize    int64           // stream final size; -1 before this is known
	inset     rangeset[int64] // received ranges

	// outgate's lock guards all send-related state.
	//
	// The gate condition is set if a write to the stream will not block,
	// either because the stream has available flow control or because
	// the write will fail.
	outgate    gate
	out        pipe            // buffered data to send
	outwin     int64           // maximum MAX_STREAM_DATA received from the peer
	outmaxbuf  int64           // maximum amount of data we will buffer
	outunsent  rangeset[int64] // ranges buffered but not yet sent
	outacked   rangeset[int64] // ranges sent and acknowledged
	outopened  sentVal         // set if we should open the stream
	outclosed  sentVal         // set by CloseWrite
	outblocked sentVal         // set when a write to the stream is blocked by flow control

	prev, next *Stream // guarded by streamsState.sendMu
}

// newStream returns a new stream.
//
// The stream's ingate and outgate are locked.
// (We create the stream with locked gates so after the caller
// initializes the flow control window,
// unlocking outgate will set the stream writability state.)
func newStream(c *Conn, id streamID) *Stream {
	s := &Stream{
		conn:    c,
		id:      id,
		insize:  -1, // -1 indicates the stream size is unknown
		ingate:  newLockedGate(),
		outgate: newLockedGate(),
	}
	return s
}

// IsReadOnly reports whether the stream is read-only
// (a unidirectional stream created by the peer).
func (s *Stream) IsReadOnly() bool {
	return s.id.streamType() == uniStream && s.id.initiator() != s.conn.side
}

// IsWriteOnly reports whether the stream is write-only
// (a unidirectional stream created locally).
func (s *Stream) IsWriteOnly() bool {
	return s.id.streamType() == uniStream && s.id.initiator() == s.conn.side
}

// Read reads data from the stream.
// See ReadContext for more details.
func (s *Stream) Read(b []byte) (n int, err error) {
	return s.ReadContext(context.Background(), b)
}

// ReadContext reads data from the stream.
//
// ReadContext returns as soon as at least one byte of data is available.
//
// If the peer closes the stream cleanly, ReadContext returns io.EOF after
// returning all data sent by the peer.
// If the peer terminates reads abruptly, ReadContext returns StreamResetError.
func (s *Stream) ReadContext(ctx context.Context, b []byte) (n int, err error) {
	if s.IsWriteOnly() {
		return 0, errors.New("read from write-only stream")
	}
	// Wait until data is available.
	if err := s.conn.waitAndLockGate(ctx, &s.ingate); err != nil {
		return 0, err
	}
	defer s.inUnlock()
	if s.insize == s.in.start {
		return 0, io.EOF
	}
	// Getting here indicates the stream contains data to be read.
	if len(s.inset) < 1 || s.inset[0].start != 0 || s.inset[0].end <= s.in.start {
		panic("BUG: inconsistent input stream state")
	}
	if size := int(s.inset[0].end - s.in.start); size < len(b) {
		b = b[:size]
	}
	start := s.in.start
	end := start + int64(len(b))
	s.in.copy(start, b)
	s.in.discardBefore(end)
	if s.insize == -1 || s.insize > s.inwin {
		if shouldUpdateFlowControl(s.inwin-s.in.start, s.inmaxbuf) {
			// Update stream flow control with a STREAM_MAX_DATA frame.
			s.insendmax.setUnsent()
		}
	}
	if end == s.insize {
		return len(b), io.EOF
	}
	return len(b), nil
}

// shouldUpdateFlowControl determines whether to send a flow control window update.
//
// We want to balance keeping the peer well-supplied with flow control with not sending
// many small updates.
func shouldUpdateFlowControl(curwin, maxwin int64) bool {
	// Update flow control if doing so gives the peer at least 64k tokens,
	// or if it will double the current window.
	return maxwin-curwin >= 64<<10 || curwin*2 < maxwin
}

// Write writes data to the stream.
// See WriteContext for more details.
func (s *Stream) Write(b []byte) (n int, err error) {
	return s.WriteContext(context.Background(), b)
}

// WriteContext writes data to the stream.
//
// WriteContext writes data to the stream write buffer.
// Buffered data is only sent when the buffer is sufficiently full.
// Call the Flush method to ensure buffered data is sent.
//
// If the peer aborts reads on the stream, ReadContext returns StreamResetError.
func (s *Stream) WriteContext(ctx context.Context, b []byte) (n int, err error) {
	if s.IsReadOnly() {
		return 0, errors.New("write to read-only stream")
	}
	canWrite := s.outgate.lock()
	if s.outclosed.isSet() {
		s.outUnlock()
		return 0, errors.New("write to closed stream")
	}
	if len(b) == 0 {
		// We aren't writing any data, but send a STREAM frame to open the stream
		// if we haven't done so already.
		s.outopened.set()
	}
	for len(b) > 0 {
		// The first time through this loop, we may or may not be write blocked.
		// We exit the loop after writing all data, so on subsequent passes through
		// the loop we are always write blocked.
		if !canWrite {
			// We're blocked, either by flow control or by our own buffer limit.
			// We either need the peer to extend our flow control window,
			// or ack some of our outstanding packets.
			if s.out.end == s.outwin {
				// We're blocked by flow control.
				// Send a STREAM_DATA_BLOCKED frame to let the peer know.
				s.outblocked.setUnsent()
			}
			s.outUnlock()
			if err := s.conn.waitAndLockGate(ctx, &s.outgate); err != nil {
				return n, err
			}
			// Successfully returning from waitAndLockGate means we are no longer
			// write blocked. (Unlike traditional condition variables, gates do not
			// have spurious wakeups.)
		}
		s.outblocked.clear()
		// Write limit is min(our own buffer limit, the peer-provided flow control window).
		// This is a stream offset.
		lim := min(s.out.start+s.outmaxbuf, s.outwin)
		// Amount to write is min(the full buffer, data up to the write limit).
		// This is a number of bytes.
		nn := min(int64(len(b)), lim-s.out.end)
		// Copy the data into the output buffer and mark it as unsent.
		s.outunsent.add(s.out.end, s.out.end+nn)
		s.out.writeAt(b[:nn], s.out.end)
		s.outopened.set()
		b = b[nn:]
		n += int(nn)
		// If we have bytes left to send, we're blocked.
		canWrite = false
	}
	s.outUnlock()
	return n, nil
}

// Close closes the stream.
// See CloseContext for more details.
func (s *Stream) Close() error {
	return s.CloseContext(context.Background())
}

// CloseContext closes the stream.
// Any blocked stream operations will be unblocked and return errors.
//
// CloseContext flushes any data in the stream write buffer and waits for the peer to
// acknowledge receipt of the data.
// If the stream has been reset, it waits for the peer to acknowledge the reset.
// If the context expires before the peer receives the stream's data,
// CloseContext discards the buffer and returns the context error.
func (s *Stream) CloseContext(ctx context.Context) error {
	s.CloseRead()
	s.CloseWrite()
	// TODO: wait for peer to acknowledge data
	// TODO: Return code from peer's RESET_STREAM frame?
	return nil
}

// CloseRead aborts reads on the stream.
// Any blocked reads will be unblocked and return errors.
//
// CloseRead notifies the peer that the stream has been closed for reading.
// It does not wait for the peer to acknowledge the closure.
// Use CloseContext to wait for the peer's acknowledgement.
func (s *Stream) CloseRead() {
	if s.IsWriteOnly() {
		return
	}
	// TODO: support read-closing streams with a STOP_SENDING frame
}

// CloseWrite aborts writes on the stream.
// Any blocked writes will be unblocked and return errors.
//
// CloseWrite sends any data in the stream write buffer to the peer.
// It does not wait for the peer to acknowledge receipt of the data.
// Use CloseContext to wait for the peer's acknowledgement.
func (s *Stream) CloseWrite() {
	if s.IsReadOnly() {
		return
	}
	s.outgate.lock()
	defer s.outUnlock()
	s.outclosed.set()
}

// inUnlock unlocks s.ingate.
// It sets the gate condition if reads from s will not block.
// If s has receive-related frames to write, it notifies the Conn.
func (s *Stream) inUnlock() {
	if s.inUnlockNoQueue() {
		s.conn.queueStreamForSend(s)
	}
}

// inUnlockNoQueue is inUnlock,
// but reports whether s has frames to write rather than notifying the Conn.
func (s *Stream) inUnlockNoQueue() (shouldSend bool) {
	// TODO: STOP_SENDING
	canRead := s.inset.contains(s.in.start) || // data available to read
		s.insize == s.in.start // at EOF
	s.ingate.unlock(canRead)
	return s.insendmax.shouldSend() // STREAM_MAX_DATA
}

// outUnlock unlocks s.outgate.
// It sets the gate condition if writes to s will not block.
// If s has send-related frames to write, it notifies the Conn.
func (s *Stream) outUnlock() {
	if s.outUnlockNoQueue() {
		s.conn.queueStreamForSend(s)
	}
}

// outUnlockNoQueue is outUnlock,
// but reports whether s has frames to write rather than notifying the Conn.
func (s *Stream) outUnlockNoQueue() (shouldSend bool) {
	lim := min(s.out.start+s.outmaxbuf, s.outwin)
	canWrite := lim > s.out.end || // available flow control
		s.outclosed.isSet() // closed
	s.outgate.unlock(canWrite)
	return len(s.outunsent) > 0 || // STREAM frame with data
		s.outclosed.shouldSend() || // STREAM frame with FIN bit
		s.outopened.shouldSend() || // STREAM frame with no data
		s.outblocked.shouldSend() // STREAM_DATA_BLOCKED
}

// handleData handles data received in a STREAM frame.
func (s *Stream) handleData(off int64, b []byte, fin bool) error {
	s.ingate.lock()
	defer s.inUnlock()
	end := off + int64(len(b))
	if end > s.inwin {
		// The peer sent us data past the maximum flow control window we gave them.
		return localTransportError(errFlowControl)
	}
	if s.insize != -1 && end > s.insize {
		// The peer sent us data past the final size of the stream they previously gave us.
		return localTransportError(errFinalSize)
	}
	s.in.writeAt(b, off)
	s.inset.add(off, end)
	if fin {
		if s.insize != -1 && s.insize != end {
			// The peer changed the final size of the stream.
			return localTransportError(errFinalSize)
		}
		s.insize = end
		// The peer has enough flow control window to send the entire stream.
		s.insendmax.clear()
	}
	return nil
}

// handleMaxStreamData handles an update received in a MAX_STREAM_DATA frame.
func (s *Stream) handleMaxStreamData(maxStreamData int64) error {
	s.outgate.lock()
	defer s.outUnlock()
	s.outwin = max(maxStreamData, s.outwin)
	return nil
}

// ackOrLoss handles the fate of stream frames other than STREAM.
func (s *Stream) ackOrLoss(pnum packetNumber, ftype byte, fate packetFate) {
	// Frames which carry new information each time they are sent
	// (MAX_STREAM_DATA, STREAM_DATA_BLOCKED) must only be marked
	// as received if the most recent packet carrying this frame is acked.
	//
	// Frames which are always the same (STOP_SENDING, RESET_STREAM)
	// can be marked as received if any packet carrying this frame is acked.
	switch ftype {
	case frameTypeMaxStreamData:
		s.ingate.lock()
		s.insendmax.ackLatestOrLoss(pnum, fate)
		s.inUnlock()
	case frameTypeStreamDataBlocked:
		s.outgate.lock()
		s.outblocked.ackLatestOrLoss(pnum, fate)
		s.outUnlock()
	default:
		// TODO: Handle STOP_SENDING, RESET_STREAM.
		panic("unhandled frame type")
	}
}

// ackOrLossData handles the fate of a STREAM frame.
func (s *Stream) ackOrLossData(pnum packetNumber, start, end int64, fin bool, fate packetFate) {
	s.outgate.lock()
	defer s.outUnlock()
	s.outopened.ackOrLoss(pnum, fate)
	if fin {
		s.outclosed.ackOrLoss(pnum, fate)
	}
	switch fate {
	case packetAcked:
		s.outacked.add(start, end)
		s.outunsent.sub(start, end)
		// If this ack is for data at the start of the send buffer, we can now discard it.
		if s.outacked.contains(s.out.start) {
			s.out.discardBefore(s.outacked[0].end)
		}
	case packetLost:
		// Mark everything lost, but not previously acked, as needing retransmission.
		// We do this by adding all the lost bytes to outunsent, and then
		// removing everything already acked.
		s.outunsent.add(start, end)
		for _, a := range s.outacked {
			s.outunsent.sub(a.start, a.end)
		}
	}
}

// appendInFrames appends STOP_SENDING and MAX_STREAM_DATA frames
// to the current packet.
//
// It returns true if no more frames need appending,
// false if not everything fit in the current packet.
func (s *Stream) appendInFrames(w *packetWriter, pnum packetNumber, pto bool) bool {
	s.ingate.lock()
	defer s.inUnlockNoQueue()
	// TODO: STOP_SENDING
	if s.insendmax.shouldSendPTO(pto) {
		// MAX_STREAM_DATA
		maxStreamData := s.in.start + s.inmaxbuf
		if !w.appendMaxStreamDataFrame(s.id, maxStreamData) {
			return false
		}
		s.inwin = maxStreamData
		s.insendmax.setSent(pnum)
	}
	return true
}

// appendOutFrames appends RESET_STREAM, STREAM_DATA_BLOCKED, and STREAM frames
// to the current packet.
//
// It returns true if no more frames need appending,
// false if not everything fit in the current packet.
func (s *Stream) appendOutFrames(w *packetWriter, pnum packetNumber, pto bool) bool {
	s.outgate.lock()
	defer s.outUnlockNoQueue()
	// TODO: RESET_STREAM
	if s.outblocked.shouldSendPTO(pto) {
		// STREAM_DATA_BLOCKED
		if !w.appendStreamDataBlockedFrame(s.id, s.out.end) {
			return false
		}
		s.outblocked.setSent(pnum)
		s.frameOpensStream(pnum)
	}
	// STREAM
	for {
		off, size := dataToSend(s.out, s.outunsent, s.outacked, pto)
		fin := s.outclosed.isSet() && off+size == s.out.end
		shouldSend := size > 0 || // have data to send
			s.outopened.shouldSendPTO(pto) || // should open the stream
			(fin && s.outclosed.shouldSendPTO(pto)) // should close the stream
		if !shouldSend {
			return true
		}
		b, added := w.appendStreamFrame(s.id, off, int(size), fin)
		if !added {
			return false
		}
		s.out.copy(off, b)
		s.outunsent.sub(off, off+int64(len(b)))
		s.frameOpensStream(pnum)
		if fin {
			s.outclosed.setSent(pnum)
		}
		if pto {
			return true
		}
		if int64(len(b)) < size {
			return false
		}
	}
}

// frameOpensStream records that we're sending a frame that will open the stream.
//
// If we don't have an acknowledgement from the peer for a previous frame opening the stream,
// record this packet as being the latest one to open it.
func (s *Stream) frameOpensStream(pnum packetNumber) {
	if !s.outopened.isReceived() {
		s.outopened.setSent(pnum)
	}
}

// dataToSend returns the next range of data to send in a STREAM or CRYPTO_STREAM.
func dataToSend(out pipe, outunsent, outacked rangeset[int64], pto bool) (start, size int64) {
	switch {
	case pto:
		// On PTO, resend unacked data that fits in the probe packet.
		// For simplicity, we send the range starting at s.out.start
		// (which is definitely unacked, or else we would have discarded it)
		// up to the next acked byte (if any).
		//
		// This may miss unacked data starting after that acked byte,
		// but avoids resending data the peer has acked.
		for _, r := range outacked {
			if r.start > out.start {
				return out.start, r.start - out.start
			}
		}
		return out.start, out.end - out.start
	case outunsent.numRanges() > 0:
		return outunsent.min(), outunsent[0].size()
	default:
		return out.end, 0
	}
}
