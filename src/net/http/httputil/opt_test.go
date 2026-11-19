// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httputil

import (
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCopyHeaderSlabIsolationAndDuplicateCanonicalKeys(t *testing.T) {
	t.Run("SliceIsolation", func(t *testing.T) {
		src := http.Header{
			"X-One":   {"1"},
			"X-Two":   {"2a", "2b"},
			"X-Empty": {},
		}
		dst := make(http.Header, len(src))
		copyHeader(dst, src)

		if _, ok := dst["X-Empty"]; ok {
			t.Errorf("X-Empty should not be copied into dst")
		}
		// Appending to X-One must not overwrite X-Two in the shared slab.
		dst["X-One"] = append(dst["X-One"], "1-mutated")
		if !reflect.DeepEqual(dst["X-Two"], []string{"2a", "2b"}) {
			t.Fatalf("appending to X-One corrupted X-Two: %#v", dst["X-Two"])
		}
		if !reflect.DeepEqual(src["X-One"], []string{"1"}) {
			t.Fatalf("appending to dst corrupted src: %#v", src["X-One"])
		}
	})

	t.Run("DuplicateCanonicalKeys", func(t *testing.T) {
		src := http.Header{
			"x-dup": {"a"},
			"X-Dup": {"b"},
			"X-Adj": {"keep"},
		}
		dst := make(http.Header)
		copyHeader(dst, src)

		got := append([]string(nil), dst["X-Dup"]...)
		slices.Sort(got)
		if !reflect.DeepEqual(got, []string{"a", "b"}) {
			t.Fatalf("X-Dup = %#v, want [a b]", dst["X-Dup"])
		}
		if !reflect.DeepEqual(dst["X-Adj"], []string{"keep"}) {
			t.Fatalf("X-Adj corrupted: %#v", dst["X-Adj"])
		}
	})

	t.Run("NonEmptyDst", func(t *testing.T) {
		dst := http.Header{"X-One": {"0"}}
		src := http.Header{"x-one": {"1"}, "X-Two": {"2"}}
		copyHeader(dst, src)
		if !reflect.DeepEqual(dst["X-One"], []string{"0", "1"}) {
			t.Fatalf("X-One = %#v, want [0 1]", dst["X-One"])
		}
		if !reflect.DeepEqual(dst["X-Two"], []string{"2"}) {
			t.Fatalf("X-Two = %#v, want [2]", dst["X-Two"])
		}
	})
}

type flushErrorWriter struct {
	header      http.Header
	body        []byte
	flushErrCnt int
	flusherCnt  int
}

func (w *flushErrorWriter) Header() http.Header { return w.header }
func (w *flushErrorWriter) WriteHeader(int)     {}
func (w *flushErrorWriter) Write(p []byte) (int, error) {
	w.body = append(w.body, p...)
	return len(p), nil
}
func (w *flushErrorWriter) Flush()            { w.flusherCnt++ }
func (w *flushErrorWriter) FlushError() error { w.flushErrCnt++; return nil }

type wrappedFlusherWriter struct {
	http.ResponseWriter
}

func (w *wrappedFlusherWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type nonFlusherWriter struct {
	header http.Header
	body   []byte
}

func (w *nonFlusherWriter) Header() http.Header { return w.header }
func (w *nonFlusherWriter) WriteHeader(int)     {}
func (w *nonFlusherWriter) Write(p []byte) (int, error) {
	w.body = append(w.body, p...)
	return len(p), nil
}

type chunkedBodyReader struct {
	chunks [][]byte
	idx    int
}

func (r *chunkedBodyReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.idx])
	r.idx++
	return n, nil
}
func (r *chunkedBodyReader) Close() error { return nil }

