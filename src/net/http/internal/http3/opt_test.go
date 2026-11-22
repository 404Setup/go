// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http3

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/net/quic"
)

func TestValidWireHeaderFieldName(t *testing.T) {
	valid := []string{"a", "content-type", "x-custom-123", "!#$%&'*+-.^_`|~"}
	for _, v := range valid {
		if !validWireHeaderFieldName(v) {
			t.Errorf("validWireHeaderFieldName(%q) = false, want true", v)
		}
	}
	invalid := []string{"", "Content-Type", "X", "bad name", "bad:name", "bad\x7f", "café", ":method"}
	for _, v := range invalid {
		if validWireHeaderFieldName(v) {
			t.Errorf("validWireHeaderFieldName(%q) = true, want false", v)
		}
	}
}

func TestResponseWriterLazyTrailerAndEmbeddedBodyWriter(t *testing.T) {
	sc := &serverConn{}
	sc.enc.init()

	// Without trailers, rw.trailer remains nil and rw.bw points to rw.bwStorage.
	rw := sc.newResponseWriter(nil, true)
	if rw.trailer != nil {
		t.Fatalf("newResponseWriter allocated trailer map eagerly")
	}
	if rw.bw != &rw.bwStorage {
		t.Fatalf("newResponseWriter did not point bw to embedded bwStorage")
	}

	// Undeclared Trailer: prefix lazily initializes rw.trailer in prepareTrailerForWriteLocked.
	rw.headers.Set(http.TrailerPrefix+"X-Late", "val")
	rw.prepareTrailerForWriteLocked()
	if rw.trailer == nil || rw.trailer.Get("X-Late") != "val" {
		t.Fatalf("undeclared trailer = %v, want X-Late=val", rw.trailer)
	}
	if rw.bw.trailer == nil || rw.bw.trailer.Get("X-Late") != "val" {
		t.Fatalf("bw.trailer = %v, want X-Late=val", rw.bw.trailer)
	}
}

func TestLookupStaticFastMatchesTable(t *testing.T) {
	staticTableOnce.Do(initStaticTableMaps)

	check := func(itype indexType, name, val string) {
		t.Helper()
		idx, matchVal, matchName := lookupStaticFast(itype, name, val)
		if matchVal {
			if itype != mayIndex {
				t.Errorf("lookupStaticFast(%v, %q, %q) matched value with itype != mayIndex", itype, name, val)
			}
			wantIdx, ok := staticTableByNameValue[tableEntry{name, val}]
			if !ok || idx != wantIdx {
				t.Errorf("lookupStaticFast(%q, %q) = idx %d, want table idx %d (ok=%v)", name, val, idx, wantIdx, ok)
			}
			ent := staticTableEntries[idx]
			if ent.name != name || ent.value != val {
				t.Errorf("staticTableEntries[%d] = %+v, want {%q, %q}", idx, ent, name, val)
			}
		} else if matchName {
			wantIdx, ok := staticTableByName[name]
			if !ok || idx != wantIdx {
				t.Errorf("lookupStaticFast(%q, %q) name-only idx = %d, want %d (ok=%v)", name, val, idx, wantIdx, ok)
			}
			if itype == mayIndex {
				if _, exact := staticTableByNameValue[tableEntry{name, val}]; exact {
					// Confirm fast path did not miss a value match for handled names.
					t.Errorf("lookupStaticFast(%q, %q) returned name-only, missed exact table match", name, val)
				}
			}
		}
	}

	for _, ent := range staticTableEntries {
		check(mayIndex, ent.name, ent.value)
		check(neverIndex, ent.name, ent.value)
		check(mayIndex, ent.name, "non-static-value-xyz")
	}
	check(mayIndex, "x-custom-non-static", "val")
}

func TestQPACKInvalidPrefixBytesAndHugeString(t *testing.T) {
	// Invalid field line prefix bytes 0x00..0x07 must return errQPACKDecompressionFailed.
	for b := byte(0x00); b <= 0x07; b++ {
		st1, st2 := newStreamPair(t)
		wire := []byte{0x00, 0x00, b}
		_, _ = st1.Write(wire)
		_ = st1.Flush()
		st2.lim = int64(len(wire))
		var dec qpackDecoder
		err := dec.decode(st2, func(indexType, string, string) error { return nil })
		if !errors.Is(err, errQPACKDecompressionFailed) {
			t.Errorf("prefix byte %#02x: err = %v, want errQPACKDecompressionFailed", b, err)
		}
	}

	// Huge declared string length on a short stream must fail without allocating huge buffer.
	// Literal field line with literal name (0x20), name length encoded as 3-bit prefix varint = 256MB.
	st1, st2 := newStreamPair(t)
	var buf []byte
	buf = append(buf, 0x00, 0x00) // RIC=0, DeltaBase=0
	buf = appendPrefixedInt(buf, 0x20, 3, 1<<28)
	buf = append(buf, "short"...)
	_, _ = st1.Write(buf)
	st1.CloseWrite()
	st2.lim = int64(len(buf))
	var dec qpackDecoder
	err := dec.decode(st2, func(indexType, string, string) error { return nil })
	if err == nil {
		t.Fatalf("expected error for truncated huge QPACK string, got nil")
	}
}

