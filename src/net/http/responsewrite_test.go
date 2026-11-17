// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bytes"
	"io"
	"net/http/httptrace"
	"slices"
	"strings"
	"testing"
)

type respWriteTest struct {
	Resp Response
	Raw  string
}

func TestResponseWrite(t *testing.T) {
	respWriteTests := []respWriteTest{
		// HTTP/1.0, identity coding; no trailer
		{
			Response{
				StatusCode:    503,
				ProtoMajor:    1,
				ProtoMinor:    0,
				Request:       dummyReq("GET"),
				Header:        Header{},
				Body:          io.NopCloser(strings.NewReader("abcdef")),
				ContentLength: 6,
			},

			"HTTP/1.0 503 Service Unavailable\r\n" +
				"Content-Length: 6\r\n\r\n" +
				"abcdef",
		},
		// Unchunked response without Content-Length.
		{
			Response{
				StatusCode:    200,
				ProtoMajor:    1,
				ProtoMinor:    0,
				Request:       dummyReq("GET"),
				Header:        Header{},
				Body:          io.NopCloser(strings.NewReader("abcdef")),
				ContentLength: -1,
			},
			"HTTP/1.0 200 OK\r\n" +
				"\r\n" +
				"abcdef",
		},
		// HTTP/1.1 response with unknown length and Connection: close
		{
			Response{
				StatusCode:    200,
				ProtoMajor:    1,
				ProtoMinor:    1,
				Request:       dummyReq("GET"),
				Header:        Header{},
				Body:          io.NopCloser(strings.NewReader("abcdef")),
				ContentLength: -1,
				Close:         true,
			},
			"HTTP/1.1 200 OK\r\n" +
				"Connection: close\r\n" +
				"\r\n" +
				"abcdef",
		},
		// HTTP/1.1 response with unknown length and not setting connection: close
		{
			Response{
				StatusCode:    200,
				ProtoMajor:    1,
				ProtoMinor:    1,
				Request:       dummyReq11("GET"),
				Header:        Header{},
				Body:          io.NopCloser(strings.NewReader("abcdef")),
				ContentLength: -1,
				Close:         false,
			},
			"HTTP/1.1 200 OK\r\n" +
				"Connection: close\r\n" +
				"\r\n" +
				"abcdef",
		},
		// HTTP/1.1 response with unknown length and not setting connection: close, but
		// setting chunked.
		{
			Response{
				StatusCode:       200,
				ProtoMajor:       1,
				ProtoMinor:       1,
				Request:          dummyReq11("GET"),
				Header:           Header{},
				Body:             io.NopCloser(strings.NewReader("abcdef")),
				ContentLength:    -1,
				TransferEncoding: []string{"chunked"},
				Close:            false,
			},
			"HTTP/1.1 200 OK\r\n" +
				"Transfer-Encoding: chunked\r\n\r\n" +
				"6\r\nabcdef\r\n0\r\n\r\n",
		},
		// HTTP/1.1 response 0 content-length, and nil body
		{
			Response{
				StatusCode:    200,
				ProtoMajor:    1,
				ProtoMinor:    1,
				Request:       dummyReq11("GET"),
				Header:        Header{},
				Body:          nil,
				ContentLength: 0,
				Close:         false,
			},
			"HTTP/1.1 200 OK\r\n" +
				"Content-Length: 0\r\n" +
				"\r\n",
		},
		// HTTP/1.1 response 0 content-length, and non-nil empty body
		{
			Response{
				StatusCode:    200,
				ProtoMajor:    1,
				ProtoMinor:    1,
				Request:       dummyReq11("GET"),
				Header:        Header{},
				Body:          io.NopCloser(strings.NewReader("")),
				ContentLength: 0,
				Close:         false,
			},
			"HTTP/1.1 200 OK\r\n" +
				"Content-Length: 0\r\n" +
				"\r\n",
		},
		// HTTP/1.1 response 0 content-length, and non-nil non-empty body
		{
			Response{
				StatusCode:    200,
				ProtoMajor:    1,
				ProtoMinor:    1,
				Request:       dummyReq11("GET"),
				Header:        Header{},
				Body:          io.NopCloser(strings.NewReader("foo")),
				ContentLength: 0,
				Close:         false,
			},
			"HTTP/1.1 200 OK\r\n" +
				"Connection: close\r\n" +
				"\r\nfoo",
		},
		// HTTP/1.1, chunked coding; empty trailer; close
		{
			Response{
				StatusCode:       200,
				ProtoMajor:       1,
				ProtoMinor:       1,
				Request:          dummyReq("GET"),
				Header:           Header{},
				Body:             io.NopCloser(strings.NewReader("abcdef")),
				ContentLength:    6,
				TransferEncoding: []string{"chunked"},
				Close:            true,
			},

			"HTTP/1.1 200 OK\r\n" +
				"Connection: close\r\n" +
				"Transfer-Encoding: chunked\r\n\r\n" +
				"6\r\nabcdef\r\n0\r\n\r\n",
		},

		// Header value with a newline character (Issue 914).
		// Also tests removal of leading and trailing whitespace.
		{
			Response{
				StatusCode: 204,
				ProtoMajor: 1,
				ProtoMinor: 1,
				Request:    dummyReq("GET"),
				Header: Header{
					"Foo": []string{" Bar\nBaz "},
				},
				Body:             nil,
				ContentLength:    0,
				TransferEncoding: []string{"chunked"},
				Close:            true,
			},

			"HTTP/1.1 204 No Content\r\n" +
				"Connection: close\r\n" +
				"Foo: Bar Baz\r\n" +
				"\r\n",
		},

		// Want a single Content-Length header. Fixing issue 8180 where
		// there were two.
		{
			Response{
				StatusCode:       StatusOK,
				ProtoMajor:       1,
				ProtoMinor:       1,
				Request:          &Request{Method: "POST"},
				Header:           Header{},
				ContentLength:    0,
				TransferEncoding: nil,
				Body:             nil,
			},
			"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n",
		},

		// When a response to a POST has Content-Length: -1, make sure we don't
		// write the Content-Length as -1.
		{
			Response{
				StatusCode:    StatusOK,
				ProtoMajor:    1,
				ProtoMinor:    1,
				Request:       &Request{Method: "POST"},
				Header:        Header{},
				ContentLength: -1,
				Body:          io.NopCloser(strings.NewReader("abcdef")),
			},
			"HTTP/1.1 200 OK\r\nConnection: close\r\n\r\nabcdef",
		},

		// Status code under 100 should be zero-padded to
		// three digits.  Still bogus, but less bogus. (be
		// consistent with generating three digits, since the
		// Transport requires it)
		{
			Response{
				StatusCode: 7,
				Status:     "license to violate specs",
				ProtoMajor: 1,
				ProtoMinor: 0,
				Request:    dummyReq("GET"),
				Header:     Header{},
				Body:       nil,
			},

			"HTTP/1.0 007 license to violate specs\r\nContent-Length: 0\r\n\r\n",
		},

		// No stutter.  Status code in 1xx range response should
		// not include a Content-Length header.  See issue #16942.
		{
			Response{
				StatusCode: 123,
				Status:     "123 Sesame Street",
				ProtoMajor: 1,
				ProtoMinor: 0,
				Request:    dummyReq("GET"),
				Header:     Header{},
				Body:       nil,
			},

			"HTTP/1.0 123 Sesame Street\r\n\r\n",
		},

		// Status code 204 (No content) response should not include a
		// Content-Length header.  See issue #16942.
		{
			Response{
				StatusCode: 204,
				Status:     "No Content",
				ProtoMajor: 1,
				ProtoMinor: 0,
				Request:    dummyReq("GET"),
				Header:     Header{},
				Body:       nil,
			},

			"HTTP/1.0 204 No Content\r\n\r\n",
		},
	}

	for i := range respWriteTests {
		tt := &respWriteTests[i]
		var braw strings.Builder
		err := tt.Resp.Write(&braw)
		if err != nil {
			t.Errorf("error writing #%d: %s", i, err)
			continue
		}
		sraw := braw.String()
		if sraw != tt.Raw {
			t.Errorf("Test %d, expecting:\n%q\nGot:\n%q\n", i, tt.Raw, sraw)
			continue
		}
	}
}

