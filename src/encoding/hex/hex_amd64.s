// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64

#include "textflag.h"

DATA hex_enc_lut<>+0(SB)/8, $0x3736353433323130
DATA hex_enc_lut<>+8(SB)/8, $0x6665646362613938
DATA hex_enc_lut<>+16(SB)/8, $0x3736353433323130
DATA hex_enc_lut<>+24(SB)/8, $0x6665646362613938
GLOBL hex_enc_lut<>(SB), RODATA|NOPTR, $32

DATA hex_dec_lut_lo<>+0(SB)/8, $0x0103030303030301
DATA hex_dec_lut_lo<>+8(SB)/8, $0x0000000000000101
DATA hex_dec_lut_lo<>+16(SB)/8, $0x0103030303030301
DATA hex_dec_lut_lo<>+24(SB)/8, $0x0000000000000101
GLOBL hex_dec_lut_lo<>(SB), RODATA|NOPTR, $32

DATA hex_dec_lut_hi<>+0(SB)/8, $0x0002000201000000
DATA hex_dec_lut_hi<>+8(SB)/8, $0x0000000000000000
DATA hex_dec_lut_hi<>+16(SB)/8, $0x0002000201000000
DATA hex_dec_lut_hi<>+24(SB)/8, $0x0000000000000000
GLOBL hex_dec_lut_hi<>(SB), RODATA|NOPTR, $32

DATA hex_dec_hi_add<>+0(SB)/8, $0x0009000900000000
DATA hex_dec_hi_add<>+8(SB)/8, $0x0000000000000000
DATA hex_dec_hi_add<>+16(SB)/8, $0x0009000900000000
DATA hex_dec_hi_add<>+24(SB)/8, $0x0000000000000000
GLOBL hex_dec_hi_add<>(SB), RODATA|NOPTR, $32

DATA hex_dec_madd<>+0(SB)/8, $0x0110011001100110
DATA hex_dec_madd<>+8(SB)/8, $0x0110011001100110
DATA hex_dec_madd<>+16(SB)/8, $0x0110011001100110
DATA hex_dec_madd<>+24(SB)/8, $0x0110011001100110
GLOBL hex_dec_madd<>(SB), RODATA|NOPTR, $32

DATA hex_c0f<>+0(SB)/8, $0x0f0f0f0f0f0f0f0f
DATA hex_c0f<>+8(SB)/8, $0x0f0f0f0f0f0f0f0f
GLOBL hex_c0f<>(SB), RODATA|NOPTR, $16

// func encodeAVX2Asm(dst, src *byte, n int)
// n must be a positive multiple of 16.
TEXT ·encodeAVX2Asm(SB), NOSPLIT, $0-24
	MOVQ	dst+0(FP), DI
	MOVQ	src+8(FP), SI
	MOVQ	n+16(FP), R8

	MOVL		$0x0f, AX
	MOVQ		AX, X15
	VPBROADCASTB	X15, Y15
	VMOVDQU		hex_enc_lut<>+0(SB), Y14

	CMPQ	R8, $64
	JL	enc_avx2_32

enc_avx2_loop64:
	VMOVDQU	(SI), Y0
	VMOVDQU	32(SI), Y6

	VPERMQ		$0xd8, Y0, Y0
	VPSRLW		$4, Y0, Y1
	VPAND		Y15, Y1, Y1
	VPAND		Y15, Y0, Y2
	VPSHUFB		Y1, Y14, Y1
	VPSHUFB		Y2, Y14, Y2
	VPUNPCKLBW	Y2, Y1, Y3
	VPUNPCKHBW	Y2, Y1, Y4

	VPERMQ		$0xd8, Y6, Y6
	VPSRLW		$4, Y6, Y7
	VPAND		Y15, Y7, Y7
	VPAND		Y15, Y6, Y8
	VPSHUFB		Y7, Y14, Y7
	VPSHUFB		Y8, Y14, Y8
	VPUNPCKLBW	Y8, Y7, Y9
	VPUNPCKHBW	Y8, Y7, Y10

	VMOVDQU	Y3, (DI)
	VMOVDQU	Y4, 32(DI)
	VMOVDQU	Y9, 64(DI)
	VMOVDQU	Y10, 96(DI)

	ADDQ	$64, SI
	ADDQ	$128, DI
	SUBQ	$64, R8
	CMPQ	R8, $64
	JGE	enc_avx2_loop64

