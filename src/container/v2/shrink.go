// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import "maps"

// ShrinkMap returns a shallow copy of m in newly allocated map storage sized
// for its current entries. The returned map has the same type as m. A nil map
// remains nil, while an empty non-nil map remains non-nil.
//
// ShrinkMap always rebuilds a non-nil map because a map's retained bucket
// capacity is not exposed. Assign the result back to the original variable to
// let its old storage be reclaimed:
//
//	m = container.ShrinkMap(m)
//
// ShrinkMap is not safe when another goroutine is mutating m.
func ShrinkMap[M ~map[K]V, K comparable, V any](m M) M {
	if m == nil {
		return nil
	}
	shrunk := make(M, len(m))
	maps.Copy(shrunk, m)
	return shrunk
}

// ShrinkSlice returns a copy of s whose capacity equals its length. The
// returned slice has the same type as s. A nil slice remains nil, while an empty
// non-nil slice remains non-nil and releases any backing array.
//
// If s already has no spare capacity, ShrinkSlice returns s unchanged. Assign
// the result back to the original variable to let excess backing storage be
// reclaimed:
//
//	s = container.ShrinkSlice(s)
func ShrinkSlice[S ~[]E, E any](s S) S {
	if s == nil || len(s) == cap(s) {
		return s
	}
	if len(s) == 0 {
		return make(S, 0)
	}
	shrunk := make(S, len(s))
	copy(shrunk, s)
	return shrunk
}
