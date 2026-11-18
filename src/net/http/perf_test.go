// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestBasicAuthHeaderMatchesBasicAuth(t *testing.T) {
	// Sizes straddle maxStackBasicAuth, which is where basicAuthHeader
	// switches from its stack buffer to the heap fallback.
	var cases [][2]string
	cases = append(cases, [2]string{"", ""}, [2]string{"a", ""}, [2]string{"", "b"},
		[2]string{"Aladdin", "open sesame"}, [2]string{"user", "p:ss:word"},
		[2]string{"\u00fcser", "p\u00e4ss"}, [2]string{"u", "p\x00q"})
	for _, total := range []int{1, 2, 3, 4, 100, 125, 126, 127, 128, 129, 130, 200, 1000} {
		for _, ulen := range []int{0, 1, total / 2} {
			if ulen > total {
				continue
			}
			u := strings.Repeat("u", ulen)
			p := strings.Repeat("p", total-ulen)
			cases = append(cases, [2]string{u, p})
		}
	}
	for _, c := range cases {
		want := "Basic " + basicAuth(c[0], c[1])
		if got := basicAuthHeader(c[0], c[1]); got != want {
			t.Errorf("basicAuthHeader(%.20q, %.20q) = %.40q, want %.40q", c[0], c[1], got, want)
		}
	}
}

func TestParseBasicAuthSizes(t *testing.T) {
	for _, total := range []int{1, 2, 3, 4, 5, 100, 126, 127, 128, 129, 130, 131, 200, 1000} {
		for _, ulen := range []int{0, 1, total / 2, total - 1} {
			if ulen < 0 || ulen > total {
				continue
			}
			user := strings.Repeat("u", ulen)
			pass := strings.Repeat("p:", (total-ulen)/2) + strings.Repeat("p", (total-ulen)%2)
			auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
			gu, gp, ok := parseBasicAuth(auth)
			if !ok || gu != user || gp != pass {
				t.Errorf("parseBasicAuth(len %d) = %.20q, %.20q, %v; want %.20q, %.20q, true", len(auth), gu, gp, ok, user, pass)
			}
		}
	}
}

func TestParseBasicAuthInvalid(t *testing.T) {
	for _, auth := range []string{
		"",
		"Basic",
		"Basic ",
		"Bearer abc",
		"Basic !!!!",
		"Basic QWxhZGRpbg==", // "Aladdin": no colon
		"Basic " + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 200))), // heap fallback: no colon
		"Basic QWxhZGRpbjpvcGVuIHNlc2FtZQ",                                             // missing padding
		"Basic " + strings.Repeat("A", 400) + "!",
	} {
		if u, p, ok := parseBasicAuth(auth); ok {
			t.Errorf("parseBasicAuth(%.30q) = %q, %q, true; want ok=false", auth, u, p)
		}
	}
	// The prefix match is case-insensitive.
	if u, p, ok := parseBasicAuth("bAsIc QWxhZGRpbjpvcGVuIHNlc2FtZQ=="); !ok || u != "Aladdin" || p != "open sesame" {
		t.Errorf("parseBasicAuth with mixed-case prefix = %q, %q, %v", u, p, ok)
	}
	// Embedded CR/LF in base64 must decode identically to DecodeString.
	if u, p, ok := parseBasicAuth("Basic QWxhZGRp\r\nbjpvcGVu\nIHNlc2FtZQ=="); !ok || u != "Aladdin" || p != "open sesame" {
		t.Errorf("parseBasicAuth with CRLF in base64 = %q, %q, %v", u, p, ok)
	}
}

