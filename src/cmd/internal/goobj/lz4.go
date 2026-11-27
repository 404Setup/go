// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goobj

import (
	"encoding/binary"
	"errors"
	"math/bits"
	"sync"
)

const (
	// MagicLZ4 identifies a Go object payload compressed with an LZ4 block
	// following an 8-byte magic and a 4-byte little-endian uncompressed length.
	MagicLZ4 = "\x00go120lz"

	lz4MinMatch     = 4
	lz4LastLiterals = 5
	lz4MFLimit      = 12
	lz4HashLog      = 14
	lz4HashSize     = 1 << lz4HashLog
	lz4MaxDistance  = 65535
	lz4MaxRawLen    = 512 << 20 // 512 MiB decompression bound
)

var (
	errLZ4Corrupt = errors.New("goobj: corrupt lz4 object data")
	lz4TablePool  = sync.Pool{
		New: func() any {
			return new([lz4HashSize]uint32)
		},
	}
)

func lz4Hash(v uint32) uint32 {
	return (v * 2654435761) >> (32 - lz4HashLog)
}

// LZ4CompressIfSmaller compresses src as an LZ4 block and returns the
// compressed block if including the 12-byte MagicLZ4+rawLen header saves at
// least 12.5% compared to raw len(src).
func LZ4CompressIfSmaller(src []byte) ([]byte, bool) {
	n := len(src)
	if n < 4096 || n > lz4MaxRawLen {
		return nil, false
	}
	limit := int(int64(n)*7/8) - (len(MagicLZ4) + 4)
	if limit <= 0 {
		return nil, false
	}

	tblPtr := lz4TablePool.Get().(*[lz4HashSize]uint32)
	clear(tblPtr[:])
	defer lz4TablePool.Put(tblPtr)
	table := tblPtr

	dst := make([]byte, 0, min(n/2+64, limit+16))
	srcLimit := n - lz4MFLimit
	matchLimit := n - lz4LastLiterals

	table[lz4Hash(binary.LittleEndian.Uint32(src[0:]))] = 1
	pos := 1
	anchor := 0

	for pos < srcLimit {
		var ref int
		step := 1
		searchMatchNb := 1 << 6
		found := false
		for pos < srcLimit {
			h := lz4Hash(binary.LittleEndian.Uint32(src[pos:]))
			ref = int(table[h]) - 1
			table[h] = uint32(pos + 1)
			if ref >= 0 && pos-ref <= lz4MaxDistance && binary.LittleEndian.Uint32(src[ref:]) == binary.LittleEndian.Uint32(src[pos:]) {
				found = true
				break
			}
			pos += step
			step = searchMatchNb >> 6
			searchMatchNb++
		}
		if !found {
			break
		}

		// Extend match backwards into the pending literal run.
		for pos > anchor && ref > 0 && src[pos-1] == src[ref-1] {
			pos--
			ref--
		}

		// Extend match forwards using 8-byte comparisons where possible.
		matchLen := 0
		mSrc := pos + lz4MinMatch
		mRef := ref + lz4MinMatch
		for mSrc+matchLen+8 <= matchLimit {
			diff := binary.LittleEndian.Uint64(src[mSrc+matchLen:]) ^ binary.LittleEndian.Uint64(src[mRef+matchLen:])
			if diff != 0 {
				matchLen += bits.TrailingZeros64(diff) >> 3
				goto matchDone
			}
			matchLen += 8
		}
		for mSrc+matchLen < matchLimit && src[mSrc+matchLen] == src[mRef+matchLen] {
			matchLen++
		}
	matchDone:

		litLen := pos - anchor
		if len(dst)+1+litLen/255+1+litLen+2+matchLen/255+1 > limit {
			return nil, false
		}

		tokIdx := len(dst)
		dst = append(dst, 0)
		if litLen >= 15 {
			dst[tokIdx] = 0xF0
			rem := litLen - 15
			for rem >= 255 {
				dst = append(dst, 255)
				rem -= 255
			}
			dst = append(dst, byte(rem))
		} else {
			dst[tokIdx] = byte(litLen << 4)
		}
		dst = append(dst, src[anchor:pos]...)
		dst = binary.LittleEndian.AppendUint16(dst, uint16(pos-ref))

		if matchLen >= 15 {
			dst[tokIdx] |= 0x0F
			rem := matchLen - 15
			for rem >= 255 {
				dst = append(dst, 255)
				rem -= 255
			}
			dst = append(dst, byte(rem))
		} else {
			dst[tokIdx] |= byte(matchLen)
		}

		pos += lz4MinMatch + matchLen
		anchor = pos
		if pos < srcLimit {
			table[lz4Hash(binary.LittleEndian.Uint32(src[pos-2:]))] = uint32(pos - 2 + 1)
		}
	}

	litLen := n - anchor
	if len(dst)+1+litLen/255+1+litLen > limit {
		return nil, false
	}
	tokIdx := len(dst)
	dst = append(dst, 0)
	if litLen >= 15 {
		dst[tokIdx] = 0xF0
		rem := litLen - 15
		for rem >= 255 {
			dst = append(dst, 255)
			rem -= 255
		}
		dst = append(dst, byte(rem))
	} else {
		dst[tokIdx] = byte(litLen << 4)
	}
	dst = append(dst, src[anchor:]...)
	return dst, true
}

