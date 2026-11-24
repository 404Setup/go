// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build arm64

package base64

const (
	haveSIMD              = true
	haveStripNewlinesSIMD = false
	minSIMDEncode         = 48
	minSIMDEncodeOut      = 64
	minSIMDDecode         = 64
	minSIMDDecodeOut      = 48
)

func stripNewlinesSIMD(p []byte) int { return 0 }

var (
	neonEncStd = [64]byte{
		'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'I', 'J', 'K', 'L', 'M', 'N', 'O', 'P',
		'Q', 'R', 'S', 'T', 'U', 'V', 'W', 'X', 'Y', 'Z', 'a', 'b', 'c', 'd', 'e', 'f',
		'g', 'h', 'i', 'j', 'k', 'l', 'm', 'n', 'o', 'p', 'q', 'r', 's', 't', 'u', 'v',
		'w', 'x', 'y', 'z', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', '+', '/',
	}
	neonEncURL = [64]byte{
		'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'I', 'J', 'K', 'L', 'M', 'N', 'O', 'P',
		'Q', 'R', 'S', 'T', 'U', 'V', 'W', 'X', 'Y', 'Z', 'a', 'b', 'c', 'd', 'e', 'f',
		'g', 'h', 'i', 'j', 'k', 'l', 'm', 'n', 'o', 'p', 'q', 'r', 's', 't', 'u', 'v',
		'w', 'x', 'y', 'z', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', '-', '_',
	}

	// 128-entry ASCII lookup tables for NEON/SVE2 decoding:
	// Valid base64 bytes map to (sextet | 0x80); invalid ASCII bytes map to 0x00.
	neonDecStd = makeDecodeLUT128(stdAlphabet)
	neonDecURL = makeDecodeLUT128(urlAlphabet)
)

func makeDecodeLUT128(alphabet string) [128]byte {
	var lut [128]byte
	for i := 0; i < 64; i++ {
		lut[alphabet[i]] = byte(i) | 0x80
	}
	return lut
}

//go:noescape
func encodeNEON(dst, src *byte, n int, lut *byte)

//go:noescape
func decodeNEON(dst, src *byte, n int, lut *byte) int

func encodeSIMD(dst, src []byte, kind encodingKind) (si, di int) {
	chunks := min(len(src)/48, len(dst)/64)
	if chunks <= 0 {
		return 0, 0
	}
	lut := &neonEncStd[0]
	if kind == encURL {
		lut = &neonEncURL[0]
	}
	if haveSVE2 && chunks >= 2 {
		n, m := encodeSVE2(&dst[di], &src[si], chunks*48, lut)
		si += n
		di += m
		if len(src)-si < 48 || len(dst)-di < 64 {
			return si, di
		}
		chunks = min((len(src)-si)/48, (len(dst)-di)/64)
	}
	if chunks > 0 {
		n := chunks * 48
		encodeNEON(&dst[di], &src[si], n, lut)
		si += n
		di += chunks * 64
	}
	return si, di
}

func decodeSIMD(dst, src []byte, si, di int, kind encodingKind) (int, int) {
	chunks := min((len(src)-si)/64, (len(dst)-di)/48)
	if chunks <= 0 {
		return si, di
	}
	lut := &neonDecStd[0]
	if kind == encURL {
		lut = &neonDecURL[0]
	}
	if haveSVE2 && chunks >= 2 {
		consumed := decodeSVE2(&dst[di], &src[si], chunks*64, lut)
		si += consumed
		di += (consumed / 64) * 48
		chunks = min((len(src)-si)/64, (len(dst)-di)/48)
		if chunks <= 0 {
			return si, di
		}
	}
	consumed := decodeNEON(&dst[di], &src[si], chunks*64, lut)
	si += consumed
	di += (consumed / 64) * 48
	return si, di
}
