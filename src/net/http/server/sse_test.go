// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/server"
	"strings"
	"testing"
	"time"
)

func TestContextSSEStreamingAndSecurity(t *testing.T) {
	s := server.New()
	s.Use(server.CompressWithConfig(server.CompressConfig{MinLength: 16}))

	s.GET("/events", func(c *server.Context) error {
		sse, err := c.SSE()
		if err != nil {
			return err
		}
		if !c.CompressionDisabled() {
			return server.NewError(http.StatusInternalServerError, "SSE did not disable compression")
		}
		if err := sse.Comment("keep-alive\nline2"); err != nil {
			return err
		}
		if err := sse.Send(server.SSEEvent{
			ID:    "1",
			Event: "update",
			Retry: 2500 * time.Millisecond,
			Data:  "line1\r\nevent: fake\nline3\rline4",
		}); err != nil {
			return err
		}
		if err := sse.SendData("plain"); err != nil {
			return err
		}
		if err := sse.SendJSON("msg", map[string]any{"ok": true, "n": 42}); err != nil {
			return err
		}
		if err := sse.Send(server.SSEEvent{Event: "ping"}); err != nil {
			return err
		}
		return sse.Send(server.SSEEvent{ID: "last"})
	})

	s.GET("/written", func(c *server.Context) error {
		_ = c.Text(http.StatusOK, "already")
		if _, err := c.SSE(); err == nil {
			t.Error("expected SSE() error after response written")
		}
		return nil
	})

	s.GET("/inject", func(c *server.Context) error {
		sse, err := c.SSE()
		if err != nil {
			return err
		}
		if err := sse.Send(server.SSEEvent{ID: "1\nevent: evil", Data: "x"}); err == nil {
			t.Error("expected error for CR/LF in SSE ID")
		}
		if err := sse.Send(server.SSEEvent{Event: "ok\r\ndata: evil", Data: "x"}); err == nil {
			t.Error("expected error for CR/LF in SSE Event")
		}
		if err := sse.SendJSON("bad\x00evt", map[string]int{"a": 1}); err == nil {
			t.Error("expected error for NUL in SSE SendJSON event")
		}
		if err := sse.Comment("bad\x00comment"); err == nil {
			t.Error("expected error for NUL in SSE Comment")
		}
		return sse.SendData("safe")
	})

	// Live HTTP server test for streaming + headers + no compression.
	ts := httptest.NewServer(s)
	defer ts.Close()
	client := ts.Client()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", cc)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("SSE must not be compressed, got Content-Encoding=%q", ce)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing X-Content-Type-Options: nosniff")
	}

	want := "" +
		": keep-alive\n: line2\n\n" +
		"id: 1\nevent: update\nretry: 2500\ndata: line1\ndata: event: fake\ndata: line3\ndata: line4\n\n" +
		"data: plain\n\n" +
		"event: msg\ndata: {\"n\":42,\"ok\":true}\n\n" +
		"event: ping\ndata:\n\n" +
		"id: last\n\n"
	if string(bodyBytes) != want {
		t.Fatalf("SSE body mismatch:\ngot:\n%q\nwant:\n%q", string(bodyBytes), want)
	}

	// HEAD on SSE returns headers with empty body.
	headResp, err := client.Head(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	headBody, _ := io.ReadAll(headResp.Body)
	headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK || len(headBody) != 0 || headResp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("HEAD /events: status=%d ct=%q body=%q", headResp.StatusCode, headResp.Header.Get("Content-Type"), headBody)
	}

	// Already written response rejects SSE().
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/written", nil))
	if rec.Body.String() != "already" {
		t.Fatalf("/written body = %q", rec.Body.String())
	}

	// Injection attempts fail cleanly without writing injected fields.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/inject", nil))
	if rec.Body.String() != "data: safe\n\n" || strings.Contains(rec.Body.String(), "evil") {
		t.Fatalf("/inject body = %q", rec.Body.String())
	}

	// Nil SSEWriter methods return errors cleanly without panicking.
	var nilSSE *server.SSEWriter
	for _, err := range []error{
		nilSSE.Send(server.SSEEvent{Data: "x"}),
		nilSSE.SendData("x"),
		nilSSE.SendJSON("ev", map[string]int{"a": 1}),
		nilSSE.Comment("hb"),
		nilSSE.Flush(),
	} {
		if err == nil {
			t.Fatal("expected error from nil *SSEWriter")
		}
	}
}
