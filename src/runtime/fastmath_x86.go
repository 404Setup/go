// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 || 386

package runtime

func initFastMath() {
	if fastMath != "1" {
		return
	}
	mask := getMXCSRMask()
	fastMathMask = mask & 0x8040 // FTZ (15), DAZ (6)
}

func getMXCSRMask() uint32
func getFPControl() uint32
func setFPControl(value uint32)
