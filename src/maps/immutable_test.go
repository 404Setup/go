// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package maps_test

import (
	"maps"
	"reflect"
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

func TestToImmutableMapBasic(t *testing.T) {
	var nilMap map[string]int
	if got := maps.ToImmutable(nilMap); got != nil {
		t.Fatalf("ToImmutable(nil) = %v, want nil", got)
	}
	if maps.IsImmutable(nilMap) {
		t.Fatal("IsImmutable(nil) = true, want false")
	}

	empty := make(map[string]int)
	immEmpty := maps.ToImmutable(empty)
	if immEmpty == nil || len(immEmpty) != 0 || !maps.IsImmutable(immEmpty) {
		t.Fatalf("ToImmutable(empty) failed: nil=%v len=%d imm=%v", immEmpty == nil, len(immEmpty), maps.IsImmutable(immEmpty))
	}
	if maps.IsImmutable(empty) {
		t.Fatal("mutable empty map reported immutable")
	}
	mustPanic(t, "empty map assign", func() { immEmpty["a"] = 1 })
	mustPanic(t, "empty map delete", func() { delete(immEmpty, "a") })
	mustPanic(t, "empty map clear", func() { clear(immEmpty) })

	orig := map[string]int64{"a": 1, "b": 2}
	imm := maps.ToImmutable(orig)
	if !maps.IsImmutable(imm) || len(imm) != 2 || imm["a"] != 1 || imm["b"] != 2 {
		t.Fatalf("ToImmutable failed: %v", imm)
	}
	// Mutating original must not affect immutable copy.
	orig["a"] = 999
	orig["c"] = 3
	if imm["a"] != 1 || len(imm) != 2 {
		t.Fatalf("immutable map affected by original mutation: %v", imm)
	}

	// Named map type.
	type namedMap map[string]int
	nm := maps.ToImmutable(namedMap{"x": 10})
	if !maps.IsImmutable(nm) || nm["x"] != 10 {
		t.Fatalf("named map ToImmutable failed: %v", nm)
	}

	// Idempotent and zero-alloc on already-immutable map.
	imm2 := maps.ToImmutable(imm)
	if !maps.IsImmutable(imm2) || !maps.Equal(imm, imm2) {
		t.Fatal("ToImmutable(imm) failed")
	}
	if allocs := testing.AllocsPerRun(50, func() {
		_ = maps.ToImmutable(imm)
	}); allocs != 0 {
		t.Fatalf("ToImmutable(imm) allocs = %v, want 0", allocs)
	}

	// Clone produces a mutable copy.
	cloned := maps.Clone(imm)
	if maps.IsImmutable(cloned) {
		t.Fatal("maps.Clone(imm) should return mutable map")
	}
	cloned["a"] = 42
	if imm["a"] != 1 {
		t.Fatal("mutating clone affected immutable map")
	}
}

func TestToImmutableMapAllPaths(t *testing.T) {
	// Fast32 (small and large).
	m32 := make(map[uint32]string)
	for i := range uint32(32) {
		m32[i] = "v"
	}
	imm32 := maps.ToImmutable(m32)
	mustPanic(t, "fast32 assign", func() { imm32[0] = "x" })
	mustPanic(t, "fast32 new key", func() { imm32[999] = "x" })
	mustPanic(t, "fast32 delete", func() { delete(imm32, 0) })
	mustPanic(t, "fast32 clear", func() { clear(imm32) })
	mustPanic(t, "fast32 grow", func() { maps.Grow(imm32, 16) })

	imm32Empty := maps.ToImmutable(map[uint32]string{})
	mustPanic(t, "fast32 empty delete", func() { delete(imm32Empty, 1) })

	// Fast64.
	m64 := make(map[int64]int64)
	for i := range int64(32) {
		m64[i] = i * 10
	}
	imm64 := maps.ToImmutable(m64)
	mustPanic(t, "fast64 assign", func() { imm64[1] = 0 })
	mustPanic(t, "fast64 delete", func() { delete(imm64, 1) })
	mustPanic(t, "fast64 clear", func() { clear(imm64) })

	imm64Empty := maps.ToImmutable(map[int64]int64{})
	mustPanic(t, "fast64 empty delete", func() { delete(imm64Empty, 1) })

	// Fastptr.
	k1, k2 := new(int), new(int)
	mptr := maps.ToImmutable(map[*int]string{k1: "k1", k2: "k2"})
	mustPanic(t, "fastptr assign", func() { mptr[k1] = "x" })
	mustPanic(t, "fastptr delete", func() { delete(mptr, k1) })

	// Faststr (small and large).
	mstr := make(map[string]int)
	for i := range 32 {
		mstr[string(rune('a'+i))] = i
	}
	immStr := maps.ToImmutable(mstr)
	mustPanic(t, "faststr assign", func() { immStr["a"] = 100 })
	mustPanic(t, "faststr delete", func() { delete(immStr, "a") })
	mustPanic(t, "faststr clear", func() { clear(immStr) })
	mustPanic(t, "faststr maps.Copy", func() { maps.Copy(immStr, map[string]int{"z": 1}) })
	mustPanic(t, "faststr maps.DeleteFunc", func() { maps.DeleteFunc(immStr, func(string, int) bool { return true }) })
	mustPanic(t, "faststr reflect SetMapIndex", func() {
		reflect.ValueOf(immStr).SetMapIndex(reflect.ValueOf("a"), reflect.ValueOf(9))
	})
	mustPanic(t, "faststr reflect delete", func() {
		reflect.ValueOf(immStr).SetMapIndex(reflect.ValueOf("a"), reflect.Value{})
	})
	mustPanic(t, "faststr reflect Clear", func() {
		reflect.ValueOf(immStr).Clear()
	})

	// Generic slow path (struct key + large indirect value).
	type bigKey struct{ a, b, c int64 }
	type bigVal [256]byte
	mslow := maps.ToImmutable(map[bigKey]bigVal{{1, 2, 3}: {1}})
	mustPanic(t, "slow assign", func() { mslow[bigKey{1, 2, 3}] = bigVal{2} })
	mustPanic(t, "slow delete", func() { delete(mslow, bigKey{1, 2, 3}) })
	mustPanic(t, "slow clear", func() { clear(mslow) })
	mustPanic(t, "slow reflect SetMapIndex", func() {
		reflect.ValueOf(mslow).SetMapIndex(reflect.ValueOf(bigKey{1, 2, 3}), reflect.ValueOf(bigVal{9}))
	})

	// Concurrent reads and panicked writes must preserve contents.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if v, ok := immStr["a"]; !ok || v != 0 {
					t.Errorf("unexpected lookup result: %d %v", v, ok)
				}
			}
		}()
	}
	wg.Wait()
}