enc_avx2_32:
	CMPQ	R8, $32
	JL	enc_avx2_16

	VMOVDQU		(SI), Y0
	VPERMQ		$0xd8, Y0, Y0
	VPSRLW		$4, Y0, Y1
	VPAND		Y15, Y1, Y1
	VPAND		Y15, Y0, Y2
	VPSHUFB		Y1, Y14, Y1
	VPSHUFB		Y2, Y14, Y2
	VPUNPCKLBW	Y2, Y1, Y3
	VPUNPCKHBW	Y2, Y1, Y4
	VMOVDQU		Y3, (DI)
	VMOVDQU		Y4, 32(DI)

	ADDQ	$32, SI
	ADDQ	$64, DI
	SUBQ	$32, R8

enc_avx2_16:
	CMPQ	R8, $16
	JL	enc_avx2_done

	VMOVDQU		(SI), X0
	VPSRLW		$4, X0, X1
	VPAND		X15, X1, X1
	VPAND		X15, X0, X2
	VPSHUFB		X1, X14, X1
	VPSHUFB		X2, X14, X2
	VPUNPCKLBW	X2, X1, X3
	VPUNPCKHBW	X2, X1, X4
	VMOVDQU		X3, (DI)
	VMOVDQU		X4, 16(DI)

enc_avx2_done:
	VZEROUPPER
	RET

// func decodeAVX2Asm(dst, src *byte, srcLen, dstLen int) (si, di int)
TEXT ·decodeAVX2Asm(SB), NOSPLIT, $0-48
	MOVQ	dst+0(FP), DI
	MOVQ	src+8(FP), SI
	MOVQ	srcLen+16(FP), R8
	MOVQ	dstLen+24(FP), R9
	MOVQ	SI, R10
	MOVQ	DI, R11

	MOVL		$0x0f, AX
	MOVQ		AX, X15
	VPBROADCASTB	X15, Y15
	VMOVDQU		hex_dec_lut_lo<>+0(SB), Y14
	VMOVDQU		hex_dec_lut_hi<>+0(SB), Y13
	VMOVDQU		hex_dec_hi_add<>+0(SB), Y12
	VMOVDQU		hex_dec_madd<>+0(SB), Y11
	VPXOR		Y10, Y10, Y10

	CMPQ	R8, $64
	JL	dec_avx2_loop1_check
	CMPQ	R9, $32
	JL	dec_avx2_loop1_check

dec_avx2_loop2:
	VMOVDQU	(SI), Y0
	VMOVDQU	32(SI), Y3

	VPSRLD		$4, Y0, Y1
	VPAND		Y15, Y0, Y2
	VPAND		Y15, Y1, Y1
	VPSHUFB		Y2, Y14, Y6
	VPSHUFB		Y1, Y13, Y7
	VPAND		Y6, Y7, Y6
	VPCMPEQB	Y10, Y6, Y6

	VPSRLD		$4, Y3, Y4
	VPAND		Y15, Y3, Y5
	VPAND		Y15, Y4, Y4
	VPSHUFB		Y5, Y14, Y7
	VPSHUFB		Y4, Y13, Y8
	VPAND		Y7, Y8, Y7
	VPCMPEQB	Y10, Y7, Y7

	VPOR	Y6, Y7, Y6
	VPTEST	Y6, Y6
	JNZ	dec_avx2_loop1_check

	VPSHUFB		Y1, Y12, Y1
	VPSHUFB		Y4, Y12, Y4
	VPADDB		Y2, Y1, Y0
	VPADDB		Y5, Y4, Y3
	VPMADDUBSW	Y11, Y0, Y0
	VPMADDUBSW	Y11, Y3, Y3
	VPACKUSWB	Y3, Y0, Y0
	VPERMQ		$0xd8, Y0, Y0
	VMOVDQU		Y0, (DI)

	ADDQ	$64, SI
	ADDQ	$32, DI
	SUBQ	$64, R8
	SUBQ	$32, R9
	CMPQ	R8, $64
	JL	dec_avx2_loop1_check
	CMPQ	R9, $32
	JGE	dec_avx2_loop2