func TestSetBasicAuthRoundTrip(t *testing.T) {
	req, err := NewRequest("GET", "http://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("Aladdin", "open sesame")
	if got, want := req.Header.Get("Authorization"), "Basic QWxhZGRpbjpvcGVuIHNlc2FtZQ=="; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	u, p, ok := req.BasicAuth()
	if !ok || u != "Aladdin" || p != "open sesame" {
		t.Errorf("BasicAuth() = %q, %q, %v", u, p, ok)
	}
}

func TestBasicAuthAllocs(t *testing.T) {
	req, err := NewRequest("GET", "http://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("user", "password")
	if n := testing.AllocsPerRun(100, func() {
		if _, _, ok := req.BasicAuth(); !ok {
			t.Fatal("BasicAuth failed")
		}
	}); n > 1 {
		t.Errorf("BasicAuth allocs = %v, want <= 1", n)
	}
	if n := testing.AllocsPerRun(100, func() {
		if s := basicAuthHeader("user", "password"); s == "" {
			t.Fatal("empty header")
		}
	}); n > 1 {
		t.Errorf("basicAuthHeader allocs = %v, want <= 1", n)
	}
	// Invalid Basic credentials without a colon should not allocate string copies.
	if n := testing.AllocsPerRun(100, func() {
		if _, _, ok := parseBasicAuth("Basic QWxhZGRpbg=="); ok {
			t.Fatal("unexpected ok")
		}
	}); n != 0 {
		t.Errorf("parseBasicAuth invalid allocs = %v, want 0", n)
	}
}

func TestParseFormBodyValues(t *testing.T) {
	const body = "a=1&b=hello+world&c=%E4%BD%A0&a=2&empty=&flag"
	newReq := func(rawQuery string) *Request {
		return &Request{
			Method: "POST",
			URL:    &url.URL{Path: "/", RawQuery: rawQuery},
			Header: Header{"Content-Type": {"application/x-www-form-urlencoded"}},
			Body:   io.NopCloser(strings.NewReader(body)),
		}
	}
	want := url.Values{
		"a":     {"1", "2"},
		"b":     {"hello world"},
		"c":     {"\u4f60"},
		"empty": {""},
		"flag":  {""},
	}
	r := newReq("")
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.PostForm, want) {
		t.Errorf("PostForm = %v, want %v", r.PostForm, want)
	}
	if !reflect.DeepEqual(r.Form, want) {
		t.Errorf("Form = %v, want %v", r.Form, want)
	}

	// Body values come before query values in Form; PostForm is unaffected.
	r = newReq("a=0&q=x")
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.PostForm, want) {
		t.Errorf("PostForm with query = %v, want %v", r.PostForm, want)
	}
	if got, want := r.Form["a"], []string{"1", "2", "0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Form[a] = %v, want %v", got, want)
	}
	if got := r.FormValue("q"); got != "x" {
		t.Errorf("FormValue(q) = %q, want x", got)
	}

	// Modifying the parsed form must not affect other keys.
	r.PostForm["b"] = append(r.PostForm["b"], "more")
	if got := r.PostForm["c"]; !reflect.DeepEqual(got, []string{"\u4f60"}) {
		t.Errorf("PostForm[c] = %v after appending to PostForm[b]", got)
	}
}

func TestParseFormGetValues(t *testing.T) {
	r := &Request{Method: "GET", URL: &url.URL{Path: "/", RawQuery: "x=1&y=2&x=3&z=%zz"}}
	err := r.ParseForm()
	if err == nil {
		t.Error("ParseForm with a bad escape: expected an error")
	}
	want := url.Values{"x": {"1", "3"}, "y": {"2"}}
	if !reflect.DeepEqual(r.Form, want) {
		t.Errorf("Form = %v, want %v", r.Form, want)
	}
}

type trickleReader struct {
	s string
	i int
}

func (tr *trickleReader) Read(p []byte) (int, error) {
	if tr.i >= len(tr.s) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = tr.s[tr.i]
	tr.i++
	return 1, nil
}

type errReader struct {
	data string
	err  error
	read bool
}

func (er *errReader) Read(p []byte) (int, error) {
	if !er.read {
		er.read = true
		n := copy(p, er.data)
		return n, nil
	}
	return 0, er.err
}

