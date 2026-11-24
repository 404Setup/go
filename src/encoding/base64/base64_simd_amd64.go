// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simdstable && amd64

package base64

import (
	"internal/byteorder"
	"math/bits"
	"simd/archsimd"
	"unsafe"
)

var haveSIMD = archsimd.X86.AVX2()
var haveStripNewlinesSIMD = haveSIMD

const (
	minSIMDEncode    = 32
	minSIMDEncodeOut = 32
	minSIMDDecode    = 32
	minSIMDDecodeOut = 24
)

var (
	encPermIdx   = [8]uint32{0, 1, 2, 2, 3, 4, 5, 5}
	encShufMask  = [32]int8{
		1, 0, 2, 1, 4, 3, 5, 4, 7, 6, 8, 7, 10, 9, 11, 10,
		1, 0, 2, 1, 4, 3, 5, 4, 7, 6, 8, 7, 10, 9, 11, 10,
	}
	encMul16     = [16]uint16{
		0x0010, 0x0100, 0x0010, 0x0100, 0x0010, 0x0100, 0x0010, 0x0100,
		0x0010, 0x0100, 0x0010, 0x0100, 0x0010, 0x0100, 0x0010, 0x0100,
	}
	encLUTStd    = [32]int8{
		71, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -19, -16, 65, 0, 0,
		71, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -19, -16, 65, 0, 0,
	}
	encLUTURL    = [32]int8{
		71, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -17, 32, 65, 0, 0,
		71, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -17, 32, 65, 0, 0,
	}

	// Exact 3-LUT validation tables for StdEncoding and URLEncoding:
	// hiVec & loVec != 0 iff the byte is in the encoding alphabet.
	decValidLoStd = [32]uint8{
		0x14, 0x15, 0x15, 0x15, 0x15, 0x15, 0x15, 0x15,
		0x15, 0x15, 0x0a, 0x22, 0x02, 0x02, 0x02, 0x42,
		0x14, 0x15, 0x15, 0x15, 0x15, 0x15, 0x15, 0x15,
		0x15, 0x15, 0x0a, 0x22, 0x02, 0x02, 0x02, 0x42,
	}
	decValidHiStd = [32]uint8{
		0x00, 0x00, 0x60, 0x10, 0x03, 0x0c, 0x03, 0x0c,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x60, 0x10, 0x03, 0x0c, 0x03, 0x0c,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	decValidLoURL = [32]uint8{
		0x14, 0x15, 0x15, 0x15, 0x15, 0x15, 0x15, 0x15,
		0x15, 0x15, 0x0a, 0x02, 0x02, 0x22, 0x02, 0x42,
		0x14, 0x15, 0x15, 0x15, 0x15, 0x15, 0x15, 0x15,
		0x15, 0x15, 0x0a, 0x02, 0x02, 0x22, 0x02, 0x42,
	}
	decValidHiURL = [32]uint8{
		0x00, 0x00, 0x20, 0x10, 0x03, 0x4c, 0x03, 0x0c,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x20, 0x10, 0x03, 0x4c, 0x03, 0x0c,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	decRollStd = [32]int8{
		0, 0, 19, 4, -65, -65, -71, -71, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 19, 4, -65, -65, -71, -71, 0, 0, 0, 0, 0, 0, 0, 0,
	}
	decRollURL = [32]int8{
		0, 0, 17, 4, -65, -65, -71, -71, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 17, 4, -65, -65, -71, -71, 0, 0, 0, 0, 0, 0, 0, 0,
	}
	decPackShuf = [32]int8{
		2, 1, 0, 6, 5, 4, 10, 9, 8, 14, 13, 12, -1, -1, -1, -1,
		2, 1, 0, 6, 5, 4, 10, 9, 8, 14, 13, 12, -1, -1, -1, -1,
	}
	decPackPerm = [8]uint32{0, 1, 2, 4, 5, 6, 0, 0}
)

func encodeSIMD(dst, src []byte, kind encodingKind) (si, di int) {
	permIdx := archsimd.LoadUint32x8Array(&encPermIdx)
	shufMask := archsimd.LoadInt8x32Array(&encShufMask)
	mul16 := archsimd.LoadUint16x16Array(&encMul16)
	mask3F := archsimd.BroadcastUint32x8(0x0000003F)
	mask3F0000 := archsimd.BroadcastUint32x8(0x003F0000)
	maskT1 := archsimd.BroadcastUint32x8(0x003F03F0)
	c51 := archsimd.BroadcastUint8x32(51)
	c26 := archsimd.BroadcastInt8x32(26)
	c13 := archsimd.BroadcastUint8x32(13)

	var lut archsimd.Uint8x32
	if kind == encURL {
		lut = archsimd.LoadInt8x32Array(&encLUTURL).ToBits()
	} else {
		lut = archsimd.LoadInt8x32Array(&encLUTStd).ToBits()
	}

	srcPtr := unsafe.Pointer(unsafe.SliceData(src))
	dstPtr := unsafe.Pointer(unsafe.SliceData(dst))

	for len(src)-si >= 56 && len(dst)-di >= 64 {
		v0 := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(srcPtr, si))).ReshapeToUint32s().LookupMod(permIdx)
		v1 := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(srcPtr, si+24))).ReshapeToUint32s().LookupMod(permIdx)
		shuf0 := v0.ReshapeToUint8s().LookupModOrZeroGrouped(shufMask).ReshapeToUint32s()
		shuf1 := v1.ReshapeToUint8s().LookupModOrZeroGrouped(shufMask).ReshapeToUint32s()

		t0Lo0 := shuf0.ShiftAllRight(10).And(mask3F)
		t0Hi0 := shuf0.ShiftAllRight(6).And(mask3F0000)
		t10 := shuf0.And(maskT1).ReshapeToUint16s().Mul(mul16).ReshapeToUint32s()
		indices0 := t0Lo0.Or(t0Hi0).Or(t10).ReshapeToUint8s()

		t0Lo1 := shuf1.ShiftAllRight(10).And(mask3F)
		t0Hi1 := shuf1.ShiftAllRight(6).And(mask3F0000)
		t11 := shuf1.And(maskT1).ReshapeToUint16s().Mul(mul16).ReshapeToUint32s()
		indices1 := t0Lo1.Or(t0Hi1).Or(t11).ReshapeToUint8s()

		res0 := indices0.SubSaturated(c51)
		less0 := c26.Greater(indices0.BitsToInt8())
		res0 = res0.Add(less0.ToInt8x32().ToBits().And(c13))
		ascii0 := indices0.Add(lut.LookupModOrZeroGrouped(res0.BitsToInt8()))

		res1 := indices1.SubSaturated(c51)
		less1 := c26.Greater(indices1.BitsToInt8())
		res1 = res1.Add(less1.ToInt8x32().ToBits().And(c13))
		ascii1 := indices1.Add(lut.LookupModOrZeroGrouped(res1.BitsToInt8()))

		ascii0.StoreArray((*[32]uint8)(unsafe.Add(dstPtr, di)))
		ascii1.StoreArray((*[32]uint8)(unsafe.Add(dstPtr, di+32)))
		si += 48
		di += 64
	}

	for len(src)-si >= 32 && len(dst)-di >= 32 {
		v := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(srcPtr, si))).ReshapeToUint32s().LookupMod(permIdx)
		shuf := v.ReshapeToUint8s().LookupModOrZeroGrouped(shufMask).ReshapeToUint32s()

		t0Lo := shuf.ShiftAllRight(10).And(mask3F)
		t0Hi := shuf.ShiftAllRight(6).And(mask3F0000)
		t1 := shuf.And(maskT1).ReshapeToUint16s().Mul(mul16).ReshapeToUint32s()
		indices := t0Lo.Or(t0Hi).Or(t1).ReshapeToUint8s()

		result := indices.SubSaturated(c51)
		less := c26.Greater(indices.BitsToInt8())
		result = result.Add(less.ToInt8x32().ToBits().And(c13))
		offset := lut.LookupModOrZeroGrouped(result.BitsToInt8())
		ascii := indices.Add(offset)

		ascii.StoreArray((*[32]uint8)(unsafe.Add(dstPtr, di)))
		si += 24
		di += 32
	}
	return si, di
}