dec_avx2_loop1_check:
	CMPQ	R8, $32
	JL	dec_avx2_16_check
	CMPQ	R9, $16
	JL	dec_avx2_16_check

	VMOVDQU		(SI), Y0
	VPSRLD		$4, Y0, Y1
	VPAND		Y15, Y0, Y2
	VPAND		Y15, Y1, Y1
	VPSHUFB		Y2, Y14, Y6
	VPSHUFB		Y1, Y13, Y7
	VPAND		Y6, Y7, Y6
	VPCMPEQB	Y10, Y6, Y6
	VPTEST		Y6, Y6
	JNZ		dec_avx2_16_check

	VPSHUFB		Y1, Y12, Y1
	VPADDB		Y2, Y1, Y0
	VPMADDUBSW	Y11, Y0, Y0
	VPACKUSWB	Y0, Y0, Y0
	VPERMQ		$0xd8, Y0, Y0
	VMOVDQU		X0, (DI)

	ADDQ	$32, SI
	ADDQ	$16, DI
	SUBQ	$32, R8
	SUBQ	$16, R9

dec_avx2_16_check:
	CMPQ	R8, $16
	JL	dec_avx2_done
	CMPQ	R9, $8
	JL	dec_avx2_done

	VMOVDQU		(SI), X0
	VPSRLD		$4, X0, X1
	VPAND		X15, X0, X2
	VPAND		X15, X1, X1
	VPSHUFB		X2, X14, X6
	VPSHUFB		X1, X13, X7
	VPAND		X6, X7, X6
	VPCMPEQB	X10, X6, X6
	VPTEST		X6, X6
	JNZ		dec_avx2_done

	VPSHUFB		X1, X12, X1
	VPADDB		X2, X1, X0
	VPMADDUBSW	X11, X0, X0
	VPACKUSWB	X0, X0, X0
	MOVQ		X0, (DI)

	ADDQ	$16, SI
	ADDQ	$8, DI

dec_avx2_done:
	SUBQ	R10, SI
	SUBQ	R11, DI
	MOVQ	SI, si+32(FP)
	MOVQ	DI, di+40(FP)
	VZEROUPPER
	RET

// func encodeSSSE3Asm(dst, src *byte, n int)
// n must be a positive multiple of 16.
TEXT ·encodeSSSE3Asm(SB), NOSPLIT, $0-24
	MOVQ	dst+0(FP), DI
	MOVQ	src+8(FP), SI
	MOVQ	n+16(FP), R8

	MOVOU	hex_c0f<>+0(SB), X15
	MOVOU	hex_enc_lut<>+0(SB), X14

enc_ssse3_loop:
	MOVOU	(SI), X0
	MOVOA	X0, X1
	PSRLW	$4, X1
	PAND	X15, X1
	PAND	X15, X0
	MOVOA	X14, X2
	PSHUFB	X1, X2
	MOVOA	X14, X3
	PSHUFB	X0, X3

	MOVOA	X2, X4
	PUNPCKLBW X3, X4
	PUNPCKHBW X3, X2
	MOVOU	X4, (DI)
	MOVOU	X2, 16(DI)

	ADDQ	$16, SI
	ADDQ	$32, DI
	SUBQ	$16, R8
	CMPQ	R8, $16
	JGE	enc_ssse3_loop
	RET

// func decodeSSSE3Asm(dst, src *byte, srcLen, dstLen int) (si, di int)
TEXT ·decodeSSSE3Asm(SB), NOSPLIT, $0-48
	MOVQ	dst+0(FP), DI
	MOVQ	src+8(FP), SI
	MOVQ	srcLen+16(FP), R8
	MOVQ	dstLen+24(FP), R9
	MOVQ	SI, R10
	MOVQ	DI, R11

	MOVOU	hex_c0f<>+0(SB), X15
	MOVOU	hex_dec_lut_lo<>+0(SB), X14
	MOVOU	hex_dec_lut_hi<>+0(SB), X13
	MOVOU	hex_dec_hi_add<>+0(SB), X12
	MOVOU	hex_dec_madd<>+0(SB), X11
	PXOR	X10, X10

	CMPQ	R8, $16
	JL	dec_ssse3_done
	CMPQ	R9, $8
	JL	dec_ssse3_done

