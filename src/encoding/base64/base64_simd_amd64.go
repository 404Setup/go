// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64

package base64

import "internal/cpu"

var (
	haveAVX2              = cpu.X86.HasAVX2
	haveSSSE3             = cpu.X86.HasSSSE3
	haveSIMD              = haveAVX2 || haveSSSE3
	haveStripNewlinesSIMD = haveAVX2
	minSIMDEncode         = 28
	minSIMDEncodeOut      = 32
	minSIMDDecode         = 32
	minSIMDDecodeOut      = 24
)

func init() {
	if !haveAVX2 && haveSSSE3 {
		minSIMDEncode = 16
		minSIMDEncodeOut = 16
		minSIMDDecode = 16
		minSIMDDecodeOut = 12
	}
}

//go:noescape
func encodeAVX2Asm(dst, src *byte, srcLen, dstLen int, kind uint8) (si, di int)

//go:noescape
func decodeAVX2Asm(dst, src *byte, srcLen, dstLen int, kind uint8) (si, di int)

//go:noescape
func encodeSSSE3Asm(dst, src *byte, srcLen, dstLen int, kind uint8) (si, di int)

//go:noescape
func decodeSSSE3Asm(dst, src *byte, srcLen, dstLen int, kind uint8) (si, di int)

//go:noescape
func stripNewlinesAVX2Asm(p *byte, n int) int

func encodeSIMD(dst, src []byte, kind encodingKind) (si, di int) {
	if haveAVX2 {
		if len(src) >= 28 && len(dst) >= 32 {
			return encodeAVX2Asm(&dst[0], &src[0], len(src), len(dst), uint8(kind))
		}
		return 0, 0
	}
	if haveSSSE3 && len(src) >= 16 && len(dst) >= 16 {
		return encodeSSSE3Asm(&dst[0], &src[0], len(src), len(dst), uint8(kind))
	}
	return 0, 0
}

func decodeSIMD(dst, src []byte, si, di int, kind encodingKind) (int, int) {
	if haveAVX2 {
		if len(src)-si >= 32 && len(dst)-di >= 24 {
			nsi, ndi := decodeAVX2Asm(&dst[di], &src[si], len(src)-si, len(dst)-di, uint8(kind))
			si += nsi
			di += ndi
		}
		return si, di
	}
	if haveSSSE3 && len(src)-si >= 16 && len(dst)-di >= 12 {
		nsi, ndi := decodeSSSE3Asm(&dst[di], &src[si], len(src)-si, len(dst)-di, uint8(kind))
		si += nsi
		di += ndi
	}
	return si, di
}

func stripNewlinesSIMD(p []byte) int {
	return stripNewlinesAVX2Asm(&p[0], len(p))
}