func TestParseFormEdgeCases(t *testing.T) {
	formHeader := Header{"Content-Type": {"application/x-www-form-urlencoded"}}

	t.Run("ContentTypeVariants", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			ct   string
			want url.Values
		}{
			{"Empty", "", url.Values{}},
			{"MultipartNoBoundary", "multipart/form-data", url.Values{}},
			{"MultipartBoundary", "multipart/form-data; boundary=abc", url.Values{}},
			{"Upper", "APPLICATION/X-WWW-FORM-URLENCODED", url.Values{"k": {"v"}}},
			{"WithCharset", "application/x-www-form-urlencoded; charset=utf-8", url.Values{"k": {"v"}}},
		} {
			r := &Request{
				Method:        "POST",
				URL:           &url.URL{Path: "/"},
				Header:        Header{"Content-Type": {tc.ct}},
				Body:          io.NopCloser(strings.NewReader("k=v")),
				ContentLength: 3,
			}
			if tc.ct == "" {
				r.Header = Header{}
			}
			if err := r.ParseForm(); err != nil {
				t.Fatalf("%s: ParseForm error = %v", tc.name, err)
			}
			if !reflect.DeepEqual(r.PostForm, tc.want) {
				t.Errorf("%s: PostForm = %v, want %v", tc.name, r.PostForm, tc.want)
			}
		}
	})

	t.Run("MediaErrorPrecedence", func(t *testing.T) {
		for _, body := range []string{"k=v", "k=%zz"} {
			r := &Request{
				Method:        "POST",
				URL:           &url.URL{Path: "/"},
				Header:        Header{"Content-Type": {"application/x-www-form-urlencoded; bad=1=2"}},
				Body:          io.NopCloser(strings.NewReader(body)),
				ContentLength: int64(len(body)),
			}
			if err := r.ParseForm(); !errors.Is(err, mime.ErrInvalidMediaParameter) {
				t.Errorf("body=%q: err = %v, want ErrInvalidMediaParameter", body, err)
			}
			if body == "k=v" && r.PostForm.Get("k") != "v" {
				t.Errorf("body=%q: PostForm = %v, want k=v", body, r.PostForm)
			}
		}
		// Size error overrides media parameter error, matching upstream parsePostForm.
		tooLarge := strings.Repeat("a", (10<<20)+1)
		r := &Request{
			Method: "POST",
			URL:    &url.URL{Path: "/"},
			Header: Header{"Content-Type": {"application/x-www-form-urlencoded; bad=1=2"}},
			Body:   io.NopCloser(strings.NewReader(tooLarge)),
		}
		if err := r.ParseForm(); err == nil || !strings.Contains(err.Error(), "POST too large") {
			t.Errorf("oversized body with bad media param: err = %v, want POST too large", err)
		}
	})

	t.Run("ContentLengthVariants", func(t *testing.T) {
		body := "a=1&b=hello&c=3"
		cases := []struct {
			name string
			cl   int64
			rd   io.Reader
		}{
			{"Accurate", int64(len(body)), strings.NewReader(body)},
			{"TrickleAccurate", int64(len(body)), &trickleReader{s: body}},
			{"SmallerThanBody", 3, strings.NewReader(body)},
			{"LargerThanBody", 100, strings.NewReader(body)},
			{"ZeroWithBody", 0, strings.NewReader(body)},
			{"Exact511", 511, strings.NewReader("k=" + strings.Repeat("x", 509))},
			{"Large2K", 2002, strings.NewReader("k=" + strings.Repeat("y", 2000))},
			{"Exact4095", 4095, strings.NewReader("k=" + strings.Repeat("z", 4093))},
			{"AboveSmall4096", 4096, strings.NewReader("k=" + strings.Repeat("w", 4094))},
		}
		for _, tc := range cases {
			r := &Request{
				Method:        "POST",
				URL:           &url.URL{Path: "/"},
				Header:        formHeader,
				Body:          io.NopCloser(tc.rd),
				ContentLength: tc.cl,
			}
			if err := r.ParseForm(); err != nil {
				t.Fatalf("%s: ParseForm error = %v", tc.name, err)
			}
			switch tc.name {
			case "Exact511":
				if len(r.PostForm.Get("k")) != 509 {
					t.Errorf("%s: len(k) = %d, want 509", tc.name, len(r.PostForm.Get("k")))
				}
			case "Large2K":
				if len(r.PostForm.Get("k")) != 2000 {
					t.Errorf("%s: len(k) = %d, want 2000", tc.name, len(r.PostForm.Get("k")))
				}
			case "Exact4095":
				if len(r.PostForm.Get("k")) != 4093 {
					t.Errorf("%s: len(k) = %d, want 4093", tc.name, len(r.PostForm.Get("k")))
				}
			case "AboveSmall4096":
				if len(r.PostForm.Get("k")) != 4094 {
					t.Errorf("%s: len(k) = %d, want 4094", tc.name, len(r.PostForm.Get("k")))
				}
			default:
				if got, want := r.PostForm, (url.Values{"a": {"1"}, "b": {"hello"}, "c": {"3"}}); !reflect.DeepEqual(got, want) {
					t.Errorf("%s: PostForm = %v, want %v", tc.name, got, want)
				}
			}
		}

		// NoBody should succeed with empty PostForm (or preserve media parameter err).
		r := &Request{Method: "POST", URL: &url.URL{Path: "/"}, Header: formHeader, Body: NoBody}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("NoBody: ParseForm error = %v", err)
		}
		if len(r.PostForm) != 0 || r.PostForm == nil {
			t.Errorf("NoBody: PostForm = %#v, want empty non-nil", r.PostForm)
		}
		rNoBodyBadCT := &Request{
			Method: "POST",
			URL:    &url.URL{Path: "/"},
			Header: Header{"Content-Type": {"application/x-www-form-urlencoded; bad=1=2"}},
			Body:   NoBody,
		}
		if err := rNoBodyBadCT.ParseForm(); !errors.Is(err, mime.ErrInvalidMediaParameter) {
			t.Errorf("NoBody with bad media param: err = %v, want ErrInvalidMediaParameter", err)
		}
		if rNoBodyBadCT.PostForm == nil || len(rNoBodyBadCT.PostForm) != 0 {
			t.Errorf("NoBody with bad media param: PostForm = %#v, want empty non-nil", rNoBodyBadCT.PostForm)
		}
	})

	t.Run("LyingSmallContentLengthTooLarge", func(t *testing.T) {
		r := &Request{
			Method:        "POST",
			URL:           &url.URL{Path: "/"},
			Header:        formHeader,
			Body:          io.NopCloser(strings.NewReader("k=" + strings.Repeat("x", (10<<20)+1))),
			ContentLength: 10,
		}
		if err := r.ParseForm(); err == nil || !strings.Contains(err.Error(), "POST too large") {
			t.Errorf("lying ContentLength oversized body: err = %v, want POST too large", err)
		}
	})

	t.Run("MaxBytesReader", func(t *testing.T) {
		body := "a=1&b=2&c=3"
		r := &Request{
			Method:        "POST",
			URL:           &url.URL{Path: "/"},
			Header:        formHeader,
			Body:          MaxBytesReader(nil, io.NopCloser(strings.NewReader(body)), int64(len(body))),
			ContentLength: int64(len(body)),
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("exact MaxBytesReader failed: %v", err)
		}
		if got := r.PostForm.Get("c"); got != "3" {
			t.Errorf("exact MaxBytesReader c = %q, want 3", got)
		}

		r2 := &Request{
			Method:        "POST",
			URL:           &url.URL{Path: "/"},
			Header:        formHeader,
			Body:          MaxBytesReader(nil, io.NopCloser(strings.NewReader(body)), int64(len(body)-1)),
			ContentLength: int64(len(body)),
		}
		var maxErr *MaxBytesError
		if err := r2.ParseForm(); !errors.As(err, &maxErr) {
			t.Errorf("exceeded MaxBytesReader err = %v, want *MaxBytesError", err)
		}

		// Small CL exceeding MaxBytesReader limit with unknown/larger underlying body.
		r3 := &Request{
			Method:        "POST",
			URL:           &url.URL{Path: "/"},
			Header:        formHeader,
			Body:          MaxBytesReader(nil, io.NopCloser(strings.NewReader("k="+strings.Repeat("x", 100))), 20),
			ContentLength: 102,
		}
		if err := r3.ParseForm(); !errors.As(err, &maxErr) {
			t.Errorf("MaxBytesReader cl>limit err = %v, want *MaxBytesError", err)
		}
	})

	t.Run("ReadError", func(t *testing.T) {
		boom := errors.New("read failed")
		r := &Request{
			Method:        "POST",
			URL:           &url.URL{Path: "/"},
			Header:        formHeader,
			Body:          io.NopCloser(&errReader{data: "a=1", err: boom}),
			ContentLength: 10,
		}
		if err := r.ParseForm(); !errors.Is(err, boom) {
			t.Errorf("ParseForm err = %v, want %v", err, boom)
		}
	})
}

