// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build arm64

#include "textflag.h"

// func encodeNEON(dst, src *byte, n int, lut *byte)
// n must be a positive multiple of 16.
TEXT ·encodeNEON(SB), NOSPLIT|NOFRAME, $0-32
	MOVD	dst+0(FP), R0
	MOVD	src+8(FP), R1
	MOVD	n+16(FP), R2
	MOVD	lut+24(FP), R3

	VLD1	(R3), [V16.B16]
	MOVD	$0x0f, R4
	VDUP	R4, V15.B16

enc_loop:
	VLD1.P	16(R1), [V0.B16]
	VUSHR	$4, V0.B16, V1.B16
	VAND	V15.B16, V0.B16, V2.B16
	VTBL	V1.B16, [V16.B16], V1.B16
	VTBL	V2.B16, [V16.B16], V2.B16
	VST2.P	[V1.B16, V2.B16], 32(R0)

	SUB	$16, R2
	CMP	$16, R2
	BGE	enc_loop
	RET

// func decodeNEON(dst, src *byte, n int, lut *byte) int
// n must be a positive multiple of 32.
TEXT ·decodeNEON(SB), NOSPLIT|NOFRAME, $0-40
	MOVD	dst+0(FP), R0
	MOVD	src+8(FP), R1
	MOVD	n+16(FP), R2
	MOVD	lut+24(FP), R3
	MOVD	R1, R6

	VLD1	(R3), [V16.B16, V17.B16, V18.B16, V19.B16]
	MOVD	$'0', R4
	VDUP	R4, V28.B16
	MOVD	$0x0f, R4
	VDUP	R4, V29.B16
	MOVD	$0x8080808080808080, R7

dec_loop:
	VLD2	(R1), [V0.B16, V1.B16]
	VSUB	V28.B16, V0.B16, V0.B16
	VSUB	V28.B16, V1.B16, V1.B16
	VTBL	V0.B16, [V16.B16, V17.B16, V18.B16, V19.B16], V0.B16
	VTBL	V1.B16, [V16.B16, V17.B16, V18.B16, V19.B16], V1.B16

	VAND	V1.B16, V0.B16, V2.B16
	VMOV	V2.D[0], R4
	VMOV	V2.D[1], R5
	AND	R4, R5, R4
	AND	R7, R4, R4
	CMP	R7, R4
	BNE	dec_done

	VSHL	$4, V0.B16, V0.B16
	VAND	V29.B16, V1.B16, V1.B16
	VORR	V1.B16, V0.B16, V0.B16
	VST1.P	[V0.B16], 16(R0)

	ADD	$32, R1
	SUB	$32, R2
	CMP	$32, R2
	BGE	dec_loop

dec_done:
	SUB	R6, R1, R0
	MOVD	R0, ret+32(FP)
	RET
