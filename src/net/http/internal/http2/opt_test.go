// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

func encodeHPACKFields(t *testing.T, fields ...hpack.HeaderField) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	for _, f := range fields {
		if err := enc.WriteField(f); err != nil {
			t.Fatalf("hpack.WriteField(%v): %v", f, err)
		}
	}
	return buf.Bytes()
}

func TestFramerReuseFramesExpanded(t *testing.T) {
	t.Run("SingleFramesAndPriorityZeroing", func(t *testing.T) {
		var wire bytes.Buffer
		wf := NewFramer(&wire, nil)

		if err := wf.WriteData(1, false, []byte("abc")); err != nil {
			t.Fatal(err)
		}
		if err := wf.WriteData(1, true, []byte("def")); err != nil {
			t.Fatal(err)
		}
		// First HEADERS with Priority, second without Priority.
		if err := wf.WriteHeaders(HeadersFrameParam{
			StreamID:      1,
			BlockFragment: []byte{0x82},
			EndHeaders:    true,
			Priority: PriorityParam{
				StreamDep: 7,
				Exclusive: true,
				Weight:    42,
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := wf.WriteHeaders(HeadersFrameParam{
			StreamID:      3,
			BlockFragment: []byte{0x82},
			EndHeaders:    true,
		}); err != nil {
			t.Fatal(err)
		}
		if err := wf.WriteWindowUpdate(0, 100); err != nil {
			t.Fatal(err)
		}
		if err := wf.WriteWindowUpdate(1, 200); err != nil {
			t.Fatal(err)
		}
		if err := wf.WriteRSTStream(1, ErrCodeCancel); err != nil {
			t.Fatal(err)
		}
		if err := wf.WriteRSTStream(3, ErrCodeNo); err != nil {
			t.Fatal(err)
		}
		if err := wf.WritePing(false, [8]byte{1, 2, 3, 4, 5, 6, 7, 8}); err != nil {
			t.Fatal(err)
		}
		if err := wf.WritePing(true, [8]byte{8, 7, 6, 5, 4, 3, 2, 1}); err != nil {
			t.Fatal(err)
		}
		if err := wf.WriteHeaders(HeadersFrameParam{
			StreamID:      5,
			BlockFragment: []byte{0x82},
			EndHeaders:    false,
		}); err != nil {
			t.Fatal(err)
		}
		if err := wf.WriteContinuation(5, false, []byte("part1")); err != nil {
			t.Fatal(err)
		}
		if err := wf.WriteContinuation(5, true, []byte("part2")); err != nil {
			t.Fatal(err)
		}
		// GoAwayFrame must NOT be reused.
		if err := wf.WriteGoAway(1, ErrCodeNo, []byte("bye1")); err != nil {
			t.Fatal(err)
		}
		if err := wf.WriteGoAway(3, ErrCodeCancel, []byte("bye2")); err != nil {
			t.Fatal(err)
		}

		rf := NewFramer(nil, &wire)
		rf.SetReuseFrames()

		f1, err := rf.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		df1 := f1.(*DataFrame)
		f2, err := rf.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		df2 := f2.(*DataFrame)
		if df1 != df2 {
			t.Errorf("DataFrame not reused: %p != %p", df1, df2)
		}

		hf1Raw, err := rf.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		hf1 := hf1Raw.(*HeadersFrame)
		if !hf1.HasPriority() || hf1.Priority.StreamDep != 7 || !hf1.Priority.Exclusive || hf1.Priority.Weight != 42 {
			t.Fatalf("first HeadersFrame priority = %+v", hf1.Priority)
		}
		hf2Raw, err := rf.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		hf2 := hf2Raw.(*HeadersFrame)
		if hf1 != hf2 {
			t.Errorf("HeadersFrame not reused: %p != %p", hf1, hf2)
		}
		if hf2.HasPriority() || hf2.Priority != (PriorityParam{}) {
			t.Errorf("second HeadersFrame retained stale Priority: %+v", hf2.Priority)
		}

		wu1Raw, err := rf.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		wu2Raw, err := rf.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if wu1Raw.(*WindowUpdateFrame) != wu2Raw.(*WindowUpdateFrame) {
			t.Errorf("WindowUpdateFrame not reused")
		}

		rst1Raw, err := rf.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		rst2Raw, err := rf.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if rst1Raw.(*RSTStreamFrame) != rst2Raw.(*RSTStreamFrame) {
			t.Errorf("RSTStreamFrame not reused")
		}

		p1Raw, err := rf.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		p2Raw, err := rf.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if p1Raw.(*PingFrame) != p2Raw.(*PingFrame) {
			t.Errorf("PingFrame not reused")
		}
		if p2Raw.(*PingFrame).Data != ([8]byte{8, 7, 6, 5, 4, 3, 2, 1}) {
			t.Errorf("PingFrame Data = %v", p2Raw.(*PingFrame).Data)
		}

		// Read the non-EndHeaders HEADERS frame preceding CONTINUATION.
		if _, err := rf.ReadFrame(); err != nil {
			t.Fatal(err)
		}
		c1Raw, err := rf.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		c2Raw, err := rf.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if c1Raw.(*ContinuationFrame) != c2Raw.(*ContinuationFrame) {
			t.Errorf("ContinuationFrame not reused")
		}

		g1Raw, err := rf.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		g2Raw, err := rf.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if g1Raw.(*GoAwayFrame) == g2Raw.(*GoAwayFrame) {
			t.Errorf("GoAwayFrame was unexpectedly reused")
		}
	})

	t.Run("MetaHeadersReuseAndContinuationAndCap", func(t *testing.T) {
		var wire bytes.Buffer
		wf := NewFramer(&wire, nil)

		hdr1 := encodeHPACKFields(t,
			hpack.HeaderField{Name: ":method", Value: "GET"},
			hpack.HeaderField{Name: ":scheme", Value: "https"},
			hpack.HeaderField{Name: ":path", Value: "/one"},
			hpack.HeaderField{Name: ":authority", Value: "example.com"},
			hpack.HeaderField{Name: "x-test", Value: "1"},
		)
		// Split hdr1 across HEADERS + CONTINUATION to test recursive ReadFrame.
		mid := len(hdr1) / 2
		if err := wf.WriteHeaders(HeadersFrameParam{
			StreamID:      1,
			BlockFragment: hdr1[:mid],
			EndHeaders:    false,
			EndStream:     true,
		}); err != nil {
			t.Fatal(err)
		}
		if err := wf.WriteContinuation(1, true, hdr1[mid:]); err != nil {
			t.Fatal(err)
		}

		hdr2 := encodeHPACKFields(t,
			hpack.HeaderField{Name: ":method", Value: "POST"},
			hpack.HeaderField{Name: ":scheme", Value: "https"},
			hpack.HeaderField{Name: ":path", Value: "/two"},
			hpack.HeaderField{Name: ":authority", Value: "example.com"},
		)
		if err := wf.WriteHeaders(HeadersFrameParam{
			StreamID:      3,
			BlockFragment: hdr2,
			EndHeaders:    true,
			EndStream:     false,
		}); err != nil {
			t.Fatal(err)
		}

		// Oversized header list (> maxCachedMetaFields) must not be pinned in cache.
		bigFields := []hpack.HeaderField{
			{Name: ":method", Value: "GET"},
			{Name: ":scheme", Value: "https"},
			{Name: ":path", Value: "/big"},
			{Name: ":authority", Value: "example.com"},
		}
		for i := 0; i < maxCachedMetaFields+20; i++ {
			bigFields = append(bigFields, hpack.HeaderField{Name: "x-extra", Value: "v"})
		}
		hdrBig := encodeHPACKFields(t, bigFields...)
		if err := wf.WriteHeaders(HeadersFrameParam{
			StreamID:      5,
			BlockFragment: hdrBig,
			EndHeaders:    true,
			EndStream:     true,
		}); err != nil {
			t.Fatal(err)
		}
		if err := wf.WritePing(false, [8]byte{}); err != nil {
			t.Fatal(err)
		}

		rf := NewFramer(nil, &wire)
		rf.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
		rf.MaxHeaderListSize = 1 << 20
		rf.SetReuseFrames()

		f1, err := rf.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame 1: %v", err)
		}
		mh1 := f1.(*MetaHeadersFrame)
		if mh1.StreamID != 1 || !mh1.StreamEnded() || len(mh1.Fields) != 5 {
			t.Fatalf("mh1 = stream %d endStream=%v len=%d", mh1.StreamID, mh1.StreamEnded(), len(mh1.Fields))
		}
		if got := mh1.PseudoValue("path"); got != "/one" {
			t.Fatalf("mh1 :path = %q, want /one", got)
		}
		fields1Ptr := &mh1.Fields[0]

		f2, err := rf.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame 2: %v", err)
		}
		mh2 := f2.(*MetaHeadersFrame)
		if mh1 != mh2 {
			t.Errorf("MetaHeadersFrame not reused: %p != %p", mh1, mh2)
		}
		if &mh2.Fields[0] != fields1Ptr {
			t.Errorf("MetaHeadersFrame Fields backing array not reused")
		}
		if mh2.StreamID != 3 || mh2.StreamEnded() || len(mh2.Fields) != 4 || mh2.PseudoValue("path") != "/two" {
			t.Fatalf("mh2 unexpected: stream=%d endStream=%v fields=%v", mh2.StreamID, mh2.StreamEnded(), mh2.Fields)
		}

		f3, err := rf.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame 3: %v", err)
		}
		mh3 := f3.(*MetaHeadersFrame)
		if len(mh3.Fields) != len(bigFields) {
			t.Fatalf("mh3 len = %d, want %d", len(mh3.Fields), len(bigFields))
		}

		// Reading next frame must invalidate and drop oversized cached slice.
		if _, err := rf.ReadFrame(); err != nil {
			t.Fatalf("ReadFrame 4: %v", err)
		}
		if cap(rf.frameCache.metaFields) > maxCachedMetaFields {
			t.Errorf("oversized metaFields slice retained in cache: cap=%d > %d", cap(rf.frameCache.metaFields), maxCachedMetaFields)
		}
		if rf.frameCache.metaHeadersFrame.Fields != nil {
			t.Errorf("metaHeadersFrame.Fields not cleared on invalidateLastFrame")
		}
	})
}

