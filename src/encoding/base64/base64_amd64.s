// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64

#include "textflag.h"

// Constants for AVX2 and SSSE3 base64 codecs.

DATA b64_enc_shuf_first<>+0(SB)/8, $0x0405030401020001
DATA b64_enc_shuf_first<>+8(SB)/8, $0x0a0b090a07080607
DATA b64_enc_shuf_first<>+16(SB)/8, $0x0405030401020001
DATA b64_enc_shuf_first<>+24(SB)/8, $0x0a0b090a07080607
GLOBL b64_enc_shuf_first<>(SB), RODATA|NOPTR, $32

DATA b64_enc_shuf_loop<>+0(SB)/8, $0x0809070805060405
DATA b64_enc_shuf_loop<>+8(SB)/8, $0x0e0f0d0e0b0c0a0b
DATA b64_enc_shuf_loop<>+16(SB)/8, $0x0405030401020001
DATA b64_enc_shuf_loop<>+24(SB)/8, $0x0a0b090a07080607
GLOBL b64_enc_shuf_loop<>(SB), RODATA|NOPTR, $32

DATA b64_enc_mask1<>+0(SB)/8, $0x003f03f0003f03f0
DATA b64_enc_mask1<>+8(SB)/8, $0x003f03f0003f03f0
DATA b64_enc_mask1<>+16(SB)/8, $0x003f03f0003f03f0
DATA b64_enc_mask1<>+24(SB)/8, $0x003f03f0003f03f0
GLOBL b64_enc_mask1<>(SB), RODATA|NOPTR, $32

DATA b64_enc_mul1<>+0(SB)/8, $0x0100001001000010
DATA b64_enc_mul1<>+8(SB)/8, $0x0100001001000010
DATA b64_enc_mul1<>+16(SB)/8, $0x0100001001000010
DATA b64_enc_mul1<>+24(SB)/8, $0x0100001001000010
GLOBL b64_enc_mul1<>(SB), RODATA|NOPTR, $32

DATA b64_enc_mask2<>+0(SB)/8, $0x0fc0fc000fc0fc00
DATA b64_enc_mask2<>+8(SB)/8, $0x0fc0fc000fc0fc00
DATA b64_enc_mask2<>+16(SB)/8, $0x0fc0fc000fc0fc00
DATA b64_enc_mask2<>+24(SB)/8, $0x0fc0fc000fc0fc00
GLOBL b64_enc_mask2<>(SB), RODATA|NOPTR, $32

DATA b64_enc_mul2<>+0(SB)/8, $0x0400004004000040
DATA b64_enc_mul2<>+8(SB)/8, $0x0400004004000040
DATA b64_enc_mul2<>+16(SB)/8, $0x0400004004000040
DATA b64_enc_mul2<>+24(SB)/8, $0x0400004004000040
GLOBL b64_enc_mul2<>(SB), RODATA|NOPTR, $32

DATA b64_enc_lut_std<>+0(SB)/8, $0xfcfcfcfcfcfcfc47
DATA b64_enc_lut_std<>+8(SB)/8, $0x000041f0edfcfcfc
DATA b64_enc_lut_std<>+16(SB)/8, $0xfcfcfcfcfcfcfc47
DATA b64_enc_lut_std<>+24(SB)/8, $0x000041f0edfcfcfc
GLOBL b64_enc_lut_std<>(SB), RODATA|NOPTR, $32

DATA b64_enc_lut_url<>+0(SB)/8, $0xfcfcfcfcfcfcfc47
DATA b64_enc_lut_url<>+8(SB)/8, $0x00004120effcfcfc
DATA b64_enc_lut_url<>+16(SB)/8, $0xfcfcfcfcfcfcfc47
DATA b64_enc_lut_url<>+24(SB)/8, $0x00004120effcfcfc
GLOBL b64_enc_lut_url<>(SB), RODATA|NOPTR, $32

DATA b64_dec_lo_std<>+0(SB)/8, $0x1111111111111115
DATA b64_dec_lo_std<>+8(SB)/8, $0x1a1b1b1b1a131111
DATA b64_dec_lo_std<>+16(SB)/8, $0x1111111111111115
DATA b64_dec_lo_std<>+24(SB)/8, $0x1a1b1b1b1a131111
GLOBL b64_dec_lo_std<>(SB), RODATA|NOPTR, $32