// LZ4Decompress decompresses an LZ4 block expecting exact uncompressed size rawLen.
func LZ4Decompress(src []byte, rawLen int) ([]byte, error) {
	if rawLen <= 0 || rawLen > lz4MaxRawLen || len(src) == 0 {
		return nil, errLZ4Corrupt
	}
	if uint64(rawLen) > uint64(len(src))*256+32 {
		return nil, errLZ4Corrupt
	}

	dst := make([]byte, rawLen)
	si := 0
	di := 0

	for si < len(src) {
		token := src[si]
		si++

		litLen := int(token >> 4)
		if litLen == 15 {
			for {
				if si >= len(src) {
					return nil, errLZ4Corrupt
				}
				b := int(src[si])
				si++
				if litLen > rawLen-di-b {
					return nil, errLZ4Corrupt
				}
				litLen += b
				if b != 255 {
					break
				}
			}
		}
		if litLen > len(src)-si || litLen > rawLen-di {
			return nil, errLZ4Corrupt
		}
		copy(dst[di:di+litLen], src[si:si+litLen])
		si += litLen
		di += litLen

		if si == len(src) {
			if di != rawLen {
				return nil, errLZ4Corrupt
			}
			return dst, nil
		}

		if si+2 > len(src) {
			return nil, errLZ4Corrupt
		}
		offset := int(binary.LittleEndian.Uint16(src[si : si+2]))
		si += 2
		if offset == 0 || offset > di {
			return nil, errLZ4Corrupt
		}

		matchLen := int(token&0x0f) + lz4MinMatch
		if (token & 0x0f) == 15 {
			for {
				if si >= len(src) {
					return nil, errLZ4Corrupt
				}
				b := int(src[si])
				si++
				if matchLen > rawLen-di-b {
					return nil, errLZ4Corrupt
				}
				matchLen += b
				if b != 255 {
					break
				}
			}
		}
		if matchLen > rawLen-di {
			return nil, errLZ4Corrupt
		}

		if offset >= matchLen {
			copy(dst[di:di+matchLen], dst[di-offset:di-offset+matchLen])
			di += matchLen
		} else {
			for matchLen > 0 {
				chunk := min(offset, matchLen)
				copy(dst[di:di+chunk], dst[di-offset:di-offset+chunk])
				di += chunk
				matchLen -= chunk
				if offset < matchLen {
					offset += chunk
				}
			}
		}
	}

	return nil, errLZ4Corrupt
}
