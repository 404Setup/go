// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package textproto

import (
	"bufio"
	"bytes"
	"strings"
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
