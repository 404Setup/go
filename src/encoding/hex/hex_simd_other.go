// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !amd64 && !arm64

package hex

const (
	haveSIMD         = false
	minSIMDEncode    = 0
	minSIMDDecode    = 0
	minSIMDDecodeOut = 0
)

func encodeSIMD(dst, src []byte) (si, di int) {
	return 0, 0
}

func decodeSIMD(dst, src []byte) (si, di int) {
	return 0, 0
}
