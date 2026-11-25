// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ring_test

import (
	"container/ring"
	"runtime"
	"testing"
	"weak"
)

func mustPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("%s: expected panic, got none", name)
		}
	}()
	fn()
}

func TestToImmutableRing(t *testing.T) {
	var nilRing *ring.Ring
	if got := ring.ToImmutable(nilRing); got != nil {
		t.Fatalf("ToImmutable(nil) = %v, want nil", got)
	}
	if ring.IsImmutable(nilRing) || nilRing.IsImmutable() {
		t.Fatal("IsImmutable(nil) = true, want false")
	}

	// Zero-value 1-element Ring without mutating source.
	zero := &ring.Ring{Value: 99}
	immZero := ring.ToImmutable(zero)
	if !immZero.IsImmutable() || immZero.Len() != 1 || immZero.Value != 99 || immZero.Next() != immZero {
		t.Fatalf("unexpected zero-ring immutable clone: len=%d val=%v", immZero.Len(), immZero.Value)
	}

	src := ring.New(4)
	for i := range 4 {
		src.Value = (i + 1) * 10
		src = src.Next()
	}

	imm := ring.ToImmutable(src)
	if !imm.IsImmutable() || !ring.IsImmutable(imm) || imm.Len() != 4 {
		t.Fatalf("imm invalid: len=%d", imm.Len())
	}
	for i := range 4 {
		if !imm.Move(i).IsImmutable() {
			t.Fatalf("element %d not marked immutable", i)
		}
	}

	src.Value = -1
	sum := 0
	imm.Do(func(v any) { sum += v.(int) })
	if sum != 100 {
		t.Fatalf("sum = %d, want 100", sum)
	}

	if imm.ToImmutable() != imm || ring.ToImmutable(imm) != imm {
		t.Fatal("ToImmutable not idempotent")
	}
	if allocs := testing.AllocsPerRun(50, func() {
		_ = ring.ToImmutable(imm)
	}); allocs != 0 {
		t.Fatalf("ToImmutable(imm) allocs = %v, want 0", allocs)
	}

	other := ring.New(2)
	mustPanic(t, "Link on imm", func() { imm.Link(other) })
	mustPanic(t, "Link on imm.Next()", func() { imm.Next().Link(other) })
	mustPanic(t, "Link mutable with imm", func() { other.Link(imm) })
	mustPanic(t, "Unlink(1)", func() { imm.Unlink(1) })
	mustPanic(t, "Unlink(0)", func() { imm.Unlink(0) })
}

func TestImmutableRingGCReclaimed(t *testing.T) {
	var wp weak.Pointer[ring.Ring]
	func() {
		r := ring.New(32)
		for i := range 32 {
			r.Value = i
			r = r.Next()
		}
		imm := ring.ToImmutable(r)
		wp = weak.Make(imm)
	}()
	for range 4 {
		runtime.GC()
		if wp.Value() == nil {
			return
		}
	}
	t.Fatal("immutable *ring.Ring was not reclaimed by GC")
}