func TestDeadlineContextSemanticsAndRace(t *testing.T) {
	t.Run("StickyDeadlineExceeded", func(t *testing.T) {
		var d deadline
		d.set(time.Now().Add(-time.Second))
		select {
		case <-d.Done():
		default:
			t.Fatal("Done() not closed for past deadline")
		}
		if err := d.Err(); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Err() = %v, want os.ErrDeadlineExceeded", err)
		}
		// Setting a future deadline must NOT clear an already expired deadline.
		d.set(time.Now().Add(time.Hour))
		if err := d.Err(); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Err() after future set = %v, want sticky os.ErrDeadlineExceeded", err)
		}
		d.stop()
	})

	t.Run("StopCancels", func(t *testing.T) {
		var d deadline
		d.set(time.Now().Add(time.Hour))
		if d.Err() != nil {
			t.Fatalf("Err() before stop = %v, want nil", d.Err())
		}
		d.stop()
		select {
		case <-d.Done():
		default:
			t.Fatal("Done() not closed after stop()")
		}
		if err := d.Err(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Err() after stop = %v, want context.Canceled", err)
		}
	})

	t.Run("ConcurrentRace", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			var d deadline
			var wg sync.WaitGroup
			wg.Add(3)
			go func() {
				defer wg.Done()
				_ = d.Done()
				_ = d.Err()
			}()
			go func() {
				defer wg.Done()
				d.set(time.Now().Add(time.Millisecond))
			}()
			go func() {
				defer wg.Done()
				d.stop()
			}()
			wg.Wait()
		}
	})
}

func TestDiscardFrameMultiKB(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tc := newTestClientConn(t)
		tc.greet()

		req, _ := http.NewRequest("GET", "https://example.tld/", nil)
		rt := tc.roundTrip(req)
		st := tc.wantStream(streamTypeRequest)
		st.wantHeaders(nil)

		// Write a 10 KiB unknown frame (larger than 4096-byte pooled discard buffer).
		big := bytes.Repeat([]byte("u"), 10*1024)
		st.writeVarint(0x1f + 0x21)
		st.writeVarint(int64(len(big)))
		st.Write(big)
		st.writeHeaders(http.Header{":status": {"200"}})
		st.writeVarint(int64(frameTypeData))
		st.writeVarint(2)
		st.Write([]byte("ok"))
		st.CloseWrite()

		rt.wantStatus(200)
		rt.wantBody([]byte("ok"))
	})
}

func TestBodyReaderZeroLengthDataFrame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tc := newTestClientConn(t)
		tc.greet()

		req, _ := http.NewRequest("GET", "https://example.tld/", nil)
		rt := tc.roundTrip(req)
		st := tc.wantStream(streamTypeRequest)
		st.wantHeaders(nil)

		st.writeHeaders(http.Header{":status": {"200"}})
		// Send multiple consecutive zero-length DATA frames followed by real data.
		for i := 0; i < 5; i++ {
			st.writeVarint(int64(frameTypeData))
			st.writeVarint(0)
		}
		st.writeVarint(int64(frameTypeData))
		st.writeVarint(5)
		st.Write([]byte("hello"))
		st.CloseWrite()

		rt.wantStatus(200)
		rt.wantBody([]byte("hello"))
	})
}

