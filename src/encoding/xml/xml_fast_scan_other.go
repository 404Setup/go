// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(goexperiment.simdstable && amd64)

package xml

const haveXMLSIMD = false

func xmlEscapeIndexSIMD(s []byte, i int) (int, bool) {
	return i, false
}

func xmlCharDataIndexSIMD(s []byte, i int) (int, bool) {
	return i, false
}

func xmlAttrValIndexSIMD(s []byte, i int, qb byte) (int, bool) {
	return i, false
}
