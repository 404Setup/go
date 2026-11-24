// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build arm64

package base64

import "internal/cpu"

var haveSVE2 = cpu.ARM64.HasSVE2 && sveVL() >= 32

//go:noescape
func sveVL() int

//go:noescape
func encodeSVE2(dst, src *byte, n int, lut *byte) (si, di int)

//go:noescape
func decodeSVE2(dst, src *byte, n int, lut *byte) int
