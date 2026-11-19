// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httpcommon

import (
	"cmp"
	"context"
	"net/url"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/http/httpguts"
)

func TestEncodeHeaders(t *testing.T) {
	type header struct {
		name  string
		value string
	}
	for _, test := range []struct {
		name               string
		in                 EncodeHeadersParam
		want               EncodeHeadersResult
		wantHeaders        []header
		disableCompression bool
	}{{
		name: "simple request",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "GET"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
			{"user-agent", "default-user-agent"},
		},
	}, {
		name: "host set from URL",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    &url.URL{Scheme: "https", Host: "example.tld", Path: "/"},
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "GET"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
			{"user-agent", "default-user-agent"},
		},
	}, {
		name: "chunked transfer-encoding",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"Transfer-Encoding": {"chunked"},
				},
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "GET"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
			{"user-agent", "default-user-agent"},
		},
	}, {
		name: "connection close",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"Connection": {"close"},
				},
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "GET"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
			{"user-agent", "default-user-agent"},
		},
	}, {
		name: "connection keep-alive",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"Connection": {"keep-alive"},
				},
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "GET"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
			{"user-agent", "default-user-agent"},
		},
	}, {
		name: "normal connect",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "CONNECT",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "CONNECT"},
			{"accept-encoding", "gzip"},
			{"user-agent", "default-user-agent"},
		},
	}, {
		name: "extended connect",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "CONNECT",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					":protocol": {"foo"},
				},
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "CONNECT"},
			{":path", "/"},
			{":protocol", "foo"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
			{"user-agent", "default-user-agent"},
		},
	}, {
		name: "trailers",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Trailer: map[string][]string{
					"A": {"1"},
					"B": {"2"},
				},
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: true,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "GET"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
			{"trailer", "A,B"},
			{"user-agent", "default-user-agent"},
		},
	}, {
		name: "override user-agent",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"User-Agent": {"GopherTron 9000"},
				},
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "GET"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
			{"user-agent", "GopherTron 9000"},
		},
	}, {
		name: "disable user-agent",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"User-Agent": nil,
				},
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "GET"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
		},
	}, {
		name: "ignore host header",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"Host": {"gophers.tld/"},
				},
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "GET"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
			{"user-agent", "default-user-agent"},
		},
	}, {
		name: "crumble cookie header",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"Cookie": {"a=b; b=c; c=d"},
				},
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "GET"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
			{"user-agent", "default-user-agent"},
			// Cookie header is split into separate header fields.
			{"cookie", "a=b"},
			{"cookie", "b=c"},
			{"cookie", "c=d"},
		},
	}, {
		name: "post with nil body",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "POST",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "POST"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
			{"user-agent", "default-user-agent"},
			{"content-length", "0"},
		},
	}, {
		name: "post with NoBody",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "POST",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "POST"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
			{"user-agent", "default-user-agent"},
			{"content-length", "0"},
		},
	}, {
		name: "post with Content-Length",
		in: EncodeHeadersParam{
			Request: Request{
				Method:              "POST",
				URL:                 must(url.Parse("https://example.tld/")),
				Host:                "example.tld",
				ActualContentLength: 10,
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     true,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "POST"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
			{"user-agent", "default-user-agent"},
			{"content-length", "10"},
		},
	}, {
		name: "post with unknown Content-Length",
		in: EncodeHeadersParam{
			Request: Request{
				Method:              "POST",
				URL:                 must(url.Parse("https://example.tld/")),
				Host:                "example.tld",
				ActualContentLength: -1,
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     true,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "POST"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "gzip"},
			{"user-agent", "default-user-agent"},
		},
	}, {
		name: "explicit accept-encoding",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"Accept-Encoding": {"deflate"},
				},
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "GET"},
			{":path", "/"},
			{":scheme", "https"},
			{"accept-encoding", "deflate"},
			{"user-agent", "default-user-agent"},
		},
	}, {
		name: "head request",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "HEAD",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "HEAD"},
			{":path", "/"},
			{":scheme", "https"},
			{"user-agent", "default-user-agent"},
		},
	}, {
		name: "range request",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "HEAD",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"Range": {"bytes=0-10"},
				},
			},
			DefaultUserAgent: "default-user-agent",
		},
		want: EncodeHeadersResult{
			HasBody:     false,
			HasTrailers: false,
		},
		wantHeaders: []header{
			{":authority", "example.tld"},
			{":method", "HEAD"},
			{":path", "/"},
			{":scheme", "https"},
			{"user-agent", "default-user-agent"},
			{"range", "bytes=0-10"},
		},
	}} {
		t.Run(test.name, func(t *testing.T) {
			var gotHeaders []header
			if IsRequestGzip(test.in.Request.Method, test.in.Request.Header, test.disableCompression) {
				test.in.AddGzipHeader = true
			}

			got, err := EncodeHeaders(context.Background(), test.in, func(name, value string) {
				gotHeaders = append(gotHeaders, header{name, value})
			})
			if err != nil {
				t.Fatalf("EncodeHeaders = %v", err)
			}
			if got.HasBody != test.want.HasBody {
				t.Errorf("HasBody = %v, want %v", got.HasBody, test.want.HasBody)
			}
			if got.HasTrailers != test.want.HasTrailers {
				t.Errorf("HasTrailers = %v, want %v", got.HasTrailers, test.want.HasTrailers)
			}
			cmpHeader := func(a, b header) int {
				return cmp.Or(
					cmp.Compare(a.name, b.name),
					cmp.Compare(a.value, b.value),
				)
			}
			slices.SortFunc(gotHeaders, cmpHeader)
			slices.SortFunc(test.wantHeaders, cmpHeader)
			if !slices.Equal(gotHeaders, test.wantHeaders) {
				t.Errorf("got headers:")
				for _, h := range gotHeaders {
					t.Errorf("  %v: %q", h.name, h.value)
				}
				t.Errorf("want headers:")
				for _, h := range test.wantHeaders {
					t.Errorf("  %v: %q", h.name, h.value)
				}
			}
		})
	}
}

func TestEncodeHeaderErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		in   EncodeHeadersParam
		want string
	}{{
		name: "URL is nil",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				Host:   "example.tld",
			},
		},
		want: "URL is nil",
	}, {
		name: "upgrade header is set",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"Upgrade": {"foo"},
				},
			},
		},
		want: "Upgrade",
	}, {
		name: "unsupported transfer-encoding header",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"Transfer-Encoding": {"identity"},
				},
			},
		},
		want: "Transfer-Encoding",
	}, {
		name: "unsupported connection header",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"Connection": {"x"},
				},
			},
		},
		want: "Connection",
	}, {
		name: "invalid host",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "\x00.tld",
			},
		},
		want: "Host",
	}, {
		name: "protocol header is set",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					":protocol": {"foo"},
				},
			},
		},
		want: ":protocol",
	}, {
		name: "invalid path",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL: &url.URL{
					Scheme: "https",
					Host:   "example.tld",
					Path:   "no_leading_slash",
				},
				Host: "example.tld",
			},
		},
		want: "path",
	}, {
		name: "invalid header name",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"x\ny": {"foo"},
				},
			},
		},
		want: "header",
	}, {
		name: "invalid header value",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"x": {"foo\nbar"},
				},
			},
		},
		want: "header",
	}, {
		name: "invalid trailer",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Trailer: map[string][]string{
					"x\ny": {"foo"},
				},
			},
		},
		want: "trailer",
	}, {
		name: "transfer-encoding trailer",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Trailer: map[string][]string{
					"Transfer-Encoding": {"chunked"},
				},
			},
		},
		want: "Trailer",
	}, {
		name: "trailer trailer",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Trailer: map[string][]string{
					"Trailer": {"chunked"},
				},
			},
		},
		want: "Trailer",
	}, {
		name: "content-length trailer",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Trailer: map[string][]string{
					"Content-Length": {"0"},
				},
			},
		},
		want: "Trailer",
	}, {
		name: "too many headers",
		in: EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					"X-Foo": {strings.Repeat("x", 1000)},
				},
			},
			PeerMaxHeaderListSize: 1000,
		},
		want: "limit",
	}} {
		t.Run(test.name, func(t *testing.T) {
			_, err := EncodeHeaders(context.Background(), test.in, func(name, value string) {})
			if err == nil {
				t.Fatalf("EncodeHeaders = nil, want %q", test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("EncodeHeaders = %q, want error containing %q", err, test.want)
			}
		})
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestValidWireHeaderFieldName(t *testing.T) {
	ref := func(v string) bool {
		if len(v) == 0 {
			return false
		}
		for _, r := range v {
			if !httpguts.IsTokenRune(r) || ('A' <= r && r <= 'Z') {
				return false
			}
		}
		return true
	}

	if ValidWireHeaderFieldName("") {
		t.Fatal("empty string accepted")
	}
	for b := range 256 {
		s := string([]byte{byte(b)})
		if got, want := ValidWireHeaderFieldName(s), ref(s); got != want {
			t.Errorf("byte 0x%02x (%q): got %v, want %v", b, s, got, want)
		}
	}
	for _, s := range []string{
		"content-type",
		"x-custom-123_!#$%&'*+-.^_`|~",
		"Content-Type",
		"x-Bad",
		":path",
		"has space",
		"tab\t",
		"utf8-\u00e9",
		"\xff\xfe",
	} {
		if got, want := ValidWireHeaderFieldName(s), ref(s); got != want {
			t.Errorf("%q: got %v, want %v", s, got, want)
		}
	}
}

func TestParseCleanOriginRequestURIMatchesURLParse(t *testing.T) {
	cases := []string{
		"/",
		"/index.html",
		"/api/v1/users/123?foo=bar&baz=1",
		"/trailing-question?",
		"/double-question?a=1?b=2",
		"//double/slash",
		"//double/slash?q=1",
		"/with%20escape",
		"/with%2Fslash?x=1",
		"/with space",
		"/with\x00ctl",
		"/with?ctl=\x7f",
		"/with?ctl=\r\n",
		"/with?frag=1#sec",
		"/colon:in:path/seg;p=1,2@host",
		"*",
		"/non-ascii-\u00e9",
	}
	for _, raw := range cases {
		fast := parseCleanOriginRequestURI(raw)
		if fast == nil {
			continue
		}
		std, err := url.ParseRequestURI(raw)
		if err != nil {
			t.Errorf("parseCleanOriginRequestURI(%q) returned %+v, but url.ParseRequestURI failed: %v", raw, fast, err)
			continue
		}
		if *fast != *std {
			t.Errorf("parseCleanOriginRequestURI(%q) = %+v, want %+v", raw, *fast, *std)
		}
	}
}

func TestEncodeHeadersInvalidValueOnSkippedHeader(t *testing.T) {
	// Even headers that are skipped during emission (Host, Connection, etc.)
	// must still have their values validated.
	for _, key := range []string{"Host", "Connection", "User-Agent"} {
		_, err := EncodeHeaders(context.Background(), EncodeHeadersParam{
			Request: Request{
				Method: "GET",
				URL:    must(url.Parse("https://example.tld/")),
				Host:   "example.tld",
				Header: map[string][]string{
					key: {"bad\r\nvalue"},
				},
			},
			PeerMaxHeaderListSize: 10 << 20,
		}, func(name, value string) {})
		if err == nil {
			t.Errorf("header %q with CRLF value unexpectedly succeeded", key)
		}
	}
}

func TestEncodeHeadersZeroAllocsEmpty(t *testing.T) {
	param := EncodeHeadersParam{
		Request: Request{
			Method: "GET",
			URL:    must(url.Parse("https://example.tld/hello")),
			Host:   "example.tld",
		},
		AddGzipHeader:         true,
		PeerMaxHeaderListSize: 10 << 20,
		DefaultUserAgent:      "Go-http-client/2.0",
	}
	ctx := context.Background()
	allocs := testing.AllocsPerRun(50, func() {
		_, err := EncodeHeaders(ctx, param, func(name, value string) {})
		if err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 0 {
		t.Errorf("EncodeHeaders empty header allocs = %v, want 0", allocs)
	}
}

func BenchmarkEncodeHeaders(b *testing.B) {
	ctx := context.Background()
	u := must(url.Parse("https://example.tld/api/v1/items?limit=20"))
	b.Run("Empty", func(b *testing.B) {
		p := EncodeHeadersParam{
			Request:               Request{Method: "GET", URL: u, Host: "example.tld"},
			AddGzipHeader:         true,
			PeerMaxHeaderListSize: 10 << 20,
			DefaultUserAgent:      "Go-http-client/2.0",
		}
		b.ReportAllocs()
		for b.Loop() {
			_, _ = EncodeHeaders(ctx, p, func(name, value string) {})
		}
	})
	b.Run("Typical", func(b *testing.B) {
		p := EncodeHeadersParam{
			Request: Request{
				Method: "POST",
				URL:    u,
				Host:   "example.tld",
				Header: map[string][]string{
					"Accept":          {"application/json"},
					"Authorization":   {"Bearer tok"},
					"Content-Type":    {"application/json"},
					"Cookie":          {"a=1; b=2; c=3"},
					"X-Request-Id":    {"req-12345"},
					"X-Custom-Multi":  {"v1", "v2", "v3"},
				},
				ActualContentLength: 1024,
			},
			AddGzipHeader:         true,
			PeerMaxHeaderListSize: 10 << 20,
			DefaultUserAgent:      "Go-http-client/2.0",
		}
		b.ReportAllocs()
		for b.Loop() {
			_, _ = EncodeHeaders(ctx, p, func(name, value string) {})
		}
	})
}

func BenchmarkNewServerRequest(b *testing.B) {
	b.Run("EmptyHeader", func(b *testing.B) {
		rp := ServerRequestParam{
			Method:    "GET",
			Scheme:    "https",
			Authority: "example.tld",
			Path:      "/api/v1/items?limit=20",
		}
		b.ReportAllocs()
		for b.Loop() {
			_ = NewServerRequest(rp)
		}
	})
}

func BenchmarkValidWireHeaderFieldName(b *testing.B) {
	names := []string{"content-type", "accept-encoding", "x-forwarded-for", "cache-control"}
	b.ReportAllocs()
	for b.Loop() {
		for _, n := range names {
			if !ValidWireHeaderFieldName(n) {
				b.Fatal(n)
			}
		}
	}
}
