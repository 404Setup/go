// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build gofips140

package fips140

import (
	"internal/godebug"
	_ "unsafe" // for linkname
)

func withoutEnforcement(f func()) {
	if !Enabled() || !Enforced() {
		f()
		return
	}
	setBypass()
	defer unsetBypass()
	f()
}

var enabled = godebug.New("fips140").Value() == "only"

func enforced() bool {
	return enabled && !isBypassed()
}

//go:linkname setBypass
func setBypass()

//go:linkname isBypassed
func isBypassed() bool

//go:linkname unsetBypass
func unsetBypass()
