// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goobj

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"testing"
)

func TestLZ4RoundTrip(t *testing.T) {
	// Small or incompressible inputs must return ok=false.
	if _, ok := LZ4CompressIfSmaller(make([]byte, 4095)); ok {
		t.Fatal("LZ4CompressIfSmaller compressed input < 4096 bytes")
	}
	rng := rand.New(rand.NewPCG(1, 2))
	incompressible := make([]byte, 16384)
	for i := range incompressible {
		incompressible[i] = byte(rng.Uint32())
	}
	if _, ok := LZ4CompressIfSmaller(incompressible); ok {
		t.Fatal("LZ4CompressIfSmaller compressed random incompressible input")
	}

	cases := []struct {
		name string
		data []byte
	}{
		{"zeros_4096", make([]byte, 4096)},
		{"zeros_65537", make([]byte, 65537)},
		{"zeros_1m", make([]byte, 1<<20)},
		{"rle_offset1", bytes.Repeat([]byte{'A'}, 8192)},
		{"rle_offset3", bytes.Repeat([]byte("xyz"), 5000)},
		{"long_match_and_lit", func() []byte {
			var b bytes.Buffer
			for i := range 40 {
				// Long pseudo-random literal run (> 270 bytes)
				for j := range 300 {
					b.WriteByte(byte((i*31 + j*17) ^ (j >> 2)))
				}
				// Followed by a long repeated run (> 270 bytes)
				b.Write(bytes.Repeat([]byte("goobj_symbol_prefix_"), 20))
			}
			return b.Bytes()
		}()},
		{"structured_goobj_like", func() []byte {
			raw := make([]byte, 32768)
			for i := 0; i < len(raw); i += 64 {
				copy(raw[i:], "runtime.main\x00\x01\x02\x03")
				binary.LittleEndian.PutUint32(raw[i+24:], uint32(i))
			}
			return raw
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			comp, ok := LZ4CompressIfSmaller(tc.data)
			if !ok {
				t.Fatalf("LZ4CompressIfSmaller failed on compressible %s (len=%d)", tc.name, len(tc.data))
			}
			if len(comp)+len(MagicLZ4)+4 >= (len(tc.data)*7)/8 {
				t.Fatalf("compressed size %d too large for %d", len(comp), len(tc.data))
			}
			got, err := LZ4Decompress(comp, len(tc.data))
			if err != nil {
				t.Fatalf("LZ4Decompress failed: %v", err)
			}
			if !bytes.Equal(got, tc.data) {
				t.Fatalf("decompressed mismatch for %s", tc.name)
			}
		})
	}
}

func TestLZ4MalformedAndBounds(t *testing.T) {
	// Empty or invalid rawLen bounds (including decompression bombs).
	badCases := []struct {
		name   string
		src    []byte
		rawLen int
	}{
		{"zero_rawlen", []byte{0x00}, 0},
		{"negative_rawlen", []byte{0x00}, -1},
		{"over_max_rawlen", []byte{0x00}, lz4MaxRawLen + 1},
		{"empty_src", nil, 16},
		{"bomb_ratio", []byte{0x10, 'a'}, 100000},
		{"truncated_litlen_ext", []byte{0xF0}, 100},
		{"lit_past_src", []byte{0x50, 'a', 'b'}, 5},
		{"lit_past_rawlen", []byte{0x50, 'a', 'b', 'c', 'd', 'e'}, 4},
		{"short_output_eof", []byte{0x20, 'a', 'b'}, 5},
		{"trailing_bytes", []byte{0x20, 'a', 'b', 0x00}, 2},
		{"truncated_offset", []byte{0x10, 'a', 0x01}, 10},
		{"zero_offset", []byte{0x10, 'a', 0x00, 0x00}, 5},
		{"offset_past_di", []byte{0x20, 'a', 'b', 0x05, 0x00}, 6},
		{"truncated_matchlen_ext", []byte{0x1F, 'a', 0x01, 0x00}, 30},
		{"match_past_rawlen", []byte{0x14, 'a', 0x01, 0x00}, 5}, // 1 + (4+4) = 9 > 5
		{"litlen_overflow_clamp", []byte{0xF0, 0xFF, 0xFF, 0xFF}, 10},
		{"matchlen_overflow_clamp", []byte{0x1F, 'a', 0x01, 0x00, 0xFF, 0xFF}, 10},
	}

	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LZ4Decompress(tc.src, tc.rawLen); err == nil {
				t.Fatalf("LZ4Decompress(%s) unexpectedly succeeded", tc.name)
			}
		})
	}
}

