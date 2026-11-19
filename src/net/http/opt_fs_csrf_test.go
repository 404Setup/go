// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"fmt"
	"math"
	"mime/multipart"
	"net/url"
	"sync"
	"testing"
)

type refCountingWriter int64

func (w *refCountingWriter) Write(p []byte) (int, error) {
	*w += refCountingWriter(len(p))
	return len(p), nil
}

func refRangesMIMESize(ranges []httpRange, contentType string, contentSize int64) (encSize int64) {
	var w refCountingWriter
	mw := multipart.NewWriter(&w)
	for _, ra := range ranges {
		mw.CreatePart(ra.mimeHeader(contentType, contentSize))
		encSize += ra.length
	}
	mw.Close()
	encSize += int64(w)
	return encSize
}

func TestRangesMIMESizeMatchesMultipartWriter(t *testing.T) {
	sizes := []int64{0, 1, 9, 10, 99, 100, 999, 1000, 1<<20 + 7, 1<<34 + 12345}
	ctypes := []string{"", "text/plain; charset=utf-8", "application/octet-stream"}

	for _, ct := range ctypes {
		// Empty range list edge case.
		if got, want := rangesMIMESize(nil, ct, 100), refRangesMIMESize(nil, ct, 100); got != want {
			t.Fatalf("nil ranges ct=%q: got %d, want %d", ct, got, want)
		}
		for _, sz := range sizes {
			if sz == 0 {
				continue
			}
			var ranges []httpRange
			ranges = append(ranges, httpRange{start: 0, length: 1})
			if sz > 10 {
				ranges = append(ranges, httpRange{start: 9, length: 2})
			}
			if sz > 100 {
				ranges = append(ranges, httpRange{start: sz - 10, length: 10})
			}
			got := rangesMIMESize(ranges, ct, sz)
			want := refRangesMIMESize(ranges, ct, sz)
			if got != want {
				t.Fatalf("ct=%q sz=%d ranges=%v: got %d, want %d", ct, sz, ranges, got, want)
			}
		}
	}

	// Verify 200 ranges (defaultMaxContentRanges) match exactly.
	many := make([]httpRange, defaultMaxContentRanges)
	for i := range many {
		many[i] = httpRange{start: int64(i * 10), length: 5}
	}
	if got, want := rangesMIMESize(many, "application/pdf", 100000), refRangesMIMESize(many, "application/pdf", 100000); got != want {
		t.Fatalf("200 ranges: got %d, want %d", got, want)
	}

	// Verify decimalLen on boundary and negative values including MinInt64.
	for _, n := range []int64{math.MinInt64, -100, -10, -9, -1, 0, 1, 9, 10, 99, 100, math.MaxInt64} {
		want := int64(len(fmt.Sprintf("%d", n)))
		if got := decimalLen(n); got != want {
			t.Errorf("decimalLen(%d) = %d, want %d", n, got, want)
		}
	}

	if allocs := testing.AllocsPerRun(100, func() {
		_ = rangesMIMESize(many[:5], "text/plain", 100000)
	}); allocs != 0 {
		t.Errorf("rangesMIMESize allocs = %v, want 0", allocs)
	}
}

func TestParseRangeIntoStackAndOverflow(t *testing.T) {
	// <= 4 ranges should stay on caller-provided backing array with 0 allocs.
	var buf [4]httpRange
	ranges, err := parseRangeInto("bytes=0-9,20-29,40-49,60-69", 100, buf[:0])
	if err != nil || len(ranges) != 4 {
		t.Fatalf("parseRangeInto(4) = (%v, %v)", ranges, err)
	}
	if &ranges[0] != &buf[0] {
		t.Errorf("expected parseRangeInto to use caller backing array for 4 ranges")
	}

	// > 4 ranges must grow cleanly.
	ranges, err = parseRangeInto("bytes=0-1,2-3,4-5,6-7,8-9", 100, buf[:0])
	if err != nil || len(ranges) != 5 {
		t.Fatalf("parseRangeInto(5) = (%v, %v)", ranges, err)
	}
	if ranges[4] != (httpRange{start: 8, length: 2}) {
		t.Errorf("ranges[4] = %+v, want {8 2}", ranges[4])
	}

	if allocs := testing.AllocsPerRun(100, func() {
		var local [4]httpRange
		r, e := parseRangeInto("bytes=0-99", 1000, local[:0])
		if e != nil || len(r) != 1 {
			t.Fatal(e)
		}
	}); allocs != 0 {
		t.Errorf("parseRangeInto single range allocs = %v, want 0", allocs)
	}
}

