// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package base64_test

import (
	"bytes"
	"crypto/sha256"
	. "encoding/base64"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

func refEncode(alphabet string, padRune rune, src []byte) []byte {
	var out []byte
	for i := 0; i < len(src); i += 3 {
		rem := len(src) - i
		var b0, b1, b2 uint
		b0 = uint(src[i])
		if rem > 1 {
			b1 = uint(src[i+1])
		}
		if rem > 2 {
			b2 = uint(src[i+2])
		}
		val := (b0 << 16) | (b1 << 8) | b2
		out = append(out, alphabet[(val>>18)&0x3F])
		out = append(out, alphabet[(val>>12)&0x3F])
		if rem > 1 {
			out = append(out, alphabet[(val>>6)&0x3F])
		} else if padRune != NoPadding {
			out = append(out, byte(padRune))
		}
		if rem > 2 {
			out = append(out, alphabet[val&0x3F])
		} else if padRune != NoPadding {
			out = append(out, byte(padRune))
		}
	}
	return out
}

func makeDeterministicBytes(n int, seed byte) []byte {
	out := make([]byte, n)
	h := sha256.Sum256([]byte{seed, byte(n), byte(n >> 8)})
	for i := range out {
		out[i] = h[i%len(h)] ^ byte(i*17+int(seed))
	}
	return out
}

func TestFastDifferentialAllLengthsAndEncodings(t *testing.T) {
	customAlpha := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	customAlt := "!\"#$%&'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`"
	cases := []struct {
		name  string
		enc   *Encoding
		alpha string
		pad   rune
	}{
		{"Std", StdEncoding, customAlpha, StdPadding},
		{"URL", URLEncoding, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", StdPadding},
		{"RawStd", RawStdEncoding, customAlpha, NoPadding},
		{"RawURL", RawURLEncoding, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", NoPadding},
		{"StdStrict", StdEncoding.Strict(), customAlpha, StdPadding},
		{"URLStrict", URLEncoding.Strict(), "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", StdPadding},
		{"CustomAlt", NewEncoding(customAlt).WithPadding('~'), customAlt, '~'},
	}

	lengths := make([]int, 0, 260)
	for n := 0; n <= 160; n++ {
		lengths = append(lengths, n)
	}
	lengths = append(lengths, 191, 192, 193, 255, 256, 257, 511, 512, 513, 768, 1024, 1535, 2048, 4096)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, n := range lengths {
				src := makeDeterministicBytes(n, byte(len(tc.name)))
				want := refEncode(tc.alpha, tc.pad, src)

				// Encode
				dst := make([]byte, tc.enc.EncodedLen(len(src)))
				tc.enc.Encode(dst, src)
				if !bytes.Equal(dst, want) {
					t.Fatalf("len=%d Encode mismatch:\ngot  %q\nwant %q", n, dst, want)
				}

				// EncodeToString
				if s := tc.enc.EncodeToString(src); s != string(want) {
					t.Fatalf("len=%d EncodeToString mismatch", n)
				}

				// AppendEncode
				app := tc.enc.AppendEncode([]byte("pre:"), src)
				if !bytes.Equal(app[4:], want) || string(app[:4]) != "pre:" {
					t.Fatalf("len=%d AppendEncode mismatch", n)
				}

				// Decode
				dec := make([]byte, tc.enc.DecodedLen(len(want)))
				dn, err := tc.enc.Decode(dec, want)
				if err != nil {
					t.Fatalf("len=%d Decode error: %v", n, err)
				}
				if !bytes.Equal(dec[:dn], src) {
					t.Fatalf("len=%d Decode bytes mismatch", n)
				}

				// DecodeString
				ds, err := tc.enc.DecodeString(string(want))
				if err != nil {
					t.Fatalf("len=%d DecodeString error: %v", n, err)
				}
				if !bytes.Equal(ds, src) {
					t.Fatalf("len=%d DecodeString mismatch", n)
				}

				// AppendDecode
				da, err := tc.enc.AppendDecode([]byte("ok:"), want)
				if err != nil || string(da[:3]) != "ok:" || !bytes.Equal(da[3:], src) {
					t.Fatalf("len=%d AppendDecode failed: err=%v", n, err)
				}
			}
		})
	}
}

