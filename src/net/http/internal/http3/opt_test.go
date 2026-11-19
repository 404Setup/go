// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http3

import (
	"net/http"
	"testing"
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
