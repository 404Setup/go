// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build arm64 && arm64.v9.0

#include "textflag.h"

// SVE2 instructions are encoded as WORD constants so GOARM64=v9.0 targets
// assemble without requiring GOEXPERIMENT=simd. All encodings are verified
// against cmd/asm and objdump.

// func sveVL() int
TEXT ·sveVL(SB), NOSPLIT|NOFRAME, $0-8
	WORD	$0x04bf5024	// RDVL $1, R4
	MOVD	R4, ret+0(FP)
	RET

// func encodeSVE2(dst, src *byte, n int, lut *byte) (si, di int)
// Only called when cpu.ARM64.HasSVE2 && sveVL() >= 32.
TEXT ·encodeSVE2(SB), NOSPLIT|NOFRAME, $0-48
	MOVD	dst+0(FP), R0
	MOVD	src+8(FP), R1
	MOVD	n+16(FP), R2
	MOVD	lut+24(FP), R3
	MOVD	R1, R11		// original src
	MOVD	R0, R12		// original dst

	WORD	$0x04bf5024	// RDVL $1, R4 (VL in bytes, >= 32)
	ADD	R4, R4, R5
	ADD	R4, R5, R6	// R6 = 3*VL (input bytes per iteration)
	LSL	$2, R4, R9	// R9 = 4*VL (output bytes per iteration)

	CMP	R6, R2
	BLT	edone

	MOVD	$0, R7
	WORD	$0x25241ce0	// PWHILELO R4, R7, P0.B (0..VL-1)
	MOVD	$64, R8
	WORD	$0x25281ce1	// PWHILELO R8, R7, P1.B (0..min(64, VL)-1)

	WORD	$0xa400a470	// ZLD1B (VL*0)(R3), P1.Z, [Z16.B]
	CMP	$32, R4
	BGT	elut_zero
	ADD	$32, R3, R8
	WORD	$0xa400a511	// ZLD1B (VL*0)(R8), P1.Z, [Z17.B]
	B	elut_done
elut_zero:
	WORD	$0x2538c011	// ZDUP $0, Z17.B

elut_done:
	WORD	$0x2538c07c	// ZDUP $0x03, Z28.B
	WORD	$0x2538c1fd	// ZDUP $0x0F, Z29.B
	WORD	$0x2538c7fe	// ZDUP $0x3F, Z30.B

eloop:
	WORD	$0xa440e020	// ZLD3B (VL*0)(R1), P0.Z, [Z0.B, Z1.B, Z2.B]

	// Z4 = Z0 >> 2
	WORD	$0x042e9404	// ZLSR $2, Z0.B, Z4.B

	// Z5 = ((Z0 & 0x03) << 4) | (Z1 >> 4)
	WORD	$0x043c3005	// ZAND Z28.D, Z0.D, Z5.D
	WORD	$0x042c9ca5	// ZLSL $4, Z5.B, Z5.B
	WORD	$0x042c9423	// ZLSR $4, Z1.B, Z3.B
	WORD	$0x046330a5	// ZORR Z3.D, Z5.D, Z5.D

	// Z6 = ((Z1 & 0x0F) << 2) | (Z2 >> 6)
	WORD	$0x043d3026	// ZAND Z29.D, Z1.D, Z6.D
	WORD	$0x042a9cc6	// ZLSL $2, Z6.B, Z6.B
	WORD	$0x042a9443	// ZLSR $6, Z2.B, Z3.B
	WORD	$0x046330c6	// ZORR Z3.D, Z6.D, Z6.D

	// Z7 = Z2 & 0x3F
	WORD	$0x043e3047	// ZAND Z30.D, Z2.D, Z7.D

	// 2-register SVE2 TBL covers 2*VL >= 64 bytes
	WORD	$0x05242a04	// ZTBL Z4.B, [Z16.B, Z17.B], Z4.B
	WORD	$0x05252a05	// ZTBL Z5.B, [Z16.B, Z17.B], Z5.B
	WORD	$0x05262a06	// ZTBL Z6.B, [Z16.B, Z17.B], Z6.B
	WORD	$0x05272a07	// ZTBL Z7.B, [Z16.B, Z17.B], Z7.B

	WORD	$0xe470e004	// ZST4B [Z4.B, Z5.B, Z6.B, Z7.B], P0, (VL*0)(R0)

	ADD	R6, R1
	ADD	R9, R0
	SUB	R6, R2
	CMP	R6, R2
	BGE	eloop

edone:
	SUB	R11, R1, R4
	SUB	R12, R0, R5
	MOVD	R4, si+32(FP)
	MOVD	R5, di+40(FP)
	RET