func decodeSIMD(dst, src []byte, si, di int, kind encodingKind) (int, int) {
	var loLUT, hiLUT archsimd.Uint8x32
	var rollLUT archsimd.Int8x32
	var specialChar archsimd.Uint8x32
	var specialDelta archsimd.Int8x32

	if kind == encURL {
		loLUT = archsimd.LoadUint8x32Array(&decValidLoURL)
		hiLUT = archsimd.LoadUint8x32Array(&decValidHiURL)
		rollLUT = archsimd.LoadInt8x32Array(&decRollURL)
		specialChar = archsimd.BroadcastUint8x32('_')
		specialDelta = archsimd.BroadcastInt8x32(33)
	} else {
		loLUT = archsimd.LoadUint8x32Array(&decValidLoStd)
		hiLUT = archsimd.LoadUint8x32Array(&decValidHiStd)
		rollLUT = archsimd.LoadInt8x32Array(&decRollStd)
		specialChar = archsimd.BroadcastUint8x32('/')
		specialDelta = archsimd.BroadcastInt8x32(-3)
	}

	c0F := archsimd.BroadcastUint8x32(0x0f)
	c0F32 := c0F.ReshapeToUint32s()
	var zero archsimd.Uint8x32
	mul1 := archsimd.BroadcastUint32x8(0x01400140).ReshapeToUint8s().BitsToInt8()
	mul2 := archsimd.BroadcastUint32x8(0x00011000).ReshapeToUint16s().BitsToInt16()
	packShuf := archsimd.LoadInt8x32Array(&decPackShuf)
	packPerm := archsimd.LoadUint32x8Array(&decPackPerm)

	srcPtr := unsafe.Pointer(unsafe.SliceData(src))
	dstPtr := unsafe.Pointer(unsafe.SliceData(dst))

	for len(src)-si >= 64 && len(dst)-di >= 56 {
		in0 := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(srcPtr, si)))
		in1 := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(srcPtr, si+32)))
		hiNib0 := in0.ReshapeToUint32s().ShiftAllRight(4).And(c0F32).ReshapeToUint8s()
		loNib0 := in0.And(c0F)
		hiNib1 := in1.ReshapeToUint32s().ShiftAllRight(4).And(c0F32).ReshapeToUint8s()
		loNib1 := in1.And(c0F)

		lo0 := loLUT.LookupModOrZeroGrouped(loNib0.BitsToInt8())
		hi0 := hiLUT.LookupModOrZeroGrouped(hiNib0.BitsToInt8())
		lo1 := loLUT.LookupModOrZeroGrouped(loNib1.BitsToInt8())
		hi1 := hiLUT.LookupModOrZeroGrouped(hiNib1.BitsToInt8())
		if lo0.And(hi0).Equal(zero).Or(lo1.And(hi1).Equal(zero)).Any() {
			break
		}

		roll0 := rollLUT.LookupModOrZeroGrouped(hiNib0.BitsToInt8())
		eq0 := in0.Equal(specialChar).ToInt8x32().And(specialDelta)
		sextets0 := in0.BitsToInt8().Add(roll0).Add(eq0).ToBits()

		roll1 := rollLUT.LookupModOrZeroGrouped(hiNib1.BitsToInt8())
		eq1 := in1.Equal(specialChar).ToInt8x32().And(specialDelta)
		sextets1 := in1.BitsToInt8().Add(roll1).Add(eq1).ToBits()

		merged0 := sextets0.DotProductPairsSaturated(mul1)
		packed0 := merged0.DotProductPairs(mul2)
		packedBytes0 := packed0.ToBits().ReshapeToUint8s().LookupModOrZeroGrouped(packShuf)
		out0 := packedBytes0.ReshapeToUint32s().LookupMod(packPerm).ReshapeToUint8s()

		merged1 := sextets1.DotProductPairsSaturated(mul1)
		packed1 := merged1.DotProductPairs(mul2)
		packedBytes1 := packed1.ToBits().ReshapeToUint8s().LookupModOrZeroGrouped(packShuf)
		out1 := packedBytes1.ReshapeToUint32s().LookupMod(packPerm).ReshapeToUint8s()

		out0.StoreArray((*[32]uint8)(unsafe.Add(dstPtr, di)))
		out1.StoreArray((*[32]uint8)(unsafe.Add(dstPtr, di+24)))
		si += 64
		di += 48
	}

	for len(src)-si >= 32 && len(dst)-di >= 32 {
		in := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(srcPtr, si)))
		hiNibbles := in.ReshapeToUint32s().ShiftAllRight(4).And(c0F32).ReshapeToUint8s()
		loNibbles := in.And(c0F)

		lo := loLUT.LookupModOrZeroGrouped(loNibbles.BitsToInt8())
		hi := hiLUT.LookupModOrZeroGrouped(hiNibbles.BitsToInt8())
		if lo.And(hi).Equal(zero).Any() {
			return si, di
		}

		roll := rollLUT.LookupModOrZeroGrouped(hiNibbles.BitsToInt8())
		eq := in.Equal(specialChar).ToInt8x32().And(specialDelta)
		sextets := in.BitsToInt8().Add(roll).Add(eq).ToBits()

		merged := sextets.DotProductPairsSaturated(mul1)
		packed := merged.DotProductPairs(mul2)
		packedBytes := packed.ToBits().ReshapeToUint8s().LookupModOrZeroGrouped(packShuf)
		out := packedBytes.ReshapeToUint32s().LookupMod(packPerm).ReshapeToUint8s()

		out.StoreArray((*[32]uint8)(unsafe.Add(dstPtr, di)))
		si += 32
		di += 24
	}

	if len(src)-si >= 32 && len(dst)-di >= 24 {
		in := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(srcPtr, si)))
		hiNibbles := in.ReshapeToUint32s().ShiftAllRight(4).And(c0F32).ReshapeToUint8s()
		loNibbles := in.And(c0F)

		lo := loLUT.LookupModOrZeroGrouped(loNibbles.BitsToInt8())
		hi := hiLUT.LookupModOrZeroGrouped(hiNibbles.BitsToInt8())
		if !lo.And(hi).Equal(zero).Any() {
			roll := rollLUT.LookupModOrZeroGrouped(hiNibbles.BitsToInt8())
			eq := in.Equal(specialChar).ToInt8x32().And(specialDelta)
			sextets := in.BitsToInt8().Add(roll).Add(eq).ToBits()

			merged := sextets.DotProductPairsSaturated(mul1)
			packed := merged.DotProductPairs(mul2)
			packedBytes := packed.ToBits().ReshapeToUint8s().LookupModOrZeroGrouped(packShuf)
			out := packedBytes.ReshapeToUint32s().LookupMod(packPerm).ReshapeToUint8s()

			out.Lo().StoreArray((*[16]uint8)(unsafe.Add(dstPtr, di)))
			byteorder.LEPutUint64(dst[di+16:], out.Hi().ReshapeToUint64s().Elem(0))
			si += 32
			di += 24
		}
	}

	return si, di
}

