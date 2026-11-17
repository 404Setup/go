// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bufio"
	"bytes"
	"errors"
	"internal/race"
	"io"
	"net/http/httptrace"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

var headerWriteTests = []struct {
	h        Header
	exclude  map[string]bool
	expected string
}{
	{Header{}, nil, ""},
	{
		Header{
			"Content-Type":   {"text/html; charset=UTF-8"},
			"Content-Length": {"0"},
		},
		nil,
		"Content-Length: 0\r\nContent-Type: text/html; charset=UTF-8\r\n",
	},
	{
		Header{
			"Content-Length": {"0", "1", "2"},
		},
		nil,
		"Content-Length: 0\r\nContent-Length: 1\r\nContent-Length: 2\r\n",
	},
	{
		Header{
			"Expires":          {"-1"},
			"Content-Length":   {"0"},
			"Content-Encoding": {"gzip"},
		},
		map[string]bool{"Content-Length": true},
		"Content-Encoding: gzip\r\nExpires: -1\r\n",
	},
	{
		Header{
			"Expires":          {"-1"},
			"Content-Length":   {"0", "1", "2"},
			"Content-Encoding": {"gzip"},
		},
		map[string]bool{"Content-Length": true},
		"Content-Encoding: gzip\r\nExpires: -1\r\n",
	},
	{
		Header{
			"Expires":          {"-1"},
			"Content-Length":   {"0"},
			"Content-Encoding": {"gzip"},
		},
		map[string]bool{"Content-Length": true, "Expires": true, "Content-Encoding": true},
		"",
	},
	{
		Header{
			"Nil":          nil,
			"Empty":        {},
			"Blank":        {""},
			"Double-Blank": {"", ""},
		},
		nil,
		"Blank: \r\nDouble-Blank: \r\nDouble-Blank: \r\n",
	},
	// Tests header sorting when over the insertion sort threshold side:
	{
		Header{
			"k1": {"1a", "1b"},
			"k2": {"2a", "2b"},
			"k3": {"3a", "3b"},
			"k4": {"4a", "4b"},
			"k5": {"5a", "5b"},
			"k6": {"6a", "6b"},
			"k7": {"7a", "7b"},
			"k8": {"8a", "8b"},
			"k9": {"9a", "9b"},
		},
		map[string]bool{"k5": true},
		"k1: 1a\r\nk1: 1b\r\nk2: 2a\r\nk2: 2b\r\nk3: 3a\r\nk3: 3b\r\n" +
			"k4: 4a\r\nk4: 4b\r\nk6: 6a\r\nk6: 6b\r\n" +
			"k7: 7a\r\nk7: 7b\r\nk8: 8a\r\nk8: 8b\r\nk9: 9a\r\nk9: 9b\r\n",
	},
	// Tests invalid characters in headers.
	{
		Header{
			"Content-Type":             {"text/html; charset=UTF-8"},
			"NewlineInValue":           {"1\r\nBar: 2"},
			"NewlineInKey\r\n":         {"1"},
			"Colon:InKey":              {"1"},
			"Evil: 1\r\nSmuggledValue": {"1"},
		},
		nil,
		"Content-Type: text/html; charset=UTF-8\r\n" +
			"NewlineInValue: 1  Bar: 2\r\n",
	},
}

func TestHeaderWrite(t *testing.T) {
	var buf strings.Builder
	for i, test := range headerWriteTests {
		test.h.WriteSubset(&buf, test.exclude)
		if buf.String() != test.expected {
			t.Errorf("#%d:\n got: %q\nwant: %q", i, buf.String(), test.expected)
		}
		buf.Reset()
	}
}

var parseTimeTests = []struct {
	h   Header
	err bool
}{
	{Header{"Date": {""}}, true},
	{Header{"Date": {"invalid"}}, true},
	{Header{"Date": {"1994-11-06T08:49:37Z00:00"}}, true},
	{Header{"Date": {"Sun, 06 Nov 1994 08:49:37 GMT"}}, false},
	{Header{"Date": {"Sunday, 06-Nov-94 08:49:37 GMT"}}, false},
	{Header{"Date": {"Sun Nov  6 08:49:37 1994"}}, false},
}

