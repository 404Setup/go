// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestParseSimpleHTTPURLParity(t *testing.T) {
	urls := []string{
		"http://example.com",
		"https://example.com",
		"http://example.com/",
		"http://127.0.0.1:8080/api/v1/items",
		"https://sub.domain-1.example.org:8443/path/to/res?a=1&b=two",
		"http://example.com/path?",
		"http://example.com?q=1",
		"http://example.com:/path",
		"http://example.com:80",
		"http://example.com:abc/path",
		"http://example.com:80:90/path",
		"http://user:pass@example.com/path",
		"http://[::1]:8080/path",
		"http://[fe80::1%25eth0]:8080/path",
		"http://example.com/a%20b?x=1%262",
		"http://example.com/path#frag",
		"http://example.com/a;b",
		"http://example.com/a\\b",
		"http://example.com/a b",
		"HTTP://EXAMPLE.COM/Path",
		"http://",
		"http:///onlypath",
		"ftp://example.com/file",
		"/relative/path?x=1",
		"",
	}

	for _, raw := range urls {
		wantURL, wantErr := url.Parse(raw)
		if wantErr == nil {
			wantURL.Host = strings.TrimSuffix(wantURL.Host, ":")
		}

		req, gotErr := NewRequestWithContext(context.Background(), "GET", raw, nil)
		if (gotErr != nil) != (wantErr != nil) {
			t.Fatalf("NewRequest(%q) err=%v, wantErr=%v", raw, gotErr, wantErr)
		}
		if gotErr != nil {
			continue
		}
		if !reflect.DeepEqual(req.URL, wantURL) {
			t.Fatalf("NewRequest(%q).URL = %#v, want %#v", raw, req.URL, wantURL)
		}
		if req.Host != wantURL.Host {
			t.Fatalf("NewRequest(%q).Host = %q, want %q", raw, req.Host, wantURL.Host)
		}
		if req.Header == nil {
			t.Fatalf("NewRequest(%q).Header is nil", raw)
		}
	}
}

func TestCanonicalHostPortEdgeCases(t *testing.T) {
	tests := []string{
		"http://example.com",
		"https://example.com",
		"http://example.com:8080",
		"http://[::1]:8080",
		"http://[::1]]:8080",
		"http://[a[b]:c]:8080",
		"http://[notipv6]:8080",
		"http://[fe80::1%25eth0]:8080",
		"http://example.com:",
		"http://example.com:abc",
		"http://[::1]",
		"https://EXAMPLE.COM:8443",
		"http://münchen.de:8080",
	}

	for _, raw := range tests {
		u, err := url.Parse(raw)
		if err != nil {
			u = &url.URL{Scheme: "http", Host: strings.TrimPrefix(raw, "http://")}
		}
		port := u.Port()
		if port == "" {
			port = schemePort(u.Scheme)
		}
		wantJoined := net.JoinHostPort(idnaASCIIFromURL(u), port)
		wantHost, wantPort := wantJoined, ""
		if colon := strings.LastIndexByte(wantJoined, ':'); colon >= 0 {
			wantHost, wantPort = wantJoined[:colon], wantJoined[colon+1:]
		}

		gotHost, gotPort, _ := canonicalHostPort(u)
		gotAddr := canonicalAddr(u)
		if gotHost != wantHost || gotPort != wantPort || gotAddr != wantJoined {
			t.Fatalf("canonicalHostPort(%q) = (%q, %q, addr=%q), want (%q, %q, addr=%q)",
				raw, gotHost, gotPort, gotAddr, wantHost, wantPort, wantJoined)
		}
	}
}

func TestReadResponseBodyCoallocModes(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		raw        string
		wantStatus int
		wantBody   string
		wantCL     int64
		wantNoBody bool
	}{
		{
			name:       "content_length_positive",
			method:     "GET",
			raw:        "HTTP/1.1 200 OK\r\nContent-Length: 11\r\nContent-Type: text/plain\r\n\r\nhello world",
			wantStatus: 200,
			wantBody:   "hello world",
			wantCL:     11,
		},
		{
			name:       "content_length_zero",
			method:     "GET",
			raw:        "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n",
			wantStatus: 200,
			wantBody:   "",
			wantCL:     0,
			wantNoBody: true,
		},
		{
			name:       "chunked_with_trailer",
			method:     "GET",
			raw:        "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nTrailer: X-Checksum\r\n\r\n5\r\nhello\r\n0\r\nX-Checksum: ok\r\n\r\n",
			wantStatus: 200,
			wantBody:   "hello",
			wantCL:     -1,
		},
		{
			name:       "http10_close_delimited",
			method:     "GET",
			raw:        "HTTP/1.0 200 OK\r\n\r\nlegacy-body",
			wantStatus: 200,
			wantBody:   "legacy-body",
			wantCL:     -1,
		},
		{
			name:       "head_with_content_length",
			method:     "HEAD",
			raw:        "HTTP/1.1 200 OK\r\nContent-Length: 42\r\n\r\n",
			wantStatus: 200,
			wantBody:   "",
			wantCL:     42,
			wantNoBody: true,
		},
		{
			name:       "no_content_204",
			method:     "GET",
			raw:        "HTTP/1.1 204 No Content\r\n\r\n",
			wantStatus: 204,
			wantBody:   "",
			wantCL:     0,
			wantNoBody: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := NewRequest(tc.method, "http://example.com/", nil)
			resp, err := ReadResponse(bufio.NewReader(strings.NewReader(tc.raw)), req)
			if err != nil {
				t.Fatalf("ReadResponse: %v", err)
			}
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if resp.ContentLength != tc.wantCL {
				t.Fatalf("ContentLength = %d, want %d", resp.ContentLength, tc.wantCL)
			}
			if tc.wantNoBody && resp.Body != NoBody {
				t.Fatalf("expected NoBody, got %T", resp.Body)
			}
			gotBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(gotBytes) != tc.wantBody {
				t.Fatalf("Body = %q, want %q", gotBytes, tc.wantBody)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	}
}

func TestNewRequestAndReadResponseConcurrent(t *testing.T) {
	const raw = "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nhello"
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				req, err := NewRequestWithContext(context.Background(), "GET", "https://example.com:8443/v1/test?x=1", nil)
				if err != nil {
					t.Errorf("NewRequestWithContext: %v", err)
					return
				}
				resp, err := ReadResponse(bufio.NewReader(strings.NewReader(raw)), req)
				if err != nil {
					t.Errorf("ReadResponse: %v", err)
					return
				}
				b, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || string(b) != "hello" {
					t.Errorf("body = %q, err = %v", b, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func BenchmarkNewRequestSimpleURL(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		req, err := NewRequestWithContext(ctx, "GET", "http://127.0.0.1:8080/api/items?limit=20", nil)
		if err != nil {
			b.Fatal(err)
		}
		_ = req
	}
}

func BenchmarkReadResponseBodyCoalloc(b *testing.B) {
	raw := []byte("HTTP/1.1 200 OK\r\nDate: Thu, 26 Nov 2026 12:00:00 GMT\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: 12\r\n\r\nHello world!")
	req, _ := NewRequest("GET", "http://example.com/", nil)
	var sr bytes.Reader
	br := bufio.NewReader(&sr)
	var sink [12]byte
	b.ReportAllocs()
	for b.Loop() {
		sr.Reset(raw)
		br.Reset(&sr)
		resp, err := ReadResponse(br, req)
		if err != nil {
			b.Fatal(err)
		}
		_, err = io.ReadFull(resp.Body, sink[:])
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
	}
}
