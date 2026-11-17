// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Tests of internal functions and things with no better homes.

package http

import (
	"bufio"
	"bytes"
	"internal/testenv"
	"io/fs"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestForeachHeaderElement(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"Foo", []string{"Foo"}},
		{" Foo", []string{"Foo"}},
		{"Foo ", []string{"Foo"}},
		{" Foo ", []string{"Foo"}},

		{"foo", []string{"foo"}},
		{"anY-cAsE", []string{"anY-cAsE"}},

		{"", nil},
		{",,,,  ,  ,,   ,,, ,", nil},

		{" Foo,Bar, Baz,lower,,Quux ", []string{"Foo", "Bar", "Baz", "lower", "Quux"}},
	}
	for _, tt := range tests {
		var got []string
		foreachHeaderElement(tt.in, func(v string) {
			got = append(got, v)
		})
		if !slices.Equal(got, tt.want) {
			t.Errorf("foreachHeaderElement(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

// Test that cmd/go doesn't link in the HTTP server.
//
// This catches accidental dependencies between the HTTP transport and
// server code.
func TestCmdGoNoHTTPServer(t *testing.T) {
	t.Parallel()
	goBin := testenv.GoToolPath(t)
	out, err := testenv.Command(t, goBin, "tool", "nm", goBin).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm: %v: %s", err, out)
	}
	wantSym := map[string]bool{
		// Verify these exist: (sanity checking this test)
		"net/http.(*Client).do":           true,
		"net/http.(*Transport).RoundTrip": true,

		// Verify these don't exist:
		"net/http.http2Server":           false,
		"net/http.(*Server).Serve":       false,
		"net/http.(*ServeMux).ServeHTTP": false,
		"net/http.DefaultServeMux":       false,
	}
	for sym, want := range wantSym {
		got := bytes.Contains(out, []byte(sym))
		if !want && got {
			t.Errorf("cmd/go unexpectedly links in HTTP server code; found symbol %q in cmd/go", sym)
		}
		if want && !got {
			t.Errorf("expected to find symbol %q in cmd/go; not found", sym)
		}
	}
}

var valuesCount int

func BenchmarkCopyValues(b *testing.B) {
	b.ReportAllocs()
	src := url.Values{
		"a": {"1", "2", "3", "4", "5"},
		"b": {"2", "2", "3", "4", "5"},
		"c": {"3", "2", "3", "4", "5"},
		"d": {"4", "2", "3", "4", "5"},
		"e": {"1", "1", "2", "3", "4", "5", "6", "7", "abcdef", "l", "a", "b", "c", "d", "z"},
		"j": {"1", "2"},
		"m": nil,
	}
	for i := 0; i < b.N; i++ {
		dst := url.Values{"a": {"b"}, "b": {"2"}, "c": {"3"}, "d": {"4"}, "j": nil, "m": {"x"}}
		copyValues(dst, src)
		if valuesCount = len(dst["a"]); valuesCount != 6 {
			b.Fatalf(`%d items in dst["a"] but expected 6`, valuesCount)
		}
	}
	if valuesCount == 0 {
		b.Fatal("Benchmark wasn't run")
	}
}

var forbiddenStringsFunctions = map[string]bool{
	// Functions that use Unicode-aware case folding.
	"EqualFold":      true,
	"Title":          true,
	"ToLower":        true,
	"ToLowerSpecial": true,
	"ToTitle":        true,
	"ToTitleSpecial": true,
	"ToUpper":        true,
	"ToUpperSpecial": true,

	// Functions that use Unicode-aware spaces.
	"Fields":    true,
	"TrimSpace": true,
}

// TestNoUnicodeStrings checks that nothing in net/http uses the Unicode-aware
// strings and bytes package functions. HTTP is mostly ASCII based, and doing
// Unicode-aware case folding or space stripping can introduce vulnerabilities.
func TestNoUnicodeStrings(t *testing.T) {
	testenv.MustHaveSource(t)

	re := regexp.MustCompile(`(strings|bytes).([A-Za-z]+)`)
	if err := fs.WalkDir(os.DirFS("."), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}

		if path == "internal/ascii" {
			return fs.SkipDir
		}
		if !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") ||
			path == "internal/http2/ascii.go" ||
			path == "internal/httpcommon/ascii.go" ||
			d.IsDir() {
			return nil
		}

		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for lineNum, line := range strings.Split(string(contents), "\n") {
			for _, match := range re.FindAllStringSubmatch(line, -1) {
				if !forbiddenStringsFunctions[match[2]] {
					continue
				}
				t.Errorf("disallowed call to %s at %s:%d", match[0], path, lineNum+1)
			}
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProtocols(t *testing.T) {
	var p Protocols
	if p.HTTP1() {
		t.Errorf("zero-value protocols: p.HTTP1() = true, want false")
	}
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	if !p.HTTP1() {
		t.Errorf("initialized protocols: p.HTTP1() = false, want true")
	}
	if !p.HTTP2() {
		t.Errorf("initialized protocols: p.HTTP2() = false, want true")
	}
	p.SetHTTP1(false)
	if p.HTTP1() {
		t.Errorf("after unsetting HTTP1: p.HTTP1() = true, want false")
	}
	if !p.HTTP2() {
		t.Errorf("after unsetting HTTP1: p.HTTP2() = false, want true")
	}
}

const redirectURL = "/thisaredirect细雪withasciilettersのけぶabcdefghijk.html"

func BenchmarkHexEscapeNonASCII(b *testing.B) {
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		hexEscapeNonASCII(redirectURL)
	}
}

func TestCopyValues(t *testing.T) {
	src := url.Values{
		"a": {"1", "2"},
		"b": {"3"},
		"z": nil,
	}
	dst := make(url.Values)
	copyValues(dst, src)
	if !reflect.DeepEqual(dst, src) {
		t.Fatalf("empty dst copyValues = %#v, want %#v", dst, src)
	}
	// Mutating appended slice on one key must not clobber adjacent slab elements.
	dst["a"] = append(dst["a"], "extra")
	if !slices.Equal(dst["b"], []string{"3"}) {
		t.Fatalf("slab aliasing corrupted dst[b] = %#v", dst["b"])
	}

	// Non-empty dst appends cleanly.
	copyValues(dst, url.Values{"b": {"4"}, "new": {"5"}})
	if !slices.Equal(dst["b"], []string{"3", "4"}) || !slices.Equal(dst["new"], []string{"5"}) {
		t.Fatalf("non-empty dst copyValues produced %#v", dst)
	}
}

func BenchmarkCopyValuesEmptyDst(b *testing.B) {
	b.ReportAllocs()
	src := url.Values{
		"a": {"1", "2", "3", "4", "5"},
		"b": {"2", "2", "3", "4", "5"},
		"c": {"3", "2", "3", "4", "5"},
		"d": {"4", "2", "3", "4", "5"},
		"e": {"1", "1", "2", "3", "4", "5", "6", "7"},
		"j": {"1", "2"},
		"m": nil,
	}
	for i := 0; i < b.N; i++ {
		dst := make(url.Values, len(src))
		copyValues(dst, src)
		if valuesCount = len(dst["a"]); valuesCount != 5 {
			b.Fatalf("len(dst[a]) = %d, want 5", valuesCount)
		}
	}
}

func TestContainsDotDot(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"/", false},
		{"/a/b/c", false},
		{"/a/..b/c", false},
		{"/a/b../c", false},
		{"..", true},
		{"/..", true},
		{"../", true},
		{"/a/../b", true},
		{`\a\..\b`, true},
		{`/a\../b`, true},
		{"/a/..../b", false},
	}
	for _, tt := range tests {
		if got := containsDotDot(tt.in); got != tt.want {
			t.Errorf("containsDotDot(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestWriteReqLine(t *testing.T) {
	var bb bytes.Buffer
	bw := bufio.NewWriterSize(&bb, 64)
	if err := writeReqLine(bw, "Host: ", "example.com", "\r\n", ""); err != nil {
		t.Fatal(err)
	}
	// Overflow bufio buffer to exercise non-bufio fallback cleanly.
	long := strings.Repeat("x", 300)
	if err := writeReqLine(bw, "X-Long: ", long, "\r\n", ""); err != nil {
		t.Fatal(err)
	}
	bw.Flush()
	want := "Host: example.com\r\nX-Long: " + long + "\r\n"
	if bb.String() != want {
		t.Fatalf("got %d bytes, want %d bytes", bb.Len(), len(want))
	}

	var raw bytes.Buffer
	if err := writeReqLine(&raw, "A", long, "B", "C"); err != nil {
		t.Fatal(err)
	}
	if raw.String() != "A"+long+"BC" {
		t.Fatalf("raw writeReqLine mismatch")
	}
}

func TestExtraHeaderWriteAndChunkWriter(t *testing.T) {
	eh := extraHeader{
		date:             []byte("Tue, 10 Nov 2009 23:00:00 GMT"),
		contentLength:    []byte("12"),
		contentType:      "text/plain",
		connection:       "keep-alive",
		transferEncoding: "chunked",
	}
	// Fast path via bufio.Writer AvailableBuffer.
	var fastBuf bytes.Buffer
	bwFast := bufio.NewWriterSize(&fastBuf, 4096)
	eh.Write(bwFast)
	bwFast.Flush()

	// Slow path via small bufio.Writer triggering writeSlow.
	var slowBuf bytes.Buffer
	bwSlow := bufio.NewWriterSize(&slowBuf, 16)
	eh.Write(bwSlow)
	bwSlow.Flush()

	if fastBuf.String() != slowBuf.String() || !strings.Contains(fastBuf.String(), "Transfer-Encoding: chunked\r\n") {
		t.Fatalf("extraHeader mismatch:\nfast=%q\nslow=%q", fastBuf.String(), slowBuf.String())
	}

	// chunkWriter zero-len, coalesced fast path, and oversized write fallback.
	var cwOut bytes.Buffer
	c := &conn{bufw: bufio.NewWriterSize(&cwOut, 64)}
	resp := &response{
		conn:          c,
		req:           &Request{Method: "GET"},
		handlerHeader: Header{},
	}
	cw := &chunkWriter{
		res:         resp,
		wroteHeader: true,
		chunking:    true,
	}
	if n, err := cw.Write(nil); n != 0 || err != nil {
		t.Fatalf("cw.Write(nil) = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := cw.Write([]byte("hi")); n != 2 || err != nil {
		t.Fatalf("cw.Write(hi) = (%d, %v)", n, err)
	}
	big := strings.Repeat("Z", 128)
	if n, err := cw.Write([]byte(big)); n != len(big) || err != nil {
		t.Fatalf("cw.Write(big) = (%d, %v)", n, err)
	}
	c.bufw.Flush()
	wantCW := "2\r\nhi\r\n80\r\n" + big + "\r\n"
	if cwOut.String() != wantCW {
		t.Fatalf("chunkWriter output = %q, want %q", cwOut.String(), wantCW)
	}
}

