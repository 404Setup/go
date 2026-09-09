// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 || 386

#include "textflag.h"

// FXSAVE needs a 16-byte-aligned, 512-byte buffer. This runs only at startup.
TEXT ·getMXCSRMask(SB),NOSPLIT,$528-4
#ifdef GO386_softfloat
	MOVL $0, ret+0(FP)
	RET
#else
#ifdef GOARCH_amd64
	LEAQ 15(SP), AX
	ANDQ $-16, AX
#else
	LEAL 15(SP), AX
	ANDL $-16, AX
#endif
	FXSAVE (AX)
	MOVL 28(AX), AX
	TESTL AX, AX
	JNZ maskReady
	// Intel's fallback mask excludes DAZ on CPUs that do not report support.
	MOVL $0xffbf, AX
maskReady:
	MOVL AX, ret+0(FP)
	RET
#endif

TEXT ·getFPControl(SB),NOSPLIT,$8-4
	STMXCSR 0(SP)
	MOVL 0(SP), AX
	MOVL AX, ret+0(FP)
	RET

TEXT ·setFPControl(SB),NOSPLIT,$0-4
	LDMXCSR value+0(FP)
	RET
