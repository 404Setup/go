// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package slices_test

import (
	"reflect"
	"runtime"
	"slices"
	"sync"
	"testing"
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

func TestToImmutableSliceBasic(t *testing.T) {
	var nilSlice []string
	if got := slices.ToImmutable(nilSlice); got != nil {
		t.Fatalf("ToImmutable(nil) = %v, want nil", got)
	}
	if slices.IsImmutable(nilSlice) {
		t.Fatal("IsImmutable(nil) = true, want false")
	}

	emptyCap := make([]string, 0, 8)
	immEmpty := slices.ToImmutable(emptyCap)
	if immEmpty == nil || len(immEmpty) != 0 || cap(immEmpty) != 0 {
		t.Fatalf("ToImmutable(empty) = (nil=%v, len=%d, cap=%d), want non-nil len=0 cap=0", immEmpty == nil, len(immEmpty), cap(immEmpty))
	}
	if !slices.IsImmutable(immEmpty) {
		t.Fatal("IsImmutable(immEmpty) = false, want true")
	}
	if slices.IsImmutable(emptyCap) {
		t.Fatal("IsImmutable(mutable empty) = true, want false")
	}

	// Zero-sized element slice.
	z := make([]struct{}, 5, 10)
	immZ := slices.ToImmutable(z)
	if len(immZ) != 5 || cap(immZ) != 5 || !slices.IsImmutable(immZ) {
		t.Fatalf("ToImmutable([]struct{}) = (len=%d, cap=%d, imm=%v)", len(immZ), cap(immZ), slices.IsImmutable(immZ))
	}

	orig := make([]string, 3, 10)
	orig[0], orig[1], orig[2] = "alpha", "beta", "gamma"
	imm := slices.ToImmutable(orig)
	if !slices.IsImmutable(imm) {
		t.Fatal("IsImmutable(imm) = false, want true")
	}
	if len(imm) != 3 || cap(imm) != 3 {
		t.Fatalf("len/cap = %d/%d, want 3/3", len(imm), cap(imm))
	}

	// Mutating original must not affect immutable copy.
	orig[0] = "mutated"
	if imm[0] != "alpha" {
		t.Fatalf("imm[0] = %q after orig mutation, want %q", imm[0], "alpha")
	}

	// Named slice type.
	type strList []string
	named := slices.ToImmutable(strList{"x", "y"})
	if !slices.IsImmutable(named) || len(named) != 2 || named[1] != "y" {
		t.Fatalf("named immutable slice failed: %v", named)
	}

	// Idempotent and zero-alloc on already-immutable slice.
	imm2 := slices.ToImmutable(imm)
	if !slices.IsImmutable(imm2) || len(imm2) != 3 || cap(imm2) != 3 {
		t.Fatalf("ToImmutable(imm) failed")
	}
	if allocs := testing.AllocsPerRun(50, func() {
		_ = slices.ToImmutable(imm)
	}); allocs != 0 {
		t.Fatalf("ToImmutable(imm) allocs = %v, want 0", allocs)
	}

	// Sub-slice trims cap without allocating.
	subImm := slices.ToImmutable(imm[:2])
	if len(subImm) != 2 || cap(subImm) != 2 || !slices.IsImmutable(subImm) {
		t.Fatalf("ToImmutable(imm[:2]) = len=%d cap=%d imm=%v", len(subImm), cap(subImm), slices.IsImmutable(subImm))
	}

	// Clone produces a mutable copy.
	cloned := slices.Clone(imm)
	if slices.IsImmutable(cloned) {
		t.Fatal("slices.Clone(imm) should return mutable slice")
	}
	cloned[0] = "changed"
	if imm[0] != "alpha" {
		t.Fatal("mutating clone affected immutable slice")
	}

	// append past cap allocates a new mutable slice without touching imm.
	appended := append(imm, "delta")
	if len(appended) != 4 || slices.IsImmutable(appended) || len(imm) != 3 {
		t.Fatalf("append on immutable slice misbehaved: len=%d imm=%v", len(appended), slices.IsImmutable(appended))
	}
}

func TestToImmutableSliceMutationPanics(t *testing.T) {
	ints := slices.ToImmutable([]int64{30, 10, 20})
	strs := slices.ToImmutable([]string{"c", "a", "b"})

	mustPanic(t, "int64 index write", func() { ints[0] = 99 })
	mustPanic(t, "int64 compound write", func() { ints[1]++ })
	mustPanic(t, "int64 copy", func() { copy(ints, []int64{1, 2, 3}) })
	mustPanic(t, "int64 clear", func() { clear(ints) })
	mustPanic(t, "int64 subslice append", func() {
		sub := ints[:1]
		_ = append(sub, 999)
	})
	mustPanic(t, "int64 sort", func() { slices.Sort(ints) })
	mustPanic(t, "int64 reverse", func() { slices.Reverse(ints) })
	mustPanic(t, "int64 reflect index set", func() { reflect.ValueOf(ints).Index(0).SetInt(42) })
	mustPanic(t, "int64 reflect copy", func() { reflect.Copy(reflect.ValueOf(ints), reflect.ValueOf([]int64{7})) })
	mustPanic(t, "int64 reflect clear", func() { reflect.ValueOf(ints).Clear() })

	mustPanic(t, "string index write", func() { strs[0] = "z" })
	mustPanic(t, "string copy", func() { copy(strs, []string{"x", "y", "z"}) })
	mustPanic(t, "string clear", func() { clear(strs) })
	mustPanic(t, "string subslice append", func() {
		sub := strs[:1]
		_ = append(sub, "hacked")
	})
	mustPanic(t, "string sort", func() { slices.Sort(strs) })
	mustPanic(t, "string reflect index set", func() { reflect.ValueOf(strs).Index(1).SetString("hacked") })
	mustPanic(t, "string reflect index setZero", func() { reflect.ValueOf(strs).Index(1).SetZero() })
	mustPanic(t, "string reflect copy", func() { reflect.Copy(reflect.ValueOf(strs), reflect.ValueOf([]string{"x"})) })
	mustPanic(t, "string reflect clear", func() { reflect.ValueOf(strs).Clear() })

	// Verify values are completely intact after all panicked attempts.
	if !slices.Equal(ints, []int64{30, 10, 20}) {
		t.Fatalf("ints corrupted: %v", ints)
	}
	if !slices.Equal(strs, []string{"c", "a", "b"}) {
		t.Fatalf("strs corrupted: %v", strs)
	}
}

func TestToImmutableSliceGCAndConcurrency(t *testing.T) {
	// Repeatedly allocate and drop immutable slices across GC cycles to
	// exercise span reuse, mprotect unprotection, and sanitizer state.
	for i := range 16 {
		tmp := slices.ToImmutable([]int64{int64(i), 2, 3, 4})
		if tmp[0] != int64(i) {
			t.Fatalf("tmp[0] = %d, want %d", tmp[0], i)
		}
		runtime.GC()
	}

	type box struct {
		val string
	}
	src := make([]*box, 64)
	for i := range src {
		src[i] = &box{val: "item"}
	}
	imm := slices.ToImmutable(src)
	src = nil
	runtime.GC()
	for i, b := range imm {
		if b == nil || b.val != "item" {
			t.Fatalf("imm[%d] corrupted after GC: %#v", i, b)
		}
	}

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 200 {
				v := imm[(id+i)%len(imm)]
				if v.val != "item" {
					t.Errorf("concurrent read mismatch: %q", v.val)
				}
			}
		}(g)
	}
	wg.Wait()
}