func TestOriginMatchesHostEquivalence(t *testing.T) {
	hosts := []string{
		"",
		"example.com",
		"example.com:8080",
		"example.com:",
		"127.0.0.1:443",
		"[::1]:8080",
		"evil.com",
	}
	origins := []string{
		"",
		"null",
		"about:blank",
		"https://",
		"http://",
		"https://example.com",
		"http://example.com",
		"https://example.com:8080",
		"http://example.com:8080",
		"https://example.com:",
		"https://127.0.0.1:443",
		"https://evil.com",
		"https://EXAMPLE.COM",
		"HTTPS://example.com",
		"ftp://example.com",
		"https://[::1]:8080",
		"https://[::1]",
		"https://example.com@evil.com",
		"https://evil@example.com",
		"https://example.com/path",
		"https://example.com?q=1",
		"https://example.com#frag",
		"https://example.com%2f.evil.com",
		"https://exa%6dple.com",
		"https://example.com:abc",
		"https://example.com:80:80",
		"https://example.com:-1",
		"https://example.com\r\n",
		"https:// example.com",
		"https://example.com ",
		"example.com",
	}

	for _, origin := range origins {
		for _, host := range hosts {
			o, err := url.Parse(origin)
			want := err == nil && o.Host == host
			got := originMatchesHost(origin, host)
			if got != want {
				t.Errorf("originMatchesHost(%q, %q) = %v, want %v (url.Parse err=%v host=%q)", origin, host, got, want, err, func() string {
					if o == nil {
						return ""
					}
					return o.Host
				}())
			}
		}
	}
}

func TestCrossOriginProtectionAllocsAndConcurrent(t *testing.T) {
	p := NewCrossOriginProtection()

	cases := []struct {
		name    string
		req     *Request
		wantErr bool
	}{
		{
			name: "SecFetchSameOrigin",
			req: &Request{
				Method: "POST",
				Host:   "example.com",
				Header: Header{"Sec-Fetch-Site": {"same-origin"}},
			},
		},
		{
			name: "SecFetchCrossSiteDenied",
			req: &Request{
				Method: "POST",
				Host:   "example.com",
				Header: Header{"Sec-Fetch-Site": {"cross-site"}},
			},
			wantErr: true,
		},
		{
			name: "OriginSameHost",
			req: &Request{
				Method: "POST",
				Host:   "example.com:8443",
				Header: Header{"Origin": {"https://example.com:8443"}},
			},
		},
		{
			name: "OriginCrossHostDenied",
			req: &Request{
				Method: "POST",
				Host:   "example.com",
				Header: Header{"Origin": {"https://evil.com"}},
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allocs := testing.AllocsPerRun(100, func() {
				err := p.Check(tc.req)
				if (err != nil) != tc.wantErr {
					t.Fatalf("Check err = %v, wantErr = %v", err, tc.wantErr)
				}
			})
			if allocs != 0 {
				t.Errorf("%s allocs = %v, want 0", tc.name, allocs)
			}
		})
	}

	// Concurrent AddTrustedOrigin + Check under -race.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = p.AddTrustedOrigin(fmt.Sprintf("https://trusted-%d.example.com", i))
		}
	}()
	go func() {
		defer wg.Done()
		req := &Request{
			Method: "POST",
			Host:   "example.com",
			Header: Header{
				"Sec-Fetch-Site": {"cross-site"},
				"Origin":         {"https://trusted-10.example.com"},
			},
		}
		for i := 0; i < 200; i++ {
			_ = p.Check(req)
		}
	}()
	wg.Wait()

	reqTrusted := &Request{
		Method: "POST",
		Host:   "example.com",
		Header: Header{
			"Sec-Fetch-Site": {"cross-site"},
			"Origin":         {"https://trusted-10.example.com"},
		},
	}
	if err := p.Check(reqTrusted); err != nil {
		t.Fatalf("trusted origin rejected after AddTrustedOrigin: %v", err)
	}
}

func BenchmarkRangesMIMESize(b *testing.B) {
	ranges := []httpRange{
		{start: 0, length: 100},
		{start: 500, length: 200},
		{start: 1000, length: 300},
		{start: 5000, length: 1000},
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = rangesMIMESize(ranges, "application/octet-stream", 100000)
	}
}

func BenchmarkCrossOriginCheck(b *testing.B) {
	p := NewCrossOriginProtection()
	b.Run("OriginSameHost", func(b *testing.B) {
		req := &Request{
			Method: "POST",
			Host:   "example.com:8443",
			Header: Header{"Origin": {"https://example.com:8443"}},
		}
		b.ReportAllocs()
		for b.Loop() {
			if err := p.Check(req); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("SecFetchCrossSiteDenied", func(b *testing.B) {
		req := &Request{
			Method: "POST",
			Host:   "example.com",
			Header: Header{"Sec-Fetch-Site": {"cross-site"}},
		}
		b.ReportAllocs()
		for b.Loop() {
			if err := p.Check(req); err == nil {
				b.Fatal("expected error")
			}
		}
	})
}
