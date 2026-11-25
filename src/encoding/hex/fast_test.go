// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package hex_test

import (
	"bytes"
	"crypto/sha256"
	. "encoding/hex"
	"errors"
	"fmt"
	"internal/race"
	"io"
	"slices"
	"strings"
	"testing"
)

func refEncode(src []byte) []byte {
	const digits = "0123456789abcdef"
	out := make([]byte, len(src)*2)
	for i, b := range src {
		out[i*2] = digits[b>>4]
		out[i*2+1] = digits[b&0x0f]
	}
	return out
}

func refDecodeByte(b byte) (byte, bool) {
	switch {
	case '0' <= b && b <= '9':
		return b - '0', true
	case 'a' <= b && b <= 'f':
		return b - 'a' + 10, true
	case 'A' <= b && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}

func refDecode(dst, src []byte) (int, error) {
	i := 0
	j := 0
	for ; j < len(src)-1; j += 2 {
		a, ok := refDecodeByte(src[j])
		if !ok {
			return i, InvalidByteError(src[j])
		}
		b, ok := refDecodeByte(src[j+1])
		if !ok {
			return i, InvalidByteError(src[j+1])
		}
		dst[i] = (a << 4) | b
		i++
	}
	if j == len(src)-1 {
		if _, ok := refDecodeByte(src[j]); !ok {
			return i, InvalidByteError(src[j])
		}
		return i, ErrLength
	}
	return i, nil
}

func makeDeterministicBytes(n int, seed byte) []byte {
	out := make([]byte, n)
	h := sha256.Sum256([]byte{seed, byte(n), byte(n >> 8)})
	for i := range out {
		out[i] = h[i%len(h)] ^ byte(i*17+int(seed))
	}
	return out
}

func TestFastDifferentialAllLengths(t *testing.T) {
	lengths := make([]int, 0, 200)
	for n := 0; n <= 140; n++ {
		lengths = append(lengths, n)
	}
	lengths = append(lengths, 191, 192, 193, 255, 256, 257, 511, 512, 513, 1024, 1535, 2048, 4096)

	for _, n := range lengths {
		src := makeDeterministicBytes(n, byte(n))
		want := refEncode(src)

		// Encode
		dst := make([]byte, EncodedLen(n))
		en := Encode(dst, src)
		if en != len(want) || !bytes.Equal(dst, want) {
			t.Fatalf("len=%d Encode mismatch: got n=%d want=%d", n, en, len(want))
		}

		// EncodeToString
		if s := EncodeToString(src); s != string(want) {
			t.Fatalf("len=%d EncodeToString mismatch", n)
		}

		// AppendEncode
		app := AppendEncode([]byte("pre:"), src)
		if string(app[:4]) != "pre:" || !bytes.Equal(app[4:], want) {
			t.Fatalf("len=%d AppendEncode mismatch", n)
		}

		// Lowercase Decode
		dec := make([]byte, DecodedLen(len(want)))
		dn, err := Decode(dec, want)
		if err != nil || dn != n || !bytes.Equal(dec, src) {
			t.Fatalf("len=%d Decode lower failed: dn=%d err=%v", n, dn, err)
		}

		// Mixed-case Decode & DecodeString & AppendDecode
		mixed := bytes.ToUpper(slices.Clone(want))
		for i := 0; i < len(mixed); i += 3 {
			mixed[i] = want[i]
		}
		dn, err = Decode(dec, mixed)
		if err != nil || dn != n || !bytes.Equal(dec, src) {
			t.Fatalf("len=%d Decode mixed failed: dn=%d err=%v", n, dn, err)
		}
		ds, err := DecodeString(string(mixed))
		if err != nil || !bytes.Equal(ds, src) {
			t.Fatalf("len=%d DecodeString failed: err=%v", n, err)
		}
		da, err := AppendDecode([]byte("ok:"), mixed)
		if err != nil || string(da[:3]) != "ok:" || !bytes.Equal(da[3:], src) {
			t.Fatalf("len=%d AppendDecode failed: err=%v", n, err)
		}
	}
}

func TestFastAlignmentCanaryAndOverlap(t *testing.T) {
	raw := makeDeterministicBytes(1024+64, 99)
	for _, n := range []int{0, 1, 2, 3, 7, 8, 15, 16, 17, 31, 32, 33, 63, 64, 65, 127, 128, 129, 513} {
		for srcOff := 0; srcOff < 32; srcOff += 7 {
			src := raw[srcOff : srcOff+n]
			want := refEncode(src)
			for dstOff := 0; dstOff < 32; dstOff += 5 {
				dstBuf := make([]byte, dstOff+EncodedLen(n)+16)
				copy(dstBuf[:dstOff], bytes.Repeat([]byte{0xAA}, dstOff))
				copy(dstBuf[dstOff+EncodedLen(n):], bytes.Repeat([]byte{0xBB}, 16))

				Encode(dstBuf[dstOff:], src)
				if !bytes.Equal(dstBuf[dstOff:dstOff+len(want)], want) {
					t.Fatalf("aligned Encode failed n=%d srcOff=%d dstOff=%d", n, srcOff, dstOff)
				}
				if !bytes.Equal(dstBuf[:dstOff], bytes.Repeat([]byte{0xAA}, dstOff)) {
					t.Fatalf("Encode underflow clobbered prefix dstOff=%d", dstOff)
				}
				if !bytes.Equal(dstBuf[dstOff+EncodedLen(n):], bytes.Repeat([]byte{0xBB}, 16)) {
					t.Fatalf("Encode overflow clobbered suffix n=%d", n)
				}
			}

			// In-place Encode when src occupies the second half of a 2*n buffer.
			encBuf := make([]byte, 2*n+16)
			copy(encBuf[n:2*n], src)
			Encode(encBuf[:2*n], encBuf[n:2*n])
			if !bytes.Equal(encBuf[:2*n], want) {
				t.Fatalf("in-place half Encode n=%d mismatch", n)
			}

			// In-place Decode(buf, buf)
			decBuf := make([]byte, len(want)+32)
			copy(decBuf, want)
			dn, err := Decode(decBuf, decBuf[:len(want)])
			if err != nil || dn != n || !bytes.Equal(decBuf[:dn], src) {
				t.Fatalf("in-place Decode n=%d failed: dn=%d err=%v", n, dn, err)
			}

			// Overlapping Decode(buf, buf[off:]) where &dst[0] <= &src[0]
			for _, off := range []int{1, 3, 17, 40} {
				backBuf := make([]byte, off+len(want)+16)
				copy(backBuf[off:], want)
				dn, err := Decode(backBuf, backBuf[off:off+len(want)])
				if err != nil || dn != n || !bytes.Equal(backBuf[:dn], src) {
					t.Fatalf("overlap Decode(buf, buf[%d:]) n=%d mismatch: got (%d, %v)", off, n, dn, err)
				}
			}
		}
	}
}

func TestFastDecodeErrorPrecisionAndAllBytes(t *testing.T) {
	sizes := []int{1, 2, 7, 8, 15, 16, 31, 32, 33, 63, 64, 65, 127, 128, 129, 256}
	for _, n := range sizes {
		src := makeDeterministicBytes(n, 7)
		encoded := refEncode(src)

		// Every position and boundary byte value around valid ASCII hex ranges.
		probeBytes := []byte{
			0x00, 0x09, 0x0a, 0x0d, ' ', '/', '0' - 1, '9' + 1,
			'@', 'G', 'F' + 1, '`', 'g', 'f' + 1, 0x7f, 0x80, 0xff,
		}
		positions := []int{0, 1, 7, 8, 15, 16, 31, 32, 63, 64, len(encoded) - 2, len(encoded) - 1}
		for _, pos := range positions {
			if pos < 0 || pos >= len(encoded) {
				continue
			}
			for _, bad := range probeBytes {
				corrupt := slices.Clone(encoded)
				corrupt[pos] = bad
				gotOut := make([]byte, DecodedLen(len(corrupt)))
				wantOut := make([]byte, DecodedLen(len(corrupt)))
				gotN, gotErr := Decode(gotOut, corrupt)
				wantN, wantErr := refDecode(wantOut, corrupt)
				if gotN != wantN || !errors.Is(gotErr, wantErr) || !bytes.Equal(gotOut[:gotN], wantOut[:wantN]) {
					t.Fatalf("n=%d pos=%d bad=%#x mismatch: got (%d, %v), want (%d, %v)", n, pos, bad, gotN, gotErr, wantN, wantErr)
				}
			}
		}

		// Odd-length truncation with valid and invalid trailing byte.
		if len(encoded) >= 2 {
			oddValid := encoded[:len(encoded)-1]
			gotOut := make([]byte, DecodedLen(len(oddValid)))
			wantOut := make([]byte, DecodedLen(len(oddValid)))
			gotN, gotErr := Decode(gotOut, oddValid)
			wantN, wantErr := refDecode(wantOut, oddValid)
			if gotN != wantN || !errors.Is(gotErr, wantErr) || !bytes.Equal(gotOut[:gotN], wantOut[:wantN]) {
				t.Fatalf("odd valid n=%d mismatch: got (%d, %v), want (%d, %v)", n, gotN, gotErr, wantN, wantErr)
			}

			oddInvalid := append(slices.Clone(oddValid[:len(oddValid)-1]), 'z')
			gotN, gotErr = Decode(gotOut, oddInvalid)
			wantN, wantErr = refDecode(wantOut, oddInvalid)
			if gotN != wantN || !errors.Is(gotErr, wantErr) || !bytes.Equal(gotOut[:gotN], wantOut[:wantN]) {
				t.Fatalf("odd invalid n=%d mismatch: got (%d, %v), want (%d, %v)", n, gotN, gotErr, wantN, wantErr)
			}
		}
	}

	// Exhaustively test every single byte 0x00..0xff in a 64-byte AVX2 block at both even and odd offsets.
	base := bytes.Repeat([]byte("aB09"), 16)
	for b := range 256 {
		_, valid := refDecodeByte(byte(b))
		for _, pos := range []int{0, 1, 30, 31, 32, 33, 62, 63} {
			buf := slices.Clone(base)
			buf[pos] = byte(b)
			gotOut := make([]byte, 32)
			wantOut := make([]byte, 32)
			gotN, gotErr := Decode(gotOut, buf)
			wantN, wantErr := refDecode(wantOut, buf)
			if valid && (gotErr != nil || gotN != 32) {
				t.Fatalf("valid byte %#x at pos %d rejected: n=%d err=%v", b, pos, gotN, gotErr)
			}
			if !valid && (gotN != wantN || !errors.Is(gotErr, wantErr) || !bytes.Equal(gotOut[:gotN], wantOut[:wantN])) {
				t.Fatalf("invalid byte %#x at pos %d mismatch: got (%d, %v), want (%d, %v)", b, pos, gotN, gotErr, wantN, wantErr)
			}
		}
	}
}

func TestFastStreamingDecoderAndDumper(t *testing.T) {
	// Large streaming Decode through NewDecoder hitting both small and >=1024-byte Read buffers.
	for _, n := range []int{0, 1, 15, 511, 512, 513, 1024, 4096, 8193} {
		raw := makeDeterministicBytes(n, 31)
		encoded := EncodeToString(raw)

		// Large read buffer (>= 1024)
		for _, readBufSize := range []int{1024, 1025, 4096} {
			dec := NewDecoder(strings.NewReader(encoded))
			var out bytes.Buffer
			buf := make([]byte, readBufSize)
			for {
				nr, err := dec.Read(buf)
				if nr > 0 {
					out.Write(buf[:nr])
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("n=%d buf=%d Read error: %v", n, readBufSize, err)
				}
			}
			if !bytes.Equal(out.Bytes(), raw) {
				t.Fatalf("n=%d buf=%d decoded mismatch", n, readBufSize)
			}
		}

		// Chunked odd-sized reader + large read buffer
		dec := NewDecoder(&chunkedReader{data: []byte(encoded), step: 17})
		got, err := io.ReadAll(dec)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("n=%d chunked NewDecoder failed: err=%v len=%d", n, err, len(got))
		}

		// Dump vs Dumper equivalence
		wantDump := Dump(raw)
		var dBuf bytes.Buffer
		dw := Dumper(&dBuf)
		if _, err := dw.Write(raw); err != nil {
			t.Fatalf("n=%d Dumper.Write err=%v", n, err)
		}
		if err := dw.Close(); err != nil {
			t.Fatalf("n=%d Dumper.Close err=%v", n, err)
		}
		if dBuf.String() != wantDump {
			t.Fatalf("n=%d Dump vs Dumper mismatch:\nDump:\n%s\nDumper:\n%s", n, wantDump, dBuf.String())
		}
	}

	// Error cases on large Read buffer in NewDecoder
	for _, tc := range []struct {
		in      string
		wantOut string
		wantErr error
	}{
		{"", "", io.EOF},
		{"a", "", io.ErrUnexpectedEOF},
		{"z", "", InvalidByteError('z')},
		{"0001z", "\x00\x01", InvalidByteError('z')},
		{"00012", "\x00\x01", io.ErrUnexpectedEOF},
		{strings.Repeat("ab", 600) + "z", string(bytes.Repeat([]byte{0xab}, 600)), InvalidByteError('z')},
		{strings.Repeat("ab", 600) + "c", string(bytes.Repeat([]byte{0xab}, 600)), io.ErrUnexpectedEOF},
		{strings.Repeat("ab", 600) + "0g", string(bytes.Repeat([]byte{0xab}, 600)), InvalidByteError('g')},
	} {
		dec := NewDecoder(strings.NewReader(tc.in))
		buf := make([]byte, 2048)
		var out []byte
		var gotErr error
		for {
			nr, err := dec.Read(buf)
			out = append(out, buf[:nr]...)
			if err != nil {
				gotErr = err
				break
			}
		}
		if string(out) != tc.wantOut || !errors.Is(gotErr, tc.wantErr) {
			t.Fatalf("large Read(%q) = (%q, %v), want (%q, %v)", tc.in, out, gotErr, tc.wantOut, tc.wantErr)
		}
	}
}

