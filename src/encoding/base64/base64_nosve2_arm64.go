// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build arm64 && !arm64.v9.0

package base64

const haveSVE2 = false

func encodeSVE2(dst, src *byte, n int, lut *byte) (int, int) {
	return 0, 0
}

func decodeSVE2(dst, src *byte, n int, lut *byte) int {
	return 0
}
