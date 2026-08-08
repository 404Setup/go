// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build gofips140

package fips140

import (
	"crypto/internal/fips140"
	"crypto/internal/fips140/check"
)

func isEnabled() bool {
	if fips140.Enabled && !check.Verified {
		panic("crypto/fips140: FIPS 140-3 mode enabled, but integrity check didn't pass")
	}
	return fips140.Enabled
}

func moduleVersion() string { return fips140.Version() }