func TestFastAlignmentAndInPlaceOverlap(t *testing.T) {
	encodings := []*Encoding{StdEncoding, URLEncoding, RawStdEncoding, RawURLEncoding}
	raw := makeDeterministicBytes(1024+64, 99)
	for _, enc := range encodings {
		for _, n := range []int{0, 1, 2, 3, 23, 24, 25, 47, 48, 49, 127, 128, 129, 513} {
			for srcOff := 0; srcOff < 32; srcOff += 7 {
				src := raw[srcOff : srcOff+n]
				want := enc.EncodeToString(src)
				for dstOff := 0; dstOff < 32; dstOff += 5 {
					dstBuf := make([]byte, dstOff+enc.EncodedLen(n)+16)
					copy(dstBuf[:dstOff], bytes.Repeat([]byte{0xAA}, dstOff))
					copy(dstBuf[dstOff+enc.EncodedLen(n):], bytes.Repeat([]byte{0xBB}, 16))

					enc.Encode(dstBuf[dstOff:], src)
					if string(dstBuf[dstOff:dstOff+len(want)]) != want {
						t.Fatalf("aligned Encode failed n=%d srcOff=%d dstOff=%d", n, srcOff, dstOff)
					}
					// Canary guards
					if !bytes.Equal(dstBuf[:dstOff], bytes.Repeat([]byte{0xAA}, dstOff)) {
						t.Fatalf("underflow clobbered prefix dstOff=%d", dstOff)
					}
					if !bytes.Equal(dstBuf[dstOff+enc.EncodedLen(n):], bytes.Repeat([]byte{0xBB}, 16)) {
						t.Fatalf("overflow clobbered suffix n=%d", n)
					}
				}

				// Overlapping in-place Decode(buf, buf)
				buf := make([]byte, len(want)+32)
				copy(buf, want)
				dn, err := enc.Decode(buf, buf[:len(want)])
				if err != nil {
					t.Fatalf("in-place Decode n=%d err=%v", n, err)
				}
				if !bytes.Equal(buf[:dn], src) {
					t.Fatalf("in-place Decode n=%d mismatch", n)
				}
			}
		}
	}
}

func TestFastSIMDCorruptAndBoundarySecurity(t *testing.T) {
	// Include sizes touching AVX2 32->24, tail 24..31, 16..23, and scalar tails.
	sizes := []int{1, 2, 3, 16, 23, 24, 31, 32, 33, 47, 48, 49, 64, 95, 96, 97, 256}
	badBytes := []byte{0x00, 0x09, 0x1f, ' ', '!', ',', '.', '=', 0x7f, 0x80, 0xff}
	stdAlpha := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	urlAlpha := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	encs := []struct {
		enc    *Encoding
		oracle *Encoding
	}{
		{StdEncoding, NewEncoding(stdAlpha)},
		{URLEncoding, NewEncoding(urlAlpha)},
		{RawStdEncoding, NewEncoding(stdAlpha).WithPadding(NoPadding)},
		{RawURLEncoding, NewEncoding(urlAlpha).WithPadding(NoPadding)},
	}

	for _, pair := range encs {
		enc, oracle := pair.enc, pair.oracle
		for _, n := range sizes {
			src := makeDeterministicBytes(n, 7)
			encoded := []byte(enc.EncodeToString(src))

			// Inject newlines at various positions: valid base64 must ignore \r and \n.
			var wrapped []byte
			for i, b := range encoded {
				if i == 15 || i == 31 || i == 32 || i == 63 || i == 76 {
					wrapped = append(wrapped, '\r', '\n')
				}
				wrapped = append(wrapped, b)
			}
			wrapped = append(wrapped, '\n')
			dst := make([]byte, enc.DecodedLen(len(wrapped)))
			dn, err := enc.Decode(dst, wrapped)
			if err != nil || !bytes.Equal(dst[:dn], src) {
				t.Fatalf("wrapped decode n=%d err=%v mismatch=%v", n, err, !bytes.Equal(dst[:dn], src))
			}

			// Streaming NewDecoder test across 32-byte boundaries
			decBytes, err := io.ReadAll(NewDecoder(enc, bytes.NewReader(wrapped)))
			if err != nil || !bytes.Equal(decBytes, src) {
				t.Fatalf("NewDecoder wrapped n=%d err=%v", n, err)
			}

			// Corrupt byte differential checks against scalar NewEncoding oracle
			for _, pos := range []int{0, 1, 7, 15, 23, 31, 32, len(encoded) - 1} {
				if pos < 0 || pos >= len(encoded) {
					continue
				}
				for _, bad := range badBytes {
					corrupt := slices.Clone(encoded)
					corrupt[pos] = bad
					out := make([]byte, enc.DecodedLen(len(corrupt)))
					wantOut := make([]byte, oracle.DecodedLen(len(corrupt)))
					gotN, gotErr := enc.Decode(out, corrupt)
					wantN, wantErr := oracle.Decode(wantOut, corrupt)
					if !errors.Is(gotErr, wantErr) || (gotErr != nil && wantErr != nil && gotErr.Error() != wantErr.Error()) {
						t.Fatalf("n=%d pos=%d bad=%#x err mismatch: got (%d, %v), want (%d, %v)", n, pos, bad, gotN, gotErr, wantN, wantErr)
					}
					if gotN != wantN || !bytes.Equal(out[:gotN], wantOut[:wantN]) {
						t.Fatalf("n=%d pos=%d bad=%#x partial decoded mismatch: got %d, want %d", n, pos, bad, gotN, wantN)
					}
				}
			}
		}
	}

	// Cross-encoding rejection ('+' / '/' invalid in URL, '-' / '_' invalid in Std)
	urlData := makeDeterministicBytes(192, 42)
	stdStr := StdEncoding.EncodeToString(urlData)
	if strings.ContainsAny(stdStr, "+/") {
		if _, err := URLEncoding.DecodeString(stdStr); err == nil {
			t.Fatal("URLEncoding accepted '+' or '/'")
		}
	}
	urlStr := URLEncoding.EncodeToString(urlData)
	if strings.ContainsAny(urlStr, "-_") {
		if _, err := StdEncoding.DecodeString(urlStr); err == nil {
			t.Fatal("StdEncoding accepted '-' or '_'")
		}
	}

	// Strict non-zero padding bits
	_, err := StdEncoding.Strict().DecodeString("AB==") // low 4 bits of 'B' (index 1) are non-zero
	if err == nil {
		t.Fatal("Strict failed to reject non-zero trailing pad bits")
	}
}