DATA b64_dec_hi_std<>+0(SB)/8, $0x0804080402011010
DATA b64_dec_hi_std<>+8(SB)/8, $0x1010101010101010
DATA b64_dec_hi_std<>+16(SB)/8, $0x0804080402011010
DATA b64_dec_hi_std<>+24(SB)/8, $0x1010101010101010
GLOBL b64_dec_hi_std<>(SB), RODATA|NOPTR, $32

DATA b64_dec_lo_url<>+0(SB)/8, $0x1111111111111115
DATA b64_dec_lo_url<>+8(SB)/8, $0x331b1a1b1b131111
DATA b64_dec_lo_url<>+16(SB)/8, $0x1111111111111115
DATA b64_dec_lo_url<>+24(SB)/8, $0x331b1a1b1b131111
GLOBL b64_dec_lo_url<>(SB), RODATA|NOPTR, $32

DATA b64_dec_hi_url<>+0(SB)/8, $0x2804080402011010
DATA b64_dec_hi_url<>+8(SB)/8, $0x1010101010101010
DATA b64_dec_hi_url<>+16(SB)/8, $0x2804080402011010
DATA b64_dec_hi_url<>+24(SB)/8, $0x1010101010101010
GLOBL b64_dec_hi_url<>(SB), RODATA|NOPTR, $32

DATA b64_dec_roll_std<>+0(SB)/8, $0xb9b9bfbf04130000
DATA b64_dec_roll_std<>+8(SB)/8, $0x0000000000000000
DATA b64_dec_roll_std<>+16(SB)/8, $0xb9b9bfbf04130000
DATA b64_dec_roll_std<>+24(SB)/8, $0x0000000000000000
GLOBL b64_dec_roll_std<>(SB), RODATA|NOPTR, $32

DATA b64_dec_roll_url<>+0(SB)/8, $0xb9b9bfbf04110000
DATA b64_dec_roll_url<>+8(SB)/8, $0x0000000000000000
DATA b64_dec_roll_url<>+16(SB)/8, $0xb9b9bfbf04110000
DATA b64_dec_roll_url<>+24(SB)/8, $0x0000000000000000
GLOBL b64_dec_roll_url<>(SB), RODATA|NOPTR, $32

DATA b64_dec_madd1<>+0(SB)/8, $0x0140014001400140
DATA b64_dec_madd1<>+8(SB)/8, $0x0140014001400140
DATA b64_dec_madd1<>+16(SB)/8, $0x0140014001400140
DATA b64_dec_madd1<>+24(SB)/8, $0x0140014001400140
GLOBL b64_dec_madd1<>(SB), RODATA|NOPTR, $32

DATA b64_dec_madd2<>+0(SB)/8, $0x0001100000011000
DATA b64_dec_madd2<>+8(SB)/8, $0x0001100000011000
DATA b64_dec_madd2<>+16(SB)/8, $0x0001100000011000
DATA b64_dec_madd2<>+24(SB)/8, $0x0001100000011000
GLOBL b64_dec_madd2<>(SB), RODATA|NOPTR, $32

DATA b64_dec_pack_shuf<>+0(SB)/8, $0x090a040506000102
DATA b64_dec_pack_shuf<>+8(SB)/8, $0xffffffff0c0d0e08
DATA b64_dec_pack_shuf<>+16(SB)/8, $0x090a040506000102
DATA b64_dec_pack_shuf<>+24(SB)/8, $0xffffffff0c0d0e08
GLOBL b64_dec_pack_shuf<>(SB), RODATA|NOPTR, $32

DATA b64_dec_pack_perm<>+0(SB)/8, $0x0000000100000000
DATA b64_dec_pack_perm<>+8(SB)/8, $0x0000000400000002
DATA b64_dec_pack_perm<>+16(SB)/8, $0x0000000600000005
DATA b64_dec_pack_perm<>+24(SB)/8, $0x0000000000000000
GLOBL b64_dec_pack_perm<>(SB), RODATA|NOPTR, $32

