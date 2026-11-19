// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newH2TestServer(t *testing.T, h http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	ts := httptest.NewUnstartedServer(h)
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts, ts.Client()
}

func TestHTTP2UndeclaredTrailersHandlerDone(t *testing.T) {
	ts, client := newH2TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Normal", "ok")
		// Set an undeclared trailer using TrailerPrefix on a small response
		// that completes synchronously when the handler returns (handlerDone=true).
		w.Header().Set(http.TrailerPrefix+"X-Late", "trailer-val")
		io.WriteString(w, "short")
	}))

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.ProtoMajor != 2 {
		t.Fatalf("Proto = %q, want HTTP/2", resp.Proto)
	}
	if got := resp.Header.Get("X-Normal"); got != "ok" {
		t.Errorf("X-Normal = %q, want ok", got)
	}
	if got := resp.Header.Get("X-Late"); got != "" {
		t.Errorf("undeclared trailer leaked into response Header: X-Late=%q", got)
	}
	if got := resp.Header.Get(http.TrailerPrefix + "X-Late"); got != "" {
		t.Errorf("TrailerPrefix key leaked into response Header: %q", got)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "short" {
		t.Errorf("body = %q, want short", body)
	}
	if got := resp.Trailer.Get("X-Late"); got != "trailer-val" {
		t.Errorf("Trailer X-Late = %q, want trailer-val", got)
	}
}

func TestHTTP2LargeHeadersAsyncAndContinuation(t *testing.T) {
	cases := []struct {
		name      string
		numHdrs   int
		valLength int
	}{
		{"SmallSyncCoalesced", 4, 16},
		{"MediumOverflowBufferedWriter", 20, 300}, // > 4KB bufferedWriter, <= 16KB single HEADERS
		{"LargeContinuation", 40, 600},            // > 16KB requiring CONTINUATION frames
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val := strings.Repeat("v", tc.valLength)
			ts, client := newH2TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for i := 0; i < tc.numHdrs; i++ {
					w.Header().Set(fmt.Sprintf("X-Hdr-%02d", i), fmt.Sprintf("%02d-%s", i, val))
				}
				io.WriteString(w, "payload-"+tc.name)
			}))

			resp, err := client.Get(ts.URL)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(body) != "payload-"+tc.name {
				t.Fatalf("body = %q, want %q", body, "payload-"+tc.name)
			}
			for i := 0; i < tc.numHdrs; i++ {
				k := fmt.Sprintf("X-Hdr-%02d", i)
				want := fmt.Sprintf("%02d-%s", i, val)
				if got := resp.Header.Get(k); got != want {
					t.Fatalf("header %s mismatch (len %d vs %d)", k, len(got), len(want))
				}
			}
		})
	}
}

func TestHTTP2InlineEndStreamAndContextCancel(t *testing.T) {
	ts, client := newH2TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		case "/small":
			io.WriteString(w, "hello")
		case "/echo":
			b, _ := io.ReadAll(r.Body)
			w.Write(b)
		}
	}))

	// Repeated requests on the same HTTP/2 connection with both non-cancelable
	// and cancelable contexts exercise frame reuse, inline endStream cleanup,
	// and stopCtxWatch ordering.
	for i := 0; i < 20; i++ {
		var ctx context.Context
		var cancel context.CancelFunc
		if i%2 == 0 {
			ctx = context.Background()
		} else {
			ctx, cancel = context.WithCancel(context.Background())
		}

		path := "/small"
		if i%3 == 0 {
			path = "/empty"
		}
		req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("iter %d (%s): %v", i, path, err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if cancel != nil {
			cancel()
		}
		if err != nil {
			t.Fatalf("iter %d ReadAll: %v", i, err)
		}
		if path == "/small" && string(b) != "hello" {
			t.Fatalf("iter %d body = %q, want hello", i, b)
		}
		if path == "/empty" && (resp.StatusCode != http.StatusNoContent || len(b) != 0) {
			t.Fatalf("iter %d status=%d body=%q", i, resp.StatusCode, b)
		}
	}

	// POST with body to exercise server pipeWithDataBuffer and client request body cleanup.
	for i := 0; i < 5; i++ {
		payload := strings.Repeat("data", 1024+i*100)
		resp, err := client.Post(ts.URL+"/echo", "text/plain", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("POST %d: %v", i, err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("POST %d ReadAll: %v", i, err)
		}
		if string(got) != payload {
			t.Fatalf("POST %d echo mismatch: len %d vs %d", i, len(got), len(payload))
		}
	}
}

func TestHTTP2AllInvalidTrailersTerminatesStream(t *testing.T) {
	ts, client := newH2TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Declare only an invalid trailer header name (contains space) or empty value
		// so encodeHeaderBlock produces a 0-byte trailer header block with endStream=true.
		w.Header().Set("Trailer", "X-Bad")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "hello")
		// Set an invalid trailer value (contains newline) so encodeHeaders skips it.
		w.Header()["X-Bad"] = []string{"bad\r\nvalue"}
	}))

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q, want hello", body)
	}
}
