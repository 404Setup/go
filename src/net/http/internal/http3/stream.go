// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http3

import (
	"context"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/quic"
)

var (
	errDeadlineExceeded = os.ErrDeadlineExceeded
	errContextCanceled  = context.Canceled
	closedChan          = func() chan struct{} {
		ch := make(chan struct{})
		close(ch)
		return ch
	}()
	discardBufPool = sync.Pool{
		New: func() any {
			return new([4096]byte)
		},
	}
)

// A stream wraps a QUIC stream, providing methods to read/write various values.
type stream struct {
	stream *quic.Stream

	// lim is the current read limit.
	// Reading a frame header sets the limit to the end of the frame.
	// Reading past the limit or reading less than the limit and ending the frame
	// results in an error.
	// -1 indicates no limit.
	lim int64

	readDeadline  deadline
	writeDeadline deadline
}

// newConnStream creates a new stream on a connection.
// It writes the stream header for unidirectional streams.
//
// The stream returned by newStream is not flushed,
// and will not be sent to the peer until the caller calls
// Flush or writes enough data to the stream.
func newConnStream(ctx context.Context, qconn *quic.Conn, stype streamType) (*stream, error) {
	var qs *quic.Stream
	var err error
	if stype == streamTypeRequest {
		// Request streams are bidirectional.
		qs, err = qconn.NewStream(ctx)
	} else {
		// All other streams are unidirectional.
		qs, err = qconn.NewSendOnlyStream(ctx)
	}
	if err != nil {
		return nil, err
	}
	st := newStream(qs)
	if stype != streamTypeRequest {
		// Unidirectional stream header.
		st.writeVarint(int64(stype))
	}
	return st, err
}

func newStream(qs *quic.Stream) *stream {
	st := &stream{
		stream: qs,
		lim:    -1, // no limit
	}
	qs.SetReadContext(&st.readDeadline)
	qs.SetWriteContext(&st.writeDeadline)
	return st
}

func (st *stream) Close() error {
	st.readDeadline.stop()
	st.writeDeadline.stop()
	return st.stream.Close()
}

func (st *stream) CloseRead(code uint64) {
	st.readDeadline.stop()
	st.stream.StopSending(code)
}

func (st *stream) CloseWrite() {
	st.writeDeadline.stop()
	st.stream.CloseWrite()
}

func (st *stream) Reset(code uint64) {
	st.readDeadline.stop()
	st.writeDeadline.stop()
	st.stream.Reset(code)
}

// deadline implements context.Context directly so each stream avoids allocating
// context.WithCancelCause wrappers and taking a mutex on every byte read/write.
// When the timer expires, it cancels with [os.ErrDeadlineExceeded]; if manually
// stopped before expiration, it cancels with [context.Canceled]. Once canceled,
// the error is sticky and subsequent attempts to extend the deadline are ignored.
type deadline struct {
	cause atomic.Pointer[error] // nil until canceled; once set, sticky

	mu     sync.Mutex // Guards timer and doneCh.
	timer  *time.Timer
	doneCh chan struct{}
}

func (d *deadline) Deadline() (time.Time, bool) { return time.Time{}, false }
func (d *deadline) Value(any) any               { return nil }

func (d *deadline) Err() error {
	if ep := d.cause.Load(); ep != nil {
		return *ep
	}
	return nil
}

func (d *deadline) Done() <-chan struct{} {
	if d.cause.Load() != nil {
		return closedChan
	}
	d.mu.Lock()
	ch := d.doneCh
	if ch == nil {
		if d.cause.Load() != nil {
			d.mu.Unlock()
			return closedChan
		}
		ch = make(chan struct{})
		d.doneCh = ch
	}
	d.mu.Unlock()
	return ch
}

// cancelLocked sets the sticky cancellation cause and closes doneCh if allocated.
// The caller must hold d.mu.
func (d *deadline) cancelLocked(ep *error) {
	if d.cause.Load() != nil {
		return
	}
	d.cause.Store(ep)
	if d.doneCh != nil {
		close(d.doneCh)
	}
}

func (d *deadline) expire() {
	d.mu.Lock()
	d.cancelLocked(&errDeadlineExceeded)
	d.mu.Unlock()
}