DATA b64_c51<>+0(SB)/8, $0x3333333333333333
DATA b64_c51<>+8(SB)/8, $0x3333333333333333
GLOBL b64_c51<>(SB), RODATA|NOPTR, $16

DATA b64_c26<>+0(SB)/8, $0x1a1a1a1a1a1a1a1a
DATA b64_c26<>+8(SB)/8, $0x1a1a1a1a1a1a1a1a
GLOBL b64_c26<>(SB), RODATA|NOPTR, $16

DATA b64_c13<>+0(SB)/8, $0x0d0d0d0d0d0d0d0d
DATA b64_c13<>+8(SB)/8, $0x0d0d0d0d0d0d0d0d
GLOBL b64_c13<>(SB), RODATA|NOPTR, $16

DATA b64_c0f<>+0(SB)/8, $0x0f0f0f0f0f0f0f0f
DATA b64_c0f<>+8(SB)/8, $0x0f0f0f0f0f0f0f0f
GLOBL b64_c0f<>(SB), RODATA|NOPTR, $16

DATA b64_slash<>+0(SB)/8, $0x2f2f2f2f2f2f2f2f
DATA b64_slash<>+8(SB)/8, $0x2f2f2f2f2f2f2f2f
GLOBL b64_slash<>(SB), RODATA|NOPTR, $16

DATA b64_m3<>+0(SB)/8, $0xfdfdfdfdfdfdfdfd
DATA b64_m3<>+8(SB)/8, $0xfdfdfdfdfdfdfdfd
GLOBL b64_m3<>(SB), RODATA|NOPTR, $16

DATA b64_under<>+0(SB)/8, $0x5f5f5f5f5f5f5f5f
DATA b64_under<>+8(SB)/8, $0x5f5f5f5f5f5f5f5f
GLOBL b64_under<>(SB), RODATA|NOPTR, $16

DATA b64_p33<>+0(SB)/8, $0x2121212121212121
DATA b64_p33<>+8(SB)/8, $0x2121212121212121
GLOBL b64_p33<>(SB), RODATA|NOPTR, $16

// func encodeAVX2Asm(dst, src *byte, srcLen, dstLen int, kind uint8) (si, di int)
// Requires srcLen >= 28 && dstLen >= 32.
TEXT ·encodeAVX2Asm(SB), NOSPLIT, $0-56
	MOVQ	dst+0(FP), DI
	MOVQ	src+8(FP), SI
	MOVQ	srcLen+16(FP), R8
	MOVQ	dstLen+24(FP), R9
	MOVBLZX	kind+32(FP), AX
	MOVQ	SI, R10
	MOVQ	DI, R11

	MOVL		$0x33, CX
	MOVQ		CX, X15
	VPBROADCASTB	X15, Y15
	MOVL		$0x1a, CX
	MOVQ		CX, X14
	VPBROADCASTB	X14, Y14
	MOVL		$0x0d, CX
	MOVQ		CX, X13
	VPBROADCASTB	X13, Y13

	CMPL	AX, $2
	JE	enc_avx2_url
	VMOVDQU	b64_enc_lut_std<>+0(SB), Y12
	JMP	enc_avx2_lut_done
enc_avx2_url:
	VMOVDQU	b64_enc_lut_url<>+0(SB), Y12
enc_avx2_lut_done:
	VMOVDQU	b64_enc_shuf_loop<>+0(SB), Y11
	VMOVDQU	b64_enc_mask1<>+0(SB), Y10
	VMOVDQU	b64_enc_mul1<>+0(SB), Y9
	VMOVDQU	b64_enc_mask2<>+0(SB), Y8
	VMOVDQU	b64_enc_mul2<>+0(SB), Y7

	// First 24-byte block: read [0:16] and [12:28] safely within srcLen >= 28.
	VMOVDQU		(SI), X0
	VINSERTI128	$1, 12(SI), Y0, Y0
	VPSHUFB		b64_enc_shuf_first<>+0(SB), Y0, Y0
	VPAND		Y10, Y0, Y1
	VPMULLW		Y9, Y1, Y1
	VPAND		Y8, Y0, Y2
	VPMULHUW	Y7, Y2, Y2
	VPOR		Y1, Y2, Y0
	VPSUBUSB	Y15, Y0, Y1
	VPCMPGTB	Y0, Y14, Y2
	VPAND		Y13, Y2, Y2
	VPADDB		Y2, Y1, Y1
	VPSHUFB		Y1, Y12, Y1
	VPADDB		Y1, Y0, Y0
	VMOVDQU		Y0, (DI)
	ADDQ		$24, SI
	ADDQ		$32, DI
	SUBQ		$24, R8
	SUBQ		$32, R9

	CMPQ	R8, $52
	JL	enc_avx2_tail_check
	CMPQ	R9, $64
	JL	enc_avx2_tail_check

