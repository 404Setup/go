// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httptest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestRecorder(t *testing.T) {
	type checkFunc func(*ResponseRecorder) error
	check := func(fns ...checkFunc) []checkFunc { return fns }

	hasStatus := func(wantCode int) checkFunc {
		return func(rec *ResponseRecorder) error {
			if rec.Code != wantCode {
				return fmt.Errorf("Status = %d; want %d", rec.Code, wantCode)
			}
			return nil
		}
	}
	hasResultStatus := func(want string) checkFunc {
		return func(rec *ResponseRecorder) error {
			if rec.Result().Status != want {
				return fmt.Errorf("Result().Status = %q; want %q", rec.Result().Status, want)
			}
			return nil
		}
	}
	hasResultStatusCode := func(wantCode int) checkFunc {
		return func(rec *ResponseRecorder) error {
			if rec.Result().StatusCode != wantCode {
				return fmt.Errorf("Result().StatusCode = %d; want %d", rec.Result().StatusCode, wantCode)
			}
			return nil
		}
	}
	hasResultContents := func(want string) checkFunc {
		return func(rec *ResponseRecorder) error {
			contentBytes, err := io.ReadAll(rec.Result().Body)
			if err != nil {
				return err
			}
			contents := string(contentBytes)
			if contents != want {
				return fmt.Errorf("Result().Body = %s; want %s", contents, want)
			}
			return nil
		}
	}
	hasContents := func(want string) checkFunc {
		return func(rec *ResponseRecorder) error {
			if rec.Body.String() != want {
				return fmt.Errorf("wrote = %q; want %q", rec.Body.String(), want)
			}
			return nil
		}
	}
	hasFlush := func(want bool) checkFunc {
		return func(rec *ResponseRecorder) error {
			if rec.Flushed != want {
				return fmt.Errorf("Flushed = %v; want %v", rec.Flushed, want)
			}
			return nil
		}
	}
	hasOldHeader := func(key, want string) checkFunc {
		return func(rec *ResponseRecorder) error {
			if got := rec.HeaderMap.Get(key); got != want {
				return fmt.Errorf("HeaderMap header %s = %q; want %q", key, got, want)
			}
			return nil
		}
	}
	hasHeader := func(key, want string) checkFunc {
		return func(rec *ResponseRecorder) error {
			if got := rec.Result().Header.Get(key); got != want {
				return fmt.Errorf("final header %s = %q; want %q", key, got, want)
			}
			return nil
		}
	}
	hasNotHeaders := func(keys ...string) checkFunc {
		return func(rec *ResponseRecorder) error {
			for _, k := range keys {
				v, ok := rec.Result().Header[http.CanonicalHeaderKey(k)]
				if ok {
					return fmt.Errorf("unexpected header %s with value %q", k, v)
				}
			}
			return nil
		}
	}
	hasTrailer := func(key, want string) checkFunc {
		return func(rec *ResponseRecorder) error {
			if got := rec.Result().Trailer.Get(key); got != want {
				return fmt.Errorf("trailer %s = %q; want %q", key, got, want)
			}
			return nil
		}
	}
	hasNotTrailers := func(keys ...string) checkFunc {
		return func(rec *ResponseRecorder) error {
			trailers := rec.Result().Trailer
			for _, k := range keys {
				_, ok := trailers[http.CanonicalHeaderKey(k)]
				if ok {
					return fmt.Errorf("unexpected trailer %s", k)
				}
			}
			return nil
		}
	}
	hasContentLength := func(length int64) checkFunc {
		return func(rec *ResponseRecorder) error {
			if got := rec.Result().ContentLength; got != length {
				return fmt.Errorf("ContentLength = %d; want %d", got, length)
			}
			return nil
		}
	}

	for _, tt := range [...]struct {
		name   string
		h      func(w http.ResponseWriter, r *http.Request)
		checks []checkFunc
	}{
		{
			"200 default",
			func(w http.ResponseWriter, r *http.Request) {},
			check(hasStatus(200), hasContents("")),
		},
		{
			"first code only",
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(201)
				w.WriteHeader(202)
				w.Write([]byte("hi"))
			},
			check(hasStatus(201), hasContents("hi")),
		},
		{
			"write sends 200",
			func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("hi first"))
				w.WriteHeader(201)
				w.WriteHeader(202)
			},
			check(hasStatus(200), hasContents("hi first"), hasFlush(false)),
		},
		{
			"write string",
			func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, "hi first")
			},
			check(
				hasStatus(200),
				hasContents("hi first"),
				hasFlush(false),
				hasHeader("Content-Type", "text/plain; charset=utf-8"),
			),
		},
		{
			"flush",
			func(w http.ResponseWriter, r *http.Request) {
				w.(http.Flusher).Flush() // also sends a 200
				w.WriteHeader(201)
			},
			check(hasStatus(200), hasFlush(true), hasContentLength(-1)),
		},
		{
			"Content-Type detection",
			func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, "<html>")
			},
			check(hasHeader("Content-Type", "text/html; charset=utf-8")),
		},
		{
			"no Content-Type detection with Transfer-Encoding",
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Transfer-Encoding", "some encoding")
				io.WriteString(w, "<html>")
			},
			check(hasHeader("Content-Type", "")), // no header
		},
		{
			"no Content-Type detection if set explicitly",
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "some/type")
				io.WriteString(w, "<html>")
			},
			check(hasHeader("Content-Type", "some/type")),
		},
		{
			"Content-Type detection doesn't crash if HeaderMap is nil",
			func(w http.ResponseWriter, r *http.Request) {
				// Act as if the user wrote new(httptest.ResponseRecorder)
				// rather than using NewRecorder (which initializes
				// HeaderMap)
				w.(*ResponseRecorder).HeaderMap = nil
				io.WriteString(w, "<html>")
			},
			check(hasHeader("Content-Type", "text/html; charset=utf-8")),
		},
		{
			"Header is not changed after write",
			func(w http.ResponseWriter, r *http.Request) {
				hdr := w.Header()
				hdr.Set("Key", "correct")
				w.WriteHeader(200)
				hdr.Set("Key", "incorrect")
			},
			check(hasHeader("Key", "correct")),
		},
		{
			"Trailer headers are correctly recorded",
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Non-Trailer", "correct")
				w.Header().Set("Trailer", "Trailer-A, Trailer-B")
				w.Header().Add("Trailer", "Trailer-C")
				io.WriteString(w, "<html>")
				w.Header().Set("Non-Trailer", "incorrect")
				w.Header().Set("Trailer-A", "valuea")
				w.Header().Set("Trailer-C", "valuec")
				w.Header().Set("Trailer-NotDeclared", "should be omitted")
				w.Header().Set("Trailer:Trailer-D", "with prefix")
			},
			check(
				hasStatus(200),
				hasHeader("Content-Type", "text/html; charset=utf-8"),
				hasHeader("Non-Trailer", "correct"),
				hasNotHeaders("Trailer-A", "Trailer-B", "Trailer-C", "Trailer-NotDeclared"),
				hasTrailer("Trailer-A", "valuea"),
				hasTrailer("Trailer-C", "valuec"),
				hasNotTrailers("Non-Trailer", "Trailer-B", "Trailer-NotDeclared"),
				hasTrailer("Trailer-D", "with prefix"),
			),
		},
		{
			"Header set without any write", // Issue 15560
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Foo", "1")

				// Simulate somebody using
				// new(ResponseRecorder) instead of
				// using the constructor which sets
				// this to 200
				w.(*ResponseRecorder).Code = 0
			},
			check(
				hasOldHeader("X-Foo", "1"),
				hasStatus(0),
				hasHeader("X-Foo", "1"),
				hasResultStatus("200 OK"),
				hasResultStatusCode(200),
			),
		},
		{
			"HeaderMap vs FinalHeaders", // more for Issue 15560
			func(w http.ResponseWriter, r *http.Request) {
				h := w.Header()
				h.Set("X-Foo", "1")
				w.Write([]byte("hi"))
				h.Set("X-Foo", "2")
				h.Set("X-Bar", "2")
			},
			check(
				hasOldHeader("X-Foo", "2"),
				hasOldHeader("X-Bar", "2"),
				hasHeader("X-Foo", "1"),
				hasNotHeaders("X-Bar"),
			),
		},
		{
			"setting Content-Length header",
			func(w http.ResponseWriter, r *http.Request) {
				body := "Some body"
				contentLength := fmt.Sprintf("%d", len(body))
				w.Header().Set("Content-Length", contentLength)
				io.WriteString(w, body)
			},
			check(hasStatus(200), hasContents("Some body"), hasContentLength(9)),
		},
		{
			"nil ResponseRecorder.Body", // Issue 26642
			func(w http.ResponseWriter, r *http.Request) {
				w.(*ResponseRecorder).Body = nil
				io.WriteString(w, "hi")
			},
			check(hasResultContents("")), // check we don't crash reading the body

		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "http://foo.com/", nil)
			h := http.HandlerFunc(tt.h)
			rec := NewRecorder()
			h.ServeHTTP(rec, r)
			for _, check := range tt.checks {
				if err := check(rec); err != nil {
					t.Error(err)
				}
			}
		})
	}
}

