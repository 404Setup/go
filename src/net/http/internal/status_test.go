// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package internal

import (
	"fmt"
	"strconv"
	"testing"
)

func TestStatusTables(t *testing.T) {
	known := map[int]string{
		100: "Continue",
		101: "Switching Protocols",
		102: "Processing",
		103: "Early Hints",
		200: "OK",
		201: "Created",
		202: "Accepted",
		203: "Non-Authoritative Information",
		204: "No Content",
		205: "Reset Content",
		206: "Partial Content",
		207: "Multi-Status",
		208: "Already Reported",
		226: "IM Used",
		300: "Multiple Choices",
		301: "Moved Permanently",
		302: "Found",
		303: "See Other",
		304: "Not Modified",
		305: "Use Proxy",
		307: "Temporary Redirect",
		308: "Permanent Redirect",
		400: "Bad Request",
		401: "Unauthorized",
		402: "Payment Required",
		403: "Forbidden",
		404: "Not Found",
		405: "Method Not Allowed",
		406: "Not Acceptable",
		407: "Proxy Authentication Required",
		408: "Request Timeout",
		409: "Conflict",
		410: "Gone",
		411: "Length Required",
		412: "Precondition Failed",
		413: "Request Entity Too Large",
		414: "Request URI Too Long",
		415: "Unsupported Media Type",
		416: "Requested Range Not Satisfiable",
		417: "Expectation Failed",
		418: "I'm a teapot",
		421: "Misdirected Request",
		422: "Unprocessable Entity",
		423: "Locked",
		424: "Failed Dependency",
		425: "Too Early",
		426: "Upgrade Required",
		428: "Precondition Required",
		429: "Too Many Requests",
		431: "Request Header Fields Too Large",
		451: "Unavailable For Legal Reasons",
		500: "Internal Server Error",
		501: "Not Implemented",
		502: "Bad Gateway",
		503: "Service Unavailable",
		504: "Gateway Timeout",
		505: "HTTP Version Not Supported",
		506: "Variant Also Negotiates",
		507: "Insufficient Storage",
		508: "Loop Detected",
		510: "Not Extended",
		511: "Network Authentication Required",
	}

	for code := -10; code <= 1010; code++ {
		text := StatusText(code)
		wantText := known[code]
		if text != wantText {
			t.Errorf("StatusText(%d) = %q, want %q", code, text, wantText)
		}
		if text == "" {
			if got := StatusLine(true, code); got != "" {
				t.Errorf("StatusLine(true, %d) = %q, want empty", code, got)
			}
			if got := StatusLine(false, code); got != "" {
				t.Errorf("StatusLine(false, %d) = %q, want empty", code, got)
			}
			if got := StatusCodeText(code); got != "" {
				t.Errorf("StatusCodeText(%d) = %q, want empty", code, got)
			}
			if got := StatusCodeString(code); got != "" {
				t.Errorf("StatusCodeString(%d) = %q, want empty", code, got)
			}
			continue
		}
		if got, want := StatusLine(true, code), fmt.Sprintf("HTTP/1.1 %d %s\r\n", code, text); got != want {
			t.Errorf("StatusLine(true, %d) = %q, want %q", code, got, want)
		}
		if got, want := StatusLine(false, code), fmt.Sprintf("HTTP/1.0 %d %s\r\n", code, text); got != want {
			t.Errorf("StatusLine(false, %d) = %q, want %q", code, got, want)
		}
		if got, want := StatusCodeText(code), fmt.Sprintf("%d %s", code, text); got != want {
			t.Errorf("StatusCodeText(%d) = %q, want %q", code, got, want)
		}
		if got, want := StatusCodeString(code), strconv.Itoa(code); got != want {
			t.Errorf("StatusCodeString(%d) = %q, want %q", code, got, want)
		}
	}
}

func BenchmarkStatusLine(b *testing.B) {
	codes := []int{200, 204, 304, 404, 500}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = StatusLine(true, codes[i%len(codes)])
	}
}

func BenchmarkStatusCodeText(b *testing.B) {
	codes := []int{200, 204, 304, 404, 500}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = StatusCodeText(codes[i%len(codes)])
	}
}

func BenchmarkStatusCodeString(b *testing.B) {
	codes := []int{200, 204, 304, 404, 500}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = StatusCodeString(codes[i%len(codes)])
	}
}