func TestParseTime(t *testing.T) {
	expect := time.Date(1994, 11, 6, 8, 49, 37, 0, time.UTC)
	for i, test := range parseTimeTests {
		d, err := ParseTime(test.h.Get("Date"))
		if err != nil {
			if !test.err {
				t.Errorf("#%d:\n got err: %v", i, err)
			}
			continue
		}
		if test.err {
			t.Errorf("#%d:\n  should err", i)
			continue
		}
		if !expect.Equal(d) {
			t.Errorf("#%d:\n got: %v\nwant: %v", i, d, expect)
		}
	}
}

func TestNilHeaderClone(t *testing.T) {
	t1 := Header(nil)
	t2 := t1.Clone()
	if t2 != nil {
		t.Errorf("cloned header does not match original: got: %+v; want: %+v", t2, nil)
	}
}

var testHeader = Header{
	"Content-Length": {"123"},
	"Content-Type":   {"text/plain"},
	"Date":           {"some date at some time Z"},
	"Server":         {DefaultUserAgent},
}

var buf bytes.Buffer

func BenchmarkHeaderWriteSubset(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		testHeader.WriteSubset(&buf, nil)
	}
}

func TestHeaderWriteSubsetAllocs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping alloc test in short mode")
	}
	if race.Enabled {
		t.Skip("skipping test under race detector")
	}
	if runtime.GOMAXPROCS(0) > 1 {
		t.Skip("skipping; GOMAXPROCS>1")
	}
	n := testing.AllocsPerRun(100, func() {
		buf.Reset()
		testHeader.WriteSubset(&buf, nil)
	})
	if n > 0 {
		t.Errorf("allocs = %g; want 0", n)
	}
}

// Issue 34878: test that every call to
// cloneOrMakeHeader never returns a nil Header.
func TestCloneOrMakeHeader(t *testing.T) {
	tests := []struct {
		name     string
		in, want Header
	}{
		{"nil", nil, Header{}},
		{"empty", Header{}, Header{}},
		{
			name: "non-empty",
			in:   Header{"foo": {"bar"}},
			want: Header{"foo": {"bar"}},
		},
		{
			name: "nil value",
			in:   Header{"foo": nil},
			want: Header{"foo": nil},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cloneOrMakeHeader(tt.in)
			if got == nil {
				t.Fatal("unexpected nil Header")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Got:  %#v\nWant: %#v", got, tt.want)
			}
			got.Add("A", "B")
			got.Get("A")
		})
	}
}

func BenchmarkHeaderClone(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = testHeader.Clone()
	}
}

func BenchmarkHeaderCloneEmpty(b *testing.B) {
	h := Header{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = h.Clone()
	}
}

type plainWriter struct{ b bytes.Buffer }

func (p *plainWriter) Write(d []byte) (int, error) { return p.b.Write(d) }

type failAfterNWriter struct{ rem int }

func (w *failAfterNWriter) Write(p []byte) (int, error) {
	if w.rem <= 0 {
		return 0, errors.New("write error")
	}
	n := min(len(p), w.rem)
	w.rem -= n
	if n < len(p) {
		return n, errors.New("short write error")
	}
	return n, nil
}