func TestBodyNotAllowed(t *testing.T) {
	rw := NewRecorder()
	rw.Body = new(bytes.Buffer)
	rw.WriteHeader(204)

	_, err := rw.Write([]byte("hello "))
	if !errors.Is(err, http.ErrBodyNotAllowed) {
		t.Errorf("expected BodyNotAllowed for Write after 204, got: %v", err)
	}

	_, err = rw.WriteString("world")
	if !errors.Is(err, http.ErrBodyNotAllowed) {
		t.Errorf("expected BodyNotAllowed for WriteString after 204, got: %v", err)
	}

	if got, want := rw.Body.String(), "hello world"; got != want {
		t.Errorf("got Body=%q, want %q", got, want)
	}
}

// issue 39017 - disallow Content-Length values such as "+3"
func TestParseContentLength(t *testing.T) {
	tests := []struct {
		cl   string
		want int64
	}{
		{
			cl:   "3",
			want: 3,
		},
		{
			cl:   "+3",
			want: -1,
		},
		{
			cl:   "-3",
			want: -1,
		},
		{
			// max int64, for safe conversion before returning
			cl:   "9223372036854775807",
			want: 9223372036854775807,
		},
		{
			cl:   "9223372036854775808",
			want: -1,
		},
	}

	for _, tt := range tests {
		if got := parseContentLength(tt.cl); got != tt.want {
			t.Errorf("%q:\n\tgot=%d\n\twant=%d", tt.cl, got, tt.want)
		}
	}
}