func TestFastAllAlphabetAndByteCoverage(t *testing.T) {
	// Cover all 256 byte values across multiple NEON/AVX2/SVE2 vector blocks.
	raw := make([]byte, 512)
	for i := range raw {
		raw[i] = byte(i)
	}
	for _, enc := range []*Encoding{StdEncoding, URLEncoding, RawStdEncoding, RawURLEncoding} {
		got := enc.EncodeToString(raw)
		dec, err := enc.DecodeString(got)
		if err != nil {
			t.Fatalf("DecodeString failed: %v", err)
		}
		if !bytes.Equal(dec, raw) {
			t.Fatalf("roundtrip mismatch for 512-byte all-byte pattern")
		}
	}

	// Cover every valid alphabet symbol repeated across 64..512 byte boundaries.
	for _, tc := range []struct {
		enc   *Encoding
		alpha string
	}{
		{StdEncoding, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"},
		{URLEncoding, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"},
	} {
		rep := strings.Repeat(tc.alpha, 8) // 512 bytes
		oracle := NewEncoding(tc.alpha)
		got, err := tc.enc.DecodeString(rep)
		want, wantErr := oracle.DecodeString(rep)
		if err != wantErr || !bytes.Equal(got, want) {
			t.Fatalf("all-alphabet decode mismatch: err=(%v,%v) eq=%v", err, wantErr, bytes.Equal(got, want))
		}
	}
}

func TestFastLargeSIMDCorruptAndNewlinePositions(t *testing.T) {
	stdAlpha := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	urlAlpha := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	encs := []struct {
		enc    *Encoding
		oracle *Encoding
	}{
		{StdEncoding, NewEncoding(stdAlpha)},
		{URLEncoding, NewEncoding(urlAlpha)},
		{RawStdEncoding, NewEncoding(stdAlpha).WithPadding(NoPadding)},
		{RawURLEncoding, NewEncoding(urlAlpha).WithPadding(NoPadding)},
	}
	sizes := []int{128, 192, 256, 384, 512, 768, 1024}
	positions := []int{0, 1, 15, 16, 31, 32, 47, 48, 63, 64, 95, 96, 127, 128, 191, 192, 255, 256, 383, 384, 511, 512}
	badBytes := []byte{0x00, 0x01, 0x1f, ' ', '!', '+', '-', '/', '_', '=', 0x7f, 0x80, 0xbf, 0xc1, 0xff}

	for _, pair := range encs {
		for _, n := range sizes {
			src := makeDeterministicBytes(n, 23)
			encoded := []byte(pair.enc.EncodeToString(src))

			for _, pos := range positions {
				if pos >= len(encoded) {
					continue
				}
				// Newline injection at SIMD chunk boundary
				withNL := make([]byte, 0, len(encoded)+2)
				withNL = append(withNL, encoded[:pos]...)
				withNL = append(withNL, '\r', '\n')
				withNL = append(withNL, encoded[pos:]...)
				dst := make([]byte, pair.enc.DecodedLen(len(withNL)))
				dn, err := pair.enc.Decode(dst, withNL)
				if err != nil || !bytes.Equal(dst[:dn], src) {
					t.Fatalf("newline at pos=%d n=%d failed: err=%v", pos, n, err)
				}

				for _, bad := range badBytes {
					corrupt := slices.Clone(encoded)
					corrupt[pos] = bad
					out := make([]byte, pair.enc.DecodedLen(len(corrupt)))
					wantOut := make([]byte, pair.oracle.DecodedLen(len(corrupt)))
					gotN, gotErr := pair.enc.Decode(out, corrupt)
					wantN, wantErr := pair.oracle.Decode(wantOut, corrupt)
					if !errors.Is(gotErr, wantErr) || (gotErr != nil && wantErr != nil && gotErr.Error() != wantErr.Error()) {
						t.Fatalf("n=%d pos=%d bad=%#x err mismatch: got (%d, %v), want (%d, %v)", n, pos, bad, gotN, gotErr, wantN, wantErr)
					}
					if gotN != wantN || !bytes.Equal(out[:gotN], wantOut[:wantN]) {
						t.Fatalf("n=%d pos=%d bad=%#x partial decoded mismatch: got %d, want %d", n, pos, bad, gotN, wantN)
					}
				}
			}
		}
	}

	// Empty DecodeString must return non-nil empty slice.
	if b, err := StdEncoding.DecodeString(""); err != nil || b == nil || len(b) != 0 {
		t.Fatalf("DecodeString(\"\") = (%#v, %v), want non-nil empty slice", b, err)
	}
}

func TestFastExhaustive256ByteValidation(t *testing.T) {
	stdAlpha := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	urlAlpha := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	encs := []struct {
		enc    *Encoding
		oracle *Encoding
	}{
		{StdEncoding, NewEncoding(stdAlpha)},
		{URLEncoding, NewEncoding(urlAlpha)},
		{StdEncoding.Strict(), NewEncoding(stdAlpha).Strict()},
		{URLEncoding.Strict(), NewEncoding(urlAlpha).Strict()},
		{RawStdEncoding, NewEncoding(stdAlpha).WithPadding(NoPadding)},
		{RawURLEncoding, NewEncoding(urlAlpha).WithPadding(NoPadding)},
	}

	base := bytes.Repeat([]byte("QUJD"), 32) // 128 bytes valid base64
	positions := []int{0, 1, 15, 16, 31, 32, 47, 63, 64, 95, 96, 127}
	for _, pair := range encs {
		for _, pos := range positions {
			buf := slices.Clone(base)
			out := make([]byte, pair.enc.DecodedLen(len(buf)))
			wantOut := make([]byte, pair.oracle.DecodedLen(len(buf)))
			for b := 0; b < 256; b++ {
				buf[pos] = byte(b)
				gotN, gotErr := pair.enc.Decode(out, buf)
				wantN, wantErr := pair.oracle.Decode(wantOut, buf)
				if !errors.Is(gotErr, wantErr) || (gotErr != nil && wantErr != nil && gotErr.Error() != wantErr.Error()) {
					t.Fatalf("pos=%d byte=%#02x err mismatch: got (%d, %v), want (%d, %v)", pos, b, gotN, gotErr, wantN, wantErr)
				}
				if gotN != wantN || !bytes.Equal(out[:gotN], wantOut[:wantN]) {
					t.Fatalf("pos=%d byte=%#02x partial decode mismatch: got %d, want %d", pos, b, gotN, wantN)
				}
			}
		}
	}
}

func BenchmarkFastSuite(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"16B", 16},
		{"256B", 256},
		{"4KB", 4096},
		{"32KB", 32768},
	}
	for _, sz := range sizes {
		src := makeDeterministicBytes(sz.size, 3)
		encDst := make([]byte, StdEncoding.EncodedLen(len(src)))
		StdEncoding.Encode(encDst, src)
		decDst := make([]byte, StdEncoding.DecodedLen(len(encDst)))
		encStr := string(encDst)

		b.Run("Encode/"+sz.name, func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				StdEncoding.Encode(encDst, src)
			}
		})
		b.Run("EncodeToString/"+sz.name, func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				_ = StdEncoding.EncodeToString(src)
			}
		})
		b.Run("AppendEncode/"+sz.name, func(b *testing.B) {
			buf := make([]byte, 0, len(encDst))
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				buf = StdEncoding.AppendEncode(buf[:0], src)
			}
		})
		b.Run("Decode/"+sz.name, func(b *testing.B) {
			b.SetBytes(int64(len(encDst)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := StdEncoding.Decode(decDst, encDst); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("DecodeString/"+sz.name, func(b *testing.B) {
			b.SetBytes(int64(len(encDst)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := StdEncoding.DecodeString(encStr); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("AppendDecode/"+sz.name, func(b *testing.B) {
			buf := make([]byte, 0, len(src))
			b.SetBytes(int64(len(encDst)))
			b.ReportAllocs()
			for b.Loop() {
				var err error
				buf, err = StdEncoding.AppendDecode(buf[:0], encDst)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestFastStripNewlinesAndWrappedSIMD(t *testing.T) {
	for _, size := range []int{0, 1, 15, 31, 32, 33, 64, 95, 192, 4096} {
		raw := make([]byte, size)
		for i := range raw {
			raw[i] = byte(i*13 + 7)
		}
		clean := StdEncoding.EncodeToString(raw)
		for _, interval := range []int{1, 7, 31, 32, 33, 64, 76} {
			for _, sep := range []string{"\n", "\r", "\r\n"} {
				var wrapped strings.Builder
				for i := 0; i < len(clean); i += interval {
					end := min(i+interval, len(clean))
					wrapped.WriteString(clean[i:end])
					wrapped.WriteString(sep)
				}
				wStr := wrapped.String()
				gotStr, err := StdEncoding.DecodeString(wStr)
				if err != nil {
					t.Fatalf("DecodeString size=%d interval=%d sep=%q error: %v", size, interval, sep, err)
				}
				if !bytes.Equal(gotStr, raw) {
					t.Fatalf("DecodeString size=%d mismatch", size)
				}

				dst := make([]byte, StdEncoding.DecodedLen(len(wStr)))
				n, err := StdEncoding.Decode(dst, []byte(wStr))
				if err != nil {
					t.Fatalf("Decode size=%d interval=%d sep=%q error: %v", size, interval, sep, err)
				}
				if !bytes.Equal(dst[:n], raw) {
					t.Fatalf("Decode size=%d mismatch", size)
				}
			}
		}

		// Non-CR/LF whitespace must still be rejected even inside large payloads.
		if len(clean) > 40 {
			for _, badByte := range []byte{' ', '\t', 0x00, 0x7f, 0xff} {
				bad := []byte(clean)
				bad[35] = badByte
				if _, err := StdEncoding.Decode(make([]byte, len(bad)), bad); err == nil {
					t.Fatalf("expected CorruptInputError for byte 0x%02x at pos 35", badByte)
				}
			}
		}
	}
}

func TestFastDecoderLargeAndSmallReads(t *testing.T) {
	encs := []struct {
		name string
		enc  *Encoding
	}{
		{"Std", StdEncoding},
		{"RawStd", RawStdEncoding},
		{"URL", URLEncoding},
		{"RawURL", RawURLEncoding},
	}
	for _, tc := range encs {
		for _, size := range []int{0, 1, 2, 3, 63, 128, 1536, 16384, 65535} {
			raw := make([]byte, size)
			for i := range raw {
				raw[i] = byte(i*29 + 3)
			}
			encStr := tc.enc.EncodeToString(raw)
			// Also test wrapped CR/LF input for large reads.
			var wrapped strings.Builder
			for i := 0; i < len(encStr); i += 76 {
				end := min(i+76, len(encStr))
				wrapped.WriteString(encStr[i:end])
				wrapped.WriteString("\r\n")
			}

			for _, input := range []string{encStr, wrapped.String()} {
				for _, readSize := range []int{1, 7, 1024, 4096, 65536} {
					dec := NewDecoder(tc.enc, strings.NewReader(input))
					buf := make([]byte, readSize)
					var out []byte
					for {
						n, err := dec.Read(buf)
						out = append(out, buf[:n]...)
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatalf("%s size=%d readSize=%d Read error: %v", tc.name, size, readSize, err)
						}
					}
					if !bytes.Equal(out, raw) {
						t.Fatalf("%s size=%d readSize=%d decoded len=%d want %d", tc.name, size, readSize, len(out), len(raw))
					}
				}
			}
		}
	}
}
