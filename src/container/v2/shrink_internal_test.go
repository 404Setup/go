// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import (
	"testing"
	"unsafe"
)

func TestShrinkBuiltinsReplaceStorage(t *testing.T) {
	t.Run("Map", func(t *testing.T) {
		m := make(map[int]int, 4096)
		for i := range 4096 {
			m[i] = i
		}
		for i := range 4080 {
			delete(m, i)
		}
		before := *(*unsafe.Pointer)(unsafe.Pointer(&m))
		shrunk := ShrinkMap(m)
		after := *(*unsafe.Pointer)(unsafe.Pointer(&shrunk))
		if before == after {
			t.Fatal("ShrinkMap retained the original map storage")
		}
	})

	t.Run("Slice", func(t *testing.T) {
		s := make([]int, 16, 4096)
		before := unsafe.SliceData(s)
		shrunk := ShrinkSlice(s)
		after := unsafe.SliceData(shrunk)
		if before == after {
			t.Fatal("ShrinkSlice retained the original backing array")
		}
	})
}
