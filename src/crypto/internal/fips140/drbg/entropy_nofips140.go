// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !gofips140 && !wasm

package drbg

// Keep the non-FIPS implementation free of the FIPS entropy source and its
// large scratch buffer. Calls to getEntropy are behind the compile-time
// fips140.Enabled branch in rand.go.
func getEntropy() *[SeedSize]byte {
	return nil
}
