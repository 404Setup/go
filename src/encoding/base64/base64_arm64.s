// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build arm64

#include "textflag.h"

// func encodeNEON(dst, src *byte, n int, lut *byte)
// n must be a positive multiple of 48.
TEXT ·encodeNEON(SB), NOSPLIT|NOFRAME, $0-32
	MOVD	dst+0(FP), R0
	MOVD	src+8(FP), R1
	MOVD	n+16(FP), R2
	MOVD	lut+24(FP), R3

	VLD1	(R3), [V16.B16, V17.B16, V18.B16, V19.B16]

	MOVD	$0x03, R4
	VDUP	R4, V28.B16
	MOVD	$0x0F, R4
	VDUP	R4, V29.B16
	MOVD	$0x3F, R4
	VDUP	R4, V30.B16

loop:
	VLD3.P	48(R1), [V0.B16, V1.B16, V2.B16]

	// V4 = b0 >> 2
	VUSHR	$2, V0.B16, V4.B16

	// V5 = ((b0 & 0x03) << 4) | (b1 >> 4)
	VAND	V28.B16, V0.B16, V5.B16
	VSHL	$4, V5.B16, V5.B16
	VSRI	$4, V1.B16, V5.B16

	// V6 = ((b1 & 0x0F) << 2) | (b2 >> 6)
	VAND	V29.B16, V1.B16, V6.B16
	VSHL	$2, V6.B16, V6.B16
	VSRI	$6, V2.B16, V6.B16

	// V7 = b2 & 0x3F
	VAND	V30.B16, V2.B16, V7.B16

	VTBL	V4.B16, [V16.B16, V17.B16, V18.B16, V19.B16], V4.B16
	VTBL	V5.B16, [V16.B16, V17.B16, V18.B16, V19.B16], V5.B16
	VTBL	V6.B16, [V16.B16, V17.B16, V18.B16, V19.B16], V6.B16
	VTBL	V7.B16, [V16.B16, V17.B16, V18.B16, V19.B16], V7.B16

	VST4.P	[V4.B16, V5.B16, V6.B16, V7.B16], 64(R0)

	SUB	$48, R2
	CMP	$48, R2
	BGE	loop
	RET

// func decodeNEON(dst, src *byte, n int, lut *byte) int
// n must be a positive multiple of 64.
// Returns the number of input bytes decoded.
TEXT ·decodeNEON(SB), NOSPLIT|NOFRAME, $0-40
	MOVD	dst+0(FP), R0
	MOVD	src+8(FP), R1
	MOVD	n+16(FP), R2
	MOVD	lut+24(FP), R3
	MOVD	R1, R6

	VLD1.P	64(R3), [V16.B16, V17.B16, V18.B16, V19.B16]
	VLD1	(R3), [V20.B16, V21.B16, V22.B16, V23.B16]

	MOVD	$64, R4
	VDUP	R4, V28.B16
	MOVD	$0x3F, R4
	VDUP	R4, V29.B16
	MOVD	$0x8080808080808080, R7

dloop:
	VLD4	(R1), [V0.B16, V1.B16, V2.B16, V3.B16]

	VSUB	V28.B16, V0.B16, V4.B16
	VSUB	V28.B16, V1.B16, V5.B16
	VSUB	V28.B16, V2.B16, V6.B16
	VSUB	V28.B16, V3.B16, V7.B16

	VTBL	V0.B16, [V16.B16, V17.B16, V18.B16, V19.B16], V0.B16
	VTBL	V4.B16, [V20.B16, V21.B16, V22.B16, V23.B16], V4.B16
	VTBL	V1.B16, [V16.B16, V17.B16, V18.B16, V19.B16], V1.B16
	VTBL	V5.B16, [V20.B16, V21.B16, V22.B16, V23.B16], V5.B16
	VTBL	V2.B16, [V16.B16, V17.B16, V18.B16, V19.B16], V2.B16
	VTBL	V6.B16, [V20.B16, V21.B16, V22.B16, V23.B16], V6.B16
	VTBL	V3.B16, [V16.B16, V17.B16, V18.B16, V19.B16], V3.B16
	VTBL	V7.B16, [V20.B16, V21.B16, V22.B16, V23.B16], V7.B16

	VORR	V4.B16, V0.B16, V0.B16
	VORR	V5.B16, V1.B16, V1.B16
	VORR	V6.B16, V2.B16, V2.B16
	VORR	V7.B16, V3.B16, V3.B16

	// Every valid decoded sextet has bit 7 == 1; invalid bytes have bit 7 == 0.
	VAND	V1.B16, V0.B16, V4.B16
	VAND	V3.B16, V2.B16, V5.B16
	VAND	V5.B16, V4.B16, V4.B16
	VMOV	V4.D[0], R4
	VMOV	V4.D[1], R5
	AND	R4, R5, R4
	AND	R7, R4, R4
	CMP	R7, R4
	BNE	ddone

	// Pack 4x 6-bit sextets (V0, V1, V2, V3) into 3 bytes (V4, V5, V6).
	VSHL	$2, V0.B16, V4.B16
	VSHL	$2, V1.B16, V7.B16
	VSRI	$6, V7.B16, V4.B16

	VSHL	$4, V1.B16, V5.B16
	VSHL	$2, V2.B16, V7.B16
	VSRI	$4, V7.B16, V5.B16

	VAND	V29.B16, V3.B16, V6.B16
	VSLI	$6, V2.B16, V6.B16

	VST3.P	[V4.B16, V5.B16, V6.B16], 48(R0)

	ADD	$64, R1
	SUB	$64, R2
	CMP	$64, R2
	BGE	dloop

ddone:
	SUB	R6, R1, R0
	MOVD	R0, ret+32(FP)
	RET