func BenchmarkToImmutableSlice(b *testing.B) {
	src := make([]int64, 1024)
	for i := range src {
		src[i] = int64(i)
	}
	b.ReportAllocs()
	for b.Loop() {
		imm := slices.ToImmutable(src)
		if len(imm) != len(src) {
			b.Fatal("unexpected len")
		}
	}
}

func BenchmarkToImmutableSliceIdempotent(b *testing.B) {
	imm := slices.ToImmutable(make([]int64, 1024))
	b.ReportAllocs()
	for b.Loop() {
		if got := slices.ToImmutable(imm); len(got) != 1024 {
			b.Fatal("unexpected len")
		}
	}
}

func BenchmarkImmutableSliceRead(b *testing.B) {
	mut := make([]int64, 1024)
	for i := range mut {
		mut[i] = int64(i)
	}
	imm := slices.ToImmutable(mut)
	b.Run("Mutable", func(b *testing.B) {
		var sum int64
		for b.Loop() {
			for _, v := range mut {
				sum += v
			}
		}
		if sum == -1 {
			b.Fatal(sum)
		}
	})
	b.Run("Immutable", func(b *testing.B) {
		var sum int64
		for b.Loop() {
			for _, v := range imm {
				sum += v
			}
		}
		if sum == -1 {
			b.Fatal(sum)
		}
	})
}
