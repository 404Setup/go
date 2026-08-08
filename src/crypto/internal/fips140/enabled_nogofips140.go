// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !gofips140

package fips140

// Enabled is a constant in non-FIPS builds so callers can eliminate FIPS-only
// branches during ordinary constant propagation.
const Enabled = false

const debug = false