func TestServerContentLengthMismatchAndForbiddenTrailers(t *testing.T) {
	t.Run("MismatchingContentLengthRejected", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			called := false
			ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
			}))
			tc := ts.connect()
			tc.greet()

			reqStream := tc.newStream(streamTypeRequest)
			reqStream.writeHeaders(requestHeader(http.Header{
				":method":        {"POST"},
				"content-length": {"5", "10"},
			}))

			reqStream.wantError(quic.StreamError(errH3MessageError))
			if called {
				t.Fatal("handler was called for mismatching Content-Length headers")
			}
		})
	})

	t.Run("InvalidContentLengthRejected", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			called := false
			ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
			}))
			tc := ts.connect()
			tc.greet()

			reqStream := tc.newStream(streamTypeRequest)
			reqStream.writeHeaders(requestHeader(http.Header{
				":method":        {"POST"},
				"content-length": {"not-a-number"},
			}))

			reqStream.wantError(quic.StreamError(errH3MessageError))
			if called {
				t.Fatal("handler was called for invalid Content-Length header")
			}
		})
	})

	t.Run("ForbiddenUndeclaredTrailersStripped", func(t *testing.T) {
		sc := &serverConn{}
		sc.enc.init()
		rw := sc.newResponseWriter(nil, true)
		rw.headers.Set(http.TrailerPrefix+"Content-Length", "999")
		rw.headers.Set(http.TrailerPrefix+"Host", "evil.example")
		rw.headers.Set(http.TrailerPrefix+"X-Valid", "ok")
		rw.prepareTrailerForWriteLocked()
		if rw.trailer == nil {
			t.Fatal("expected rw.trailer non-nil for X-Valid")
		}
		if got := rw.trailer.Get("Content-Length"); got != "" {
			t.Errorf("forbidden trailer Content-Length was not stripped: %q", got)
		}
		if got := rw.trailer.Get("Host"); got != "" {
			t.Errorf("forbidden trailer Host was not stripped: %q", got)
		}
		if got := rw.trailer.Get("X-Valid"); got != "ok" {
			t.Errorf("valid trailer X-Valid = %q, want ok", got)
		}
	})
}

func TestServerConnShutdownStartupRace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		}))
		tc := ts.connect()
		// Concurrently trigger graceful shutdown while connection initializes.
		done := make(chan struct{})
		go func() {
			ts.s.shutdown(t.Context())
			close(done)
		}()
		// Client must receive server control stream starting with SETTINGS before any GOAWAY.
		ctrl := tc.wantStream(streamTypeControl)
		ctrl.wantSettings(nil)
		<-done
	})
}

func TestTransportLazyResponseTrailer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tc := newTestClientConn(t)
		tc.greet()

		// Case 1: response without trailers leaves resp.Trailer == nil.
		req1, _ := http.NewRequest("GET", "https://example.tld/no-trailer", nil)
		rt1 := tc.roundTrip(req1)
		st1 := tc.wantStream(streamTypeRequest)
		st1.wantHeaders(nil)
		st1.writeHeaders(http.Header{":status": {"200"}})
		st1.writeVarint(int64(frameTypeData))
		st1.writeVarint(2)
		st1.Write([]byte("hi"))
		st1.CloseWrite()
		rt1.wantStatus(200)
		rt1.wantBody([]byte("hi"))
		if rt1.resp.Trailer != nil {
			t.Errorf("resp.Trailer = %v, want nil when no trailers sent", rt1.resp.Trailer)
		}

		// Case 2: response with undeclared trailers lazily populates resp.Trailer.
		req2, _ := http.NewRequest("GET", "https://example.tld/with-trailer", nil)
		rt2 := tc.roundTrip(req2)
		st2 := tc.wantStream(streamTypeRequest)
		st2.wantHeaders(nil)
		st2.writeHeaders(http.Header{":status": {"200"}})
		st2.writeVarint(int64(frameTypeData))
		st2.writeVarint(2)
		st2.Write([]byte("ok"))
		st2.writeHeaders(http.Header{"x-late": {"yes"}})
		st2.CloseWrite()
		rt2.wantStatus(200)
		rt2.wantBody([]byte("ok"))
		if got := rt2.resp.Trailer.Get("X-Late"); got != "yes" {
			t.Errorf("undeclared trailer X-Late = %q, want yes", got)
		}
	})
}

func BenchmarkQPACKWriteHeadersFrame(b *testing.B) {
	var enc qpackEncoder
	enc.init()
	st1, st2 := newStreamPair(b)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			if _, err := st2.Read(buf); err != nil {
				return
			}
		}
	}()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc.writeHeadersFrame(st1, func(yield func(itype indexType, name, value string)) {
			yield(mayIndex, ":status", "200")
			yield(mayIndex, "content-type", "application/json")
			yield(mayIndex, "content-length", "42")
			yield(mayIndex, "date", "Thu, 19 Nov 2026 12:00:00 GMT")
		})
		if err := st1.Flush(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	st1.CloseWrite()
	<-done
}
