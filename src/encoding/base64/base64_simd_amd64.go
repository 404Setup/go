// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simdstable && amd64

package base64

import (
	"internal/byteorder"
	"simd/archsimd"
)

var haveSIMD = archsimd.X86.AVX2()

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

	for len(src)-si >= 32 && len(dst)-di >= 32 {
		v := archsimd.LoadUint8x32(src[si:]).ReshapeToUint32s().LookupMod(permIdx)
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

		ascii.Store(dst[di:])
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
	c0F32 := archsimd.BroadcastUint32x8(0x0f0f0f0f)
	var zero archsimd.Uint8x32
	mul1 := archsimd.BroadcastUint32x8(0x01400140).ReshapeToUint8s().BitsToInt8()
	mul2 := archsimd.BroadcastUint32x8(0x00011000).ReshapeToUint16s().BitsToInt16()
	packShuf := archsimd.LoadInt8x32Array(&decPackShuf)
	packPerm := archsimd.LoadUint32x8Array(&decPackPerm)

	for len(src)-si >= 32 && len(dst)-di >= 32 {
		in := archsimd.LoadUint8x32(src[si:])
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

		out.Store(dst[di:])
		si += 32
		di += 24
	}

	if len(src)-si >= 32 && len(dst)-di >= 24 {
		in := archsimd.LoadUint8x32(src[si:])
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

			out.Lo().Store(dst[di:])
			byteorder.LEPutUint64(dst[di+16:], out.Hi().ReshapeToUint64s().Elem(0))
			si += 32
			di += 24
		}
	}

	return si, di
}
