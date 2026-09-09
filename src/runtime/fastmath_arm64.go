// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

func initFastMath() {
	if fastMath == "1" {
		fastMathMask = 1 << 24 // FPCR.FZ flushes binary32/64 inputs and results.
	}
}

func getFPControl() uint32
func setFPControl(value uint32)