func TestReverseProxyStreamingFlushIntervalNegative(t *testing.T) {
	t.Run("FlushErrorPrecedenceAndUnwrap", func(t *testing.T) {
		inner := &flushErrorWriter{header: make(http.Header)}
		wrapped := &wrappedFlusherWriter{ResponseWriter: inner}

		rp := &ReverseProxy{
			FlushInterval: -1,
			Rewrite:       func(pr *ProxyRequest) { pr.Out.URL.Scheme = "http"; pr.Out.URL.Host = "backend" },
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": {"text/plain"}},
					Body:       &chunkedBodyReader{chunks: [][]byte{[]byte("one"), []byte("two")}},
				}, nil
			}),
		}

		req, _ := http.NewRequest("GET", "http://proxy.test/stream", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		rp.ServeHTTP(wrapped, req)

		if string(inner.body) != "onetwo" {
			t.Fatalf("body = %q, want onetwo", inner.body)
		}
		if inner.flusherCnt != 0 {
			t.Fatalf("Flush() called %d times, want 0 (FlushError must take precedence)", inner.flusherCnt)
		}
		// 1 initial flush + 2 chunk flushes = 3.
		if inner.flushErrCnt < 3 {
			t.Fatalf("FlushError() called %d times, want >= 3", inner.flushErrCnt)
		}
	})

	t.Run("NonFlusherWriter", func(t *testing.T) {
		w := &nonFlusherWriter{header: make(http.Header)}
		rp := &ReverseProxy{
			FlushInterval: -1,
			Rewrite:       func(pr *ProxyRequest) { pr.Out.URL.Scheme = "http"; pr.Out.URL.Host = "backend" },
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: 200,
					Header:     make(http.Header),
					Body:       &chunkedBodyReader{chunks: [][]byte{[]byte("abc")}},
				}, nil
			}),
		}
		req, _ := http.NewRequest("GET", "http://proxy.test/", nil)
		rp.ServeHTTP(w, req)
		if string(w.body) != "abc" {
			t.Fatalf("body = %q, want abc", w.body)
		}
	})

	t.Run("ImmediateHeaderFlushBeforeBody", func(t *testing.T) {
		flushed := make(chan struct{}, 1)
		release := make(chan struct{})
		w := &signalFlushWriter{
			header: make(http.Header),
			onFlush: func() {
				select {
				case flushed <- struct{}{}:
				default:
				}
			},
		}
		pr, pw := io.Pipe()
		rp := &ReverseProxy{
			FlushInterval: -1,
			Rewrite:       func(pr *ProxyRequest) { pr.Out.URL.Scheme = "http"; pr.Out.URL.Host = "backend" },
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body:       pr,
				}, nil
			}),
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			req, _ := http.NewRequest("GET", "http://proxy.test/events", nil)
			rp.ServeHTTP(w, req)
		}()

		select {
		case <-flushed:
		case <-time.After(2 * time.Second):
			pw.Close()
			t.Fatal("timed out waiting for immediate initial flush before body arrived")
		}
		close(release)
		_, _ = pw.Write([]byte("data: hi\n\n"))
		_ = pw.Close()
		<-done
		if string(w.body) != "data: hi\n\n" {
			t.Fatalf("body = %q, want SSE payload", w.body)
		}
	})
}

type signalFlushWriter struct {
	header  http.Header
	body    []byte
	onFlush func()
}

func (w *signalFlushWriter) Header() http.Header { return w.header }
func (w *signalFlushWriter) WriteHeader(int)     {}
func (w *signalFlushWriter) Write(p []byte) (int, error) {
	w.body = append(w.body, p...)
	return len(p), nil
}
func (w *signalFlushWriter) Flush() { w.onFlush() }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReverseProxyXFFAndUserAgentSliceIsolation(t *testing.T) {
	var seenReq *http.Request
	rp := &ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "backend"
		},
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			seenReq = req
			return &http.Response{
				StatusCode: 200,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}),
	}

	req, _ := http.NewRequest("GET", "http://proxy.test/", nil)
	req.RemoteAddr = "198.51.100.7:5555"
	// No User-Agent header triggers co-allocated X-Forwarded-For + empty User-Agent.
	rp.ServeHTTP(&nonFlusherWriter{header: make(http.Header)}, req)

	if seenReq == nil {
		t.Fatal("RoundTrip not called")
	}
	xff := seenReq.Header["X-Forwarded-For"]
	ua := seenReq.Header["User-Agent"]
	if len(xff) != 1 || xff[0] != "198.51.100.7" || cap(xff) != 1 {
		t.Fatalf("X-Forwarded-For = %#v (cap=%d), want [198.51.100.7] cap=1", xff, cap(xff))
	}
	if len(ua) != 1 || ua[0] != "" || cap(ua) != 1 {
		t.Fatalf("User-Agent = %#v (cap=%d), want [\"\"] cap=1", ua, cap(ua))
	}
	// Appending to X-Forwarded-For must NOT overwrite User-Agent.
	seenReq.Header["X-Forwarded-For"] = append(xff, "mutated")
	if seenReq.Header["User-Agent"][0] != "" {
		t.Fatalf("appending to X-Forwarded-For corrupted User-Agent: %#v", seenReq.Header["User-Agent"])
	}
}

func BenchmarkReverseProxyStreaming(b *testing.B) {
	chunk := []byte("data: event payload\n\n")
	rp := &ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "backend"
		},
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       &chunkedBodyReader{chunks: [][]byte{chunk, chunk, chunk}},
			}, nil
		}),
	}
	req, _ := http.NewRequest("GET", "http://proxy.test/stream", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	w := &flushErrorWriter{header: make(http.Header)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clear(w.header)
		w.body = w.body[:0]
		rp.ServeHTTP(w, req)
	}
}
