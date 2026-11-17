// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package internal

import (
	"bytes"
	"testing"
)

func TestDetectContentTypeEdgeCases(t *testing.T) {
	eotWithPNG := make([]byte, 40)
	copy(eotWithPNG, "\x89PNG\r\n\x1a\n")
	eotWithPNG[34] = 'L'
	eotWithPNG[35] = 'P'

	eotPlain := make([]byte, 40)
	eotPlain[34] = 'L'
	eotPlain[35] = 'P'

	longTextWithLateBinary := append(bytes.Repeat([]byte("a"), 512), 0x00, 0x01)

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"pdf", []byte("%PDF-1.7\n"), "application/pdf"},
		{"postscript", []byte("%!PS-Adobe-3.0\n"), "application/postscript"},
		{"utf16be", []byte{0xFE, 0xFF, 0x00, 'a'}, "text/plain; charset=utf-16be"},
		{"utf16le", []byte{0xFF, 0xFE, 'a', 0x00}, "text/plain; charset=utf-16le"},
		{"utf8_bom", []byte{0xEF, 0xBB, 0xBF, 'a'}, "text/plain; charset=utf-8"},
		{"webm", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x9F}, "video/webm"},
		{"gzip", []byte{0x1F, 0x8B, 0x08, 0x00}, "application/x-gzip"},
		{"zip", []byte{'P', 'K', 0x03, 0x04, 0x14}, "application/zip"},
		{"ttf", []byte{0x00, 0x01, 0x00, 0x00, 0x00}, "font/ttf"},
		{"collection", []byte("ttcf\x00\x01"), "font/collection"},
		{"eot", eotPlain, "application/vnd.ms-fontobject"},
		// Binary PNG signature precedes EOT in WHATWG MIME Sniffing order.
		{"png_precedes_eot", eotWithPNG, "image/png"},
		// MP4 validation edge cases: unaligned boxSize / short slice.
		{"mp4_valid", []byte("\x00\x00\x00\x14ftypisom\x00\x00\x02\x00mp41"), "video/mp4"},
		{"mp4_unaligned_box", []byte("\x00\x00\x00\x15ftypmp42\x00\x00\x00\x00mp42isom"), "application/octet-stream"},
		{"mp4_short_slice", []byte("\x00\x00\x00\x18ftypmp42\x00\x00"), "application/octet-stream"},
		// HTML tag termination & leading whitespace.
		{"html_ws_iframe", []byte("\t\f <!doctype html><html>"), "text/html; charset=utf-8"},
		{"html_doctype_space", []byte("<!DoCtYpE hTmL >"), "text/html; charset=utf-8"},
		{"html_doctype_nul_space", []byte("<!doctype\x00html>"), "application/octet-stream"},
		{"html_fake_bang", []byte("<\x01doctype html>"), "application/octet-stream"},
		{"html_comment", []byte("<!-- hello -->"), "text/html; charset=utf-8"},
		{"html_h1", []byte("<H1>Title"), "text/html; charset=utf-8"},
		{"html_script", []byte("<script src=a>"), "text/html; charset=utf-8"},
		{"html_a", []byte("<a href=x>"), "text/html; charset=utf-8"},
		{"html_b", []byte("<b>bold"), "text/html; charset=utf-8"},
		{"html_p", []byte("<p>para"), "text/html; charset=utf-8"},
		{"html_br", []byte("<br>"), "text/html; charset=utf-8"},
		{"html_body", []byte("<body>"), "text/html; charset=utf-8"},
		{"html_unterminated", []byte("<html>"), "text/html; charset=utf-8"},
		{"html_missing_tt", []byte("<htmlx>"), "text/plain; charset=utf-8"},
		{"html_too_short", []byte("<html"), "text/plain; charset=utf-8"},
		{"xml_case_sensitive", []byte("<?XML!"), "text/plain; charset=utf-8"},
		{"xml_fake_q", []byte("<\x1fxml>"), "application/octet-stream"},
		// Control byte boundaries for plain text vs octet-stream.
		{"plain_allowed_ctrls", []byte{'\t', '\n', '\f', '\r', 0x1B, 'x'}, "text/plain; charset=utf-8"},
		{"binary_08", []byte{'a', 0x08}, "application/octet-stream"},
		{"binary_0b", []byte{'a', 0x0B}, "application/octet-stream"},
		{"binary_1a", []byte{'a', 0x1A}, "application/octet-stream"},
		{"binary_1c", []byte{'a', 0x1C}, "application/octet-stream"},
		{"truncated_at_512", longTextWithLateBinary, "text/plain; charset=utf-8"},
	}

	for _, tt := range tests {
		if got := DetectContentType(tt.data); got != tt.want {
			t.Errorf("%s: DetectContentType = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func BenchmarkDetectContentType(b *testing.B) {
	cases := []struct {
		name string
		data []byte
	}{
		{"HTML", []byte("<!DOCTYPE html><html><head><title>Hi</title></head><body>Hello</body></html>")},
		{"PlainText512", bytes.Repeat([]byte(`{"ok":true,"msg":"hello world"},`), 16)},
		{"PNG", append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 256)...)},
		{"PDF", append([]byte("%PDF-1.7\n%"), bytes.Repeat([]byte("obj "), 64)...)},
		{"MP4", []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom<\x06t\xbfmdat")},
		{"Binary512", append([]byte{0x01, 0x02, 0x03}, make([]byte, 509)...)},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = DetectContentType(tc.data)
			}
		})
	}
}
