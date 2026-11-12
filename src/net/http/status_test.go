// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bufio"
	"io"
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