enc_avx2_loop2:
	VMOVDQU		-4(SI), Y0
	VMOVDQU		20(SI), Y3
	VPSHUFB		Y11, Y0, Y0
	VPSHUFB		Y11, Y3, Y3

	VPAND		Y10, Y0, Y1
	VPAND		Y10, Y3, Y4
	VPMULLW		Y9, Y1, Y1
	VPMULLW		Y9, Y4, Y4
	VPAND		Y8, Y0, Y2
	VPAND		Y8, Y3, Y5
	VPMULHUW	Y7, Y2, Y2
	VPMULHUW	Y7, Y5, Y5
	VPOR		Y1, Y2, Y0
	VPOR		Y4, Y5, Y3

	VPSUBUSB	Y15, Y0, Y1
	VPSUBUSB	Y15, Y3, Y4
	VPCMPGTB	Y0, Y14, Y2
	VPCMPGTB	Y3, Y14, Y5
	VPAND		Y13, Y2, Y2
	VPAND		Y13, Y5, Y5
	VPADDB		Y2, Y1, Y1
	VPADDB		Y5, Y4, Y4
	VPSHUFB		Y1, Y12, Y1
	VPSHUFB		Y4, Y12, Y4
	VPADDB		Y1, Y0, Y0
	VPADDB		Y4, Y3, Y3

	VMOVDQU		Y0, (DI)
	VMOVDQU		Y3, 32(DI)
	ADDQ		$48, SI
	ADDQ		$64, DI
	SUBQ		$48, R8
	SUBQ		$64, R9
	CMPQ		R8, $52
	JL		enc_avx2_tail_check
	CMPQ		R9, $64
	JGE		enc_avx2_loop2

enc_avx2_tail_check:
	CMPQ	R8, $28
	JL	enc_avx2_done
	CMPQ	R9, $32
	JL	enc_avx2_done

	VMOVDQU		-4(SI), Y0
	VPSHUFB		Y11, Y0, Y0
	VPAND		Y10, Y0, Y1
	VPMULLW		Y9, Y1, Y1
	VPAND		Y8, Y0, Y2
	VPMULHUW	Y7, Y2, Y2
	VPOR		Y1, Y2, Y0
	VPSUBUSB	Y15, Y0, Y1
	VPCMPGTB	Y0, Y14, Y2
	VPAND		Y13, Y2, Y2
	VPADDB		Y2, Y1, Y1
	VPSHUFB		Y1, Y12, Y1
	VPADDB		Y1, Y0, Y0
	VMOVDQU		Y0, (DI)
	ADDQ		$24, SI
	ADDQ		$32, DI

enc_avx2_done:
	SUBQ	R10, SI
	SUBQ	R11, DI
	MOVQ	SI, si+40(FP)
	MOVQ	DI, di+48(FP)
	VZEROUPPER
	RET

// func decodeAVX2Asm(dst, src *byte, srcLen, dstLen int, kind uint8) (si, di int)
TEXT ·decodeAVX2Asm(SB), NOSPLIT, $0-56
	MOVQ	dst+0(FP), DI
	MOVQ	src+8(FP), SI
	MOVQ	srcLen+16(FP), R8
	MOVQ	dstLen+24(FP), R9
	MOVBLZX	kind+32(FP), AX
	MOVQ	SI, R10
	MOVQ	DI, R11

	MOVL		$0x0f, CX
	MOVQ		CX, X15
	VPBROADCASTB	X15, Y15

	CMPL	AX, $2
	JE	dec_avx2_url
	VMOVDQU	b64_dec_lo_std<>+0(SB), Y14
	VMOVDQU	b64_dec_hi_std<>+0(SB), Y13
	VMOVDQU	b64_dec_roll_std<>+0(SB), Y12
	MOVL		$0x2f, CX
	MOVQ		CX, X11
	VPBROADCASTB	X11, Y11
	MOVL		$0xfd, CX
	MOVQ		CX, X10
	VPBROADCASTB	X10, Y10
	JMP	dec_avx2_lut_done
