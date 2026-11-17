// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http3

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrefixedInt(t *testing.T) {
	st1, st2 := newStreamPair(t)
	for _, test := range []struct {
		value     int64
		prefixLen uint8
		encoded   []byte
	}{
		// https://www.rfc-editor.org/rfc/rfc7541#appendix-C.1.1
		{
			value:     10,
			prefixLen: 5,
			encoded: []byte{
				0b_0000_1010,
			},
		},
		// https://www.rfc-editor.org/rfc/rfc7541#appendix-C.1.2
		{
			value:     1337,
			prefixLen: 5,
			encoded: []byte{
				0b0001_1111,
				0b1001_1010,
				0b0000_1010,
			},
		},
		// https://www.rfc-editor.org/rfc/rfc7541#appendix-C.1.3
		{
			value:     42,
			prefixLen: 8,
			encoded: []byte{
				0b0010_1010,
			},
		},
	} {
		highBitMask := ^((byte(1) << test.prefixLen) - 1)
		for _, highBits := range []byte{
			0, highBitMask, 0b1010_1010 & highBitMask,
		} {
			gotEnc := appendPrefixedInt(nil, highBits, test.prefixLen, test.value)
			wantEnc := append([]byte{}, test.encoded...)
			wantEnc[0] |= highBits
			if !bytes.Equal(gotEnc, wantEnc) {
				t.Errorf("appendPrefixedInt(nil, 0b%08b, %v, %v) = {%x}, want {%x}",
					highBits, test.prefixLen, test.value, gotEnc, wantEnc)
			}

			st1.Write(gotEnc)
			if err := st1.Flush(); err != nil {
				t.Fatal(err)
			}
			gotFirstByte, v, err := st2.readPrefixedInt(test.prefixLen)
			if err != nil || gotFirstByte&highBitMask != highBits || v != test.value {
				t.Errorf("st.readPrefixedInt(%v) = 0b%08b, %v, %v; want 0b%08b, %v, nil", test.prefixLen, gotFirstByte, v, err, highBits, test.value)
			}
		}
	}
}

func TestPrefixedString(t *testing.T) {
	st1, st2 := newStreamPair(t)
	for _, test := range []struct {
		value     string
		prefixLen uint8
		encoded   []byte
	}{
		// https://www.rfc-editor.org/rfc/rfc7541#appendix-C.6.1
		{
			value:     "302",
			prefixLen: 7,
			encoded: []byte{
				0x82, // H bit + length 2
				0x64, 0x02,
			},
		},
		{
			value:     "private",
			prefixLen: 5,
			encoded: []byte{
				0x25, // H bit + length 5
				0xae, 0xc3, 0x77, 0x1a, 0x4b,
			},
		},
		{
			value:     "Mon, 21 Oct 2013 20:13:21 GMT",
			prefixLen: 7,
			encoded: []byte{
				0x96, // H bit + length 22
				0xd0, 0x7a, 0xbe, 0x94, 0x10, 0x54, 0xd4, 0x44,
				0xa8, 0x20, 0x05, 0x95, 0x04, 0x0b, 0x81, 0x66,
				0xe0, 0x82, 0xa6, 0x2d, 0x1b, 0xff,
			},
		},
		{
			value:     "https://www.example.com",
			prefixLen: 7,
			encoded: []byte{
				0x91, // H bit + length 17
				0x9d, 0x29, 0xad, 0x17, 0x18, 0x63, 0xc7, 0x8f,
				0x0b, 0x97, 0xc8, 0xe9, 0xae, 0x82, 0xae, 0x43,
				0xd3,
			},
		},
		// Not Huffman encoded (encoded size == unencoded size).
		{
			value:     "a",
			prefixLen: 7,
			encoded: []byte{
				0x01, // length 1
				0x61,
			},
		},
		// Empty string.
		{
			value:     "",
			prefixLen: 7,
			encoded: []byte{
				0x00, // length 0
			},
		},
	} {
		highBitMask := ^((byte(1) << (test.prefixLen + 1)) - 1)
		for _, highBits := range []byte{
			0, highBitMask, 0b1010_1010 & highBitMask,
		} {
			gotEnc := appendPrefixedString(nil, highBits, test.prefixLen, test.value)
			wantEnc := append([]byte{}, test.encoded...)
			wantEnc[0] |= highBits
			if !bytes.Equal(gotEnc, wantEnc) {
				t.Errorf("appendPrefixedString(nil, 0b%08b, %v, %v) = {%x}, want {%x}",
					highBits, test.prefixLen, test.value, gotEnc, wantEnc)
			}

			st1.Write(gotEnc)
			if err := st1.Flush(); err != nil {
				t.Fatal(err)
			}
			gotFirstByte, v, err := st2.readPrefixedString(test.prefixLen)
			if err != nil || gotFirstByte&highBitMask != highBits || v != test.value {
				t.Errorf("st.readPrefixedInt(%v) = 0b%08b, %q, %v; want 0b%08b, %q, nil", test.prefixLen, gotFirstByte, v, err, highBits, test.value)
			}
		}
	}
}