func stripNewlinesSIMD(p []byte) int {
	cr := archsimd.BroadcastUint8x32('\r')
	lf := archsimd.BroadcastUint8x32('\n')
	ptr := unsafe.Pointer(unsafe.SliceData(p))
	i := 0
	for len(p)-i >= 32 {
		in := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(ptr, i)))
		if m := in.Equal(cr).Or(in.Equal(lf)).ToBits(); m != 0 {
			i += bits.TrailingZeros32(m)
			break
		}
		i += 32
	}
	n := i
	if i < len(p) && (p[i] == '\r' || p[i] == '\n') {
		for i < len(p) && (p[i] == '\r' || p[i] == '\n') {
			i++
		}
	}
	for len(p)-i >= 32 {
		in := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(ptr, i)))
		m := in.Equal(cr).Or(in.Equal(lf)).ToBits()
		if m == 0 {
			in.StoreArray((*[32]uint8)(unsafe.Add(ptr, n)))
			n += 32
			i += 32
			continue
		}
		k := bits.TrailingZeros32(m)
		if k > 0 {
			if n < i {
				copy(p[n:], p[i:i+k])
			}
			n += k
			i += k
		}
		for i < len(p) && (p[i] == '\r' || p[i] == '\n') {
			i++
		}
	}
	for ; len(p)-i >= 8; i += 8 {
		x := byteorder.LEUint64(p[i:])
		if mayHaveNewline(x) {
			n += dropNewlines(p[n:], p[i:i+8])
			continue
		}
		if n < i {
			byteorder.LEPutUint64(p[n:], x)
		}
		n += 8
	}
	if n == i {
		for ; i < len(p); i++ {
			b := p[i]
			if b == '\r' || b == '\n' {
				return n + dropNewlines(p[n:], p[i:])
			}
			n++
		}
		return n
	}
	return n + dropNewlines(p[n:], p[i:])
}