dec_avx2_url:
	VMOVDQU	b64_dec_lo_url<>+0(SB), Y14
	VMOVDQU	b64_dec_hi_url<>+0(SB), Y13
	VMOVDQU	b64_dec_roll_url<>+0(SB), Y12
	MOVL		$0x5f, CX
	MOVQ		CX, X11
	VPBROADCASTB	X11, Y11
	MOVL		$0x21, CX
	MOVQ		CX, X10
	VPBROADCASTB	X10, Y10
dec_avx2_lut_done:
	VMOVDQU	b64_dec_pack_perm<>+0(SB), Y9

	CMPQ	R8, $64
	JL	dec_avx2_loop1_check
	CMPQ	R9, $56
	JL	dec_avx2_loop1_check

dec_avx2_loop2:
	VMOVDQU	(SI), Y0
	VMOVDQU	32(SI), Y3

	VPSRLD	$4, Y0, Y1
	VPAND	Y15, Y0, Y2
	VPAND	Y15, Y1, Y1
	VPSHUFB	Y2, Y14, Y2
	VPSHUFB	Y1, Y13, Y6
	VPAND	Y2, Y6, Y6

	VPSRLD	$4, Y3, Y4
	VPAND	Y15, Y3, Y5
	VPAND	Y15, Y4, Y4
	VPSHUFB	Y5, Y14, Y5
	VPSHUFB	Y4, Y13, Y7
	VPAND	Y5, Y7, Y7

	VPOR	Y6, Y7, Y6
	VPTEST	Y6, Y6
	JNZ	dec_avx2_loop1_check

	VPCMPEQB	Y11, Y0, Y2
	VPAND		Y10, Y2, Y2
	VPSHUFB		Y1, Y12, Y1
	VPADDB		Y1, Y0, Y0
	VPADDB		Y2, Y0, Y0

	VPCMPEQB	Y11, Y3, Y5
	VPAND		Y10, Y5, Y5
	VPSHUFB		Y4, Y12, Y4
	VPADDB		Y4, Y3, Y3
	VPADDB		Y5, Y3, Y3

	VPMADDUBSW	b64_dec_madd1<>+0(SB), Y0, Y0
	VPMADDUBSW	b64_dec_madd1<>+0(SB), Y3, Y3
	VPMADDWD	b64_dec_madd2<>+0(SB), Y0, Y0
	VPMADDWD	b64_dec_madd2<>+0(SB), Y3, Y3
	VPSHUFB		b64_dec_pack_shuf<>+0(SB), Y0, Y0
	VPSHUFB		b64_dec_pack_shuf<>+0(SB), Y3, Y3
	VPERMD		Y0, Y9, Y0
	VPERMD		Y3, Y9, Y3

	VMOVDQU	Y0, (DI)
	VMOVDQU	Y3, 24(DI)
	ADDQ	$64, SI
	ADDQ	$48, DI
	SUBQ	$64, R8
	SUBQ	$48, R9
	CMPQ	R8, $64
	JL	dec_avx2_loop1_check
	CMPQ	R9, $56
	JGE	dec_avx2_loop2

dec_avx2_loop1_check:
	CMPQ	R8, $32
	JL	dec_avx2_done
	CMPQ	R9, $32
	JL	dec_avx2_tail24