// func decodeSVE2(dst, src *byte, n int, lut *byte) int
// Only called when cpu.ARM64.HasSVE2 && sveVL() >= 32.
TEXT ·decodeSVE2(SB), NOSPLIT|NOFRAME, $0-40
	MOVD	dst+0(FP), R0
	MOVD	src+8(FP), R1
	MOVD	n+16(FP), R2
	MOVD	lut+24(FP), R3
	MOVD	R1, R11		// original src

	WORD	$0x04bf5024	// RDVL $1, R4 (VL in bytes, >= 32)
	LSL	$2, R4, R6	// R6 = 4*VL (input bytes per iteration)
	ADD	R4, R4, R9
	ADD	R4, R9, R9	// R9 = 3*VL (output bytes per iteration)

	CMP	R6, R2
	BLT	ddone

	MOVD	$0, R7
	WORD	$0x25241ce0	// PWHILELO R4, R7, P0.B (0..VL-1)
	MOVD	$64, R8
	WORD	$0x25281ce1	// PWHILELO R8, R7, P1.B (0..min(64, VL)-1)

	WORD	$0xa400a470	// ZLD1B (VL*0)(R3), P1.Z, [Z16.B]
	ADD	$64, R3, R10
	WORD	$0xa400a554	// ZLD1B (VL*0)(R10), P1.Z, [Z20.B]
	CMP	$32, R4
	BGT	dlut_zero
	ADD	$32, R3, R8
	WORD	$0xa400a511	// ZLD1B (VL*0)(R8), P1.Z, [Z17.B]
	ADD	$96, R3, R10
	WORD	$0xa400a555	// ZLD1B (VL*0)(R10), P1.Z, [Z21.B]
	B	dlut_done
dlut_zero:
	WORD	$0x2538c011	// ZDUP $0, Z17.B
	WORD	$0x2538c015	// ZDUP $0, Z21.B

dlut_done:
	WORD	$0x2538c81c	// ZDUP $64, Z28.B
	WORD	$0x2538c7fe	// ZDUP $0x3F, Z30.B

dloop:
	WORD	$0xa460e020	// ZLD4B (VL*0)(R1), P0.Z, [Z0.B, Z1.B, Z2.B, Z3.B]

	WORD	$0x043c0404	// ZSUB Z28.B, Z0.B, Z4.B
	WORD	$0x043c0425	// ZSUB Z28.B, Z1.B, Z5.B
	WORD	$0x043c0446	// ZSUB Z28.B, Z2.B, Z6.B
	WORD	$0x043c0467	// ZSUB Z28.B, Z3.B, Z7.B

	WORD	$0x05202a00	// ZTBL Z0.B, [Z16.B, Z17.B], Z0.B
	WORD	$0x05242a84	// ZTBL Z4.B, [Z20.B, Z21.B], Z4.B
	WORD	$0x05212a01	// ZTBL Z1.B, [Z16.B, Z17.B], Z1.B
	WORD	$0x05252a85	// ZTBL Z5.B, [Z20.B, Z21.B], Z5.B
	WORD	$0x05222a02	// ZTBL Z2.B, [Z16.B, Z17.B], Z2.B
	WORD	$0x05262a86	// ZTBL Z6.B, [Z20.B, Z21.B], Z6.B
	WORD	$0x05232a03	// ZTBL Z3.B, [Z16.B, Z17.B], Z3.B
	WORD	$0x05272a87	// ZTBL Z7.B, [Z20.B, Z21.B], Z7.B

	WORD	$0x04643000	// ZORR Z4.D, Z0.D, Z0.D
	WORD	$0x04653021	// ZORR Z5.D, Z1.D, Z1.D
	WORD	$0x04663042	// ZORR Z6.D, Z2.D, Z2.D
	WORD	$0x04673063	// ZORR Z7.D, Z3.D, Z3.D

	// Every valid decoded sextet has bit 7 == 1; invalid bytes have bit 7 == 0.
	WORD	$0x04213004	// ZAND Z1.D, Z0.D, Z4.D
	WORD	$0x04233045	// ZAND Z3.D, Z2.D, Z5.D
	WORD	$0x04253084	// ZAND Z5.D, Z4.D, Z4.D
	WORD	$0x040b2084	// ZUMINVB Z4.B, P0, V4
	VMOV	V4.B[0], R8
	TBZ	$7, R8, ddone

	// Pack 4x 6-bit sextets (Z0, Z1, Z2, Z3) into 3 bytes (Z4, Z5, Z6).
	WORD	$0x042a9c04	// ZLSL $2, Z0.B, Z4.B
	WORD	$0x042a9c27	// ZLSL $2, Z1.B, Z7.B
	WORD	$0x042a94e7	// ZLSR $6, Z7.B, Z7.B
	WORD	$0x04673084	// ZORR Z7.D, Z4.D, Z4.D

	WORD	$0x042c9c25	// ZLSL $4, Z1.B, Z5.B
	WORD	$0x042a9c47	// ZLSL $2, Z2.B, Z7.B
	WORD	$0x042c94e7	// ZLSR $4, Z7.B, Z7.B
	WORD	$0x046730a5	// ZORR Z7.D, Z5.D, Z5.D

	WORD	$0x042e9c46	// ZLSL $6, Z2.B, Z6.B
	WORD	$0x043e3067	// ZAND Z30.D, Z3.D, Z7.D
	WORD	$0x046730c6	// ZORR Z7.D, Z6.D, Z6.D

	WORD	$0xe450e004	// ZST3B [Z4.B, Z5.B, Z6.B], P0, (VL*0)(R0)

	ADD	R6, R1
	ADD	R9, R0
	SUB	R6, R2
	CMP	R6, R2
	BGE	dloop

ddone:
	SUB	R11, R1, R0
	MOVD	R0, ret+32(FP)
	RET
