// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httpcommon

import (
	"fmt"
	"net/textproto"
	"strings"
	"testing"
)

var knownCanonicalHeaders = []string{
	"Accept",
	"Accept-Charset",
	"Accept-Encoding",
	"Accept-Language",
	"Accept-Ranges",
	"Age",
	"Access-Control-Allow-Credentials",
	"Access-Control-Allow-Headers",
	"Access-Control-Allow-Methods",
	"Access-Control-Allow-Origin",
	"Access-Control-Expose-Headers",
	"Access-Control-Max-Age",
	"Access-Control-Request-Headers",
	"Access-Control-Request-Method",
	"Allow",
	"Authorization",
	"Cache-Control",
	"Content-Disposition",
	"Content-Encoding",
	"Content-Language",
	"Content-Length",
	"Content-Location",
	"Content-Range",
	"Content-Type",
	"Cookie",
	"Date",
	"Etag",
	"Expect",
	"Expires",
	"From",
	"Host",
	"If-Match",
	"If-Modified-Since",
	"If-None-Match",
	"If-Unmodified-Since",
	"Last-Modified",
	"Link",
	"Location",
	"Max-Forwards",
	"Origin",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Range",
	"Referer",
	"Refresh",
	"Retry-After",
	"Server",
	"Set-Cookie",
	"Strict-Transport-Security",
	"Trailer",
	"Transfer-Encoding",
	"User-Agent",
	"Vary",
	"Via",
	"Www-Authenticate",
	"X-Forwarded-For",
	"X-Forwarded-Proto",
}

func TestHeaderMapTables(t *testing.T) {
	for _, canon := range knownCanonicalHeaders {
		lower := strings.ToLower(canon)
		gotLower, okASCII := LowerHeader(canon)
		if gotLower != lower || !okASCII {
			t.Errorf("LowerHeader(%q) = (%q, %v), want (%q, true)", canon, gotLower, okASCII, lower)
		}
		gotCanon, okCached := CachedCanonicalHeader(lower)
		if gotCanon != canon || !okCached {
			t.Errorf("CachedCanonicalHeader(%q) = (%q, %v), want (%q, true)", lower, gotCanon, okCached, canon)
		}
		if got := CanonicalHeader(lower); got != canon {
			t.Errorf("CanonicalHeader(%q) = %q, want %q", lower, got, canon)
		}
	}

	// Non-table, mixed-case, and non-ASCII fallback inputs.
	for _, in := range []string{"X-Custom-Header", "x-custom-header", "ACCEPT-ENCODING", "bad\xffkey", ""} {
		if got := CanonicalHeader(in); got != textproto.CanonicalMIMEHeaderKey(in) {
			t.Errorf("CanonicalHeader(%q) = %q, want %q", in, got, textproto.CanonicalMIMEHeaderKey(in))
		}
	}
	if got, ok := LowerHeader("X-Custom-Header"); got != "x-custom-header" || !ok {
		t.Errorf("LowerHeader custom = (%q, %v), want (x-custom-header, true)", got, ok)
	}
	if _, ok := LowerHeader("Non\xffAscii"); ok {
		t.Error("LowerHeader accepted non-ASCII")
	}
}

func TestCommaSeparatedTrailers(t *testing.T) {
	t.Run("sizes", func(t *testing.T) {
		for _, count := range []int{0, 1, 2, 8, 9, 16, 17, 24} {
			tr := make(map[string][]string, count)
			for i := count - 1; i >= 0; i-- {
				tr[fmt.Sprintf("x-trailer-%02d", i)] = nil
			}
			got, err := commaSeparatedTrailers(tr)
			if err != nil {
				t.Fatalf("count=%d err = %v", count, err)
			}
			if count == 0 && got != "" {
				t.Fatalf("count=0 got %q", got)
			}
			if count > 0 {
				parts := strings.Split(got, ",")
				if len(parts) != count {
					t.Fatalf("count=%d len=%d (%q)", count, len(parts), got)
				}
				for i := range count {
					want := fmt.Sprintf("X-Trailer-%02d", i)
					if parts[i] != want {
						t.Fatalf("count=%d part[%d]=%q, want %q", count, i, parts[i], want)
					}
				}
			}
		}
	})

	t.Run("forbidden", func(t *testing.T) {
		for _, bad := range []string{"Transfer-Encoding", "trailer", "content-length"} {
			if _, err := commaSeparatedTrailers(map[string][]string{bad: nil}); err == nil {
				t.Errorf("expected error for %q", bad)
			}
			if _, err := commaSeparatedTrailers(map[string][]string{"X-Ok": nil, bad: nil}); err == nil {
				t.Errorf("expected error for multi-trailer containing %q", bad)
			}
		}
	})
}

func BenchmarkLowerHeader(b *testing.B) {
	b.Run("Known", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = LowerHeader(knownCanonicalHeaders[i%len(knownCanonicalHeaders)])
		}
	})
	b.Run("Custom", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = LowerHeader("X-Request-Id")
		}
	})
}

func BenchmarkCanonicalHeader(b *testing.B) {
	b.Run("Known", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = CanonicalHeader("content-type")
		}
	})
	b.Run("Custom", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = CanonicalHeader("x-request-id")
		}
	})
}
