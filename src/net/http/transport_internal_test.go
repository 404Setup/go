// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// White-box tests for transport.go (in package http instead of http_test).

package http

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http/httptrace"
	"net/http/internal"
	"net/http/internal/http2"
	"net/http/internal/testcert"
	"net/url"
	"strings"
	"testing"
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
