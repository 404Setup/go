// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simdstable && amd64

package xml

import (
	"math/bits"
	"simd/archsimd"
	"unsafe"
)

var haveXMLSIMD = archsimd.X86.AVX2()

func xmlEscapeIndexSIMD(s []byte, i int) (int, bool) {
	c32 := archsimd.BroadcastInt8x32(32)
	cQuot := archsimd.BroadcastUint8x32('"')
	cApos := archsimd.BroadcastUint8x32('\'')
	cAmp := archsimd.BroadcastUint8x32('&')
	cLt := archsimd.BroadcastUint8x32('<')
	cGt := archsimd.BroadcastUint8x32('>')
	ptr := unsafe.Pointer(unsafe.SliceData(s))
	for len(s)-i >= 32 {
		v := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(ptr, i)))
		m := v.BitsToInt8().Less(c32).
			Or(v.Equal(cQuot)).
			Or(v.Equal(cApos)).
			Or(v.Equal(cAmp)).
			Or(v.Equal(cLt)).
			Or(v.Equal(cGt)).ToBits()
		if m != 0 {
			return i + bits.TrailingZeros32(m), true
		}
		i += 32
	}
	return i, false
}

func xmlCharDataIndexSIMD(s []byte, i int) (int, bool) {
	c32 := archsimd.BroadcastInt8x32(32)
	cTab := archsimd.BroadcastUint8x32('\t')
	cAmp := archsimd.BroadcastUint8x32('&')
	cLt := archsimd.BroadcastUint8x32('<')
	cRb := archsimd.BroadcastUint8x32(']')
	ptr := unsafe.Pointer(unsafe.SliceData(s))
	for len(s)-i >= 32 {
		v := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(ptr, i)))
		m := v.BitsToInt8().Less(c32).AndNot(v.Equal(cTab)).
			Or(v.Equal(cAmp)).
			Or(v.Equal(cLt)).
			Or(v.Equal(cRb)).ToBits()
		if m != 0 {
			return i + bits.TrailingZeros32(m), true
		}
		i += 32
	}
	return i, false
}

func xmlAttrValIndexSIMD(s []byte, i int, qb byte) (int, bool) {
	c32 := archsimd.BroadcastInt8x32(32)
	cTab := archsimd.BroadcastUint8x32('\t')
	cAmp := archsimd.BroadcastUint8x32('&')
	cLt := archsimd.BroadcastUint8x32('<')
	cQ := archsimd.BroadcastUint8x32(qb)
	ptr := unsafe.Pointer(unsafe.SliceData(s))
	for len(s)-i >= 32 {
		v := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(ptr, i)))
		m := v.BitsToInt8().Less(c32).AndNot(v.Equal(cTab)).
			Or(v.Equal(cAmp)).
			Or(v.Equal(cLt)).
			Or(v.Equal(cQ)).ToBits()
		if m != 0 {
			return i + bits.TrailingZeros32(m), true
		}
		i += 32
	}
	return i, false
}
