// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http_test

import (
	"bufio"
	"context"
	"io"
	"log"
	"net"
	. "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestResponseHeaderNotAliasedAcrossPooledRequests verifies that retaining
// w.Header() past ServeHTTP return (for example, in an async logger) does not
// race with or observe headers from subsequent requests on the same or another
// keep-alive connection.
func TestResponseHeaderNotAliasedAcrossPooledRequests(t *testing.T) {
	var (
		mu       sync.Mutex
		retained []Header
	)
	ts := httptest.NewServer(HandlerFunc(func(w ResponseWriter, r *Request) {
		token := r.URL.Query().Get("token")
		hdr := w.Header()
		hdr.Set("X-Secret", token)
		hdr.Set("Set-Cookie", "sid="+token)
		mu.Lock()
		retained = append(retained, hdr)
		mu.Unlock()
		w.WriteHeader(StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer ts.Close()

	client := ts.Client()
	const n = 32
	for i := range n {
		tok := "tok-" + string(rune('A'+(i%26))) + "-" + strings.Repeat("x", i%5)
		res, err := client.Get(ts.URL + "/?token=" + tok)
		if err != nil {
			t.Fatalf("req %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		mu.Lock()
		for j, h := range retained {
			if got := h.Get("X-Secret"); got == "" {
				mu.Unlock()
				t.Fatalf("retained header %d was cleared by later request %d", j, i)
			}
		}
		mu.Unlock()
	}

	mu.Lock()
	defer mu.Unlock()
	if len(retained) != n {
		t.Fatalf("retained %d headers, want %d", len(retained), n)
	}
	for i := range retained {
		want := "tok-" + string(rune('A'+(i%26))) + "-" + strings.Repeat("x", i%5)
		if got := retained[i].Get("X-Secret"); got != want {
			t.Fatalf("retained[%d][X-Secret] = %q, want %q (map aliased across requests)", i, got, want)
		}
		if got := retained[i].Get("Set-Cookie"); got != "sid="+want {
			t.Fatalf("retained[%d][Set-Cookie] = %q, want %q", i, got, "sid="+want)
		}
	}
}

// TestRequestWithContextAndCloneNoRaceWithLazyContext exercises concurrent
// Request.WithContext, Request.Clone, StripPrefix, and Request.Context calls
// against an incoming HTTP/1.1 server request whose context is materialized
// lazily and canceled when ServeHTTP returns.
func TestRequestWithContextAndCloneNoRaceWithLazyContext(t *testing.T) {
	type ctxKey struct{}
	var wg sync.WaitGroup
	inner := HandlerFunc(func(w ResponseWriter, r *Request) {
		const workers = 6
		start := make(chan struct{})
		for i := range workers {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				<-start
				switch id % 3 {
				case 0:
					_ = r.Context()
				case 1:
					r2 := r.WithContext(context.WithValue(context.Background(), ctxKey{}, id))
					if got := r2.Context().Value(ctxKey{}); got != id {
						t.Errorf("WithContext value = %v, want %d", got, id)
					}
				case 2:
					r2 := r.Clone(context.WithValue(context.Background(), ctxKey{}, id))
					if got := r2.Context().Value(ctxKey{}); got != id {
						t.Errorf("Clone value = %v, want %d", got, id)
					}
				}
			}(i)
		}
		close(start)
		// Deliberately return without waiting for all workers so cancelServerCtx
		// also races with WithContext/Clone/Context.
		w.WriteHeader(StatusOK)
	})

	ts := httptest.NewServer(HandlerFunc(func(w ResponseWriter, r *Request) {
		if strings.HasPrefix(r.URL.Path, "/strip/") {
			StripPrefix("/strip", inner).ServeHTTP(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer ts.Close()

	client := ts.Client()
	for i := range 20 {
		path := "/race"
		if i%2 == 1 {
			path = "/strip/race"
		}
		res, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("req %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	wg.Wait()
}

// TestLazyServerRequestContextPreservesLocalAddrAfterCancel verifies that
// calling r.Context() after ServeHTTP returns still exposes LocalAddrContextKey.
func TestLazyServerRequestContextPreservesLocalAddrAfterCancel(t *testing.T) {
	var (
		mu          sync.Mutex
		capturedReq *Request
	)
	done := make(chan struct{}, 1)
	ts := httptest.NewUnstartedServer(HandlerFunc(func(w ResponseWriter, r *Request) {
		mu.Lock()
		capturedReq = r
		mu.Unlock()
		w.WriteHeader(StatusOK)
	}))
	ts.Config.ConnState = func(c net.Conn, s ConnState) {
		if s == StateIdle {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	}
	ts.Start()
	defer ts.Close()

	res, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for connection to become idle")
	}

	mu.Lock()
	r := capturedReq
	mu.Unlock()
	ctx := r.Context()
	if ctx.Err() != context.Canceled {
		t.Fatalf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
	if got := ctx.Value(LocalAddrContextKey); got == nil {
		t.Fatal("LocalAddrContextKey missing from post-cancel lazy server request context")
	}
}

// TestConnStateDeadlineClearedOnNextKeepAliveRequest verifies that a read/write
// deadline set directly on net.Conn inside Server.ConnState is cleared before
// the next keep-alive request rather than elided.
func TestConnStateDeadlineClearedOnNextKeepAliveRequest(t *testing.T) {
	ts := httptest.NewUnstartedServer(HandlerFunc(func(w ResponseWriter, r *Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(60 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	var reqCount int
	var mu sync.Mutex
	ts.Config.ConnState = func(c net.Conn, state ConnState) {
		if state == StateActive {
			mu.Lock()
			reqCount++
			n := reqCount
			mu.Unlock()
			if n == 1 {
				// Set a tight deadline on the first request directly on the
				// raw conn; after this request completes, the server must clear
				// it so the next keep-alive request is not prematurely aborted.
				_ = c.SetWriteDeadline(time.Now().Add(30 * time.Millisecond))
			}
		}
	}
	ts.Start()
	defer ts.Close()

	client := ts.Client()
	res1, err := client.Get(ts.URL + "/fast")
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	_, _ = io.Copy(io.Discard, res1.Body)
	res1.Body.Close()

	res2, err := client.Get(ts.URL + "/slow")
	if err != nil {
		t.Fatalf("second keep-alive request failed due to stale ConnState write deadline: %v", err)
	}
	_, _ = io.Copy(io.Discard, res2.Body)
	res2.Body.Close()
}

// TestResponseControllerDeadlineRace verifies that concurrent
// ResponseController.SetReadDeadline/SetWriteDeadline calls do not race with
// keep-alive deadline clears.
func TestResponseControllerDeadlineRace(t *testing.T) {
	ts := httptest.NewServer(HandlerFunc(func(w ResponseWriter, r *Request) {
		rc := NewResponseController(w)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = rc.SetReadDeadline(time.Now().Add(time.Second))
		}()
		go func() {
			defer wg.Done()
			_ = rc.SetWriteDeadline(time.Now().Add(time.Second))
		}()
		wg.Wait()
		_, _ = io.WriteString(w, "ok")
	}))
	defer ts.Close()

	client := ts.Client()
	for i := range 10 {
		res, err := client.Get(ts.URL + "/")
		if err != nil {
			t.Fatalf("req %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
}

// TestInvalidContentLengthNotEmittedOnHead verifies that setting an invalid
// Content-Length header before WriteHeader on a HEAD request strips it before
// snapshotting headers and does not emit it on the wire.
func TestInvalidContentLengthNotEmittedOnHead(t *testing.T) {
	ts := httptest.NewUnstartedServer(HandlerFunc(func(w ResponseWriter, r *Request) {
		w.Header().Set("Content-Length", "not-a-number")
		w.WriteHeader(StatusOK)
	}))
	ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	ts.Start()
	defer ts.Close()

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "HEAD / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(bufio.NewReader(conn))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "content-length: not-a-number") {
		t.Fatalf("invalid Content-Length leaked onto wire:\n%s", raw)
	}
}
