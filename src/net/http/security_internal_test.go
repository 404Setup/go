// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"context"
	"crypto/tls"
	"io"
	"mime/multipart"
	"net"
	"net/url"
	"reflect"
	"testing"
)

// TestRequestCopyNonContextFieldsComplete ensures copyNonContextFieldsTo covers
// every field of Request other than the lazy context internals.
func TestRequestCopyNonContextFieldsComplete(t *testing.T) {
	rt := reflect.TypeFor[Request]()
	src := &Request{
		Method:           "POST",
		URL:              &url.URL{Scheme: "http", Host: "example.com", Path: "/a"},
		Proto:            "HTTP/1.1",
		ProtoMajor:       1,
		ProtoMinor:       1,
		Header:           Header{"X-Test": {"1"}},
		Body:             NoBody,
		GetBody:          func() (io.ReadCloser, error) { return NoBody, nil },
		ContentLength:    42,
		TransferEncoding: []string{"chunked"},
		Close:            true,
		Host:             "example.com",
		Form:             url.Values{"a": {"b"}},
		PostForm:         url.Values{"c": {"d"}},
		MultipartForm:    &multipart.Form{},
		Trailer:          Header{"X-Tr": {"2"}},
		RemoteAddr:       "127.0.0.1:1234",
		RequestURI:       "/a",
		TLS:              &tls.ConnectionState{},
		Cancel:           make(chan struct{}),
		Response:         &Response{StatusCode: 200},
		Pattern:          "POST /a",
		pat:              &pattern{},
		matches:          []string{"m1"},
	}
	sv := reflect.ValueOf(src).Elem()

	dst := &Request{}
	src.copyNonContextFieldsTo(dst)
	dv := reflect.ValueOf(dst).Elem()

	skipped := map[string]bool{
		"ctx":       true,
		"ctxState":  true,
		"cancelCtx": true,
	}
	for i := range rt.NumField() {
		f := rt.Field(i)
		if skipped[f.Name] {
			if !dv.Field(i).IsZero() {
				t.Errorf("field %s should not be copied, got non-zero", f.Name)
			}
			continue
		}
		if sv.Field(i).IsZero() {
			t.Fatalf("test bug: src.%s was not populated", f.Name)
		}
		if dv.Field(i).IsZero() {
			t.Errorf("copyNonContextFieldsTo omitted Request.%s", f.Name)
		}
	}
}

// TestLazyServerRequestContextInactivePreservesLocalAddr verifies that when
// lazyServerRequestContext races with cancelReqCtx (cr.reqActive == false),
// the returned pre-canceled context still exposes LocalAddrContextKey.
func TestLazyServerRequestContextInactivePreservesLocalAddr(t *testing.T) {
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}
	scc := &serverConnContext{
		Context:   context.Background(),
		localAddr: addr,
	}
	cr := &connReader{reqActive: false}
	ctx, cancel := cr.lazyServerRequestContext(scc)
	if cancel != nil {
		t.Fatalf("cancel = non-nil, want nil")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
	if got := ctx.Value(LocalAddrContextKey); got != addr {
		t.Fatalf("LocalAddrContextKey = %v, want %v", got, addr)
	}
}