dec_avx2_loop1:
	VMOVDQU	(SI), Y0
	VPSRLD	$4, Y0, Y1
	VPAND	Y15, Y0, Y2
	VPAND	Y15, Y1, Y1
	VPSHUFB	Y2, Y14, Y2
	VPSHUFB	Y1, Y13, Y6
	VPTEST	Y2, Y6
	JNZ	dec_avx2_done

	VPCMPEQB	Y11, Y0, Y2
	VPAND		Y10, Y2, Y2
	VPSHUFB		Y1, Y12, Y1
	VPADDB		Y1, Y0, Y0
	VPADDB		Y2, Y0, Y0

	VPMADDUBSW	b64_dec_madd1<>+0(SB), Y0, Y0
	VPMADDWD	b64_dec_madd2<>+0(SB), Y0, Y0
	VPSHUFB		b64_dec_pack_shuf<>+0(SB), Y0, Y0
	VPERMD		Y0, Y9, Y0
	VMOVDQU		Y0, (DI)
	ADDQ		$32, SI
	ADDQ		$24, DI
	SUBQ		$32, R8
	SUBQ		$24, R9
	CMPQ		R8, $32
	JL		dec_avx2_done
	CMPQ		R9, $32
	JGE		dec_avx2_loop1

dec_avx2_tail24:
	CMPQ	R8, $32
	JL	dec_avx2_done
	CMPQ	R9, $24
	JL	dec_avx2_done

	VMOVDQU	(SI), Y0
	VPSRLD	$4, Y0, Y1
	VPAND	Y15, Y0, Y2
	VPAND	Y15, Y1, Y1
	VPSHUFB	Y2, Y14, Y2
	VPSHUFB	Y1, Y13, Y6
	VPTEST	Y2, Y6
	JNZ	dec_avx2_done

	VPCMPEQB	Y11, Y0, Y2
	VPAND		Y10, Y2, Y2
	VPSHUFB		Y1, Y12, Y1
	VPADDB		Y1, Y0, Y0
	VPADDB		Y2, Y0, Y0

	VPMADDUBSW	b64_dec_madd1<>+0(SB), Y0, Y0
	VPMADDWD	b64_dec_madd2<>+0(SB), Y0, Y0
	VPSHUFB		b64_dec_pack_shuf<>+0(SB), Y0, Y0
	VPERMD		Y0, Y9, Y0
	VMOVDQU		X0, (DI)
	VEXTRACTI128	$1, Y0, X1
	VMOVQ		X1, 16(DI)
	ADDQ		$32, SI
	ADDQ		$24, DI

dec_avx2_done:
	SUBQ	R10, SI
	SUBQ	R11, DI
	MOVQ	SI, si+40(FP)
	MOVQ	DI, di+48(FP)
	VZEROUPPER
	RET

// func encodeSSSE3Asm(dst, src *byte, srcLen, dstLen int, kind uint8) (si, di int)
// Requires srcLen >= 16 && dstLen >= 16.
TEXT ·encodeSSSE3Asm(SB), NOSPLIT, $0-56
	MOVQ	dst+0(FP), DI
	MOVQ	src+8(FP), SI
	MOVQ	srcLen+16(FP), R8
	MOVQ	dstLen+24(FP), R9
	MOVBLZX	kind+32(FP), AX
	MOVQ	SI, R10
	MOVQ	DI, R11

	MOVOU	b64_c51<>+0(SB), X15
	MOVOU	b64_c26<>+0(SB), X14
	MOVOU	b64_c13<>+0(SB), X13
	CMPL	AX, $2
	JE	enc_ssse3_url
	MOVOU	b64_enc_lut_std<>+0(SB), X12
	JMP	enc_ssse3_lut_done
enc_ssse3_url:
	MOVOU	b64_enc_lut_url<>+0(SB), X12
enc_ssse3_lut_done:
	MOVOU	b64_enc_shuf_first<>+0(SB), X11
	MOVOU	b64_enc_mask1<>+0(SB), X10
	MOVOU	b64_enc_mul1<>+0(SB), X9
	MOVOU	b64_enc_mask2<>+0(SB), X8
	MOVOU	b64_enc_mul2<>+0(SB), X7

	CMPQ	R8, $16
	JL	enc_ssse3_done
	CMPQ	R9, $16
	JL	enc_ssse3_done

enc_ssse3_loop:
	MOVOU	(SI), X0
	PSHUFB	X11, X0
	MOVOA	X0, X1
	PAND	X10, X1
	PMULLW	X9, X1
	PAND	X8, X0
	PMULHUW	X7, X0
	POR	X1, X0

	MOVOA	X0, X1
	PSUBUSB	X15, X1
	MOVOA	X14, X2
	PCMPGTB	X0, X2
	PAND	X13, X2
	PADDB	X2, X1
	MOVOA	X12, X2
	PSHUFB	X1, X2
	PADDB	X2, X0
	MOVOU	X0, (DI)

	ADDQ	$12, SI
	ADDQ	$16, DI
	SUBQ	$12, R8
	SUBQ	$16, R9
	CMPQ	R8, $16
	JL	enc_ssse3_done
	CMPQ	R9, $16
	JGE	enc_ssse3_loop