type countingWriter struct {
	buf    bytes.Buffer
	writes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.buf.Write(p)
}

type errConn struct {
	net.Conn
	err error
}

func (c errConn) Write([]byte) (int, error)        { return 0, c.err }
func (c errConn) SetWriteDeadline(time.Time) error { return nil }

func TestWriteFrameDirect(t *testing.T) {
	payloadSmall := []byte("hello world")
	payloadLarge := bytes.Repeat([]byte("x"), 5000)

	writeSeq := func(f *Framer) error {
		if err := f.WriteData(1, false, payloadSmall); err != nil {
			return err
		}
		if err := f.WriteData(1, true, payloadLarge); err != nil {
			return err
		}
		if err := f.WriteHeaders(HeadersFrameParam{
			StreamID:      3,
			BlockFragment: payloadSmall,
			EndHeaders:    false,
		}); err != nil {
			return err
		}
		if err := f.WriteContinuation(3, true, payloadLarge); err != nil {
			return err
		}
		return nil
	}

	// Reference using generic io.Writer (also verifies single Write per frame).
	var ref countingWriter
	refFramer := NewFramer(&ref, nil)
	if err := writeSeq(refFramer); err != nil {
		t.Fatalf("ref writeSeq: %v", err)
	}
	if ref.writes != 4 {
		t.Errorf("generic io.Writer got %d Write calls, want 4", ref.writes)
	}

	// *bufio.Writer path (small fits direct, large overflows to fallback).
	var bufioOut bytes.Buffer
	bw := bufio.NewWriterSize(&bufioOut, 4096)
	bufioFramer := NewFramer(bw, nil)
	if err := writeSeq(bufioFramer); err != nil {
		t.Fatalf("bufio writeSeq: %v", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bufioOut.Bytes(), ref.buf.Bytes()) {
		t.Fatalf("*bufio.Writer wire bytes differ from reference")
	}

	// *bufferedWriter path over net.Pipe.
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	var pipeOut bytes.Buffer
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(&pipeOut, c2)
		readDone <- err
	}()
	h2bw := newBufferedWriter(c1, 0)
	h2bwFramer := NewFramer(h2bw, nil)
	if err := writeSeq(h2bwFramer); err != nil {
		t.Fatalf("bufferedWriter writeSeq: %v", err)
	}
	if err := h2bw.Flush(); err != nil {
		t.Fatalf("bufferedWriter Flush: %v", err)
	}
	c1.Close()
	if err := <-readDone; err != nil {
		t.Fatalf("pipe read: %v", err)
	}
	if !bytes.Equal(pipeOut.Bytes(), ref.buf.Bytes()) {
		t.Fatalf("*bufferedWriter wire bytes differ from reference")
	}

	// Sticky error on *bufferedWriter must be returned immediately by direct path.
	wantErr := errors.New("boom")
	errBW := newBufferedWriter(errConn{err: wantErr}, 0)
	errBW.werr = wantErr
	errFramer := NewFramer(errBW, nil)
	if err := errFramer.WriteData(1, true, payloadSmall); err != wantErr {
		t.Errorf("sticky error = %v, want %v", err, wantErr)
	}
}