func TestHuffmanDecodingFailure(t *testing.T) {
	st1, st2 := newStreamPair(t)
	st1.Write([]byte{
		0x82, // H bit + length 4
		0b_1111_1111,
		0b_1111_1111,
		0b_1111_1111,
		0b_1111_1111,
	})
	if err := st1.Flush(); err != nil {
		t.Fatal(err)
	}
	if b, v, err := st2.readPrefixedString(7); err == nil {
		t.Fatalf("readPrefixedString(7) = %x, %v, nil; want error", b, v)
	}
}

func TestQPACKScratchBoundary(t *testing.T) {
	st1, st2 := newStreamPair(t)
	// Use uncompressible byte pattern (Huffman len >= raw len) and highly compressible ASCII
	// so both plain and Huffman branches cross the 256-byte scratch buffer boundary.
	makeUncompressible := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(0x80 + (i % 64))
		}
		return string(b)
	}
	for _, size := range []int{255, 256, 257, 410, 1024} {
		for _, prefixLen := range []uint8{3, 7} {
			for _, tc := range []struct {
				name  string
				value string
			}{
				{"plain", makeUncompressible(size)},
				{"huffman", strings.Repeat("a", size)},
			} {
				enc := appendPrefixedString(nil, 0, prefixLen, tc.value)
				if _, err := st1.Write(enc); err != nil {
					t.Fatal(err)
				}
				if err := st1.Flush(); err != nil {
					t.Fatal(err)
				}
				_, got, err := st2.readPrefixedString(prefixLen)
				if err != nil {
					t.Fatalf("size=%d prefix=%d %s: readPrefixedString error: %v", size, prefixLen, tc.name, err)
				}
				if got != tc.value {
					t.Fatalf("size=%d prefix=%d %s: mismatch (got len=%d want len=%d)", size, prefixLen, tc.name, len(got), len(tc.value))
				}
			}
		}
	}
}

func BenchmarkQPACKPrefixedString(b *testing.B) {
	for _, tc := range []struct {
		name string
		s    string
	}{
		{"Small", "application/json; charset=utf-8"},
		{"Boundary256", strings.Repeat("abcd1234", 32)},
		{"Large1024", strings.Repeat("abcd1234", 128)},
	} {
		b.Run("Encode/"+tc.name, func(b *testing.B) {
			var dst []byte
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				dst = appendPrefixedString(dst[:0], 0, 7, tc.s)
			}
		})
	}

	b.Run("EncoderHeaders", func(b *testing.B) {
		var qe qpackEncoder
		qe.init()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = qe.encode(func(emit func(itype indexType, name, value string)) {
				emit(mayIndex, ":status", "200")
				emit(mayIndex, "Content-Type", "application/json")
				emit(mayIndex, "Cache-Control", "max-age=3600")
				emit(mayIndex, "X-Custom-Header", "value")
			})
		}
	})

	b.Run("Decode/Small", func(b *testing.B) {
		st1, st2 := newStreamPair(b)
		done := make(chan struct{})
		enc := appendPrefixedString(nil, 0, 7, "application/json; charset=utf-8")
		go func() {
			defer close(done)
			for i := range b.N {
				if _, err := st1.Write(enc); err != nil {
					return
				}
				if i&63 == 63 {
					if err := st1.Flush(); err != nil {
						return
					}
				}
			}
			_ = st1.Flush()
			st1.CloseWrite()
		}()
		b.Cleanup(func() {
			st1.CloseWrite()
			<-done
		})
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := st2.readPrefixedString(7); err != nil {
				b.Fatal(err)
			}
		}
		<-done
	})
}