enc_ssse3_done:
	SUBQ	R10, SI
	SUBQ	R11, DI
	MOVQ	SI, si+40(FP)
	MOVQ	DI, di+48(FP)
	RET

// func decodeSSSE3Asm(dst, src *byte, srcLen, dstLen int, kind uint8) (si, di int)
TEXT ·decodeSSSE3Asm(SB), NOSPLIT, $0-56
	MOVQ	dst+0(FP), DI
	MOVQ	src+8(FP), SI
	MOVQ	srcLen+16(FP), R8
	MOVQ	dstLen+24(FP), R9
	MOVBLZX	kind+32(FP), AX
	MOVQ	SI, R10
	MOVQ	DI, R11

	MOVOU	b64_c0f<>+0(SB), X15
	PXOR	X6, X6

	CMPL	AX, $2
	JE	dec_ssse3_url
	MOVOU	b64_dec_lo_std<>+0(SB), X14
	MOVOU	b64_dec_hi_std<>+0(SB), X13
	MOVOU	b64_dec_roll_std<>+0(SB), X12
	MOVOU	b64_slash<>+0(SB), X11
	MOVOU	b64_m3<>+0(SB), X10
	JMP	dec_ssse3_lut_done
dec_ssse3_url:
	MOVOU	b64_dec_lo_url<>+0(SB), X14
	MOVOU	b64_dec_hi_url<>+0(SB), X13
	MOVOU	b64_dec_roll_url<>+0(SB), X12
	MOVOU	b64_under<>+0(SB), X11
	MOVOU	b64_p33<>+0(SB), X10
dec_ssse3_lut_done:
	MOVOU	b64_dec_madd1<>+0(SB), X9
	MOVOU	b64_dec_madd2<>+0(SB), X8
	MOVOU	b64_dec_pack_shuf<>+0(SB), X7

	CMPQ	R8, $16
	JL	dec_ssse3_done
	CMPQ	R9, $12
	JL	dec_ssse3_done

dec_ssse3_loop:
	MOVOU	(SI), X0
	MOVOA	X0, X1
	PSRLL	$4, X1
	MOVOA	X0, X2
	PAND	X15, X2
	PAND	X15, X1

	MOVOA	X14, X3
	PSHUFB	X2, X3
	MOVOA	X13, X4
	PSHUFB	X1, X4
	PAND	X4, X3
	PCMPEQB	X6, X3
	PMOVMSKB X3, CX
	CMPL	CX, $0xffff
	JNE	dec_ssse3_done

	MOVOA	X0, X2
	PCMPEQB	X11, X2
	PAND	X10, X2
	MOVOA	X12, X3
	PSHUFB	X1, X3
	PADDB	X3, X0
	PADDB	X2, X0

	PMADDUBSW	X9, X0
	PMADDWL		X8, X0
	PSHUFB		X7, X0

	CMPQ	R9, $16
	JL	dec_ssse3_store12
	MOVOU	X0, (DI)
	ADDQ	$16, SI
	ADDQ	$12, DI
	SUBQ	$16, R8
	SUBQ	$12, R9
	CMPQ	R8, $16
	JL	dec_ssse3_done
	CMPQ	R9, $12
	JGE	dec_ssse3_loop
	JMP	dec_ssse3_done

dec_ssse3_store12:
	MOVQ	X0, (DI)
	PSRLO	$8, X0
	MOVL	X0, 8(DI)
	ADDQ	$16, SI
	ADDQ	$12, DI

dec_ssse3_done:
	SUBQ	R10, SI
	SUBQ	R11, DI
	MOVQ	SI, si+40(FP)
	MOVQ	DI, di+48(FP)
	RET

