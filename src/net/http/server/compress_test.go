// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server_test

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/server"
	"strings"
	"sync"
	"testing"
)

func gunzipBody(t *testing.T, r io.Reader) string {
	t.Helper()
	gr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gr.Close()
	data, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	return string(data)
}

func TestMiddlewareCompressBasicAndThreshold(t *testing.T) {
	s := server.New()
	s.Use(server.CompressWithConfig(server.CompressConfig{
		MinLength: 64,
	}))

	small := "short body"
	large := strings.Repeat("hello gzip compression! ", 20)

	s.GET("/small", func(c *server.Context) error {
		return c.Text(http.StatusOK, small)
	})
	s.GET("/large", func(c *server.Context) error {
		return c.Text(http.StatusOK, large)
	})
	s.GET("/nocontent", func(c *server.Context) error {
		return c.NoContent()
	})

	// Large response with Accept-Encoding: gzip is compressed.
	req := httptest.NewRequest(http.MethodGet, "/large", nil)
	req.Header.Set("Accept-Encoding", "br, gzip;q=0.8")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
	if !strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), "Accept-Encoding") {
		t.Fatalf("missing Vary: Accept-Encoding: %v", rec.Header().Values("Vary"))
	}
	if got := gunzipBody(t, rec.Body); got != large {
		t.Fatalf("decompressed mismatch: len=%d want=%d", len(got), len(large))
	}

	// Small response below MinLength is sent uncompressed.
	req = httptest.NewRequest(http.MethodGet, "/small", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != small {
		t.Fatalf("small: code=%d ce=%q body=%q", rec.Code, rec.Header().Get("Content-Encoding"), rec.Body.String())
	}

	// 204 No Content is never compressed.
	req = httptest.NewRequest(http.MethodGet, "/nocontent", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Content-Encoding") != "" || rec.Body.Len() != 0 {
		t.Fatalf("204: code=%d ce=%q len=%d", rec.Code, rec.Header().Get("Content-Encoding"), rec.Body.Len())
	}

	// HEAD request is never compressed.
	req = httptest.NewRequest(http.MethodHead, "/large", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("HEAD: code=%d ce=%q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
}

func TestMiddlewareCompressAcceptEncodingNegotiation(t *testing.T) {
	s := server.New()
	s.Use(server.CompressWithConfig(server.CompressConfig{MinLength: 16}))
	body := strings.Repeat("compressible text ", 10)
	s.GET("/data", func(c *server.Context) error {
		return c.Text(http.StatusOK, body)
	})

	cases := []struct {
		ae       string
		wantGzip bool
	}{
		{"", false},
		{"deflate", false},
		{"gzip", true},
		{"GZIP", true},
		{"x-gzip", true},
		{"*", true},
		{"*;q=0", false},
		{"gzip;q=0", false},
		{"gzip;q=0.000", false},
		{"gzip;q=0, *;q=1", false},
		{"deflate, gzip;q=0.5", true},
		{"gzip;q=invalid", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/data", nil)
		if tc.ae != "" {
			req.Header.Set("Accept-Encoding", tc.ae)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		gotGzip := rec.Header().Get("Content-Encoding") == "gzip"
		if gotGzip != tc.wantGzip {
			t.Errorf("Accept-Encoding=%q: gotGzip=%v, want %v", tc.ae, gotGzip, tc.wantGzip)
		}
		if gotGzip {
			if got := gunzipBody(t, rec.Body); got != body {
				t.Errorf("Accept-Encoding=%q: body mismatch", tc.ae)
			}
		} else if rec.Body.String() != body {
			t.Errorf("Accept-Encoding=%q: raw body mismatch", tc.ae)
		}
	}
}

func TestMiddlewareCompressContentTypesSniffFlushSkipAndRecovery(t *testing.T) {
	s := server.New()
	s.Use(server.Recover())
	s.Use(server.CompressWithConfig(server.CompressConfig{
		MinLength: 64,
		Skip: func(c *server.Context) bool {
			return c.Path() == "/skip"
		},
	}))

	large := strings.Repeat("abcdef0123456789", 20)

	s.GET("/png", func(c *server.Context) error {
		return c.Bytes(http.StatusOK, "image/png", []byte(large))
	})
	s.GET("/svg", func(c *server.Context) error {
		return c.Bytes(http.StatusOK, "image/svg+xml", []byte("<svg>"+large+"</svg>"))
	})
	s.GET("/sse", func(c *server.Context) error {
		return c.Bytes(http.StatusOK, "text/event-stream", []byte("data: "+large+"\n\n"))
	})
	s.GET("/preencoded", func(c *server.Context) error {
		c.SetHeader("Content-Encoding", "br")
		return c.Text(http.StatusOK, large)
	})
	s.GET("/sniff", func(c *server.Context) error {
		// Write without explicit Content-Type in two chunks crossing MinLength.
		_, _ = c.ResponseWriter().Write([]byte("<!DOC"))
		_, _ = c.ResponseWriter().Write([]byte("TYPE html><html><body>" + large + "</body></html>"))
		return nil
	})
	s.GET("/flush", func(c *server.Context) error {
		_, _ = c.ResponseWriter().Write([]byte(large))
		if err := c.Flush(); err != nil {
			return err
		}
		_, _ = c.ResponseWriter().Write([]byte("tail"))
		return nil
	})
	s.GET("/skip", func(c *server.Context) error {
		return c.Text(http.StatusOK, large)
	})
	s.GET("/err", func(c *server.Context) error {
		return server.NewError(http.StatusBadRequest, "bad input")
	})
	s.GET("/panic", func(c *server.Context) error {
		_, _ = c.ResponseWriter().Write([]byte(large))
		panic("mid-stream boom")
	})

	// PNG must not be compressed.
	req := httptest.NewRequest(http.MethodGet, "/png", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != large {
		t.Fatalf("png compressed unexpectedly: ce=%q", rec.Header().Get("Content-Encoding"))
	}

	// SVG should be compressed.
	req = httptest.NewRequest(http.MethodGet, "/svg", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" || gunzipBody(t, rec.Body) != "<svg>"+large+"</svg>" {
		t.Fatalf("svg not compressed properly: ce=%q", rec.Header().Get("Content-Encoding"))
	}

	// SSE must not be compressed.
	req = httptest.NewRequest(http.MethodGet, "/sse", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("sse compressed unexpectedly: ce=%q", rec.Header().Get("Content-Encoding"))
	}

	// Already Content-Encoding: br must not be double-compressed.
	req = httptest.NewRequest(http.MethodGet, "/preencoded", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "br" || rec.Body.String() != large {
		t.Fatalf("preencoded altered: ce=%q", rec.Header().Get("Content-Encoding"))
	}

	// Sniffed Content-Type must be text/html (not application/x-gzip).
	req = httptest.NewRequest(http.MethodGet, "/sniff", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("sniffed Content-Type = %q, want text/html", ct)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("sniff Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
	wantHTML := "<!DOCTYPE html><html><body>" + large + "</body></html>"
	if got := gunzipBody(t, rec.Body); got != wantHTML {
		t.Fatalf("sniff body mismatch")
	}

	// Flush flushes gzip stream cleanly.
	req = httptest.NewRequest(http.MethodGet, "/flush", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if !rec.Flushed || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("flush: flushed=%v ce=%q", rec.Flushed, rec.Header().Get("Content-Encoding"))
	}
	if got := gunzipBody(t, rec.Body); got != large+"tail" {
		t.Fatalf("flush body mismatch")
	}

	// Skip bypasses compression and Vary header.
	req = httptest.NewRequest(http.MethodGet, "/skip", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != large {
		t.Fatalf("skip compressed unexpectedly: ce=%q", rec.Header().Get("Content-Encoding"))
	}

	// Error returned without writing goes through ErrorHandler cleanly.
	req = httptest.NewRequest(http.MethodGet, "/err", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || rec.Body.String() != "bad input" {
		t.Fatalf("err: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// Mid-stream panic must not poison pool for subsequent requests.
	req = httptest.NewRequest(http.MethodGet, "/panic", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	req = httptest.NewRequest(http.MethodGet, "/svg", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || gunzipBody(t, rec.Body) != "<svg>"+large+"</svg>" {
		t.Fatalf("post-panic request failed: code=%d", rec.Code)
	}

	// Invalid compression level panics.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic for invalid gzip level")
			}
		}()
		server.CompressWithConfig(server.CompressConfig{Level: 99})
	}()
}

func TestMiddlewareCompressConcurrent(t *testing.T) {
	s := server.New()
	s.Use(server.Compress())
	s.GET("/item/{id}", func(c *server.Context) error {
		id := c.Param("id")
		return c.Text(http.StatusOK, strings.Repeat("item="+id+";", 120))
	})

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 30 {
				id := fmt.Sprintf("%d-%d", worker, i)
				req := httptest.NewRequest(http.MethodGet, "/item/"+id, nil)
				if i%2 == 0 {
					req.Header.Set("Accept-Encoding", "gzip")
				}
				rec := httptest.NewRecorder()
				s.ServeHTTP(rec, req)
				want := strings.Repeat("item="+id+";", 120)
				if i%2 == 0 {
					if rec.Header().Get("Content-Encoding") != "gzip" || gunzipBody(t, rec.Body) != want {
						t.Errorf("compressed mismatch for %s", id)
						return
					}
				} else if rec.Body.String() != want {
					t.Errorf("uncompressed mismatch for %s", id)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}
