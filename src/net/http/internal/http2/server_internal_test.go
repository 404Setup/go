// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"errors"
	"fmt"
	"golang.org/x/net/http2/hpack"
	"net/http/internal"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestCheckValidHTTP2Request(t *testing.T) {
	tests := []struct {
		h    Header
		want error
	}{
		{
			h:    Header{"Te": {"trailers"}},
			want: nil,
		},
		{
			h:    Header{"Te": {"trailers", "bogus"}},
			want: errors.New(`request header "TE" may only be "trailers" in HTTP/2`),
		},
		{
			h:    Header{"Foo": {""}},
			want: nil,
		},
		{
			h:    Header{"Connection": {""}},
			want: errors.New(`request header "Connection" is not valid in HTTP/2`),
		},
		{
			h:    Header{"Proxy-Connection": {""}},
			want: errors.New(`request header "Proxy-Connection" is not valid in HTTP/2`),
		},
		{
			h:    Header{"Keep-Alive": {""}},
			want: errors.New(`request header "Keep-Alive" is not valid in HTTP/2`),
		},
		{
			h:    Header{"Upgrade": {""}},
			want: errors.New(`request header "Upgrade" is not valid in HTTP/2`),
		},
	}
	for i, tt := range tests {
		got := checkValidHTTP2RequestHeaders(tt.h)
		if !equalError(got, tt.want) {
			t.Errorf("%d. checkValidHTTP2Request = %v; want %v", i, got, tt.want)
		}
	}
}

// TestCanonicalHeaderCacheGrowth verifies that the canonical header cache
// size is capped to a reasonable level.
func TestCanonicalHeaderCacheGrowth(t *testing.T) {
	for _, size := range []int{1, (1 << 20) - 10} {
		base := strings.Repeat("X", size)
		sc := &serverConn{}
		sc.serveG.setOwner()
		count := 0
		added := 0
		for added < 10*maxCachedCanonicalHeadersKeysSize {
			h := fmt.Sprintf("%v-%v", base, count)
			c := sc.canonicalHeader(h)
			if len(h) != len(c) {
				t.Errorf("sc.canonicalHeader(%q) = %q, want same length", h, c)
			}
			count++
			added += len(h)
		}
		total := 0
		for k, v := range sc.canonHeader {
			total += len(k) + len(v) + 100
		}
		if total > maxCachedCanonicalHeadersKeysSize {
			t.Errorf("after adding %v ~%v-byte headers, canonHeader cache is ~%v bytes, want <%v", count, size, total, maxCachedCanonicalHeadersKeysSize)
		}
	}
}

func TestHTTPCodeString(t *testing.T) {
	for _, code := range []int{0, 99, 100, 101, 200, 204, 299, 304, 404, 500, 999, 1000} {
		want := strconv.Itoa(code)
		if code >= 100 && code <= 999 {
			if s := internal.StatusCodeString(code); s != "" && s != want {
				t.Fatalf("internal.StatusCodeString(%d) = %q, want %q", code, s, want)
			}
		}
		if got := httpCodeString(code); got != want {
			t.Errorf("httpCodeString(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestEncodeHeadersEdgeCases(t *testing.T) {
	decode := func(t *testing.T, buf *bytes.Buffer) [][2]string {
		t.Helper()
		var got [][2]string
		dec := hpack.NewDecoder(4096, func(f hpack.HeaderField) {
			got = append(got, [2]string{f.Name, f.Value})
		})
		if _, err := dec.Write(buf.Bytes()); err != nil {
			t.Fatalf("hpack decode error: %v", err)
		}
		if err := dec.Close(); err != nil {
			t.Fatalf("hpack close error: %v", err)
		}
		return got
	}

	t.Run("empty_map", func(t *testing.T) {
		var buf bytes.Buffer
		enc := hpack.NewEncoder(&buf)
		encodeHeaders(enc, nil, nil)
		encodeHeaders(enc, Header{}, nil)
		encodeHeaders(enc, Header{}, []string{"X-Test"})
		if buf.Len() != 0 {
			t.Fatalf("encoded %d bytes for empty header map, want 0", buf.Len())
		}
	})

	t.Run("single_key_fast_path", func(t *testing.T) {
		var buf bytes.Buffer
		enc := hpack.NewEncoder(&buf)
		h := Header{"Content-Type": {"application/json"}}
		encodeHeaders(enc, h, nil)
		got := decode(t, &buf)
		want := [][2]string{{"content-type", "application/json"}}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}

		// With explicit keys omitting the single entry, nothing should be encoded.
		buf.Reset()
		encodeHeaders(enc, h, []string{"Other-Key"})
		if buf.Len() != 0 {
			t.Fatalf("encoded %d bytes when keys filter excludes entry, want 0", buf.Len())
		}
	})

	t.Run("validation_and_te_filtering", func(t *testing.T) {
		var buf bytes.Buffer
		enc := hpack.NewEncoder(&buf)
		h := Header{
			"Transfer-Encoding": {"gzip", "trailers", "chunked"},
			"X-Valid":           {"ok", "bad\nval", "also-ok"},
			"Bad Key":           {"v"},
			"Non\x80Ascii":      {"v"},
			"Cache-Control":     {"max-age=60"},
		}
		encodeHeaders(enc, h, nil)
		got := decode(t, &buf)
		want := [][2]string{
			{"cache-control", "max-age=60"},
			{"transfer-encoding", "trailers"},
			{"x-valid", "ok"},
			{"x-valid", "also-ok"},
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("explicit_keys_order", func(t *testing.T) {
		var buf bytes.Buffer
		enc := hpack.NewEncoder(&buf)
		h := Header{
			"X-B": {"2"},
			"X-A": {"1"},
			"X-C": {"3"},
		}
		encodeHeaders(enc, h, []string{"X-C", "X-A"})
		got := decode(t, &buf)
		want := [][2]string{
			{"x-c", "3"},
			{"x-a", "1"},
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}

func BenchmarkEncodeHeaders(b *testing.B) {
	b.Run("Empty", func(b *testing.B) {
		var buf bytes.Buffer
		enc := hpack.NewEncoder(&buf)
		var h Header
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf.Reset()
			encodeHeaders(enc, h, nil)
		}
	})
	b.Run("SingleHeader", func(b *testing.B) {
		var buf bytes.Buffer
		enc := hpack.NewEncoder(&buf)
		h := Header{"Content-Type": {"application/json"}}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf.Reset()
			encodeHeaders(enc, h, nil)
		}
	})
	b.Run("MultipleHeaders", func(b *testing.B) {
		var buf bytes.Buffer
		enc := hpack.NewEncoder(&buf)
		h := Header{
			"Content-Type":   {"application/json"},
			"Cache-Control":  {"public, max-age=3600"},
			"X-Request-Id":   {"req-123456"},
			"Content-Length": {"42"},
			"Server":         {"go-http2"},
		}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf.Reset()
			encodeHeaders(enc, h, nil)
		}
	})
}

