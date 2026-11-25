// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64

package hex

import (
	"bytes"
	"errors"
	"testing"
)

func TestSSSE3OnlyMode(t *testing.T) {
	if !haveSSSE3 {
		t.Skip("SSSE3 not supported")
	}
	origAVX2 := haveAVX2
	origSIMD := haveSIMD
	origMinEnc := minSIMDEncode
	origMinDec := minSIMDDecode
	origMinDecOut := minSIMDDecodeOut
	defer func() {
		haveAVX2 = origAVX2
		haveSIMD = origSIMD
		minSIMDEncode = origMinEnc
		minSIMDDecode = origMinDec
		minSIMDDecodeOut = origMinDecOut
	}()

	haveAVX2 = false
	haveSIMD = true
	minSIMDEncode = 16
	minSIMDDecode = 16
	minSIMDDecodeOut = 8

	var raw [135]byte
	for i := range raw {
		raw[i] = byte(i*7 + 5)
	}
	for _, n := range []int{0, 1, 15, 16, 17, 31, 32, 33, 64, 135} {
		src := raw[:n]
		enc := make([]byte, EncodedLen(n))
		Encode(enc, src)
		dec := make([]byte, n)
		dn, err := Decode(dec, enc)
		if err != nil || dn != n || !bytes.Equal(dec, src) {
			t.Fatalf("SSSE3 roundtrip(%d) = (%d, %v)", n, dn, err)
		}
		if len(enc) >= 16 {
			corrupt := bytes.Clone(enc)
			corrupt[15] = 'g'
			out := make([]byte, n)
			cn, cerr := Decode(out, corrupt)
			if cn != 7 || !errors.Is(cerr, InvalidByteError('g')) {
				t.Fatalf("SSSE3 corrupt(%d) = (%d, %v), want (7, InvalidByteError('g'))", n, cn, cerr)
			}
		}
	}
}

func TestScalarFallbackMode(t *testing.T) {
	origSIMD := haveSIMD
	haveSIMD = false
	defer func() { haveSIMD = origSIMD }()

	var raw [129]byte
	for i := range raw {
		raw[i] = byte(i*3 + 1)
	}
	for _, n := range []int{0, 1, 7, 15, 16, 17, 31, 32, 33, 64, 129} {
		src := raw[:n]
		enc := make([]byte, EncodedLen(n))
		if en := Encode(enc, src); en != len(enc) {
			t.Fatalf("scalar Encode(%d) len=%d", n, en)
		}
		dec := make([]byte, n)
		dn, err := Decode(dec, enc)
		if err != nil || dn != n || !bytes.Equal(dec, src) {
			t.Fatalf("scalar Decode(%d) = (%d, %v)", n, dn, err)
		}
		if d := Dump(src); d == "" && n > 0 {
			t.Fatalf("scalar Dump(%d) empty", n)
		}
	}
}
