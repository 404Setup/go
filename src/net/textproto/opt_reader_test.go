// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package textproto

import (
	"bufio"
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestCanonicalMIMEHeaderKeyExtended(t *testing.T) {
	common := []string{
		"accept", "accept-charset", "accept-encoding", "accept-language",
		"accept-ranges", "authorization", "cache-control", "connection",
		"content-disposition", "content-encoding", "content-language",
		"content-length", "content-location", "content-range",
		"content-security-policy", "content-type", "cookie", "date",
		"etag", "expect", "expires", "forwarded", "from", "host",
		"if-match", "if-modified-since", "if-none-match", "if-range",
		"if-unmodified-since", "last-modified", "link", "location",
		"max-forwards", "origin", "pragma", "proxy-authenticate",
		"proxy-authorization", "range", "referer", "retry-after",
		"server", "set-cookie", "strict-transport-security", "te",
		"trailer", "transfer-encoding", "upgrade", "user-agent",
		"vary", "via", "warning", "www-authenticate",
		"x-content-type-options", "x-forwarded-for", "x-forwarded-host",
		"x-forwarded-proto", "x-frame-options", "x-powered-by",
		"x-real-ip", "x-request-id", "x-requested-with",
		"cc", "to", "subject", "received", "content-id", "message-id",
		"in-reply-to", "return-path", "mime-version", "x-imforwards",
		"dkim-signature", "content-transfer-encoding",
	}
	for _, k := range common {
		want, ok := canonicalMIMEHeaderKey([]byte(k))
		if !ok {
			t.Fatalf("canonicalMIMEHeaderKey(%q) ok=false", k)
		}
		if got := CanonicalMIMEHeaderKey(k); got != want {
			t.Fatalf("CanonicalMIMEHeaderKey(%q) = %q, want %q", k, got, want)
		}
		if got := CanonicalMIMEHeaderKey(want); got != want {
			t.Fatalf("CanonicalMIMEHeaderKey(%q) = %q, want %q", want, got, want)
		}
		if allocs := testing.AllocsPerRun(50, func() {
			if got := CanonicalMIMEHeaderKey(k); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
			if got := CanonicalMIMEHeaderKey(want); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		}); allocs != 0 {
			t.Fatalf("CanonicalMIMEHeaderKey(%q/%q) allocs = %.0f, want 0", k, want, allocs)
		}
	}

	edgeCases := []string{
		"",
		"a",
		"A",
		"x-custom-thing-id",
		"X-CUSTOM-THING-ID",
		"x-very-long-custom-header-name-exceeding-sixty-four-bytes-buffer-limit-12345",
		"X-Very-Long-Custom-Header-Name-Exceeding-Sixty-Four-Bytes-Buffer-Limit-12345",
		"foo bar",
		"Foo Bar",
		"x-foo bar",
		"foo\x00bar",
		"x-foo\x00bar",
		"x-foo\x7fbar",
		"x-foo\x80bar",
		"123-abc-DEF",
		"dkim-signature",
		"content-transfer-encoding",
	}
	for _, k := range edgeCases {
		want, _ := canonicalMIMEHeaderKey([]byte(k))
		if got := CanonicalMIMEHeaderKey(k); got != want {
			t.Fatalf("CanonicalMIMEHeaderKey(%q) = %q, want %q", k, got, want)
		}
	}
}

func TestValidHeaderValueBytesParity(t *testing.T) {
	for b := 0; b < 256; b++ {
		want := validHeaderValueByte(byte(b))
		for length := 1; length <= 25; length++ {
			for pos := 0; pos < length; pos++ {
				buf := bytes.Repeat([]byte{'a'}, length)
				buf[pos] = byte(b)
				if got := validHeaderValueBytes(buf); got != want {
					t.Fatalf("byte 0x%02x at pos %d (len %d): got %v, want %v", b, pos, length, got, want)
				}
			}
		}
	}
	if !validHeaderValueBytes(nil) {
		t.Fatal("validHeaderValueBytes(nil) = false, want true")
	}
}

func TestReadMIMEHeaderBufferedMalformedError(t *testing.T) {
	cases := []string{
		"Host: example.com\r\nBad\x01Key: val\r\nNext: ok\r\n\r\n",
		"Host: example.com\r\nX-Test: bad\x7fval\r\nNext: ok\r\n\r\n",
		"Host: example.com\r\nX-Test: bad\x00val\r\nNext: ok\r\n\r\n",
	}
	for _, raw := range cases {
		br := bufio.NewReader(strings.NewReader(raw))
		_, _, _, _, handled, err := readMIMEHeaderFast(br, 1<<20, 100, false)
		if !handled {
			t.Fatalf("expected buffered fast path for %q", raw)
		}
		if err == nil {
			t.Fatalf("expected error for %q", raw)
		}
		if _, ok := err.(ProtocolError); !ok {
			t.Fatalf("expected ProtocolError for %q, got %T (%v)", raw, err, err)
		}
	}
}

func TestInternHeaderValueExtended(t *testing.T) {
	vals := []string{
		"gzip", "close", "Close", "bytes", "chunked", "private", "nosniff",
		"no-cache", "no-store", "max-age=0", "localhost", "text/html",
		"127.0.0.1", "keep-alive", "Keep-Alive", "text/plain", "golang.org",
		"gzip,deflate", "gzip, deflate", "localhost:8080", "en-US,en;q=0.8",
		"application/json", "gzip,deflate,sdch", "gzip, deflate, br",
		"Go-http-client/1.1", "Go-http-client/2.0",
		"text/html; charset=utf-8", "text/plain; charset=utf-8",
		"application/json; charset=utf-8",
	}
	for _, want := range vals {
		b := []byte(want)
		got, ok := internHeaderValue(b)
		if !ok || got != want {
			t.Fatalf("internHeaderValue(%q) = (%q, %v), want (%q, true)", want, got, ok, want)
		}
	}
	if _, ok := internHeaderValue([]byte("non-interned-custom-value")); ok {
		t.Fatal("unexpected intern hit for custom value")
	}
}

func BenchmarkValidHeaderValueBytes(b *testing.B) {
	vals := [][]byte{
		[]byte("Thu, 26 Nov 2026 12:00:00 GMT"),
		[]byte("text/plain; charset=utf-8"),
		[]byte("max-age=31536000; includeSubDomains; preload"),
	}
	b.ReportAllocs()
	for b.Loop() {
		for _, v := range vals {
			if !validHeaderValueBytes(v) {
				b.Fatal("unexpected invalid header value")
			}
		}
	}
}

func TestInternHeaderValueNumericAndDateCache(t *testing.T) {
	// 1. Single and two-digit decimal values without leading zero.
	for i := range 100 {
		s := string(rune('0'+i/10)) + string(rune('0'+i%10))
		if i < 10 {
			s = string(rune('0' + i))
		}
		b := []byte(s)
		got, ok := internHeaderValue(b)
		if !ok || got != s {
			t.Fatalf("internHeaderValue(%q) = (%q, %v), want (%q, true)", s, got, ok, s)
		}
		b[0] = 'X'
		if got != s {
			t.Fatalf("interned numeric string aliased caller slice: got %q, want %q", got, s)
		}
	}
	for _, leadingZero := range []string{"00", "01", "09"} {
		if _, ok := internHeaderValue([]byte(leadingZero)); ok {
			t.Fatalf("leading zero %q should not hit two-digit intern table", leadingZero)
		}
	}

	// 2. Date header caching must copy bytes (no aliasing) and reject non-GMT.
	dateBuf := []byte("Thu, 26 Nov 2026 15:00:01 GMT")
	got1, ok := internHeaderValue(dateBuf)
	if !ok || got1 != "Thu, 26 Nov 2026 15:00:01 GMT" {
		t.Fatalf("first date intern = (%q, %v)", got1, ok)
	}
	dateBuf[24] = '9' // mutate caller slice
	if got1 != "Thu, 26 Nov 2026 15:00:01 GMT" {
		t.Fatalf("date cache aliased caller buffer: %q", got1)
	}
	dateBuf[24] = '1'
	if allocs := testing.AllocsPerRun(100, func() {
		v, ok := internHeaderValue(dateBuf)
		if !ok || v != "Thu, 26 Nov 2026 15:00:01 GMT" {
			t.Fatalf("cached date mismatch: %q %v", v, ok)
		}
	}); allocs != 0 {
		t.Fatalf("cached Date intern allocs = %v, want 0", allocs)
	}
	if _, ok := internHeaderValue([]byte("Thu, 26 Nov 2026 15:00:01 UTC")); ok {
		t.Fatal("non-GMT 29-byte header unexpectedly cached")
	}

	// 3. CRLF/NUL injection inside Date-shaped header must still be rejected by readMIMEHeaderBuffered.
	badDate := "Date: Thu, 26 Nov 2026 15:00:\x001 GMT\r\n\r\n"
	br := bufio.NewReader(strings.NewReader(badDate))
	if _, _, _, _, handled, err := readMIMEHeaderFast(br, 1<<20, 100, false); !handled || err == nil {
		t.Fatalf("invalid Date header with NUL not rejected: handled=%v err=%v", handled, err)
	}

	// 4. Concurrent cache updates and reads under -race.
	d1 := "Thu, 26 Nov 2026 15:00:02 GMT"
	d2 := "Thu, 26 Nov 2026 15:00:03 GMT"
	var wg sync.WaitGroup
	for g := range 6 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			want := d1
			if id%2 == 1 {
				want = d2
			}
			buf := []byte(want)
			for range 200 {
				got, ok := internHeaderValue(buf)
				if !ok || got != want {
					t.Errorf("concurrent internHeaderValue got (%q, %v), want (%q, true)", got, ok, want)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

func BenchmarkReadMIMEHeaderResponse(b *testing.B) {
	raw := []byte("Date: Thu, 26 Nov 2026 15:00:01 GMT\r\nContent-Length: 42\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	// Warm Date cache.
	brWarm := bufio.NewReader(bytes.NewReader(raw))
	_, _, _, _, _, _ = readMIMEHeaderFast(brWarm, 1<<20, 100, false)

	rd := bytes.NewReader(raw)
	br := bufio.NewReaderSize(rd, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rd.Reset(raw)
		br.Reset(rd)
		m, _, _, _, handled, err := readMIMEHeaderFast(br, 1<<20, 100, false)
		if !handled || err != nil || len(m) != 3 {
			b.Fatalf("readMIMEHeaderFast failed: handled=%v err=%v len=%d", handled, err, len(m))
		}
	}
}
