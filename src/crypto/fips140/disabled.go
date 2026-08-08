// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !gofips140

package fips140

func isEnabled() bool { return false }

func moduleVersion() string { return "latest" }