dec_ssse3_loop:
	MOVOU	(SI), X0
	MOVOA	X0, X1
	PSRLL	$4, X1
	MOVOA	X0, X2
	PAND	X15, X2
	PAND	X15, X1

	MOVOA	X14, X6
	PSHUFB	X2, X6
	MOVOA	X13, X7
	PSHUFB	X1, X7
	PAND	X7, X6
	PCMPEQB	X10, X6
	PMOVMSKB X6, AX
	TESTL	AX, AX
	JNZ	dec_ssse3_done

	MOVOA	X12, X6
	PSHUFB	X1, X6
	PADDB	X2, X6
	PMADDUBSW X11, X6
	PACKUSWB X6, X6
	MOVQ	X6, (DI)

	ADDQ	$16, SI
	ADDQ	$8, DI
	SUBQ	$16, R8
	SUBQ	$8, R9
	CMPQ	R8, $16
	JL	dec_ssse3_done
	CMPQ	R9, $8
	JGE	dec_ssse3_loop

dec_ssse3_done:
	SUBQ	R10, SI
	SUBQ	R11, DI
	MOVQ	SI, si+32(FP)
	MOVQ	DI, di+40(FP)
	RET

DATA hex_dump_shuf1<>+0(SB)/8, $0x0504800302800100
DATA hex_dump_shuf1<>+8(SB)/8, $0x0a80090880070680
DATA hex_dump_shuf1<>+16(SB)/8, $0x0504800302800100
DATA hex_dump_shuf1<>+24(SB)/8, $0x0a80090880070680
GLOBL hex_dump_shuf1<>(SB), RODATA|NOPTR, $32

DATA hex_dump_sp1<>+0(SB)/8, $0x0000200000200000
DATA hex_dump_sp1<>+8(SB)/8, $0x0020000020000020
DATA hex_dump_sp1<>+16(SB)/8, $0x0000200000200000
DATA hex_dump_sp1<>+24(SB)/8, $0x0020000020000020
GLOBL hex_dump_sp1<>(SB), RODATA|NOPTR, $32

DATA hex_dump_shuf2<>+0(SB)/8, $0x800f0e800d0c800b
DATA hex_dump_shuf2<>+8(SB)/8, $0x8080808080808080
DATA hex_dump_shuf2<>+16(SB)/8, $0x800f0e800d0c800b
DATA hex_dump_shuf2<>+24(SB)/8, $0x8080808080808080
GLOBL hex_dump_shuf2<>(SB), RODATA|NOPTR, $32

DATA hex_dump_sp2<>+0(SB)/8, $0x2000002000002000
DATA hex_dump_sp2<>+8(SB)/8, $0x0000000000000020
DATA hex_dump_sp2<>+16(SB)/8, $0x2000002000002000
DATA hex_dump_sp2<>+24(SB)/8, $0x0000000000007c20
GLOBL hex_dump_sp2<>(SB), RODATA|NOPTR, $32

// func dumpLinesAVX2Asm(out, data *byte, lines int, offset uint32)
// lines must be > 0; out must have at least lines*79 bytes; data must have at least lines*16 bytes.
TEXT ·dumpLinesAVX2Asm(SB), NOSPLIT, $0-28
	MOVQ	out+0(FP), DI
	MOVQ	data+8(FP), SI
	MOVQ	lines+16(FP), CX
	MOVL	offset+24(FP), R9

	MOVL		$0x0f, AX
	MOVQ		AX, X15
	VPBROADCASTB	X15, Y15
	VMOVDQU		hex_enc_lut<>+0(SB), Y14
	MOVL		$0x60, AX
	MOVD		AX, X13
	VPBROADCASTB	X13, Y13
	MOVL		$0xde, AX
	MOVD		AX, X12
	VPBROADCASTB	X12, Y12
	MOVL		$0x2e, AX
	MOVD		AX, X11
	VPBROADCASTB	X11, Y11
	VMOVDQU		hex_dump_shuf1<>+0(SB), Y10
	VMOVDQU		hex_dump_sp1<>+0(SB), Y9
	VMOVDQU		hex_dump_shuf2<>+0(SB), Y8
	VMOVDQU		hex_dump_sp2<>+0(SB), Y7

	CMPQ	CX, $2
	JL	dump_avx2_1line_check

