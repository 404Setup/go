// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !nethttpomithttp2 && !nethttpomithttp2client

package http

import (
	"net/http/internal/http2"
	"testing"
)

func TestPopulateHTTP2Response(t *testing.T) {
	req := &Request{Method: "GET"}
	tests := []struct {
		status     string
		statusCode int
		wantStatus string
	}{
		{"200", 200, "200 OK"},
		{"404", 404, "404 Not Found"},
		{"299", 299, "299 "},
		{"0200", 200, "0200 OK"},
	}
	for _, tt := range tests {
		resp := &Response{}
		cresp := &http2.ClientResponse{
			Status:        tt.status,
			StatusCode:    tt.statusCode,
			Header:        http2.Header{"X-K": {"v"}},
			ContentLength: 5,
			Body:          NoBody,
		}
		got := populateHTTP2Response(resp, req, cresp)
		if got != resp {
			t.Fatalf("populateHTTP2Response returned unexpected pointer")
		}
		if got.Status != tt.wantStatus || got.StatusCode != tt.statusCode || got.Proto != "HTTP/2.0" || got.ProtoMajor != 2 || got.ProtoMinor != 0 {
			t.Errorf("status (%q, %d) => Status=%q StatusCode=%d Proto=%q, want %q", tt.status, tt.statusCode, got.Status, got.StatusCode, got.Proto, tt.wantStatus)
		}
		if got.Request != req || got.Header.Get("X-K") != "v" || got.ContentLength != 5 {
			t.Errorf("unexpected response fields: %+v", got)
		}
	}
}