func BenchmarkToImmutableMap(b *testing.B) {
	src := make(map[string]int64, 256)
	for i := range 256 {
		src[string(rune('A'+i%26))+string(rune('a'+i/26))] = int64(i)
	}
	b.ReportAllocs()
	for b.Loop() {
		imm := maps.ToImmutable(src)
		if len(imm) != len(src) {
			b.Fatal("unexpected len")
		}
	}
}

func BenchmarkToImmutableMapIdempotent(b *testing.B) {
	src := map[string]int64{"a": 1, "b": 2, "c": 3}
	imm := maps.ToImmutable(src)
	b.ReportAllocs()
	for b.Loop() {
		if got := maps.ToImmutable(imm); len(got) != 3 {
			b.Fatal("unexpected len")
		}
	}
}

func BenchmarkImmutableMapLookup(b *testing.B) {
	mut := make(map[string]int64, 256)
	keys := make([]string, 256)
	for i := range 256 {
		k := string(rune('A'+i%26)) + string(rune('a'+i/26))
		mut[k] = int64(i)
		keys[i] = k
	}
	imm := maps.ToImmutable(mut)
	b.Run("Mutable", func(b *testing.B) {
		var sum int64
		for b.Loop() {
			for _, k := range keys {
				sum += mut[k]
			}
		}
		if sum == -1 {
			b.Fatal(sum)
		}
	})
	b.Run("Immutable", func(b *testing.B) {
		var sum int64
		for b.Loop() {
			for _, k := range keys {
				sum += imm[k]
			}
		}
		if sum == -1 {
			b.Fatal(sum)
		}
	})
}