dump_avx2_loop2:
	// Format 2x 8-hex-digit offsets
	MOVL		R9, AX
	BSWAPL		AX
	VMOVD		AX, X0
	LEAL		16(R9), BX
	BSWAPL		BX
	VPINSRD		$1, BX, X0, X0
	VPSRLW		$4, X0, X1
	VPAND		X15, X1, X1
	VPAND		X15, X0, X2
	VPUNPCKLBW	X2, X1, X1
	VPSHUFB		X1, X14, X1
	MOVQ		X1, (DI)
	MOVW		$0x2020, 8(DI)
	VPEXTRQ		$1, X1, 79(DI)
	MOVW		$0x2020, 87(DI)

	// Load 32 input bytes (2 lines)
	VMOVDQU		(SI), Y0

	// Printable ASCII [0x20..0x7e] for both lines
	VPADDB		Y13, Y0, Y5
	VPCMPGTB	Y12, Y5, Y5
	VPBLENDVB	Y5, Y11, Y0, Y6

	// Encode 32 bytes -> Y3 (line 0: 32 hex chars), Y4 (line 1: 32 hex chars)
	VPERMQ		$0xd8, Y0, Y0
	VPSRLW		$4, Y0, Y1
	VPAND		Y15, Y1, Y1
	VPAND		Y15, Y0, Y2
	VPSHUFB		Y1, Y14, Y1
	VPSHUFB		Y2, Y14, Y2
	VPUNPCKLBW	Y2, Y1, Y3
	VPUNPCKHBW	Y2, Y1, Y4

	// Line 0 hex bytes -> [10:61] (16-byte stores overlap into [35:] and [61:], overwritten immediately below)
	VPSHUFB		Y10, Y3, Y5
	VPOR		Y9, Y5, Y5
	VPSHUFB		Y8, Y3, Y2
	VPOR		Y7, Y2, Y2
	VMOVDQU		X5, 10(DI)
	VMOVDQU		X2, 26(DI)
	VEXTRACTI128	$1, Y5, 35(DI)
	VEXTRACTI128	$1, Y2, 51(DI)

	// Line 0 ASCII -> [61:79]
	VMOVDQU		X6, 61(DI)
	MOVW		$0x0a7c, 77(DI)

	// Line 1 hex bytes -> [89:140]
	VPSHUFB		Y10, Y4, Y5
	VPOR		Y9, Y5, Y5
	VPSHUFB		Y8, Y4, Y2
	VPOR		Y7, Y2, Y2
	VMOVDQU		X5, 89(DI)
	VMOVDQU		X2, 105(DI)
	VEXTRACTI128	$1, Y5, 114(DI)
	VEXTRACTI128	$1, Y2, 130(DI)

	// Line 1 ASCII -> [140:158]
	VEXTRACTI128	$1, Y6, 140(DI)
	MOVW		$0x0a7c, 156(DI)

	ADDL	$32, R9
	ADDQ	$32, SI
	ADDQ	$158, DI
	SUBQ	$2, CX
	CMPQ	CX, $2
	JGE	dump_avx2_loop2

dump_avx2_1line_check:
	TESTQ	CX, CX
	JZ	dump_avx2_done

	// Single remaining line
	MOVL		R9, AX
	BSWAPL		AX
	VMOVD		AX, X0
	VPSRLW		$4, X0, X1
	VPAND		X15, X1, X1
	VPAND		X15, X0, X2
	VPUNPCKLBW	X2, X1, X1
	VPSHUFB		X1, X14, X1
	MOVQ		X1, (DI)
	MOVW		$0x2020, 8(DI)

	VMOVDQU		(SI), X0
	VPSRLW		$4, X0, X1
	VPAND		X15, X1, X1
	VPAND		X15, X0, X2
	VPSHUFB		X1, X14, X1
	VPSHUFB		X2, X14, X2
	VPUNPCKLBW	X2, X1, X3
	VPUNPCKHBW	X2, X1, X4

	VPSHUFB		X10, X3, X5
	VPOR		X9, X5, X5
	VMOVDQU		X5, 10(DI)
	VPSHUFB		X8, X3, X6
	VPOR		X7, X6, X6
	MOVQ		X6, 26(DI)
	MOVB		$0x20, 34(DI)

	VPSHUFB		X10, X4, X5
	VPOR		X9, X5, X5
	VMOVDQU		X5, 35(DI)
	VPSHUFB		X8, X4, X6
	VPOR		X7, X6, X6
	MOVQ		X6, 51(DI)
	MOVW		$0x7c20, 59(DI)

	VPADDB		X13, X0, X5
	VPCMPGTB	X12, X5, X5
	VPBLENDVB	X5, X11, X0, X0
	VMOVDQU		X0, 61(DI)
	MOVW		$0x0a7c, 77(DI)

dump_avx2_done:
	VZEROUPPER
	RET
