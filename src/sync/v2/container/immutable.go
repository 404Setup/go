// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

const errImmutableContainer = "sync/v2/container: modification of immutable container"

// ToImmutable returns an immutable copy of c. Any subsequent mutating method
// call on the returned container panics at runtime.
// If c is already immutable or nil, ToImmutable returns c without allocating.
func ToImmutable[C interface {
	ToImmutable() C
	IsImmutable() bool
}](c C) C {
	return c.ToImmutable()
}

// IsImmutable reports whether c is immutable.
// It returns false if c is nil.
func IsImmutable[C interface {
	ToImmutable() C
	IsImmutable() bool
}](c C) bool {
	return c.IsImmutable()
}