func TestDataBufferInlineAndRelease(t *testing.T) {
	var b dataBuffer
	if _, err := b.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if len(b.chunks) != 1 || &b.chunks[0] != &b.inlineChunks[0] {
		t.Fatalf("expected inlineChunks backing for first chunk")
	}

	buf := make([]byte, 5)
	n, err := b.Read(buf)
	if err != nil || n != 5 || string(buf) != "hello" {
		t.Fatalf("Read = (%d, %v, %q)", n, err, buf)
	}
	if b.size != 0 || b.r != 0 || b.w != 0 {
		t.Fatalf("after draining to 0: size=%d r=%d w=%d, want all 0", b.size, b.r, b.w)
	}

	// Write again into the same chunk from offset 0 without growing chunks.
	if _, err := b.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if len(b.chunks) != 1 {
		t.Fatalf("expected 1 chunk after rewrite, got %d", len(b.chunks))
	}

	// Verify pipe releases dataBuffer chunks on EOF.
	var p1 pipe
	db1 := &dataBuffer{}
	p1.setBuffer(db1)
	if _, err := p1.Write([]byte("eof-test")); err != nil {
		t.Fatal(err)
	}
	p1.CloseWithError(io.EOF)
	got, err := io.ReadAll(&p1)
	if err != nil || string(got) != "eof-test" {
		t.Fatalf("pipe ReadAll = (%q, %v)", got, err)
	}
	if db1.chunks != nil || db1.size != 0 {
		t.Errorf("db1 not released on pipe EOF: chunks=%v size=%d", db1.chunks, db1.size)
	}

	// Verify pipe releases dataBuffer chunks on BreakWithError.
	var p2 pipe
	db2 := &dataBuffer{}
	p2.setBuffer(db2)
	if _, err := p2.Write([]byte("broken-test")); err != nil {
		t.Fatal(err)
	}
	p2.BreakWithError(errors.New("reset"))
	if db2.chunks != nil || db2.size != 0 {
		t.Errorf("db2 not released on BreakWithError: chunks=%v size=%d", db2.chunks, db2.size)
	}
}