// Ensure that httptest.Recorder panics when given a non-3 digit (XXX)
// status HTTP code. See https://golang.org/issues/45353
func TestRecorderPanicsOnNonXXXStatusCode(t *testing.T) {
	badCodes := []int{
		-100, 0, 99, 1000, 20000,
	}
	for _, badCode := range badCodes {
		t.Run(fmt.Sprintf("Code=%d", badCode), func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("Expected a panic")
				}
			}()

			handler := func(rw http.ResponseWriter, _ *http.Request) {
				rw.WriteHeader(badCode)
			}
			r, _ := http.NewRequest("GET", "http://example.org/", nil)
			rw := NewRecorder()
			handler(rw, r)
		})
	}
}

func TestResponseRecorderOptimizations(t *testing.T) {
	t.Run("status_strings", func(t *testing.T) {
		for _, tc := range []struct {
			code int
			want string
		}{
			{200, "200 OK"},
			{299, "299 "},
			{404, "404 Not Found"},
			{999, "999 "},
		} {
			rw := NewRecorder()
			rw.WriteHeader(tc.code)
			res := rw.Result()
			if res.StatusCode != tc.code || res.Status != tc.want {
				t.Errorf("code=%d: StatusCode=%d Status=%q, want %d %q", tc.code, res.StatusCode, res.Status, tc.code, tc.want)
			}
			if res2 := rw.Result(); res2 != res {
				t.Errorf("Result() not cached")
			}
		}

		// Zero Code falls back to 200 OK.
		rwZero := NewRecorder()
		rwZero.Code = 0
		res := rwZero.Result()
		if res.StatusCode != 200 || res.Status != "200 OK" {
			t.Errorf("Code=0: StatusCode=%d Status=%q, want 200 200 OK", res.StatusCode, res.Status)
		}
	})

	t.Run("write_string_sniff_and_te", func(t *testing.T) {
		rw := NewRecorder()
		rw.Header()["Transfer-Encoding"] = []string{""}
		longHTML := "<!DOCTYPE html><html><body>" + strings.Repeat("a", 600) + "</body></html>"
		n, err := rw.WriteString(longHTML)
		if err != nil || n != len(longHTML) {
			t.Fatalf("WriteString = (%d, %v), want (%d, nil)", n, err, len(longHTML))
		}
		res := rw.Result()
		if got := res.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Fatalf("Content-Type = %q, want text/html; charset=utf-8", got)
		}

		// Non-empty Transfer-Encoding suppresses Content-Type sniffing.
		rwTE := NewRecorder()
		rwTE.Header()["Transfer-Encoding"] = []string{"chunked"}
		rwTE.WriteString("<html>hello</html>")
		if got := rwTE.Result().Header.Get("Content-Type"); got != "" {
			t.Fatalf("with Transfer-Encoding, Content-Type = %q, want empty", got)
		}
	})

	t.Run("trailers_and_header_snapshot", func(t *testing.T) {
		rw := NewRecorder()
		rw.Header().Set("X-Initial", "v1")
		rw.Header().Add("Trailer", "X-Tr-A, x-tr-b, Connection")
		rw.Write([]byte("body"))
		// Mutating HeaderMap after Write should not affect snapped Response.Header,
		// except declared trailer values and TrailerPrefix entries.
		rw.Header().Set("X-Initial", "mutated")
		rw.Header().Set("X-Tr-A", "va")
		rw.Header().Add("X-Tr-B", "vb1")
		rw.Header().Add("X-Tr-B", "vb2")
		rw.Header().Set("Connection", "close")
		rw.Header().Set(http.TrailerPrefix+"X-Tr-C", "vc")

		res := rw.Result()
		if got := res.Header.Get("X-Initial"); got != "v1" {
			t.Errorf("snapped X-Initial = %q, want v1", got)
		}
		if got := res.Trailer["X-Tr-A"]; !slices.Equal(got, []string{"va"}) {
			t.Errorf("Trailer X-Tr-A = %v, want [va]", got)
		}
		if got := res.Trailer["X-Tr-B"]; !slices.Equal(got, []string{"vb1", "vb2"}) {
			t.Errorf("Trailer X-Tr-B = %v, want [vb1 vb2]", got)
		}
		if _, ok := res.Trailer["Connection"]; ok {
			t.Errorf("invalid trailer Connection included: %v", res.Trailer)
		}
		if got := res.Trailer["X-Tr-C"]; !slices.Equal(got, []string{"vc"}) {
			t.Errorf("Trailer X-Tr-C = %v, want [vc]", got)
		}
	})

	t.Run("nil_body_and_body_disallowed", func(t *testing.T) {
		for _, code := range []int{http.StatusContinue, http.StatusNoContent, http.StatusNotModified} {
			rw := &ResponseRecorder{HeaderMap: make(http.Header)}
			rw.WriteHeader(code)
			if n, err := rw.WriteString("x"); err != http.ErrBodyNotAllowed || n != 0 {
				t.Errorf("status %d WriteString = (%d, %v), want (0, ErrBodyNotAllowed)", code, n, err)
			}
			res := rw.Result()
			if res.Body != http.NoBody {
				t.Errorf("status %d Body != http.NoBody", code)
			}
		}
	})
}

func BenchmarkResponseRecorderWriteString(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rw := NewRecorder()
		rw.Header()["Content-Type"] = []string{"text/plain"}
		rw.WriteString("hello, world")
		_ = rw.Result()
	}
}

func BenchmarkResponseRecorderResult(b *testing.B) {
	rw := NewRecorder()
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Content-Length", "13")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteString(`{"ok":true}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rw.result = nil
		_ = rw.Result()
	}
}