type chunkedReader struct {
	data []byte
	step int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), c.step, len(c.data))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

func TestFastAllocations(t *testing.T) {
	if race.Enabled {
		t.Skip("skipping allocation assertions under race detector")
	}
	srcSmall := makeDeterministicBytes(16, 1)
	srcLarge := makeDeterministicBytes(1024, 2)
	dstSmall := make([]byte, EncodedLen(len(srcSmall)))
	dstLarge := make([]byte, EncodedLen(len(srcLarge)))
	encSmall := refEncode(srcSmall)
	encLarge := refEncode(srcLarge)

	if allocs := testing.AllocsPerRun(100, func() {
		Encode(dstSmall, srcSmall)
	}); allocs != 0 {
		t.Errorf("Encode(16) allocs = %v, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		Encode(dstLarge, srcLarge)
	}); allocs != 0 {
		t.Errorf("Encode(1024) allocs = %v, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		_, _ = Decode(srcSmall, encSmall)
	}); allocs != 0 {
		t.Errorf("Decode(16) allocs = %v, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		_, _ = Decode(srcLarge, encLarge)
	}); allocs != 0 {
		t.Errorf("Decode(1024) allocs = %v, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		_ = EncodeToString(srcSmall)
	}); allocs != 1 {
		t.Errorf("EncodeToString(16) allocs = %v, want 1", allocs)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		_ = EncodeToString(srcLarge)
	}); allocs != 1 {
		t.Errorf("EncodeToString(1024) allocs = %v, want 1", allocs)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		_ = Dump(srcSmall)
	}); allocs != 1 {
		t.Errorf("Dump(16) allocs = %v, want 1", allocs)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		_ = Dump(srcLarge)
	}); allocs != 1 {
		t.Errorf("Dump(1024) allocs = %v, want 1", allocs)
	}
}

func BenchmarkEncodeToString(b *testing.B) {
	for _, size := range []int{16, 64, 1024, 8192} {
		src := makeDeterministicBytes(size, 5)
		b.Run(fmt.Sprintf("%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				_ = EncodeToString(src)
			}
		})
	}
}

func BenchmarkEncoderWriter(b *testing.B) {
	src := makeDeterministicBytes(8192, 9)
	b.ReportAllocs()
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		w := NewEncoder(io.Discard)
		if _, err := w.Write(src); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecoderReader(b *testing.B) {
	encoded := refEncode(makeDeterministicBytes(8192, 11))
	dst := make([]byte, 8192)
	var r bytes.Reader
	b.ReportAllocs()
	b.SetBytes(int64(len(encoded)))
	for b.Loop() {
		r.Reset(encoded)
		dec := NewDecoder(&r)
		for {
			_, err := dec.Read(dst)
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkDumperWriter(b *testing.B) {
	src := makeDeterministicBytes(4096, 13)
	b.ReportAllocs()
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		dw := Dumper(io.Discard)
		if _, err := dw.Write(src); err != nil {
			b.Fatal(err)
		}
		if err := dw.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