func TestNewReaderFromBytesLZ4AndTruncated(t *testing.T) {
	// Build a valid goobj in memory via NewBufferWriter + Header.Put.
	w, buf := NewBufferWriter()
	h := Header{
		Magic:       Magic,
		Fingerprint: FingerprintType{1, 2, 3, 4, 5, 6, 7, 8},
		Flags:       ObjFlagStd,
	}
	h.Write(w)
	w.AddString("runtime.main")
	for i := range h.Offsets {
		h.Offsets[i] = w.Offset()
	}
	// Pad with compressible dummy payload so LZ4CompressIfSmaller triggers (>4096 bytes).
	w.Bytes(bytes.Repeat([]byte("goobj_pad_"), 600))
	h.Offsets[BlkEnd] = w.Offset()

	raw := buf.Bytes()
	h.Put(raw[:h.Size()])

	// Uncompressed reader.
	rPlain := NewReaderFromBytes(raw, true)
	if rPlain == nil {
		t.Fatal("NewReaderFromBytes(raw) returned nil")
	}
	if rPlain.Fingerprint() != h.Fingerprint || !rPlain.Std() {
		t.Fatalf("plain reader mismatch: fp=%v std=%v", rPlain.Fingerprint(), rPlain.Std())
	}

	// Compressed reader.
	comp, ok := LZ4CompressIfSmaller(raw)
	if !ok {
		t.Fatal("expected test goobj buffer to compress")
	}
	var lz4Obj bytes.Buffer
	lz4Obj.WriteString(MagicLZ4)
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(raw)))
	lz4Obj.Write(lenBuf[:])
	lz4Obj.Write(comp)

	rComp := NewReaderFromBytes(lz4Obj.Bytes(), true)
	if rComp == nil {
		t.Fatal("NewReaderFromBytes(lz4Obj) returned nil")
	}
	if rComp.Fingerprint() != h.Fingerprint || !rComp.Std() {
		t.Fatalf("compressed reader mismatch: fp=%v std=%v", rComp.Fingerprint(), rComp.Std())
	}

	// Truncated header / bad LZ4 envelopes must return nil without panicking.
	badObjs := [][]byte{
		nil,
		[]byte(Magic),
		[]byte(Magic + "short"),
		[]byte(MagicLZ4),
		[]byte(MagicLZ4 + "\x00\x00\x00\x00\x00"),                         // rawLen == 0
		[]byte(MagicLZ4 + "\xff\xff\xff\x7f\x10\x00"),                     // bomb rawLen
		[]byte(MagicLZ4 + "\x10\x00\x00\x00\xff"),                         // corrupt LZ4 stream
		append([]byte(MagicLZ4+"\x10\x00\x00\x00"), []byte("\xF0\x00")...), // mismatch len
	}
	for i, bad := range badObjs {
		if r := NewReaderFromBytes(bad, true); r != nil {
			t.Fatalf("case %d: NewReaderFromBytes unexpectedly succeeded on malformed input", i)
		}
	}
}

func FuzzLZ4Decompress(f *testing.F) {
	f.Add([]byte{0x30, 'a', 'b', 'c'}, 3)
	f.Add([]byte{0x10, 'a', 0x01, 0x00}, 6)
	f.Fuzz(func(t *testing.T, src []byte, rawLen int) {
		if rawLen > 1<<20 {
			return
		}
		_, _ = LZ4Decompress(src, rawLen)
	})
}
