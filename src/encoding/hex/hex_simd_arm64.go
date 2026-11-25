// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build arm64

package hex

import "unsafe"

const (
	haveSIMD         = true
	minSIMDEncode    = 16
	minSIMDDecode    = 32
	minSIMDDecodeOut = 16
)

var neonEncLUT = [16]byte{
	'0', '1', '2', '3', '4', '5', '6', '7',
	'8', '9', 'a', 'b', 'c', 'd', 'e', 'f',
}

// neonDecLUT maps (c - '0') for c in ['0'..'o'] (64 bytes):
// valid hex chars map to (nibble | 0x80), invalid chars map to 0x00.
var neonDecLUT = [64]byte{
	'0' - '0': 0x80 | 0x0,
	'1' - '0': 0x80 | 0x1,
	'2' - '0': 0x80 | 0x2,
	'3' - '0': 0x80 | 0x3,
	'4' - '0': 0x80 | 0x4,
	'5' - '0': 0x80 | 0x5,
	'6' - '0': 0x80 | 0x6,
	'7' - '0': 0x80 | 0x7,
	'8' - '0': 0x80 | 0x8,
	'9' - '0': 0x80 | 0x9,
	'A' - '0': 0x80 | 0xa,
	'B' - '0': 0x80 | 0xb,
	'C' - '0': 0x80 | 0xc,
	'D' - '0': 0x80 | 0xd,
	'E' - '0': 0x80 | 0xe,
	'F' - '0': 0x80 | 0xf,
	'a' - '0': 0x80 | 0xa,
	'b' - '0': 0x80 | 0xb,
	'c' - '0': 0x80 | 0xc,
	'd' - '0': 0x80 | 0xd,
	'e' - '0': 0x80 | 0xe,
	'f' - '0': 0x80 | 0xf,
}

//go:noescape
func encodeNEON(dst, src *byte, n int, lut *byte)

//go:noescape
func decodeNEON(dst, src *byte, n int, lut *byte) int

func dumpLinesSIMD(out, data []byte, offset uint32) int {
	return 0
}

func encodeSIMD(dst, src []byte) (si, di int) {
	n := len(src) &^ 15
	if n == 0 || hasUnsafeForwardOverlap(dst, src, n*2, n) {
		return 0, 0
	}
	encodeNEON(&dst[0], &src[0], n, &neonEncLUT[0])
	return n, n * 2
}

func decodeSIMD(dst, src []byte) (si, di int) {
	if len(src) < minSIMDDecode || len(dst) < minSIMDDecodeOut {
		return 0, 0
	}
	if hasUnsafeForwardOverlap(dst, src, len(src)/2, len(src)) {
		return 0, 0
	}
	n := min(len(src)/32, len(dst)/16) * 32
	if n == 0 {
		return 0, 0
	}
	consumed := decodeNEON(&dst[0], &src[0], n, &neonDecLUT[0])
	return consumed, consumed / 2
}

func hasUnsafeForwardOverlap(dst, src []byte, dstLen, srcLen int) bool {
	if dstLen == 0 || srcLen == 0 {
		return false
	}
	d0 := uintptr(unsafe.Pointer(&dst[0]))
	s0 := uintptr(unsafe.Pointer(&src[0]))
	d1 := d0 + uintptr(dstLen)
	s1 := s0 + uintptr(srcLen)
	return d0 < s1 && s0 < d1 && d0 > s0
}