func TestFrameWriteRequestConsumeReuse(t *testing.T) {
	done := make(chan error, 1)
	wd := &writeData{
		streamID:  7,
		p:         []byte("0123456789"),
		endStream: true,
	}
	st := &stream{
		id: 7,
		sc: &serverConn{maxFrameSize: 16384},
	}
	st.flow.add(100)
	wr := FrameWriteRequest{
		stream: st,
		write:  wd,
		done:   done,
	}

	consumed, rest, n := wr.Consume(4)
	if n != 2 {
		t.Fatalf("Consume returned %d frames, want 2", n)
	}
	cwd := consumed.write.(*writeData)
	rwd := rest.write.(*writeData)
	if rwd != wd {
		t.Errorf("expected rest to reuse writeData pointer")
	}
	if cwd.streamID != 7 || cwd.endStream || string(cwd.p) != "0123" || consumed.done != nil {
		t.Errorf("consumed = %+v (done=%v)", cwd, consumed.done)
	}
	if rwd.streamID != 7 || !rwd.endStream || string(rwd.p) != "456789" || rest.done != done {
		t.Errorf("rest = %+v (done=%v)", rwd, rest.done)
	}
}

func TestCloneHeaderSliceIsolation(t *testing.T) {
	orig := Header{
		"X-A":   {"a1"},
		"X-B":   {"b1", "b2"},
		"X-Nil": nil,
		"X-Emp": {},
	}
	cloned := cloneHeader(orig)
	if !reflect.DeepEqual(cloned, orig) {
		t.Fatalf("cloneHeader = %#v, want %#v", cloned, orig)
	}
	if cloned["X-Nil"] != nil {
		t.Errorf("X-Nil = %#v, want nil", cloned["X-Nil"])
	}
	if cloned["X-Emp"] == nil || len(cloned["X-Emp"]) != 0 {
		t.Errorf("X-Emp = %#v, want non-nil empty slice", cloned["X-Emp"])
	}
	// Mutating via append must not overwrite adjacent slices in the shared slab.
	cloned["X-A"] = append(cloned["X-A"], "a2-mutated")
	if !reflect.DeepEqual(cloned["X-B"], []string{"b1", "b2"}) {
		t.Fatalf("appending to X-A corrupted X-B: %#v", cloned["X-B"])
	}
	if !reflect.DeepEqual(orig["X-A"], []string{"a1"}) {
		t.Fatalf("appending to clone corrupted orig: %#v", orig["X-A"])
	}
}
