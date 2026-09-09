// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

#include "textflag.h"

TEXT ·getFPControl(SB),NOSPLIT,$0-4
	MRS FPCR, R0
	MOVW R0, ret+0(FP)
	RET

TEXT ·setFPControl(SB),NOSPLIT,$0-4
	MOVWU value+0(FP), R0
	MSR R0, FPCR
	RET