// Response.Write shares the trailer validation added for Issue #78775 with
// Request.Write, so an invalid trailer name or value must be rejected rather
// than written.
func TestResponseWriteInvalidTrailer(t *testing.T) {
	tests := []struct {
		name    string
		trailer Header
		wantErr string
	}{
		{
			name:    "key",
			trailer: Header{"X-Trailer\r\nInjected: 1": {"ok"}},
			wantErr: `net/http: invalid trailer field name "X-Trailer\r\nInjected: 1"`,
		},
		{
			name:    "value",
			trailer: Header{"X-Trailer": {"evil\r\nInjected: 1"}},
			wantErr: `net/http: invalid trailer field value for "X-Trailer"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := Response{
				StatusCode:       200,
				ProtoMajor:       1,
				ProtoMinor:       1,
				Request:          dummyReq("GET"),
				Header:           Header{},
				Body:             io.NopCloser(strings.NewReader("abcdef")),
				ContentLength:    -1,
				TransferEncoding: []string{"chunked"},
				Trailer:          tt.trailer,
			}
			var b strings.Builder
			err := resp.Write(&b)
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("Response.Write error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestResponseWriteStatusLinesAndTrailers(t *testing.T) {
	// Long status (>128B stack buffer) + custom code.
	longStatus := "299 " + strings.Repeat("Z", 160)
	resp := Response{
		StatusCode:    299,
		Status:        longStatus,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Request:       dummyReq11("GET"),
		Header:        Header{},
		Body:          NoBody,
		ContentLength: 0,
	}
	var bb bytes.Buffer
	if err := resp.Write(&bb); err != nil {
		t.Fatalf("Response.Write long status: %v", err)
	}
	wantPrefix := "HTTP/1.1 " + longStatus + "\r\nContent-Length: 0\r\n\r\n"
	if bb.String() != wantPrefix {
		t.Fatalf("got %q, want %q", bb.String(), wantPrefix)
	}

	// Trailer header fast stack sort (<=8 keys) vs slices.Sort (>8 keys) + >128B Trailer line.
	for _, nKeys := range []int{1, 4, 10} {
		tr := make(Header, nKeys)
		var wantKeys []string
		for i := range nKeys {
			k := "X-Trailer-" + string(rune('A'+nKeys-1-i)) + strings.Repeat("k", 12)
			tr[k] = []string{"v"}
			wantKeys = append(wantKeys, k)
		}
		slices.Sort(wantKeys)
		r := Response{
			StatusCode:       200,
			ProtoMajor:       1,
			ProtoMinor:       1,
			Request:          dummyReq11("GET"),
			Header:           Header{},
			Body:             io.NopCloser(strings.NewReader("hi")),
			ContentLength:    -1,
			TransferEncoding: []string{"chunked"},
			Trailer:          tr,
		}
		var out strings.Builder
		if err := r.Write(&out); err != nil {
			t.Fatalf("Response.Write(%d trailers): %v", nKeys, err)
		}
		wantLine := "Trailer: " + strings.Join(wantKeys, ",") + "\r\n"
		if !strings.Contains(out.String(), wantLine) {
			t.Fatalf("%d trailers: missing %q in %q", nKeys, wantLine, out.String())
		}
		// Ensure transferWriter.writeHeader emits WroteHeaderField for Trailer.
		var gotTrailer []string
		tw := &transferWriter{
			Method:           "GET",
			TransferEncoding: []string{"chunked"},
			Trailer:          tr,
			Body:             NoBody,
			BodyCloser:       NoBody,
		}
		var discard bytes.Buffer
		err := tw.writeHeader(&discard, &httptrace.ClientTrace{
			WroteHeaderField: func(key string, val []string) {
				if key == "Trailer" {
					gotTrailer = slices.Clone(val)
				}
			},
		})
		if err != nil {
			t.Fatalf("tw.writeHeader: %v", err)
		}
		if !slices.Equal(gotTrailer, wantKeys) {
			t.Fatalf("traced Trailer = %v, want %v", gotTrailer, wantKeys)
		}
	}
}

func BenchmarkResponseWrite(b *testing.B) {
	resp := Response{
		StatusCode:    StatusOK,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Request:       dummyReq11("GET"),
		Header:        Header{"Content-Type": {"text/plain"}, "X-Req-Id": {"12345"}},
		Body:          NoBody,
		ContentLength: 0,
	}
	var bb bytes.Buffer
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bb.Reset()
		if err := resp.Write(&bb); err != nil {
			b.Fatal(err)
		}
	}
}