// stopTimerLocked stops the deadline timer and sets it to nil.
// The caller must hold d.mu.
func (d *deadline) stopTimerLocked() {
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

// stop stops the deadline timer and cancels the context with
// [context.Canceled] as the cause.
func (d *deadline) stop() {
	d.mu.Lock()
	d.stopTimerLocked()
	d.cancelLocked(&errContextCanceled)
	d.mu.Unlock()
}

// err returns the deadline's context cancelation cause, if any.
func (d *deadline) err() error {
	if ep := d.cause.Load(); ep != nil {
		return *ep
	}
	return nil
}

// errOf returns the deadline's context cancelation cause if the given err is
// non-nil. This can be used to check whether an error value returned by I/O
// operations at the QUIC layer is non-nil because the deadline has expired.
func (d *deadline) errOf(err error) error {
	if err == nil {
		return nil
	}
	if ep := d.cause.Load(); ep != nil {
		return *ep
	}
	return err
}

// set configures a new deadline using the given deadlineTime.
// Once deadline is exceeded, it remains in the expired (sticky) state, and
// subsequent attempts to extend or reset the deadline are ignored.
func (d *deadline) set(deadlineTime time.Time) {
	if d.cause.Load() != nil { // Already expired, sticky error.
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.cause.Load() != nil { // Already expired, sticky error.
		return
	}
	if deadlineTime.IsZero() {
		d.stopTimerLocked()
		return
	}
	dur := time.Until(deadlineTime)
	if dur <= 0 {
		d.stopTimerLocked()
		d.cancelLocked(&errDeadlineExceeded)
		return
	}
	if d.timer == nil {
		d.timer = time.AfterFunc(dur, d.expire)
	} else {
		d.timer.Reset(dur)
	}
}

// readFrameHeader reads the type and length fields of an HTTP/3 frame.
// It sets the read limit to the end of the frame.
//
// https://www.rfc-editor.org/rfc/rfc9114.html#section-7.1
func (st *stream) readFrameHeader() (ftype frameType, err error) {
	if st.lim >= 0 {
		// We shouldn't call readFrameHeader before ending the previous frame.
		return 0, errH3FrameError
	}
	ftype, err = readVarint[frameType](st)
	if err != nil {
		return 0, err
	}
	size, err := st.readVarint()
	if err != nil {
		return 0, err
	}
	st.lim = size
	return ftype, nil
}

// endFrame is called after reading a frame to reset the read limit.
// It returns an error if the entire contents of a frame have not been read.
func (st *stream) endFrame() error {
	if st.lim != 0 {
		return &connectionError{
			code:    errH3FrameError,
			message: "invalid HTTP/3 frame",
		}
	}
	st.lim = -1
	return nil
}

// readFrameData returns the remaining data in the current frame.
func (st *stream) readFrameData() ([]byte, error) {
	if st.lim < 0 {
		return nil, errH3FrameError
	}
	// TODO: Pool buffers to avoid allocation here.
	b := make([]byte, st.lim)
	_, err := io.ReadFull(st, b)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ReadByte reads one byte from the stream.
func (st *stream) ReadByte() (b byte, err error) {
	// Check the deadline before doing I/O operations on the QUIC layer. We do
	// this because the QUIC layer implements a fast path for I/O operations,
	// allowing Read & Write to succeed depending on the state of buffer, even
	// if its context has been canceled. By always checking the deadline here,
	// we make it so that I/O operations fail as soon as its relevant deadline
	// has been exceeded.
	if err := st.readDeadline.err(); err != nil {
		return 0, err
	}
	if err := st.recordBytesRead(1); err != nil {
		return 0, err
	}
	b, err = st.stream.ReadByte()
	if err == io.EOF && st.lim >= 0 {
		return 0, errH3FrameError
	}
	return b, st.readDeadline.errOf(err)
}

// Read reads from the stream.
func (st *stream) Read(b []byte) (int, error) {
	// Check the deadline before doing I/O operations on the QUIC layer. We do
	// this because the QUIC layer implements a fast path for I/O operations,
	// allowing Read & Write to succeed depending on the state of buffer, even
	// if its context has been canceled. By always checking the deadline here,
	// we make it so that I/O operations fail as soon as its relevant deadline
	// has been exceeded.
	if err := st.readDeadline.err(); err != nil {
		return 0, err
	}
	n, err := st.stream.Read(b)
	if e2 := st.recordBytesRead(n); e2 != nil {
		return 0, e2
	}
	if err == io.EOF {
		if st.lim == 0 {
			// EOF at end of frame, ignore.
			return n, nil
		} else if st.lim > 0 {
			// EOF inside frame, error.
			return 0, errH3FrameError
		} else {
			// EOF outside of frame, surface to caller.
			return n, io.EOF
		}
	}
	return n, st.readDeadline.errOf(err)
}

// discardUnknownFrame discards an unknown frame.
//
// HTTP/3 requires that unknown frames be ignored on all streams.
// However, a known frame appearing in an unexpected place is a fatal error,
// so this returns an error if the frame is one we know.
func (st *stream) discardUnknownFrame(ftype frameType) error {
	switch ftype {
	case frameTypeData,
		frameTypeHeaders,
		frameTypeCancelPush,
		frameTypeSettings,
		frameTypePushPromise,
		frameTypeGoaway,
		frameTypeMaxPushID:
		return &connectionError{
			code:    errH3FrameUnexpected,
			message: "unexpected " + ftype.String() + " frame",
		}
	}
	return st.discardFrame()
}

// discardFrame discards any remaining data in the current frame and resets the read limit.
func (st *stream) discardFrame() error {
	if st.lim <= 0 {
		st.lim = -1
		return nil
	}
	bp := discardBufPool.Get().(*[4096]byte)
	defer discardBufPool.Put(bp)
	for st.lim > 0 {
		buf := bp[:]
		if st.lim < int64(len(buf)) {
			buf = buf[:st.lim]
		}
		n, err := st.Read(buf)
		if err != nil {
			return &streamError{errH3FrameError, err.Error()}
		}
		if n == 0 {
			return &streamError{errH3FrameError, io.ErrUnexpectedEOF.Error()}
		}
	}
	st.lim = -1
	return nil
}

// Write writes to the stream.
func (st *stream) Write(b []byte) (int, error) {
	// Check the deadline before doing I/O operations on the QUIC layer. We do
	// this because the QUIC layer implements a fast path for I/O operations,
	// allowing Read & Write to succeed depending on the state of buffer, even
	// if its context has been canceled. By always checking the deadline here,
	// we make it so that I/O operations fail as soon as its relevant deadline
	// has been exceeded.
	if err := st.writeDeadline.err(); err != nil {
		return 0, err
	}
	n, err := st.stream.Write(b)
	return n, st.writeDeadline.errOf(err)
}

// Flush commits data written to the stream.
func (st *stream) Flush() error {
	// Check the deadline before doing I/O operations on the QUIC layer. We do
	// this because the QUIC layer implements a fast path for I/O operations,
	// allowing Read & Write to succeed depending on the state of buffer, even
	// if its context has been canceled. By always checking the deadline here,
	// we make it so that I/O operations fail as soon as its relevant deadline
	// has been exceeded.
	if err := st.writeDeadline.err(); err != nil {
		return err
	}
	return st.writeDeadline.errOf(st.stream.Flush())
}

// WriteByte writes one byte to the stream.
func (st *stream) WriteByte(c byte) error {
	// Check the deadline before doing I/O operations on the QUIC layer. We do
	// this because the QUIC layer implements a fast path for I/O operations,
	// allowing Read & Write to succeed depending on the state of buffer, even
	// if its context has been canceled. By always checking the deadline here,
	// we make it so that I/O operations fail as soon as its relevant deadline
	// has been exceeded.
	if err := st.writeDeadline.err(); err != nil {
		return err
	}
	return st.writeDeadline.errOf(st.stream.WriteByte(c))
}

// readVarint reads a QUIC variable-length integer from the stream.
func (st *stream) readVarint() (v int64, err error) {
	b, err := st.ReadByte()
	if err != nil {
		return 0, err
	}
	v = int64(b & 0x3f)
	n := 1 << (b >> 6)
	for i := 1; i < n; i++ {
		b, err := st.ReadByte()
		if err != nil {
			if err == io.EOF {
				return 0, errH3FrameError
			}
			return 0, err
		}
		v = (v << 8) | int64(b)
	}
	return v, nil
}

// readVarint reads a varint of a particular type.
func readVarint[T ~int64 | ~uint64](st *stream) (T, error) {
	v, err := st.readVarint()
	return T(v), err
}

// writeVarint writes a QUIC variable-length integer to the stream.
func (st *stream) writeVarint(v int64) {
	var buf [8]byte
	switch {
	case v <= (1<<6)-1:
		st.WriteByte(byte(v))
	case v <= (1<<14)-1:
		buf[0] = (1 << 6) | byte(v>>8)
		buf[1] = byte(v)
		st.Write(buf[:2])
	case v <= (1<<30)-1:
		buf[0] = (2 << 6) | byte(v>>24)
		buf[1] = byte(v >> 16)
		buf[2] = byte(v >> 8)
		buf[3] = byte(v)
		st.Write(buf[:4])
	case v <= (1<<62)-1:
		buf[0] = (3 << 6) | byte(v>>56)
		buf[1] = byte(v >> 48)
		buf[2] = byte(v >> 40)
		buf[3] = byte(v >> 32)
		buf[4] = byte(v >> 24)
		buf[5] = byte(v >> 16)
		buf[6] = byte(v >> 8)
		buf[7] = byte(v)
		st.Write(buf[:8])
	default:
		panic("varint too large")
	}
}

// writeFrameHeader writes an HTTP/3 frame type and length in a single stream write.
func (st *stream) writeFrameHeader(ftype frameType, size int64) {
	var buf [16]byte
	b := appendVarint(buf[:0], uint64(ftype))
	b = appendVarint(b, uint64(size))
	st.Write(b)
}

// recordBytesRead records that n bytes have been read.
// It returns an error if the read passes the current limit.
func (st *stream) recordBytesRead(n int) error {
	if st.lim < 0 {
		return nil
	}
	st.lim -= int64(n)
	if st.lim < 0 {
		st.stream = nil // panic if we try to read again
		return &connectionError{
			code:    errH3FrameError,
			message: "invalid HTTP/3 frame",
		}
	}
	return nil
}
