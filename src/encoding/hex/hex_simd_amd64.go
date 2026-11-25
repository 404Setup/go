// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64

package hex

import (
	"internal/cpu"
	"unsafe"
)

var (
	haveAVX2         = cpu.X86.HasAVX2
	haveSSSE3        = cpu.X86.HasSSSE3
	haveSIMD         = haveAVX2 || haveSSSE3
	minSIMDEncode    = 16
	minSIMDDecode    = 32
	minSIMDDecodeOut = 16
)

func init() {
	if !haveAVX2 && haveSSSE3 {
		minSIMDDecode = 16
		minSIMDDecodeOut = 8
	}
}

//go:noescape
func encodeAVX2Asm(dst, src *byte, n int)

//go:noescape
func decodeAVX2Asm(dst, src *byte, srcLen, dstLen int) (si, di int)

//go:noescape
func encodeSSSE3Asm(dst, src *byte, n int)

//go:noescape
func decodeSSSE3Asm(dst, src *byte, srcLen, dstLen int) (si, di int)

func encodeSIMD(dst, src []byte) (si, di int) {
	n := len(src) &^ 15
	if n == 0 {
		return 0, 0
	}
	if hasUnsafeForwardOverlap(dst, src, n*2, n) {
		return 0, 0
	}
	if haveAVX2 {
		encodeAVX2Asm(&dst[0], &src[0], n)
		return n, n * 2
	}
	if haveSSSE3 {
		encodeSSSE3Asm(&dst[0], &src[0], n)
		return n, n * 2
	}
	return 0, 0
}

func decodeSIMD(dst, src []byte) (si, di int) {
	if len(src) < minSIMDDecode || len(dst) < minSIMDDecodeOut {
		return 0, 0
	}
	if hasUnsafeForwardOverlap(dst, src, len(src)/2, len(src)) {
		return 0, 0
	}
	if haveAVX2 {
		return decodeAVX2Asm(&dst[0], &src[0], len(src), len(dst))
	}
	if haveSSSE3 {
		return decodeSSSE3Asm(&dst[0], &src[0], len(src), len(dst))
	}
	return 0, 0
}

// hasUnsafeForwardOverlap reports whether dst and src overlap with &dst[0] > &src[0],
// or in Encode when dst overlaps src in a direction that SIMD batching would clobber.
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
