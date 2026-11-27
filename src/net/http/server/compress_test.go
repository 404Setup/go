// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server_test

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/server"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
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

func TestMiddlewareCompressLoggerInnerAndPartialWriteErrorIsolation(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	s := server.New()
	// Place Compress OUTSIDE Logger to verify StatusCode(), BytesWritten(),
	// and Written() are accurate even while responses are buffered in Compress.
	s.Use(server.Compress(), server.Logger(logger))

	s.POST("/small-created", func(c *server.Context) error {
		return c.Text(http.StatusCreated, "created-small")
	})
	s.GET("/partial-then-err", func(c *server.Context) error {
		_, _ = c.ResponseWriter().Write([]byte("partial-body"))
		return errors.New("late failure after write")
	})
	bigText := strings.Repeat("chunked-write-string-payload-0123456789;", 300) // ~12 KiB (> 4096 maxPooledCompressBuf)
	s.GET("/big-string", func(c *server.Context) error {
		return c.Text(http.StatusOK, bigText)
	})

	// 1. Small 201 response (< MinLength) logged inside Compress must record status=201 and bytes=13.
	req := httptest.NewRequest(http.MethodPost, "/small-created", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || rec.Body.String() != "created-small" {
		t.Fatalf("small-created: code=%d body=%q", rec.Code, rec.Body.String())
	}
	var entry map[string]any
	if err := json.Unmarshal(logBuf.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal log: %v (raw=%q)", err, logBuf.String())
	}
	if entry["status"] != float64(http.StatusCreated) || entry["bytes"] != float64(len("created-small")) {
		t.Fatalf("inner Logger recorded wrong status/bytes: %#v", entry)
	}

	// 2. Partial write (< MinLength) followed by an error must NOT append error message to the buffered body.
	logBuf.Reset()
	req = httptest.NewRequest(http.MethodGet, "/partial-then-err", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "partial-body" {
		t.Fatalf("partial-then-err corrupted body: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// 3. Multi-chunk WriteString (> 4 KiB) compresses and decompresses cleanly,
	// and inner Logger records the full uncompressed payload byte count.
	logBuf.Reset()
	req = httptest.NewRequest(http.MethodGet, "/big-string", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("big-string: code=%d ce=%q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	if got := gunzipBody(t, rec.Body); got != bigText {
		t.Fatalf("big-string decompressed len=%d, want %d", len(got), len(bigText))
	}
	if err := json.Unmarshal(logBuf.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["bytes"] != float64(len(bigText)) {
		t.Fatalf("inner Logger bytes for /big-string = %v, want %d", entry["bytes"], len(bigText))
	}

	// 4. Multiple Accept-Encoding header lines: second line enabling gzip works,
	// and a subsequent line with gzip;q=0 overrides and disables gzip.
	req = httptest.NewRequest(http.MethodGet, "/big-string", nil)
	req.Header.Add("Accept-Encoding", "deflate")
	req.Header.Add("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("multi-header Accept-Encoding should enable gzip, got %q", rec.Header().Get("Content-Encoding"))
	}

	req = httptest.NewRequest(http.MethodGet, "/big-string", nil)
	req.Header.Add("Accept-Encoding", "gzip")
	req.Header.Add("Accept-Encoding", "gzip;q=0")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("multi-header gzip;q=0 should disable gzip, got %q", rec.Header().Get("Content-Encoding"))
	}

	// 5. Outer Logger + inner Compress + handler returning error or pre-setting Content-Encoding.
	logBuf.Reset()
	s2 := server.New()
	s2.Use(server.Logger(logger), server.Compress())
	s2.GET("/stale-ce-err", func(c *server.Context) error {
		c.SetHeader("Content-Encoding", "br")
		return server.NewError(http.StatusBadRequest, "bad input")
	})
	s2.GET("/partial-content", func(c *server.Context) error {
		c.SetHeader("Content-Range", "bytes 0-1999/5000")
		return c.Text(http.StatusPartialContent, bigText)
	})
	req = httptest.NewRequest(http.MethodGet, "/stale-ce-err", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s2.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != "bad input" {
		t.Fatalf("stale-ce-err: code=%d ce=%q body=%q", rec.Code, rec.Header().Get("Content-Encoding"), rec.Body.String())
	}
	if err := json.Unmarshal(logBuf.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["status"] != float64(http.StatusBadRequest) || entry["bytes"] != float64(len("bad input")) {
		t.Fatalf("outer Logger recorded wrong status/bytes: %#v", entry)
	}

	// 6. 206 Partial Content / Content-Range responses must never be compressed.
	req = httptest.NewRequest(http.MethodGet, "/partial-content", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s2.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != bigText {
		t.Fatalf("206 Partial Content should bypass compression: code=%d ce=%q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
}

func TestMiddlewareCompressETagWeakeningNoTransformAndStrictQValue(t *testing.T) {
	s := server.New()
	s.Use(server.CompressWithConfig(server.CompressConfig{MinLength: 32}))

	large := strings.Repeat("compressible-etag-payload-", 10)
	s.GET("/strong-etag", func(c *server.Context) error {
		c.SetHeader("ETag", `"v1-strong"`)
		return c.Text(http.StatusOK, large)
	})
	s.GET("/weak-etag", func(c *server.Context) error {
		c.SetHeader("ETag", `W/"v1-weak"`)
		return c.Text(http.StatusOK, large)
	})
	s.GET("/small-etag", func(c *server.Context) error {
		c.SetHeader("ETag", `"v1-small"`)
		return c.Text(http.StatusOK, "tiny")
	})
	s.GET("/no-transform", func(c *server.Context) error {
		c.SetHeader("Cache-Control", "public, no-transform, max-age=60")
		return c.Text(http.StatusOK, large)
	})

	// Strong ETag is weakened when compressed.
	req := httptest.NewRequest(http.MethodGet, "/strong-etag", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("ETag") != `W/"v1-strong"` {
		t.Fatalf("strong ETag not weakened: ce=%q etag=%q", rec.Header().Get("Content-Encoding"), rec.Header().Get("ETag"))
	}

	// Already-weak ETag stays weak without double prefixing.
	req = httptest.NewRequest(http.MethodGet, "/weak-etag", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("ETag") != `W/"v1-weak"` {
		t.Fatalf("weak ETag altered: ce=%q etag=%q", rec.Header().Get("Content-Encoding"), rec.Header().Get("ETag"))
	}

	// Below MinLength: uncompressed strong ETag stays strong.
	req = httptest.NewRequest(http.MethodGet, "/small-etag", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("ETag") != `"v1-small"` {
		t.Fatalf("small ETag altered: ce=%q etag=%q", rec.Header().Get("Content-Encoding"), rec.Header().Get("ETag"))
	}

	// Cache-Control: no-transform bypasses compression.
	req = httptest.NewRequest(http.MethodGet, "/no-transform", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != large {
		t.Fatalf("no-transform compressed unexpectedly: ce=%q", rec.Header().Get("Content-Encoding"))
	}

	// Invalid RFC 9110 qvalues (+Inf, >1, negative, >3 decimals) must not enable gzip.
	for _, ae := range []string{"gzip;q=+Inf", "gzip;q=1.5", "gzip;q=-0.5", "gzip;q=0.1234", "gzip;q=1.001"} {
		req := httptest.NewRequest(http.MethodGet, "/strong-etag", nil)
		req.Header.Set("Accept-Encoding", ae)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Header().Get("Content-Encoding") != "" {
			t.Errorf("Accept-Encoding=%q enabled gzip unexpectedly", ae)
		}
	}
}

func TestCompressExplicitNilContentType(t *testing.T) {
	s := server.New()
	s.Use(server.CompressWithConfig(server.CompressConfig{MinLength: 16}))
	large := strings.Repeat("<html><body>hello</body></html>", 10)

	s.GET("/write-bytes", func(c *server.Context) error {
		c.ResponseWriter().Header()["Content-Type"] = nil
		_, err := c.ResponseWriter().Write([]byte(large))
		return err
	})
	s.GET("/write-string", func(c *server.Context) error {
		c.ResponseWriter().Header()["Content-Type"] = nil
		_, err := io.WriteString(c.ResponseWriter(), large)
		return err
	})

	for _, path := range []string{"/write-bytes", "/write-string"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if vals, ok := rec.Header()["Content-Type"]; !ok || len(vals) != 0 {
			t.Fatalf("%s: expected Content-Type key present with nil/empty slice, got ok=%v vals=%v", path, ok, vals)
		}
	}
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zlibBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func inflateZlibBody(t *testing.T, r io.Reader) string {
	t.Helper()
	zr, err := zlib.NewReader(r)
	if err != nil {
		t.Fatalf("zlib.NewReader: %v", err)
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("zlib read: %v", err)
	}
	return string(data)
}

// framedWriter/framedReader simulate custom zstd/brotli codecs with Reset support.
type framedWriter struct {
	tag     string
	dst     io.Writer
	buf     bytes.Buffer
	flushes atomic.Int64
	resets  *atomic.Int64
}

func (w *framedWriter) Write(p []byte) (int, error) {
	return w.buf.Write(p)
}

func (w *framedWriter) Flush() error {
	w.flushes.Add(1)
	return nil
}

func (w *framedWriter) Close() error {
	if w.dst == nil {
		return nil
	}
	if _, err := io.WriteString(w.dst, w.tag); err != nil {
		return err
	}
	_, err := w.dst.Write(w.buf.Bytes())
	w.dst = nil
	return err
}

func (w *framedWriter) Reset(dst io.Writer) {
	w.dst = dst
	w.buf.Reset()
	if w.resets != nil {
		w.resets.Add(1)
	}
}

type framedReader struct {
	tag     string
	src     io.Reader
	checked bool
	resets  *atomic.Int64
}

func (r *framedReader) Read(p []byte) (int, error) {
	if !r.checked {
		r.checked = true
		hdr := make([]byte, len(r.tag))
		if _, err := io.ReadFull(r.src, hdr); err != nil {
			return 0, err
		}
		if string(hdr) != r.tag {
			return 0, fmt.Errorf("invalid %s magic header %q", r.tag, hdr)
		}
	}
	return r.src.Read(p)
}

func (r *framedReader) Close() error {
	return nil
}

func (r *framedReader) Reset(src io.Reader) error {
	r.src = src
	r.checked = false
	if r.resets != nil {
		r.resets.Add(1)
	}
	return nil
}

type simpleWriteCloser struct {
	tag string
	dst io.Writer
	buf bytes.Buffer
}

func (w *simpleWriteCloser) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *simpleWriteCloser) Close() error {
	if _, err := io.WriteString(w.dst, w.tag); err != nil {
		return err
	}
	_, err := w.dst.Write(w.buf.Bytes())
	return err
}

type simpleReadCloser struct {
	tag     string
	src     io.Reader
	checked bool
}

func (r *simpleReadCloser) Read(p []byte) (int, error) {
	if !r.checked {
		r.checked = true
		hdr := make([]byte, len(r.tag))
		if _, err := io.ReadFull(r.src, hdr); err != nil {
			return 0, err
		}
		if string(hdr) != r.tag {
			return 0, fmt.Errorf("invalid %s header", r.tag)
		}
	}
	return r.src.Read(p)
}
func (r *simpleReadCloser) Close() error { return nil }

func TestServerDefaultCompressionAndDecompression(t *testing.T) {
	s := server.New()
	large := strings.Repeat("default-server-compression-payload-", 48) // > 1024 bytes

	s.GET("/large", func(c *server.Context) error {
		c.SetHeader("ETag", `"v1"`)
		return c.Text(http.StatusOK, large)
	})
	s.GET("/small", func(c *server.Context) error {
		return c.Text(http.StatusOK, "tiny")
	})
	s.POST("/echo", func(c *server.Context) error {
		b, err := c.BodyBytes()
		if err != nil {
			return err
		}
		return c.Text(http.StatusOK, fmt.Sprintf("ce=%q|cl=%d|%s", c.Header("Content-Encoding"), c.Request().ContentLength, string(b)))
	})
	s.POST("/json", func(c *server.Context) error {
		var in map[string]string
		if err := c.BindJSON(&in); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]string{
			"msg":  in["msg"],
			"fill": strings.Repeat("x", 1100),
		})
	})

	// 1. Default gzip response compression + ETag weakening + Vary.
	req := httptest.NewRequest(http.MethodGet, "/large", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("ETag") != `W/"v1"` {
		t.Fatalf("gzip: code=%d ce=%q etag=%q", rec.Code, rec.Header().Get("Content-Encoding"), rec.Header().Get("ETag"))
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("missing Vary: Accept-Encoding: %v", rec.Header().Values("Vary"))
	}
	if got := gunzipBody(t, rec.Body); got != large {
		t.Fatalf("gzip body mismatch")
	}

	// 2. Default deflate (zlib) response compression.
	req = httptest.NewRequest(http.MethodGet, "/large", nil)
	req.Header.Set("Accept-Encoding", "deflate")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "deflate" {
		t.Fatalf("deflate: code=%d ce=%q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	if got := inflateZlibBody(t, rec.Body); got != large {
		t.Fatalf("deflate body mismatch")
	}

	// 3. Unregistered zstd/br are NOT negotiated until registered.
	req = httptest.NewRequest(http.MethodGet, "/large", nil)
	req.Header.Set("Accept-Encoding", "zstd, br")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != large {
		t.Fatalf("unregistered zstd/br compressed unexpectedly: ce=%q", rec.Header().Get("Content-Encoding"))
	}

	// 4. Negotiation: zstd/br skipped -> gzip preferred over deflate at equal q; client q overrides.
	req = httptest.NewRequest(http.MethodGet, "/large", nil)
	req.Header.Set("Accept-Encoding", "zstd, br, deflate, gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("want gzip over deflate at equal q, got %q", rec.Header().Get("Content-Encoding"))
	}

	req = httptest.NewRequest(http.MethodGet, "/large", nil)
	req.Header.Set("Accept-Encoding", "deflate;q=1.0, gzip;q=0.5")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "deflate" {
		t.Fatalf("want deflate with higher q, got %q", rec.Header().Get("Content-Encoding"))
	}

	req = httptest.NewRequest(http.MethodGet, "/large", nil)
	req.Header.Set("Accept-Encoding", "gzip;q=0, *;q=1")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "deflate" {
		t.Fatalf("want deflate via wildcard when gzip;q=0, got %q", rec.Header().Get("Content-Encoding"))
	}

	// 5. Small payload (< 1024) stays uncompressed.
	req = httptest.NewRequest(http.MethodGet, "/small", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != "tiny" {
		t.Fatalf("small body compressed unexpectedly: ce=%q", rec.Header().Get("Content-Encoding"))
	}

	// 6. Automatic request decompression: gzip, x-gzip, deflate, stacked gzip+deflate.
	plain := "hello compressed request body"
	for _, tc := range []struct {
		name string
		ce   string
		body []byte
	}{
		{"gzip", "gzip", gzipBytes(t, []byte(plain))},
		{"x-gzip", "x-gzip", gzipBytes(t, []byte(plain))},
		{"deflate", "deflate", zlibBytes(t, []byte(plain))},
		{"stacked_gzip_deflate", "gzip, deflate", zlibBytes(t, gzipBytes(t, []byte(plain)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(tc.body))
			req.Header.Set("Content-Encoding", tc.ce)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			want := fmt.Sprintf(`ce=""|cl=-1|%s`, plain)
			if rec.Code != http.StatusOK || rec.Body.String() != want {
				t.Fatalf("got code=%d body=%q, want %q", rec.Code, rec.Body.String(), want)
			}
		})
	}

	// 7. Partial/unregistered outermost request encoding (gzip, zstd) is left untouched.
	rawPayload := []byte("raw-zstd-wrapped-payload")
	req = httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(rawPayload))
	req.Header.Set("Content-Encoding", "gzip, zstd")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `ce="gzip, zstd"`) || !strings.Contains(rec.Body.String(), string(rawPayload)) {
		t.Fatalf("unregistered outermost encoding altered: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// 8. Roundtrip: compressed JSON request (deflate) + compressed JSON response (gzip).
	jsonPayload := gzipBytes(t, []byte(`{"msg":"roundtrip-ok"}`))
	req = httptest.NewRequest(http.MethodPost, "/json", bytes.NewReader(jsonPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Accept-Encoding", "deflate")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "deflate" {
		t.Fatalf("roundtrip: code=%d ce=%q body=%q", rec.Code, rec.Header().Get("Content-Encoding"), rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(inflateZlibBody(t, rec.Body)), &out); err != nil {
		t.Fatal(err)
	}
	if out["msg"] != "roundtrip-ok" {
		t.Fatalf("roundtrip msg = %q", out["msg"])
	}
}

func TestServerCustomCodecsZstdBrotliAndReplacement(t *testing.T) {
	var zstdWriterResets, zstdReaderResets atomic.Int64

	s := server.New()
	payload := strings.Repeat("custom-codec-payload-data-", 60)
	s.GET("/data", func(c *server.Context) error {
		return c.Text(http.StatusOK, payload)
	})
	s.POST("/echo", func(c *server.Context) error {
		b, err := c.BodyBytes()
		if err != nil {
			return err
		}
		return c.Text(http.StatusOK, string(b))
	})

	// 1. Register Brotli (non-resettable simple codec) -> br now handled and preferred over gzip/deflate.
	s.SetCompressor(server.EncodingBrotli, server.CompressorFunc(func(dst io.Writer) (io.WriteCloser, error) {
		return &simpleWriteCloser{tag: "BR:", dst: dst}, nil
	}))
	s.SetDecompressor(server.EncodingBrotli, server.DecompressorFunc(func(src io.Reader) (io.ReadCloser, error) {
		return &simpleReadCloser{tag: "BR:", src: src}, nil
	}))

	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "br" || rec.Body.String() != "BR:"+payload {
		t.Fatalf("brotli response: ce=%q len=%d", rec.Header().Get("Content-Encoding"), rec.Body.Len())
	}

	req = httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader("BR:hello-brotli"))
	req.Header.Set("Content-Encoding", "br")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello-brotli" {
		t.Fatalf("brotli request: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// 2. Register Zstd (resettable pooled codec) -> zstd wins over br/gzip/deflate and pools instances.
	s.SetCompressor(server.EncodingZstd, server.CompressorFunc(func(dst io.Writer) (io.WriteCloser, error) {
		return &framedWriter{tag: "ZSTD:", dst: dst, resets: &zstdWriterResets}, nil
	}))
	s.SetDecompressor(server.EncodingZstd, server.DecompressorFunc(func(src io.Reader) (io.ReadCloser, error) {
		return &framedReader{tag: "ZSTD:", src: src, resets: &zstdReaderResets}, nil
	}))

	for range 3 {
		req = httptest.NewRequest(http.MethodGet, "/data", nil)
		req.Header.Set("Accept-Encoding", "br, zstd, gzip")
		rec = httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Header().Get("Content-Encoding") != "zstd" || rec.Body.String() != "ZSTD:"+payload {
			t.Fatalf("zstd response: ce=%q", rec.Header().Get("Content-Encoding"))
		}

		req = httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader("ZSTD:hello-zstd"))
		req.Header.Set("Content-Encoding", "zstd")
		rec = httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "hello-zstd" {
			t.Fatalf("zstd request: code=%d body=%q", rec.Code, rec.Body.String())
		}
	}
	if zstdWriterResets.Load() == 0 || zstdReaderResets.Load() == 0 {
		t.Fatalf("expected pooled reset reuse, got writerResets=%d readerResets=%d", zstdWriterResets.Load(), zstdReaderResets.Load())
	}

	// 3. Replace built-in gzip compressor & decompressor.
	s.SetCompressor(server.EncodingGzip, server.CompressorFunc(func(dst io.Writer) (io.WriteCloser, error) {
		return &simpleWriteCloser{tag: "CUSTOMGZ:", dst: dst}, nil
	}))
	s.SetDecompressor(server.EncodingGzip, server.DecompressorFunc(func(src io.Reader) (io.ReadCloser, error) {
		return &simpleReadCloser{tag: "CUSTOMGZ:", src: src}, nil
	}))

	req = httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Body.String() != "CUSTOMGZ:"+payload {
		t.Fatalf("replaced gzip response failed: ce=%q", rec.Header().Get("Content-Encoding"))
	}
	req = httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader("CUSTOMGZ:ok"))
	req.Header.Set("Content-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("replaced gzip request failed: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// 4. Custom encoding priority order & disabling via EncodingNone / SetCompression / SetDecompression.
	s.SetCompressionOrder(server.EncodingDeflate, server.EncodingGzip)
	req = httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Accept-Encoding", "zstd, br, gzip, deflate")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "deflate" {
		t.Fatalf("SetCompressionOrder: got %q, want deflate", rec.Header().Get("Content-Encoding"))
	}

	s.SetCompressionOrder(server.EncodingNone)
	req = httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("EncodingNone should disable compression, got %q", rec.Header().Get("Content-Encoding"))
	}
	s.SetCompressionOrder() // restore default order

	// Disable and re-enable compression & decompression.
	s.SetCompression(false)
	req = httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Accept-Encoding", "zstd")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("SetCompression(false) compressed unexpectedly: %q", rec.Header().Get("Content-Encoding"))
	}
	s.SetCompression(true)

	s.SetDecompression(false)
	req = httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader("ZSTD:raw"))
	req.Header.Set("Content-Encoding", "zstd")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Body.String() != "ZSTD:raw" {
		t.Fatalf("SetDecompression(false) should keep raw body, got %q", rec.Body.String())
	}
	s.SetDecompression(true)

	// Removing a single codec with nil disables only that codec.
	s.SetCompressor(server.EncodingZstd, nil)
	s.SetDecompressor(server.EncodingZstd, nil)
	req = httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Accept-Encoding", "zstd, br")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "br" {
		t.Fatalf("after removing zstd, expected br, got %q", rec.Header().Get("Content-Encoding"))
	}

	// Failing compressor falls back cleanly to uncompressed without corrupting headers.
	s.SetCompressor(server.EncodingZstd, server.CompressorFunc(func(dst io.Writer) (io.WriteCloser, error) {
		return nil, errors.New("compressor init error")
	}))
	req = httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Accept-Encoding", "zstd")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != payload {
		t.Fatalf("failing compressor did not fall back cleanly: code=%d ce=%q", rec.Code, rec.Header().Get("Content-Encoding"))
	}

	// Config-based setup with nil override & NewGzipCompressor/NewDeflateCompressor.
	cfg := server.DefaultConfig()
	cfg.Compression = server.CompressionConfig{
		MinLength: 32,
		Compressors: map[server.Encoding]server.Compressor{
			server.EncodingGzip:    nil, // disable gzip
			server.EncodingDeflate: server.NewDeflateCompressor(1),
		},
	}
	sCfg := server.NewWithConfig(cfg)
	sCfg.GET("/d", func(c *server.Context) error {
		return c.Text(http.StatusOK, strings.Repeat("config-deflate-", 5))
	})
	req = httptest.NewRequest(http.MethodGet, "/d", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	rec = httptest.NewRecorder()
	sCfg.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "deflate" {
		t.Fatalf("config nil gzip override failed: ce=%q", rec.Header().Get("Content-Encoding"))
	}

	// Invalid encoding / level panics.
	for _, fn := range []func(){
		func() { s.SetCompressor("lz4", nil) },
		func() { s.SetDecompressor("lz4", nil) },
		func() { server.NewGzipCompressor(99) },
		func() { server.NewDeflateCompressor(99) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			fn()
		}()
	}
}

func TestServerPerRequestDisableCompressionAndHijack(t *testing.T) {
	s := server.New()
	large := strings.Repeat("sensitive-secret-token-response-", 60)

	s.GET("/no-compress", func(c *server.Context) error {
		c.DisableCompression()
		if !c.CompressionDisabled() {
			return server.NewError(http.StatusInternalServerError, "CompressionDisabled should be true")
		}
		return c.Text(http.StatusOK, large)
	})
	s.GET("/no-compress-mid", func(c *server.Context) error {
		_, _ = c.ResponseWriter().Write([]byte("head-"))
		c.DisableCompression()
		_, _ = c.ResponseWriter().Write([]byte(large))
		return nil
	})
	s.GET("/normal", func(c *server.Context) error {
		if c.CompressionDisabled() {
			return server.NewError(http.StatusInternalServerError, "stale CompressionDisabled leaked")
		}
		return c.Text(http.StatusOK, large)
	})
	s.GET("/hijack-check", func(c *server.Context) error {
		rw := c.ResponseWriter()
		if _, ok := rw.(http.Hijacker); !ok {
			return server.NewError(http.StatusInternalServerError, "missing http.Hijacker")
		}
		if _, ok := rw.(http.Flusher); !ok {
			return server.NewError(http.StatusInternalServerError, "missing http.Flusher")
		}
		return c.NoContent()
	})

	// 1. DisableCompression prevents compression on this request.
	req := httptest.NewRequest(http.MethodGet, "/no-compress", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != large {
		t.Fatalf("/no-compress: code=%d ce=%q", rec.Code, rec.Header().Get("Content-Encoding"))
	}

	// 2. DisableCompression mid-buffer (before MinLength committed) flushes uncompressed.
	req = httptest.NewRequest(http.MethodGet, "/no-compress-mid", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != "head-"+large {
		t.Fatalf("/no-compress-mid: code=%d ce=%q len=%d", rec.Code, rec.Header().Get("Content-Encoding"), rec.Body.Len())
	}

	// 3. Subsequent pooled request still compresses normally.
	req = httptest.NewRequest(http.MethodGet, "/normal", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" || gunzipBody(t, rec.Body) != large {
		t.Fatalf("/normal: code=%d ce=%q", rec.Code, rec.Header().Get("Content-Encoding"))
	}

	// 4. http.Hijacker and http.Flusher interfaces preserved when Accept-Encoding is set.
	req = httptest.NewRequest(http.MethodGet, "/hijack-check", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("/hijack-check failed: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestServerPrecompressedFilesAndPayloads(t *testing.T) {
	plainJS := strings.Repeat("console.log('hello compression');\n", 80)
	gzFile := gzipBytes(t, []byte(plainJS))
	zstMock := append([]byte{0x28, 0xb5, 0x2f, 0xfd}, bytes.Repeat([]byte("z"), 1500)...)
	brMock := bytes.Repeat([]byte{0xa1, 0xb2, 0xc3, 0xd4}, 400)

	fsys := fstest.MapFS{
		"app.js":          {Data: []byte(plainJS)},
		"app.js.gz":       {Data: gzFile},
		"bundle.js.br":    {Data: brMock},
		"archive.tar.zst": {Data: zstMock},
	}

	s := server.New()
	s.StaticFS("/static", fsys)
	s.GET("/raw-zstd-magic", func(c *server.Context) error {
		return c.Bytes(http.StatusOK, "application/octet-stream", zstMock)
	})
	s.GET("/raw-woff2", func(c *server.Context) error {
		return c.Bytes(http.StatusOK, "font/woff2", bytes.Repeat([]byte("w"), 1500))
	})
	s.GET("/grpc", func(c *server.Context) error {
		return c.Bytes(http.StatusOK, "application/grpc+proto", bytes.Repeat([]byte("g"), 1500))
	})
	s.GET("/preencoded-zstd", func(c *server.Context) error {
		c.SetHeader("Content-Encoding", "zstd")
		return c.Bytes(http.StatusOK, "application/json", zstMock)
	})

	// Uncompressed JS file IS compressed.
	req := httptest.NewRequest(http.MethodGet, "/static/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" || gunzipBody(t, rec.Body) != plainJS {
		t.Fatalf("/static/app.js: code=%d ce=%q", rec.Code, rec.Header().Get("Content-Encoding"))
	}

	// Pre-compressed static files MUST NOT be compressed again.
	for _, tc := range []struct {
		path string
		want []byte
	}{
		{"/static/app.js.gz", gzFile},
		{"/static/bundle.js.br", brMock},
		{"/static/archive.tar.zst", zstMock},
		{"/raw-zstd-magic", zstMock},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" || !bytes.Equal(rec.Body.Bytes(), tc.want) {
			t.Fatalf("%s double-compressed or altered: code=%d ce=%q len=%d want=%d", tc.path, rec.Code, rec.Header().Get("Content-Encoding"), rec.Body.Len(), len(tc.want))
		}
	}

	// Incompressible types (woff2, grpc) and pre-set Content-Encoding bypass compression.
	for _, path := range []string{"/raw-woff2", "/grpc"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Header().Get("Content-Encoding") != "" {
			t.Fatalf("%s compressed unexpectedly: ce=%q", path, rec.Header().Get("Content-Encoding"))
		}
	}
	req = httptest.NewRequest(http.MethodGet, "/preencoded-zstd", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "zstd" || !bytes.Equal(rec.Body.Bytes(), zstMock) {
		t.Fatalf("/preencoded-zstd altered: ce=%q", rec.Header().Get("Content-Encoding"))
	}
}

func TestSecurityCompressionBombsAndConcurrency(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.MaxBodyBytes = 1024
	cfg.Compression.MaxDecompressedBytes = 1024
	s := server.NewWithConfig(cfg)

	var lastErr error
	var mu sync.Mutex
	setErr := func(err error) {
		mu.Lock()
		lastErr = err
		mu.Unlock()
	}

	s.POST("/upload", func(c *server.Context) error {
		b, err := c.BodyBytes()
		setErr(err)
		if err != nil {
			return err
		}
		return c.Text(http.StatusOK, fmt.Sprintf("%d", len(b)))
	})
	s.POST("/stream", func(c *server.Context) error {
		b, err := io.ReadAll(c.Request().Body)
		setErr(err)
		if err != nil {
			return err
		}
		return c.Text(http.StatusOK, fmt.Sprintf("%d", len(b)))
	})
	s.POST("/tight", server.BodyLimit(16), func(c *server.Context) error {
		b, err := c.BodyBytes()
		setErr(err)
		if err != nil {
			return err
		}
		return c.Text(http.StatusOK, string(b))
	})

	// 1. Gzip & deflate bombs expanding beyond MaxDecompressedBytes -> 413 + live connection close.
	bombPlain := bytes.Repeat([]byte("A"), 64*1024)
	gzBomb := gzipBytes(t, bombPlain)
	zlibBomb := zlibBytes(t, bombPlain)

	ts := httptest.NewServer(s)
	defer ts.Close()
	client := ts.Client()

	for _, tc := range []struct {
		name string
		path string
		ce   string
		body []byte
	}{
		{"gzip_bomb_bodybytes", "/upload", "gzip", gzBomb},
		{"gzip_bomb_stream", "/stream", "gzip", gzBomb},
		{"deflate_bomb_bodybytes", "/upload", "deflate", zlibBomb},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, ts.URL+tc.path, bytes.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Encoding", tc.ce)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413", resp.StatusCode)
			}
			if !resp.Close {
				t.Fatalf("expected Connection: close (resp.Close == true) after decompression bomb 413")
			}
			mu.Lock()
			errCopy := lastErr
			mu.Unlock()
			if !errors.Is(errCopy, server.ErrBodyTooLarge) {
				t.Fatalf("expected ErrBodyTooLarge, got %v", errCopy)
			}
			if tc.path == "/stream" {
				var maxErr *http.MaxBytesError
				if !errors.As(errCopy, &maxErr) {
					t.Fatalf("expected MaxBytesError on /stream, got %v", errCopy)
				}
			}
		})
	}

	// 2. Per-route BodyLimit(16) blocks small gzip expanding to 64 bytes -> 413.
	smallBomb := gzipBytes(t, bytes.Repeat([]byte("B"), 64))
	req := httptest.NewRequest(http.MethodPost, "/tight", bytes.NewReader(smallBomb))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("per-route BodyLimit compressed bomb: status=%d, want 413", rec.Code)
	}

	// 3. Too many stacked Content-Encoding layers (> 4) -> 400 Bad Request.
	req = httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("ignored"))
	req.Header.Set("Content-Encoding", "gzip, gzip, gzip, gzip, gzip")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("excessive Content-Encoding layers: status=%d, want 400", rec.Code)
	}

	// 4. Corrupt gzip/deflate request body -> 400 (not 500) and does not poison pool.
	for _, ce := range []string{"gzip", "deflate"} {
		req = httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("not-valid-compressed-bytes"))
		req.Header.Set("Content-Encoding", ce)
		rec = httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("corrupt %s body: status=%d, want 400", ce, rec.Code)
		}
		mu.Lock()
		errCopy := lastErr
		mu.Unlock()
		if !errors.Is(errCopy, server.ErrInvalidCompressedBody) {
			t.Fatalf("corrupt %s error = %v, want ErrInvalidCompressedBody", ce, errCopy)
		}
	}

	// Subsequent valid requests succeed cleanly after corrupt requests.
	for _, tc := range []struct {
		ce   string
		body []byte
	}{
		{"gzip", gzipBytes(t, []byte("ok-after-corrupt"))},
		{"deflate", zlibBytes(t, []byte("ok-after-corrupt"))},
	} {
		req = httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(tc.body))
		req.Header.Set("Content-Encoding", tc.ce)
		rec = httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != fmt.Sprintf("%d", len("ok-after-corrupt")) {
			t.Fatalf("post-corrupt %s failed: code=%d body=%q", tc.ce, rec.Code, rec.Body.String())
		}
	}

	// 5. Concurrent codec mutations and compressed traffic under -race.
	sRace := server.New()
	bigResp := strings.Repeat("race-safe-compression-", 80)
	sRace.POST("/race", func(c *server.Context) error {
		b, err := c.BodyBytes()
		if err != nil {
			return err
		}
		if len(b)%2 == 1 {
			c.DisableCompression()
		}
		return c.Text(http.StatusOK, string(b)+":"+bigResp)
	})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 20 {
			if i%2 == 0 {
				sRace.SetCompressor(server.EncodingZstd, server.CompressorFunc(func(dst io.Writer) (io.WriteCloser, error) {
					return &framedWriter{tag: "Z:", dst: dst}, nil
				}))
				sRace.SetDecompressor(server.EncodingZstd, server.DecompressorFunc(func(src io.Reader) (io.ReadCloser, error) {
					return &framedReader{tag: "Z:", src: src}, nil
				}))
			} else {
				sRace.SetCompressor(server.EncodingZstd, nil)
				sRace.SetDecompressor(server.EncodingZstd, nil)
			}
			sRace.SetCompressionOrder(server.EncodingZstd, server.EncodingGzip, server.EncodingDeflate)
		}
	}()
	for w := range 6 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 25 {
				in := fmt.Sprintf("w%d-%d", id, i)
				var req *http.Request
				if i%2 == 0 {
					req = httptest.NewRequest(http.MethodPost, "/race", bytes.NewReader(gzipBytes(t, []byte(in))))
					req.Header.Set("Content-Encoding", "gzip")
				} else {
					req = httptest.NewRequest(http.MethodPost, "/race", bytes.NewReader(zlibBytes(t, []byte(in))))
					req.Header.Set("Content-Encoding", "deflate")
				}
				req.Header.Set("Accept-Encoding", "gzip, deflate")
				rec := httptest.NewRecorder()
				sRace.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("race worker %d iter %d status=%d", id, i, rec.Code)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

func TestDecompressUnlimitedWithMaxBodyBytesErrorLimit(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.MaxBodyBytes = 64
	cfg.Compression.MaxDecompressedBytes = -1
	s := server.NewWithConfig(cfg)
	var capturedErr error
	s.POST("/stream", func(c *server.Context) error {
		_, err := io.ReadAll(c.Request().Body)
		capturedErr = err
		return err
	})

	// Incompressible payload whose gzip stream exceeds MaxBodyBytes + compressedInputHeadroom.
	big := make([]byte, 128*1024)
	x := uint32(0x12345678)
	for i := range big {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		big[i] = byte(x)
	}
	gz := gzipBytes(t, big)
	req := httptest.NewRequest(http.MethodPost, "/stream", bytes.NewReader(gz))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; err = %v", rec.Code, capturedErr)
	}
	var maxErr *http.MaxBytesError
	if !errors.As(capturedErr, &maxErr) {
		t.Fatalf("expected *http.MaxBytesError, got %v", capturedErr)
	}
	if maxErr.Limit != 64 {
		t.Fatalf("MaxBytesError.Limit = %d, want 64", maxErr.Limit)
	}
}

func TestStackedGzipDecompressionBombBypass(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.MaxBodyBytes = 1024
	cfg.Compression.MaxDecompressedBytes = 1024
	s := server.NewWithConfig(cfg)
	s.POST("/upload", func(c *server.Context) error {
		b, err := c.BodyBytes()
		if err != nil {
			return err
		}
		return c.Text(http.StatusOK, fmt.Sprintf("read=%d", len(b)))
	})

	var emptyMember bytes.Buffer
	gw := gzip.NewWriter(&emptyMember)
	_ = gw.Close()
	inner := bytes.Repeat(emptyMember.Bytes(), 500_000)

	var outer bytes.Buffer
	gw2 := gzip.NewWriter(&outer)
	_, _ = gw2.Write(inner)
	_ = gw2.Close()

	req := httptest.NewRequest(http.MethodPost, "/upload", &outer)
	req.Header.Set("Content-Encoding", "gzip, gzip")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got status %d (%q), want 413", rec.Code, rec.Body.String())
	}
}

func TestDisableCompressionWithCompressMiddleware(t *testing.T) {
	s := server.New()
	s.Use(server.Compress())
	s.GET("/secret", func(c *server.Context) error {
		c.DisableCompression()
		return c.Text(http.StatusOK, strings.Repeat("secret-token-", 200))
	})

	req := httptest.NewRequest(http.MethodGet, "/secret", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("Content-Encoding = %q despite DisableCompression()", ce)
	}
}

type failingWriteResetWriter struct {
	dst        io.Writer
	failWrites bool
	reused     *atomic.Bool
	writes     int
}

func (w *failingWriteResetWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.failWrites {
		return 0, errors.New("simulated write failure")
	}
	return w.dst.Write(p)
}

func (w *failingWriteResetWriter) Close() error {
	// Intentionally returns nil even after Write failed to verify compressResponseWriter
	// does not rely solely on Close() error when deciding whether to pool.
	return nil
}

func (w *failingWriteResetWriter) Reset(dst io.Writer) {
	w.dst = dst
	if w.reused != nil {
		w.reused.Store(true)
	}
	w.failWrites = false
}

func TestDecompressorConstructorsAndWriteErrorPoolIsolation(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.Compression.Decompressors = map[server.Encoding]server.Decompressor{
		server.EncodingGzip:    server.NewGzipDecompressor(),
		server.EncodingDeflate: server.NewDeflateDecompressor(),
	}
	s := server.NewWithConfig(cfg)

	var created atomic.Int64
	var reusedAfterErr atomic.Bool
	s.SetCompressor(server.EncodingZstd, server.CompressorFunc(func(dst io.Writer) (io.WriteCloser, error) {
		n := created.Add(1)
		var reusedFlag *atomic.Bool
		if n == 1 {
			reusedFlag = &reusedAfterErr
		}
		return &failingWriteResetWriter{
			dst:        dst,
			failWrites: n == 1, // First writer fails on Write but returns nil on Close
			reused:     reusedFlag,
		}, nil
	}))

	s.POST("/echo", func(c *server.Context) error {
		b, err := c.BodyBytes()
		if err != nil {
			return err
		}
		return c.Text(http.StatusOK, string(b))
	})
	s.GET("/z", func(c *server.Context) error {
		return c.Text(http.StatusOK, strings.Repeat("compressible-zstd-data-", 80))
	})

	// Test explicit NewGzipDecompressor & NewDeflateDecompressor.
	for _, tc := range []struct {
		ce   string
		body []byte
	}{
		{"gzip", gzipBytes(t, []byte("gz-ctor"))},
		{"deflate", zlibBytes(t, []byte("def-ctor"))},
	} {
		req := httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(tc.body))
		req.Header.Set("Content-Encoding", tc.ce)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: code=%d body=%q", tc.ce, rec.Code, rec.Body.String())
		}
	}

	// First /z write fails inside encWriter.Write (Close returns nil).
	req := httptest.NewRequest(http.MethodGet, "/z", nil)
	req.Header.Set("Accept-Encoding", "zstd")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	// Second /z must create a fresh writer rather than reusing the errored one.
	req = httptest.NewRequest(http.MethodGet, "/z", nil)
	req.Header.Set("Accept-Encoding", "zstd")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || reusedAfterErr.Load() || created.Load() < 2 {
		t.Fatalf("errored compressor was unexpectedly pooled: created=%d code=%d", created.Load(), rec.Code)
	}
}
