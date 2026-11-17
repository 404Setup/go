// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bufio"
	"bytes"
	"io"
	"net/http/internal"
	"testing"
)

func TestStatusTextAll(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{StatusContinue, "Continue"},
		{StatusOK, "OK"},
		{StatusNotFound, "Not Found"},
		{StatusInternalServerError, "Internal Server Error"},
		{StatusNetworkAuthenticationRequired, "Network Authentication Required"},
		{99, ""},
		{512, ""},
		{1000, ""},
	}
	for _, tt := range tests {
		if got := StatusText(tt.code); got != tt.want {
			t.Errorf("StatusText(%d) = %q, want %q", tt.code, got, tt.want)
		}
	}
}

func BenchmarkStatusText(b *testing.B) {
	codes := []int{200, 201, 204, 301, 302, 304, 400, 401, 403, 404, 500, 502, 503}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = StatusText(codes[i%len(codes)])
	}
}

func BenchmarkResponseStatusLine404(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		bw := bufio.NewWriter(io.Discard)
		var buf3 [3]byte
		for pb.Next() {
			writeStatusLine(bw, true, 404, buf3[:])
		}
	})
}

func TestStatusConsistencyWithInternal(t *testing.T) {
	for code := 0; code < 1000; code++ {
		if got, want := StatusText(code), internal.StatusText(code); got != want {
			t.Errorf("StatusText(%d) = %q, want %q", code, got, want)
		}
		for _, is11 := range []bool{false, true} {
			got := statusLine(is11, code)
			want := internal.StatusLine(is11, code)
			if got != want {
				t.Errorf("statusLine(%v, %d) = %q, want %q", is11, code, got, want)
			}
			var bw bytes.Buffer
			var scratch [3]byte
			bu := bufio.NewWriter(&bw)
			writeStatusLine(bu, is11, code, scratch[:])
			bu.Flush()
			if want != "" && bw.String() != want {
				t.Errorf("writeStatusLine(%v, %d) = %q, want %q", is11, code, bw.String(), want)
			}
		}
	}
	// Non-standard 3-digit code and 4-digit fallback.
	for _, tc := range []struct {
		is11 bool
		code int
		want string
	}{
		{true, 299, "HTTP/1.1 299 status code 299\r\n"},
		{false, 299, "HTTP/1.0 299 status code 299\r\n"},
		{true, 7, "HTTP/1.1 007 status code 7\r\n"},
		{true, 1000, "HTTP/1.1 1000 status code 1000\r\n"},
	} {
		var bw bytes.Buffer
		var scratch [3]byte
		bu := bufio.NewWriter(&bw)
		writeStatusLine(bu, tc.is11, tc.code, scratch[:])
		bu.Flush()
		if bw.String() != tc.want {
			t.Errorf("writeStatusLine(%v, %d) = %q, want %q", tc.is11, tc.code, bw.String(), tc.want)
		}
	}
}

