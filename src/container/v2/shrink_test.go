// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container_test

import (
	"container/v2"
	"reflect"
	"testing"
)

func TestShrinkMap(t *testing.T) {
	type namedMap map[int]string
	m := make(namedMap, 4096)
	for i := range 4096 {
		m[i] = "value"
	}
	for i := range 4080 {
		delete(m, i)
	}

	shrunk := container.ShrinkMap(m)
	if !reflect.DeepEqual(shrunk, m) {
		t.Fatalf("ShrinkMap result differs from input")
	}
	shrunk[4080] = "changed"
	if m[4080] != "value" {
		t.Fatal("ShrinkMap did not create independent map storage")
	}

	var nilMap namedMap
	if got := container.ShrinkMap(nilMap); got != nil {
		t.Fatalf("ShrinkMap(nil) = %v; want nil", got)
	}
	empty := make(namedMap)
	if got := container.ShrinkMap(empty); got == nil || len(got) != 0 {
		t.Fatalf("ShrinkMap(empty) = %v; want empty non-nil map", got)
	}
}

func TestShrinkSlice(t *testing.T) {
	type namedSlice []int
	s := make(namedSlice, 16, 4096)
	for i := range s {
		s[i] = i
	}

	shrunk := container.ShrinkSlice(s)
	if !reflect.DeepEqual(shrunk, s) {
		t.Fatalf("ShrinkSlice result = %v; want %v", shrunk, s)
	}
	if cap(shrunk) != len(shrunk) {
		t.Fatalf("ShrinkSlice capacity = %v; want length %v", cap(shrunk), len(shrunk))
	}
	shrunk[0] = 100
	if s[0] == 100 {
		t.Fatal("ShrinkSlice did not create independent backing storage")
	}

	var nilSlice namedSlice
	if got := container.ShrinkSlice(nilSlice); got != nil {
		t.Fatalf("ShrinkSlice(nil) = %v; want nil", got)
	}
	empty := make(namedSlice, 0, 4096)
	if got := container.ShrinkSlice(empty); got == nil || len(got) != 0 || cap(got) != 0 {
		t.Fatalf("ShrinkSlice(empty) len/cap/nil = %v/%v/%v; want 0/0/false", len(got), cap(got), got == nil)
	}

	tight := namedSlice{1, 2, 3}
	if got := container.ShrinkSlice(tight); &got[0] != &tight[0] {
		t.Fatal("ShrinkSlice reallocated a slice with no spare capacity")
	}
}
