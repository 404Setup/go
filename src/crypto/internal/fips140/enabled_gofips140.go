// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build gofips140

package fips140

import "crypto/internal/fips140deps/godebug"

// Enabled reports whether FIPS 140 mode is enabled at run time.
var Enabled bool

var debug bool

func init() {
	v := godebug.Value("#fips140")
	switch v {
	case "on", "only":
		Enabled = true
	case "debug":
		Enabled = true
		debug = true
	case "off", "":
	default:
		panic("fips140: unknown GODEBUG setting fips140=" + v)
	}
}
