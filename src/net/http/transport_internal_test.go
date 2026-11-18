// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// White-box tests for transport.go (in package http instead of http_test).

package http

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptrace"
	"net/http/internal"
	"net/http/internal/http2"
	"net/http/internal/testcert"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Issue 15446: incorrect wrapping of errors when server closes an idle connection.
func TestTransportPersistConnReadLoopEOF(t *testing.T) {
	ln := newLocalListener(t)
	defer ln.Close()

	connc := make(chan net.Conn, 1)
	go func() {
		defer close(connc)
		c, err := ln.Accept()
		if err != nil {
			t.Error(err)
			return
		}
		connc <- c
	}()

	tr := new(Transport)
	req, _ := NewRequest("GET", "http://"+ln.Addr().String(), nil)
	req = req.WithT(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	treq := &transportRequest{Request: req, ctx: ctx, cancel: cancel}
	cm := connectMethod{targetScheme: "http", targetAddr: ln.Addr().String()}
	pconn, _, err := tr.getConn(ctx, cm, false)
	if err != nil {
		t.Fatal(err)
	}
	pc := pconn.cc.(*persistConn)
	defer pc.CloseWithError(errors.New("test over"))

	conn := <-connc
	if conn == nil {
		// Already called t.Error in the accept goroutine.
		return
	}
	conn.Close() // simulate the server hanging up on the client

	_, err = pc.roundTrip(treq)
	if !isNothingWrittenError(err) && !isTransportReadFromServerError(err) && err != errServerClosedIdle {
		t.Errorf("roundTrip = %#v, %v; want errServerClosedIdle, transportReadFromServerError, or nothingWrittenError", err, err)
	}

	<-pc.closech
	err = pc.closed
	if !isNothingWrittenError(err) && !isTransportReadFromServerError(err) && err != errServerClosedIdle {
		t.Errorf("pc.closed = %#v, %v; want errServerClosedIdle or transportReadFromServerError, or nothingWrittenError", err, err)
	}
}

func isNothingWrittenError(err error) bool {
	_, ok := err.(nothingWrittenError)
	return ok
}

func isTransportReadFromServerError(err error) bool {
	_, ok := err.(transportReadFromServerError)
	return ok
}

func newLocalListener(t *testing.T) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		ln, err = net.Listen("tcp6", "[::1]:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func dummyRequest(method string) *Request {
	req, err := NewRequest(method, "http://fake.tld/", nil)
	if err != nil {
		panic(err)
	}
	return req
}
func dummyRequestWithBody(method string) *Request {
	req, err := NewRequest(method, "http://fake.tld/", strings.NewReader("foo"))
	if err != nil {
		panic(err)
	}
	return req
}

func dummyRequestWithBodyNoGetBody(method string) *Request {
	req := dummyRequestWithBody(method)
	req.GetBody = nil
	return req
}

// issue22091Error acts like a golang.org/x/net/http2.ErrNoCachedConn.
type issue22091Error struct{}

func (issue22091Error) IsHTTP2NoCachedConnError() {}
func (issue22091Error) Error() string             { return "issue22091Error" }

func TestTransportShouldRetryRequest(t *testing.T) {
	tests := []struct {
		reused bool
		req    *Request

		err         error
		wantRetry   bool
		wantBackoff bool
	}{
		0: {
			reused:    false,
			req:       dummyRequest("POST"),
			err:       nothingWrittenError{},
			wantRetry: false,
		},
		1: {
			reused:    true,
			req:       dummyRequest("POST"),
			err:       nothingWrittenError{},
			wantRetry: true,
		},
		2: {
			reused:    true,
			req:       dummyRequest("POST"),
			err:       http2.ErrNoCachedConn,
			wantRetry: true,
		},
		3: {
			req:       nil,
			err:       issue22091Error{}, // like an external http2ErrNoCachedConn
			wantRetry: true,
		},
		4: {
			reused:    true,
			req:       dummyRequest("POST"),
			err:       errMissingHost,
			wantRetry: false,
		},
		5: {
			reused:    true,
			req:       dummyRequest("POST"),
			err:       transportReadFromServerError{},
			wantRetry: false,
		},
		6: {
			reused:    true,
			req:       dummyRequest("GET"),
			err:       transportReadFromServerError{},
			wantRetry: true,
		},
		7: {
			reused:    true,
			req:       dummyRequest("GET"),
			err:       errServerClosedIdle,
			wantRetry: true,
		},
		8: {
			reused:    true,
			req:       dummyRequestWithBody("POST"),
			err:       nothingWrittenError{},
			wantRetry: true,
		},
		9: {
			reused:    true,
			req:       dummyRequestWithBodyNoGetBody("POST"),
			err:       nothingWrittenError{},
			wantRetry: false,
		},
		10: {
			reused:      false,
			req:         dummyRequest("POST"),
			err:         internal.RetryableError{Backoff: false},
			wantRetry:   true,
			wantBackoff: false,
		},
		11: {
			reused:      false,
			req:         dummyRequest("POST"),
			err:         internal.RetryableError{Backoff: true},
			wantRetry:   true,
			wantBackoff: true,
		},
	}
	for i, tt := range tests {
		gotRetry, gotBackoff := shouldRetryRequest(0, tt.reused, tt.req, tt.err)
		if gotRetry != tt.wantRetry || gotBackoff != tt.wantBackoff {
			t.Errorf("%d. shouldRetryRequest = retry:%v, backoff:%v; want %v, %v", i, gotRetry, tt.wantRetry, gotBackoff, tt.wantBackoff)
		}
	}
}

type roundTripFunc func(r *Request) (*Response, error)

func (f roundTripFunc) RoundTrip(r *Request) (*Response, error) {
	return f(r)
}

// Issue 25009
func TestTransportBodyAltRewind(t *testing.T) {
	cert, err := tls.X509KeyPair(testcert.LocalhostCert, testcert.LocalhostKey)
	if err != nil {
		t.Fatal(err)
	}
	ln := newLocalListener(t)
	defer ln.Close()

	go func() {
		tln := tls.NewListener(ln, &tls.Config{
			NextProtos:   []string{"foo"},
			Certificates: []tls.Certificate{cert},
		})
		for i := 0; i < 2; i++ {
			sc, err := tln.Accept()
			if err != nil {
				t.Error(err)
				return
			}
			if err := sc.(*tls.Conn).Handshake(); err != nil {
				t.Error(err)
				return
			}
			sc.Close()
		}
	}()

	addr := ln.Addr().String()
	req, _ := NewRequest("POST", "https://example.org/", bytes.NewBufferString("request"))
	roundTripped := false
	tr := &Transport{
		DisableKeepAlives: true,
		TLSNextProto: map[string]func(string, *tls.Conn) RoundTripper{
			"foo": func(authority string, c *tls.Conn) RoundTripper {
				return roundTripFunc(func(r *Request) (*Response, error) {
					n, _ := io.Copy(io.Discard, r.Body)
					if n == 0 {
						t.Error("body length is zero")
					}
					if roundTripped {
						return &Response{
							Body:       NoBody,
							StatusCode: 200,
						}, nil
					}
					roundTripped = true
					return nil, http2.ErrNoCachedConn
				})
			},
		},
		DialTLS: func(_, _ string) (net.Conn, error) {
			tc, err := tls.Dial("tcp", addr, &tls.Config{
				InsecureSkipVerify: true,
				NextProtos:         []string{"foo"},
			})
			if err != nil {
				return nil, err
			}
			if err := tc.Handshake(); err != nil {
				return nil, err
			}
			return tc, nil
		},
	}
	c := &Client{Transport: tr}
	_, err = c.Do(req)
	if err != nil {
		t.Error(err)
	}
}

func TestCanonicalAddr(t *testing.T) {
	ref := func(u *url.URL) string {
		port := u.Port()
		if port == "" {
			port = schemePort(u.Scheme)
		}
		return net.JoinHostPort(idnaASCIIFromURL(u), port)
	}

	cases := []struct {
		scheme string
		host   string
		want   string
	}{
		{"http", "127.0.0.1:8080", "127.0.0.1:8080"},
		{"https", "example.com:8443", "example.com:8443"},
		{"http", "[::1]:8080", "[::1]:8080"},
		{"http", "[fe80::1%25eth0]:8080", "[fe80::1%25eth0]:8080"},
		{"http", "[::1]", "[::1]:80"},
		{"https", "example.com", "example.com:443"},
		{"socks5", "proxy.example", "proxy.example:1080"},
		{"ftp", "example.com", "example.com:"},
		{"http", "[127.0.0.1]:8080", "127.0.0.1:8080"},
		{"http", "a:b:8080", "[a:b]:8080"},
		{"http", "[[::1]]:8080", "[[::1]]:8080"},
		{"http", "[foo]:8080", "foo:8080"},
		{"http", "example.com:", "example.com:80"},
		{"http", "example.com:abc", "[example.com:abc]:80"},
		{"http", "b\u00fccher.example:8080", "xn--bcher-kva.example:8080"},
		{"http", ":8080", ":8080"},
	}

	for _, tc := range cases {
		u := &url.URL{Scheme: tc.scheme, Host: tc.host}
		got := canonicalAddr(u)
		wantRef := ref(u)
		if got != wantRef || got != tc.want {
			t.Errorf("canonicalAddr(%q://%q) = %q, ref=%q, want=%q", tc.scheme, tc.host, got, wantRef, tc.want)
		}
	}

	uIPv4 := &url.URL{Scheme: "http", Host: "127.0.0.1:8080"}
	if allocs := testing.AllocsPerRun(50, func() {
		if got := canonicalAddr(uIPv4); got != "127.0.0.1:8080" {
			t.Fatalf("unexpected %q", got)
		}
	}); allocs != 0 {
		t.Errorf("canonicalAddr(%q) allocs = %v, want 0", uIPv4.Host, allocs)
	}

	uIPv6 := &url.URL{Scheme: "http", Host: "[::1]:8080"}
	if allocs := testing.AllocsPerRun(50, func() {
		if got := canonicalAddr(uIPv6); got != "[::1]:8080" {
			t.Fatalf("unexpected %q", got)
		}
	}); allocs != 0 {
		t.Errorf("canonicalAddr(%q) allocs = %v, want 0", uIPv6.Host, allocs)
	}
}

func TestRequestWriteAddedGzipAndClose(t *testing.T) {
	var traced []string
	trace := &httptrace.ClientTrace{
		WroteHeaderField: func(key string, value []string) {
			traced = append(traced, key+"="+strings.Join(value, ","))
		},
	}
	ctx := httptrace.WithClientTrace(context.Background(), trace)
	req, err := NewRequestWithContext(ctx, "GET", "http://example.com/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := req.write(&buf, false, nil, true, true, nil); err != nil {
		t.Fatal(err)
	}
	wire := buf.String()
	if !strings.Contains(wire, "Accept-Encoding: gzip\r\n") {
		t.Errorf("missing Accept-Encoding: gzip in %q", wire)
	}
	if !strings.Contains(wire, "Connection: close\r\n") {
		t.Errorf("missing Connection: close in %q", wire)
	}
	var gotGzipTrace, gotCloseTrace bool
	for _, entry := range traced {
		if entry == "Accept-Encoding=gzip" {
			gotGzipTrace = true
		}
		if entry == "Connection=close" {
			gotCloseTrace = true
		}
	}
	if !gotGzipTrace || !gotCloseTrace {
		t.Errorf("missing WroteHeaderField traces: got %v", traced)
	}
}

type fakeBodyReadCloser struct {
	data   string
	off    int
	err    error
	closed bool
}

func (f *fakeBodyReadCloser) Read(p []byte) (int, error) {
	if f.off < len(f.data) {
		n := copy(p, f.data[f.off:])
		f.off += n
		if f.off == len(f.data) {
			if f.err != nil {
				return n, f.err
			}
			return n, io.EOF
		}
		return n, nil
	}
	if f.err != nil {
		return 0, f.err
	}
	return 0, io.EOF
}

func (f *fakeBodyReadCloser) Close() error {
	f.closed = true
	return nil
}

func TestBodyEOFSignalDirect(t *testing.T) {
	t.Run("EOFAndClose", func(t *testing.T) {
		waitCh := make(chan bodyReadStatus, 1)
		eofc := make(chan struct{})
		close(eofc)
		inner := &fakeBodyReadCloser{data: "ok"}
		es := &bodyEOFSignal{
			body:            inner,
			waitForBodyRead: waitCh,
			eofc:            eofc,
		}
		buf := make([]byte, 8)
		n, err := es.Read(buf)
		if n != 2 || err != io.EOF {
			t.Fatalf("Read = (%d, %v), want (2, EOF)", n, err)
		}
		if st := <-waitCh; st != bodyReadEOF {
			t.Fatalf("status = %v, want bodyReadEOF", st)
		}
		if n2, err2 := es.Read(buf); n2 != 0 || err2 != io.EOF {
			t.Fatalf("second Read = (%d, %v), want (0, EOF)", n2, err2)
		}
		if err := es.Close(); err != nil {
			t.Fatalf("Close err = %v", err)
		}
		if !inner.closed {
			t.Fatalf("inner body not closed")
		}
		if n3, err3 := es.Read(buf); n3 != 0 || err3 != errReadOnClosedResBody {
			t.Fatalf("Read after close = (%d, %v), want errReadOnClosedResBody", n3, err3)
		}
	})

	t.Run("EarlyClose", func(t *testing.T) {
		waitCh := make(chan bodyReadStatus, 1)
		eofc := make(chan struct{})
		close(eofc)
		inner := &fakeBodyReadCloser{data: "abcdef"}
		es := &bodyEOFSignal{
			body:            inner,
			waitForBodyRead: waitCh,
			eofc:            eofc,
		}
		if err := es.Close(); err != nil {
			t.Fatalf("early Close err = %v", err)
		}
		if st := <-waitCh; st != bodyReadClosedEarly {
			t.Fatalf("status = %v, want bodyReadClosedEarly", st)
		}
		if err := es.Close(); err != nil {
			t.Fatalf("duplicate Close err = %v", err)
		}
		if n, err := es.Read(make([]byte, 4)); n != 0 || err != errReadOnClosedResBody {
			t.Fatalf("Read after early close = (%d, %v)", n, err)
		}
	})

	t.Run("ReadErrorWithCanceledPC", func(t *testing.T) {
		waitCh := make(chan bodyReadStatus, 1)
		eofc := make(chan struct{})
		pc := &persistConn{
			canceledErr: errRequestCanceled,
			closech:     make(chan struct{}),
		}
		readErr := errors.New("boom")
		inner := &fakeBodyReadCloser{err: readErr}
		es := &bodyEOFSignal{
			body:            inner,
			pc:              pc,
			waitForBodyRead: waitCh,
			eofc:            eofc,
		}
		_, err := es.Read(make([]byte, 4))
		if err != errRequestCanceled {
			t.Fatalf("Read err = %v, want %v", err, errRequestCanceled)
		}
		if st := <-waitCh; st != bodyReadError {
			t.Fatalf("status = %v, want bodyReadError", st)
		}
		if err := es.Close(); err != nil {
			t.Fatalf("Close after error = %v, want nil", err)
		}
		if inner.closed {
			t.Fatalf("inner body unexpectedly closed after non-EOF Read error")
		}
	})
}

func TestBodyInlineEOFSignal(t *testing.T) {
	t.Run("DirectNormalEOF", func(t *testing.T) {
		waitCh := make(chan bodyReadStatus, 1)
		eofc := make(chan struct{})
		close(eofc)

		hitEOF := false
		b := &body{
			src: &io.LimitedReader{R: strings.NewReader("ok"), N: 2},
			onHitEOF: eofListenerFunc(func() {
				hitEOF = true
			}),
		}
		b.attachEOFSignal(nil, waitCh, eofc)

		buf := make([]byte, 8)
		n, err := b.Read(buf)
		if n != 2 || string(buf[:n]) != "ok" || err != io.EOF {
			t.Fatalf("first Read = (%d, %q, %v), want (2, ok, EOF)", n, buf[:n], err)
		}
		if !hitEOF {
			t.Fatal("onHitEOF was not invoked")
		}
		if st := <-waitCh; st != bodyReadEOF {
			t.Fatalf("status = %v, want bodyReadEOF", st)
		}
		if n2, err2 := b.Read(buf); n2 != 0 || err2 != io.EOF {
			t.Fatalf("second Read = (%d, %v), want (0, EOF)", n2, err2)
		}
		if err := b.Close(); err != nil {
			t.Fatalf("Close = %v", err)
		}
		if !b.closed {
			t.Fatal("b.closed = false after Close")
		}
		if n3, err3 := b.Read(buf); n3 != 0 || err3 != errReadOnClosedResBody {
			t.Fatalf("Read after Close = (%d, %v), want errReadOnClosedResBody", n3, err3)
		}
	})

	t.Run("DirectEarlyCloseAndDrainReader", func(t *testing.T) {
		waitCh := make(chan bodyReadStatus, 2)
		eofc := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			if st := <-waitCh; st != bodyReadClosedEarly {
				t.Errorf("status = %v, want bodyReadClosedEarly", st)
			}
			eofc <- struct{}{}
		}()

		resp := &Response{}
		b := &body{
			src:          &io.LimitedReader{R: strings.NewReader("hello-rest"), N: 10},
			hdr:          resp,
			r:            bufio.NewReader(strings.NewReader("X-Tr: ignore\r\n\r\n")),
			doEarlyClose: true,
		}
		b.attachEOFSignal(nil, waitCh, eofc)

		var first [5]byte
		if n, err := b.Read(first[:]); n != 5 || err != nil || string(first[:]) != "hello" {
			t.Fatalf("partial Read = (%d, %q, %v)", n, first[:], err)
		}
		if err := b.Close(); err != nil {
			t.Fatalf("early Close = %v", err)
		}
		<-done

		if err := b.Close(); err != nil {
			t.Fatalf("duplicate Close = %v", err)
		}
		select {
		case extra := <-waitCh:
			t.Fatalf("unexpected second waitForBodyRead signal %v", extra)
		default:
		}
		if n, err := b.Read(first[:]); n != 0 || err != errReadOnClosedResBody {
			t.Fatalf("Read after early Close = (%d, %v), want errReadOnClosedResBody", n, err)
		}

		dr := bodyDrainReader{b}
		dr.b.discardTrailer()
		rest, err := io.ReadAll(dr)
		if err != nil {
			t.Fatalf("drain err = %v", err)
		}
		if string(rest) != "-rest" {
			t.Fatalf("drained = %q, want -rest", rest)
		}
		if resp.Trailer.Get("X-Tr") != "" {
			t.Fatalf("dropTrailer failed, got X-Tr=%q", resp.Trailer.Get("X-Tr"))
		}
	})

	t.Run("DirectErrorWithCanceledPC", func(t *testing.T) {
		waitCh := make(chan bodyReadStatus, 2)
		eofc := make(chan struct{})
		pc := &persistConn{
			canceledErr: errRequestCanceled,
			closech:     make(chan struct{}),
		}
		boom := errors.New("boom")
		b := &body{
			src: &fakeBodyReadCloser{data: "x", err: boom},
		}
		b.attachEOFSignal(pc, waitCh, eofc)

		_, err := b.Read(make([]byte, 4))
		if err != errRequestCanceled {
			t.Fatalf("Read err = %v, want %v", err, errRequestCanceled)
		}
		if st := <-waitCh; st != bodyReadError {
			t.Fatalf("status = %v, want bodyReadError", st)
		}
		if err := b.Close(); err != nil {
			t.Fatalf("Close after Read error = %v", err)
		}
		if b.closed {
			t.Fatal("b.closed unexpectedly true after non-EOF Read error + Close")
		}
		if n, err := b.Read(make([]byte, 4)); n != 0 || err != errReadOnClosedResBody {
			t.Fatalf("Read after Close = (%d, %v), want errReadOnClosedResBody", n, err)
		}
	})

	t.Run("LoopbackTrickleReuseAndDiscard", func(t *testing.T) {
		t.Run("ReuseAfterDrain", func(t *testing.T) {
			ln := newLocalListener(t)
			defer ln.Close()

			var accepts atomic.Int32
			srvErr := make(chan error, 1)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					srvErr <- err
					return
				}
				accepts.Add(1)
				defer c.Close()
				br := bufio.NewReader(c)

				// Request 1: trickle chunked body + trailer, small enough to drain.
				if _, err := ReadRequest(br); err != nil {
					srvErr <- err
					return
				}
				if _, err := io.WriteString(c, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nTrailer: X-Tr\r\n\r\n5\r\nhello\r\n"); err != nil {
					srvErr <- err
					return
				}
				time.Sleep(5 * time.Millisecond)
				if _, err := io.WriteString(c, "6\r\n world\r\n0\r\nX-Tr: ignore\r\n\r\n"); err != nil {
					srvErr <- err
					return
				}

				// Request 2 must reuse the same connection.
				if _, err := ReadRequest(br); err != nil {
					srvErr <- err
					return
				}
				if _, err := io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"); err != nil {
					srvErr <- err
					return
				}
				srvErr <- nil
			}()

			tr := &Transport{DisableKeepAlives: false, MaxIdleConnsPerHost: 1}
			defer tr.CloseIdleConnections()

			var reused1 atomic.Bool
			ctx1 := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
				GotConn: func(info httptrace.GotConnInfo) { reused1.Store(info.Reused) },
			})
			req1, _ := NewRequestWithContext(ctx1, MethodGet, "http://"+ln.Addr().String()+"/1", nil)
			res1, err := tr.RoundTrip(req1)
			if err != nil {
				t.Fatalf("RoundTrip 1: %v", err)
			}
			var head [5]byte
			if _, err := io.ReadFull(res1.Body, head[:]); err != nil {
				t.Fatalf("ReadFull 1: %v", err)
			}
			if string(head[:]) != "hello" {
				t.Fatalf("head = %q, want hello", head[:])
			}
			// Early close; readLoop drains remaining body via bodyDrainReader.
			if err := res1.Body.Close(); err != nil {
				t.Fatalf("Close 1: %v", err)
			}

			// Poll until readLoop finishes draining into the idle LRU.
			hasIdleConn := func() bool {
				tr.pool.mu.Lock()
				defer tr.pool.mu.Unlock()
				return tr.pool.lru.size > 0
			}
			deadline := time.Now().Add(2 * time.Second)
			for !hasIdleConn() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}

			var reused2 atomic.Bool
			ctx2 := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
				GotConn: func(info httptrace.GotConnInfo) { reused2.Store(info.Reused) },
			})
			req2, _ := NewRequestWithContext(ctx2, MethodGet, "http://"+ln.Addr().String()+"/2", nil)
			res2, err := tr.RoundTrip(req2)
			if err != nil {
				t.Fatalf("RoundTrip 2: %v", err)
			}
			got2, err := io.ReadAll(res2.Body)
			res2.Body.Close()
			if err != nil {
				t.Fatalf("ReadAll 2: %v", err)
			}
			if string(got2) != "ok" {
				t.Fatalf("body 2 = %q, want ok", got2)
			}
			if err := <-srvErr; err != nil {
				t.Fatalf("server err: %v", err)
			}
			if reused1.Load() {
				t.Error("request 1 unexpectedly reused a connection")
			}
			if !reused2.Load() || accepts.Load() != 1 {
				t.Errorf("request 2 did not reuse drained connection (reused=%v, accepts=%d)", reused2.Load(), accepts.Load())
			}
			if res1.Trailer.Get("X-Tr") != "" {
				t.Errorf("early-closed res1 Trailer unexpectedly populated: %q", res1.Trailer.Get("X-Tr"))
			}
		})

		t.Run("DiscardWhenOverLimit", func(t *testing.T) {
			ln := newLocalListener(t)
			defer ln.Close()

			var accepts atomic.Int32
			srvErr := make(chan error, 1)
			go func() {
				c1, err := ln.Accept()
				if err != nil {
					srvErr <- err
					return
				}
				accepts.Add(1)
				br1 := bufio.NewReader(c1)
				if _, err := ReadRequest(br1); err != nil {
					c1.Close()
					srvErr <- err
					return
				}
				hdr := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\nhello", maxPostCloseReadBytes+1024)
				if _, err := io.WriteString(c1, hdr); err != nil {
					c1.Close()
					srvErr <- err
					return
				}
				// Client should discard c1 instead of draining > maxPostCloseReadBytes.
				c2, err := ln.Accept()
				c1.Close()
				if err != nil {
					srvErr <- err
					return
				}
				accepts.Add(1)
				defer c2.Close()
				br2 := bufio.NewReader(c2)
				if _, err := ReadRequest(br2); err != nil {
					srvErr <- err
					return
				}
				if _, err := io.WriteString(c2, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"); err != nil {
					srvErr <- err
					return
				}
				srvErr <- nil
			}()

			tr := &Transport{DisableKeepAlives: false}
			defer tr.CloseIdleConnections()

			req1, _ := NewRequest(MethodGet, "http://"+ln.Addr().String()+"/big", nil)
			res1, err := tr.RoundTrip(req1)
			if err != nil {
				t.Fatalf("RoundTrip 1: %v", err)
			}
			var head [5]byte
			if _, err := io.ReadFull(res1.Body, head[:]); err != nil {
				t.Fatalf("ReadFull 1: %v", err)
			}
			res1.Body.Close()

			var reused2 atomic.Bool
			ctx2 := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
				GotConn: func(info httptrace.GotConnInfo) { reused2.Store(info.Reused) },
			})
			req2, _ := NewRequestWithContext(ctx2, MethodGet, "http://"+ln.Addr().String()+"/next", nil)
			res2, err := tr.RoundTrip(req2)
			if err != nil {
				t.Fatalf("RoundTrip 2: %v", err)
			}
			got2, err := io.ReadAll(res2.Body)
			res2.Body.Close()
			if err != nil || string(got2) != "ok" {
				t.Fatalf("req 2 = (%q, %v), want ok", got2, err)
			}
			if err := <-srvErr; err != nil {
				t.Fatalf("server err: %v", err)
			}
			if reused2.Load() || accepts.Load() != 2 {
				t.Errorf("expected fresh connection for over-limit body (reused=%v, accepts=%d)", reused2.Load(), accepts.Load())
			}
		})
	})

	t.Run("ConcurrentReadCloseRace", func(t *testing.T) {
		for iter := 0; iter < 50; iter++ {
			waitCh := make(chan bodyReadStatus, 1)
			eofc := make(chan struct{})
			b := &body{
				src: &io.LimitedReader{R: strings.NewReader("abcdefghijklmnopqrstuvwxyz"), N: 26},
			}
			b.attachEOFSignal(nil, waitCh, eofc)

			loopDone := make(chan struct{})
			go func() {
				defer close(loopDone)
				st := <-waitCh
				eofc <- struct{}{}
				if st == bodyReadClosedEarly {
					io.Copy(io.Discard, bodyDrainReader{b})
				}
			}()

			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				var buf [4]byte
				for {
					_, err := b.Read(buf[:])
					if err != nil {
						return
					}
				}
			}()
			go func() {
				defer wg.Done()
				_ = b.Close()
			}()
			wg.Wait()
			<-loopDone
		}
	})
}