var benchBasicAuthRequest = func() *Request {
	r := &Request{Header: make(Header)}
	r.SetBasicAuth("user", "password")
	return r
}()

func BenchmarkBasicAuth(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchBasicAuthRequest.BasicAuth()
	}
}

func BenchmarkSetBasicAuth(b *testing.B) {
	r := &Request{Header: make(Header)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.SetBasicAuth("user", "password")
	}
}

func BenchmarkParseFormGet(b *testing.B) {
	u := &url.URL{Path: "/", RawQuery: "a=1&b=2&c=hello%20world&d=x&e=5&f=6"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := &Request{Method: "GET", URL: u}
		r.ParseForm()
	}
}

func BenchmarkParseFormPost(b *testing.B) {
	const body = "a=1&b=2&c=hello%20world&d=x&e=5&f=6"
	h := Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	u := &url.URL{Path: "/"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := &Request{Method: "POST", URL: u, Header: h, Body: io.NopCloser(strings.NewReader(body))}
		r.ParseForm()
	}
}

func BenchmarkParseFormPostWithCL(b *testing.B) {
	const body = "a=1&b=2&c=hello%20world&d=x&e=5&f=6"
	h := Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	u := &url.URL{Path: "/"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := &Request{
			Method:        "POST",
			URL:           u,
			Header:        h,
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)),
		}
		r.ParseForm()
	}
}

func BenchmarkParseFormPostLargeWithCL(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 32; i++ {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString("field")
		sb.WriteByte(byte('a' + i%26))
		sb.WriteByte('=')
		sb.WriteString("value1234567890abcdef")
	}
	body := sb.String()
	h := Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	u := &url.URL{Path: "/"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := &Request{
			Method:        "POST",
			URL:           u,
			Header:        h,
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)),
		}
		r.ParseForm()
	}
}