func TestHeaderWriteSubsetWriters(t *testing.T) {
	longVal := strings.Repeat("x", 600)
	// Cover <=16 keys (stack array) and >16 keys (headerSorterPool).
	smallHdr := Header{
		"Content-Type":   {"text/plain"},
		"X-Exclude":      {"skip"},
		"Invalid\nKey":   {"bad"},
		"X-Newline":      {" a\r\nb "},
		"X-Long":         {longVal},
		"X-Multi":        {"v1", "v2"},
	}
	largeHdr := Header{}
	for i := range 20 {
		largeHdr[string(rune('A'+i))+"-Header"] = []string{"val"}
	}
	largeHdr["X-Long"] = []string{longVal}
	largeHdr["Bad Key"] = []string{"ignored"}
	largeHdr["X-Exclude"] = []string{"skip"}

	exclude := map[string]bool{"X-Exclude": true}

	for _, tc := range []struct {
		name string
		h    Header
	}{
		{"small", smallHdr},
		{"large", largeHdr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ref plainWriter
			if err := tc.h.WriteSubset(&ref, exclude); err != nil {
				t.Fatalf("plainWriter WriteSubset: %v", err)
			}
			want := ref.b.String()

			var bb bytes.Buffer
			if err := tc.h.WriteSubset(&bb, exclude); err != nil || bb.String() != want {
				t.Errorf("*bytes.Buffer mismatch: err=%v, eq=%v", err, bb.String() == want)
			}

			var sb strings.Builder
			if err := tc.h.WriteSubset(&sb, exclude); err != nil || sb.String() != want {
				t.Errorf("*strings.Builder mismatch: err=%v, eq=%v", err, sb.String() == want)
			}

			// Large bufio.Writer exercises AvailableBuffer coalescing + oversized writeHeaderLine fallback.
			var bwLarge bytes.Buffer
			bw := bufio.NewWriterSize(&bwLarge, 4096)
			if err := tc.h.WriteSubset(bw, exclude); err != nil {
				t.Errorf("*bufio.Writer(4096): %v", err)
			}
			bw.Flush()
			if bwLarge.String() != want {
				t.Errorf("*bufio.Writer(4096) mismatch")
			}

			// Small bufio.Writer forces flush between headers inside the loop.
			var bwSmall bytes.Buffer
			bws := bufio.NewWriterSize(&bwSmall, 16)
			if err := tc.h.WriteSubset(bws, exclude); err != nil {
				t.Errorf("*bufio.Writer(16): %v", err)
			}
			bws.Flush()
			if bwSmall.String() != want {
				t.Errorf("*bufio.Writer(16) mismatch:\n got %q\nwant %q", bwSmall.String(), want)
			}

			// Trace path bypasses writer fast paths cleanly.
			var traced []string
			trace := &httptrace.ClientTrace{
				WroteHeaderField: func(k string, v []string) {
					traced = append(traced, k)
				},
			}
			var tw bytes.Buffer
			if err := tc.h.writeSubset(&tw, exclude, trace); err != nil || tw.String() != want {
				t.Errorf("traced writeSubset mismatch: err=%v, eq=%v", err, tw.String() == want)
			}
			if len(traced) == 0 {
				t.Errorf("WroteHeaderField trace callback not invoked")
			}
		})
	}
}

func TestHeaderWriteSubsetErrorReturnsSorter(t *testing.T) {
	largeHdr := Header{}
	for i := range 20 {
		largeHdr[string(rune('A'+i))+"-Header"] = []string{strings.Repeat("v", 32)}
	}
	for _, bufSize := range []int{16, 4096} {
		bw := bufio.NewWriterSize(&failAfterNWriter{rem: 10}, bufSize)
		if err := largeHdr.WriteSubset(bw, nil); err == nil {
			bw.Flush()
		}
	}
	if err := largeHdr.writeSubset(&failAfterNWriter{rem: 10}, nil, &httptrace.ClientTrace{
		WroteHeaderField: func(string, []string) {},
	}); err == nil {
		t.Fatal("expected error from failing writer")
	}
}

func BenchmarkHeaderWriteSubsetBuilder(b *testing.B) {
	var sb strings.Builder
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sb.Reset()
		testHeader.WriteSubset(&sb, nil)
	}
}

func BenchmarkHeaderWriteSubsetBufio(b *testing.B) {
	bw := bufio.NewWriterSize(io.Discard, 4096)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bw.Reset(io.Discard)
		testHeader.WriteSubset(bw, nil)
	}
}

func BenchmarkHeaderWriteSubsetLarge(b *testing.B) {
	h := make(Header, 20)
	for i := range 20 {
		h[string(rune('A'+i))+"-Header"] = []string{"some-value-here"}
	}
	var bb bytes.Buffer
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bb.Reset()
		h.WriteSubset(&bb, nil)
	}
}

