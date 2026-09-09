// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// fastMath is set by the linker's -fmth flag. The mask is initialized before
// starting any Ms, and is zero on targets without a supported flush mode.
var fastMath string
var fastMathMask uint32

//go:nosplit
func enableFastMath() uint32 {
	old := getFPControl()
	value := old | fastMathMask
	if GOARCH == "arm64" {
		value &^= 2 // FPCR.AH must be clear for FZ to flush inputs as well.
	}
	setFPControl(value)
	return old
}