// func stripNewlinesAVX2Asm(p *byte, n int) int
TEXT ·stripNewlinesAVX2Asm(SB), NOSPLIT, $0-24
	MOVQ	p+0(FP), SI
	MOVQ	n+8(FP), R8
	MOVQ	SI, R10

	MOVL		$0x0d, AX
	MOVQ		AX, X15
	VPBROADCASTB	X15, Y15
	MOVL		$0x0a, AX
	MOVQ		AX, X14
	VPBROADCASTB	X14, Y14

strip_scan_clean:
	CMPQ	R8, $32
	JL	strip_scan_tail
	VMOVDQU		(SI), Y0
	VPCMPEQB	Y15, Y0, Y1
	VPCMPEQB	Y14, Y0, Y2
	VPOR		Y1, Y2, Y1
	VPMOVMSKB	Y1, AX
	TESTL		AX, AX
	JNZ		strip_found_first
	ADDQ		$32, SI
	SUBQ		$32, R8
	JMP		strip_scan_clean

strip_scan_tail:
	MOVQ	SI, DI
	JMP	strip_scalar_tail

strip_found_first:
	BSFL	AX, CX
	ADDQ	CX, SI
	SUBQ	CX, R8
	MOVQ	SI, DI

strip_skip_first_nl:
	TESTQ	R8, R8
	JZ	strip_done
	MOVBLZX	(SI), AX
	CMPL	AX, $0x0d
	JE	strip_skip_first_adv
	CMPL	AX, $0x0a
	JNE	strip_loop
strip_skip_first_adv:
	INCQ	SI
	DECQ	R8
	JMP	strip_skip_first_nl

strip_loop:
	CMPQ	R8, $32
	JL	strip_scalar_tail
	VMOVDQU		(SI), Y0
	VPCMPEQB	Y15, Y0, Y1
	VPCMPEQB	Y14, Y0, Y2
	VPOR		Y1, Y2, Y1
	VPMOVMSKB	Y1, AX
	TESTL		AX, AX
	JNZ		strip_has_nl
	VMOVDQU		Y0, (DI)
	ADDQ		$32, SI
	ADDQ		$32, DI
	SUBQ		$32, R8
	JMP		strip_loop

strip_has_nl:
	BSFL	AX, CX
	TESTL	CX, CX
	JZ	strip_skip_nl
	CMPL	CX, $16
	JL	strip_copy8
	MOVOU	(SI), X1
	MOVOU	X1, (DI)
	ADDQ	$16, SI
	ADDQ	$16, DI
	SUBQ	$16, R8
	SUBL	$16, CX
strip_copy8:
	CMPL	CX, $8
	JL	strip_copy4
	MOVQ	(SI), BX
	MOVQ	BX, (DI)
	ADDQ	$8, SI
	ADDQ	$8, DI
	SUBQ	$8, R8
	SUBL	$8, CX
strip_copy4:
	CMPL	CX, $4
	JL	strip_copy1
	MOVL	(SI), BX
	MOVL	BX, (DI)
	ADDQ	$4, SI
	ADDQ	$4, DI
	SUBQ	$4, R8
	SUBL	$4, CX
strip_copy1:
	TESTL	CX, CX
	JZ	strip_skip_nl
	MOVBLZX	(SI), BX
	MOVB	BX, (DI)
	INCQ	SI
	INCQ	DI
	DECQ	R8
	DECL	CX
	JMP	strip_copy1

strip_skip_nl:
	TESTQ	R8, R8
	JZ	strip_done
	MOVBLZX	(SI), AX
	CMPL	AX, $0x0d
	JE	strip_skip_adv
	CMPL	AX, $0x0a
	JNE	strip_loop
strip_skip_adv:
	INCQ	SI
	DECQ	R8
	JMP	strip_skip_nl

strip_scalar_tail:
	TESTQ	R8, R8
	JZ	strip_done
	MOVBLZX	(SI), AX
	INCQ	SI
	DECQ	R8
	CMPL	AX, $0x0d
	JE	strip_scalar_tail
	CMPL	AX, $0x0a
	JE	strip_scalar_tail
	MOVB	AX, (DI)
	INCQ	DI
	JMP	strip_scalar_tail

strip_done:
	SUBQ	R10, DI
	MOVQ	DI, ret+16(FP)
	VZEROUPPER
	RET
