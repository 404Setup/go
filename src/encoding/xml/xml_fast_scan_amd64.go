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

var (
	xmlVec32 = [32]int8{
		32, 32, 32, 32, 32, 32, 32, 32, 32, 32, 32, 32, 32, 32, 32, 32,
		32, 32, 32, 32, 32, 32, 32, 32, 32, 32, 32, 32, 32, 32, 32, 32,
	}
	xmlVecQuot = [32]uint8{
		'"', '"', '"', '"', '"', '"', '"', '"', '"', '"', '"', '"', '"', '"', '"', '"',
		'"', '"', '"', '"', '"', '"', '"', '"', '"', '"', '"', '"', '"', '"', '"', '"',
	}
	xmlVecApos = [32]uint8{
		'\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'',
		'\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'', '\'',
	}
	xmlVecAmp = [32]uint8{
		'&', '&', '&', '&', '&', '&', '&', '&', '&', '&', '&', '&', '&', '&', '&', '&',
		'&', '&', '&', '&', '&', '&', '&', '&', '&', '&', '&', '&', '&', '&', '&', '&',
	}
	xmlVecLt = [32]uint8{
		'<', '<', '<', '<', '<', '<', '<', '<', '<', '<', '<', '<', '<', '<', '<', '<',
		'<', '<', '<', '<', '<', '<', '<', '<', '<', '<', '<', '<', '<', '<', '<', '<',
	}
	xmlVecGt = [32]uint8{
		'>', '>', '>', '>', '>', '>', '>', '>', '>', '>', '>', '>', '>', '>', '>', '>',
		'>', '>', '>', '>', '>', '>', '>', '>', '>', '>', '>', '>', '>', '>', '>', '>',
	}
	xmlVecTab = [32]uint8{
		'\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t',
		'\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t', '\t',
	}
	xmlVecRb = [32]uint8{
		']', ']', ']', ']', ']', ']', ']', ']', ']', ']', ']', ']', ']', ']', ']', ']',
		']', ']', ']', ']', ']', ']', ']', ']', ']', ']', ']', ']', ']', ']', ']', ']',
	}
)

func xmlEscapeIndexSIMD(s []byte, i int) (int, bool) {
	c32 := archsimd.LoadInt8x32Array(&xmlVec32)
	cQuot := archsimd.LoadUint8x32Array(&xmlVecQuot)
	cApos := archsimd.LoadUint8x32Array(&xmlVecApos)
	cAmp := archsimd.LoadUint8x32Array(&xmlVecAmp)
	cLt := archsimd.LoadUint8x32Array(&xmlVecLt)
	cGt := archsimd.LoadUint8x32Array(&xmlVecGt)
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
	c32 := archsimd.LoadInt8x32Array(&xmlVec32)
	cTab := archsimd.LoadUint8x32Array(&xmlVecTab)
	cAmp := archsimd.LoadUint8x32Array(&xmlVecAmp)
	cLt := archsimd.LoadUint8x32Array(&xmlVecLt)
	cRb := archsimd.LoadUint8x32Array(&xmlVecRb)
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
	c32 := archsimd.LoadInt8x32Array(&xmlVec32)
	cTab := archsimd.LoadUint8x32Array(&xmlVecTab)
	cAmp := archsimd.LoadUint8x32Array(&xmlVecAmp)
	cLt := archsimd.LoadUint8x32Array(&xmlVecLt)
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