type eofListenerFunc func()

func (f eofListenerFunc) onBodyHitEOF() { f() }

var (
	benchCanonHostSink string
	benchCanonPortSink string
	benchCanonJoinSink string
	benchCMKeySink     connectMethodKey
)

func TestCanonicalHostPortAndConnectMethodKey(t *testing.T) {
	cases := []struct {
		scheme, host string
		wantAddr     string
	}{
		{"http", "example.com", "example.com:80"},
		{"https", "example.com", "example.com:443"},
		{"http", "[::1]", "[::1]:80"},
		{"https", "[::1]", "[::1]:443"},
		{"http", "127.0.0.1", "127.0.0.1:80"},
		{"http", "127.0.0.1:8080", "127.0.0.1:8080"},
		{"https", "example.com:8443", "example.com:8443"},
		{"http", "[::1]:8080", "[::1]:8080"},
		{"http", "[fe80::1%25eth0]:8080", "[fe80::1%25eth0]:8080"},
		{"socks5", "proxy.example", "proxy.example:1080"},
		{"ftp", "example.com", "example.com:"},
		{"http", "[127.0.0.1]:8080", "127.0.0.1:8080"},
		{"http", "a:b:8080", "[a:b]:8080"},
		{"http", "[[::1]]:8080", "[[::1]]:8080"},
		{"http", "[foo]:8080", "foo:8080"},
		{"http", "example.com:", "example.com:80"},
		{"http", "example.com:abc", "[example.com:abc]:80"},
		{"http", "b\u00fccher.example", "xn--bcher-kva.example:80"},
		{"http", "b\u00fccher.example:8080", "xn--bcher-kva.example:8080"},
		{"http", ":8080", ":8080"},
	}

	for _, tc := range cases {
		u := &url.URL{Scheme: tc.scheme, Host: tc.host}
		h, p, joined := canonicalHostPort(u)
		gotAddr := canonicalAddr(u)
		if gotAddr != tc.wantAddr {
			t.Errorf("canonicalAddr(%q://%q) = %q, want %q", tc.scheme, tc.host, gotAddr, tc.wantAddr)
		}
		if joined != "" && joined != tc.wantAddr {
			t.Errorf("canonicalHostPort(%q://%q) joined = %q, want %q", tc.scheme, tc.host, joined, tc.wantAddr)
		}
		if h+":"+p != tc.wantAddr {
			t.Errorf("canonicalHostPort(%q://%q) = (%q, %q), h:p = %q, want %q", tc.scheme, tc.host, h, p, h+":"+p, tc.wantAddr)
		}
	}

	// connectMethodKey equality between split targetHost/targetPort and joined targetAddr.
	cmSplit := connectMethod{targetScheme: "http", targetHost: "example.com", targetPort: "80"}
	cmJoined := connectMethod{targetScheme: "http", targetAddr: "example.com:80"}
	if cmSplit.key() != cmJoined.key() {
		t.Fatalf("key mismatch: split=%+v vs joined=%+v", cmSplit.key(), cmJoined.key())
	}
	if cmSplit.dialAddr() != "example.com:80" || cmJoined.dialAddr() != "example.com:80" {
		t.Fatalf("dialAddr mismatch: split=%q joined=%q", cmSplit.dialAddr(), cmJoined.dialAddr())
	}
	if cmSplit.addr() != "example.com:80" {
		t.Fatalf("addr() = %q, want example.com:80", cmSplit.addr())
	}

	cmV6Split := connectMethod{targetScheme: "https", targetHost: "[::1]", targetPort: "443", onlyH1: true}
	cmV6Joined := connectMethod{targetScheme: "https", targetAddr: "[::1]:443", onlyH1: true}
	if cmV6Split.key() != cmV6Joined.key() {
		t.Fatalf("IPv6 key mismatch: split=%+v vs joined=%+v", cmV6Split.key(), cmV6Joined.key())
	}
	if got := cmV6Split.key().String(); got != "|https,h1|[::1]:443" {
		t.Fatalf("IPv6 key.String() = %q, want |https,h1|[::1]:443", got)
	}

	proxyURL, _ := url.Parse("http://proxy.internal:8080")
	cmProxyA := connectMethod{proxyURL: proxyURL, targetScheme: "http", targetHost: "a.example", targetPort: "80"}
	cmProxyB := connectMethod{proxyURL: proxyURL, targetScheme: "http", targetAddr: "b.example:9090"}
	if cmProxyA.key() != cmProxyB.key() {
		t.Fatalf("HTTP-over-proxy key mismatch: %+v vs %+v", cmProxyA.key(), cmProxyB.key())
	}
	if cmProxyA.addr() != "proxy.internal:8080" {
		t.Fatalf("proxy addr() = %q, want proxy.internal:8080", cmProxyA.addr())
	}

	// Bracketed IPv6 without port in targetAddr preserves host cleanly.
	cmV6NoPort := connectMethod{targetScheme: "http", targetAddr: "[::1]"}
	if k := cmV6NoPort.key(); k.host != "[::1]" || k.port != "" {
		t.Fatalf("targetAddr=[::1] key = %+v, want host=[::1] port=\"\"", k)
	}

	// Allocation guards on common ASCII targets.
	uNoPort := &url.URL{Scheme: "http", Host: "example.com"}
	uWithPort := &url.URL{Scheme: "http", Host: "127.0.0.1:8080"}
	for _, tc := range []struct {
		name string
		u    *url.URL
	}{
		{"NoPort", uNoPort},
		{"WithPort", uWithPort},
	} {
		t.Run("Allocs/"+tc.name, func(t *testing.T) {
			if allocs := testing.AllocsPerRun(50, func() {
				benchCanonHostSink, benchCanonPortSink, benchCanonJoinSink = canonicalHostPort(tc.u)
			}); allocs != 0 {
				t.Errorf("canonicalHostPort(%q) allocs = %v, want 0", tc.u, allocs)
			}
			cm := connectMethod{targetScheme: tc.u.Scheme}
			cm.targetHost, cm.targetPort, cm.targetAddr = canonicalHostPort(tc.u)
			if allocs := testing.AllocsPerRun(50, func() {
				benchCMKeySink = cm.key()
			}); allocs != 0 {
				t.Errorf("cm.key(%q) allocs = %v, want 0", tc.u, allocs)
			}
		})
	}
	if allocs := testing.AllocsPerRun(50, func() {
		benchCanonJoinSink = canonicalAddr(uWithPort)
	}); allocs != 0 {
		t.Errorf("canonicalAddr(%q) allocs = %v, want 0", uWithPort, allocs)
	}
}

func BenchmarkConnectMethodKey(b *testing.B) {
	b.Run("HostNoPort", func(b *testing.B) {
		u := &url.URL{Scheme: "http", Host: "example.com"}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			cm := connectMethod{targetScheme: u.Scheme}
			cm.targetHost, cm.targetPort, cm.targetAddr = canonicalHostPort(u)
			benchCMKeySink = cm.key()
		}
	})
	b.Run("HostWithPort", func(b *testing.B) {
		u := &url.URL{Scheme: "http", Host: "127.0.0.1:8080"}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			cm := connectMethod{targetScheme: u.Scheme}
			cm.targetHost, cm.targetPort, cm.targetAddr = canonicalHostPort(u)
			benchCMKeySink = cm.key()
		}
	})
	b.Run("IPv6WithPort", func(b *testing.B) {
		u := &url.URL{Scheme: "https", Host: "[::1]:8443"}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			cm := connectMethod{targetScheme: u.Scheme}
			cm.targetHost, cm.targetPort, cm.targetAddr = canonicalHostPort(u)
			benchCMKeySink = cm.key()
		}
	})
}

