// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package reflect_test

import (
	"fmt"
	. "reflect"
	"runtime"
	"slices"
	"strconv"
	"testing"
)

// mapKeysChunkSizes covers map sizes below, at, and above the chunk
// boundary used by MapKeys for the key types below.
var mapKeysChunkSizes = []int{0, 1, 2, 3, 63, 64, 65, 127, 128, 129, 1000}

func TestMapKeysString(t *testing.T) {
	for _, n := range mapKeysChunkSizes {
		m := make(map[string]int, n)
		for i := 0; i < n; i++ {
			m[strconv.Itoa(i)] = i
		}
		keys := ValueOf(m).MapKeys()
		if len(keys) != n {
			t.Fatalf("n=%d: len(MapKeys) = %d", n, len(keys))
		}
		got := make([]string, 0, n)
		for _, k := range keys {
			if k.Kind() != String || k.CanSet() || k.CanAddr() {
				t.Fatalf("n=%d: unexpected key Value %v (CanSet=%v CanAddr=%v)", n, k, k.CanSet(), k.CanAddr())
			}
			got = append(got, k.String())
			if want := m[k.String()]; ValueOf(m).MapIndex(k).Int() != int64(want) {
				t.Fatalf("n=%d: MapIndex(%q) mismatch", n, k.String())
			}
		}
		slices.Sort(got)
		want := make([]string, 0, n)
		for k := range m {
			want = append(want, k)
		}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("n=%d: keys mismatch", n)
		}
	}
}

type mapKeysLarge [100]int

type mapKeysPtrStruct struct {
	P *int
	S string
}

func testMapKeysType[K comparable](t *testing.T, name string, mk func(i int) K) {
	t.Helper()
	for _, n := range mapKeysChunkSizes {
		m := make(map[K]int, n)
		for i := 0; i < n; i++ {
			m[mk(i)] = i
		}
		keys := ValueOf(m).MapKeys()
		if len(keys) != len(m) {
			t.Fatalf("%s n=%d: len(MapKeys) = %d, want %d", name, n, len(keys), len(m))
		}
		seen := make(map[K]bool, n)
		for _, k := range keys {
			kk := k.Interface().(K)
			if seen[kk] {
				t.Fatalf("%s n=%d: duplicate key %v", name, n, kk)
			}
			seen[kk] = true
			if _, ok := m[kk]; !ok {
				t.Fatalf("%s n=%d: unknown key %v", name, n, kk)
			}
		}
	}
}

func TestMapKeysTypes(t *testing.T) {
	testMapKeysType(t, "int", func(i int) int { return i })
	testMapKeysType(t, "int16", func(i int) int16 { return int16(i) })
	testMapKeysType(t, "float64", func(i int) float64 { return float64(i) + 0.5 })
	testMapKeysType(t, "complex128", func(i int) complex128 { return complex(float64(i), 1) })
	testMapKeysType(t, "[3]byte", func(i int) [3]byte { return [3]byte{byte(i), byte(i >> 8), 1} })
	testMapKeysType(t, "large", func(i int) mapKeysLarge { return mapKeysLarge{0: i, 99: ^i} })
	testMapKeysType(t, "any", func(i int) any {
		if i%2 == 0 {
			return i
		}
		return strconv.Itoa(i)
	})
	// Zero-size keys: the map holds a single entry.
	testMapKeysType(t, "struct{}", func(i int) struct{} { return struct{}{} })
	// Pointer-shaped keys take the direct path.
	ptrs := make([]*int, 1000)
	for i := range ptrs {
		ptrs[i] = new(int)
	}
	testMapKeysType(t, "*int", func(i int) *int { return ptrs[i] })
}

// TestMapKeysCopyIndependent checks that the returned keys are copies:
// changing the map afterwards must not change the Values.
func TestMapKeysCopyIndependent(t *testing.T) {
	m := make(map[string]int)
	for i := 0; i < 200; i++ {
		m["k"+strconv.Itoa(i)] = i
	}
	keys := ValueOf(m).MapKeys()
	want := make([]string, len(keys))
	for i, k := range keys {
		want[i] = k.String()
	}
	clear(m)
	for i := 0; i < 500; i++ {
		m["other"+strconv.Itoa(i)] = i
	}
	for i, k := range keys {
		if k.String() != want[i] {
			t.Fatalf("key %d changed from %q to %q", i, want[i], k.String())
		}
	}
}

// TestMapKeysKeysDistinctStorage checks that Values handed out for
// different keys do not alias one another, even when they share a chunk.
func TestMapKeysKeysDistinctStorage(t *testing.T) {
	m := make(map[[2]int]bool)
	for i := 0; i < 300; i++ {
		m[[2]int{i, -i}] = true
	}
	keys := ValueOf(m).MapKeys()
	for _, k := range keys {
		a := k.Interface().([2]int)
		if a[0] != -a[1] {
			t.Fatalf("corrupted key %v", a)
		}
	}
	// Interface values obtained from keys stay valid across GC.
	ifaces := make([]any, len(keys))
	for i, k := range keys {
		ifaces[i] = k.Interface()
	}
	runtime.GC()
	for _, x := range ifaces {
		a := x.([2]int)
		if a[0] != -a[1] || !m[a] {
			t.Fatalf("corrupted key %v after GC", a)
		}
	}
}

// TestMapKeysPointerStruct uses keys that contain pointers in a
// non-pointer-shaped struct, which requires the GC to scan the copies.
func TestMapKeysPointerStruct(t *testing.T) {
	m := make(map[mapKeysPtrStruct]int)
	for i := 0; i < 300; i++ {
		v := i
		m[mapKeysPtrStruct{&v, fmt.Sprint("s", i)}] = i
	}
	keys := ValueOf(m).MapKeys()
	m = nil
	for range 3 {
		runtime.GC()
	}
	seen := make(map[int]bool)
	for _, k := range keys {
		s := k.Interface().(mapKeysPtrStruct)
		if want := fmt.Sprint("s", *s.P); s.S != want {
			t.Fatalf("key %q does not match pointee %d", s.S, *s.P)
		}
		seen[*s.P] = true
	}
	if len(seen) != 300 {
		t.Fatalf("got %d distinct keys, want 300", len(seen))
	}
}

func TestMapKeysAllocs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping allocation test in short mode")
	}
	m := make(map[string]int)
	for i := 0; i < 64; i++ {
		m[strconv.Itoa(i)] = i
	}
	v := ValueOf(m)
	// One allocation for the result slice and one for the key chunk
	// (64 string headers fit in a single chunk).
	allocs := testing.AllocsPerRun(20, func() { v.MapKeys() })
	if allocs > 2 {
		t.Errorf("MapKeys of map[string]int with 64 keys allocates %v times, want <= 2", allocs)
	}
}

func BenchmarkMapKeysSizes(b *testing.B) {
	type bigKey [64]int
	run := func(b *testing.B, v Value) {
		b.ReportAllocs()
		for b.Loop() {
			v.MapKeys()
		}
	}
	for _, n := range []int{1, 8, 64, 1024} {
		b.Run("string/"+strconv.Itoa(n), func(b *testing.B) {
			m := make(map[string]int, n)
			for i := 0; i < n; i++ {
				m[strconv.Itoa(i)] = i
			}
			run(b, ValueOf(m))
		})
		b.Run("int/"+strconv.Itoa(n), func(b *testing.B) {
			m := make(map[int]int, n)
			for i := 0; i < n; i++ {
				m[i] = i
			}
			run(b, ValueOf(m))
		})
		b.Run("ptr/"+strconv.Itoa(n), func(b *testing.B) {
			m := make(map[*int]int, n)
			for i := 0; i < n; i++ {
				m[new(int)] = i
			}
			run(b, ValueOf(m))
		})
		b.Run("big/"+strconv.Itoa(n), func(b *testing.B) {
			m := make(map[bigKey]int, n)
			for i := 0; i < n; i++ {
				m[bigKey{0: i}] = i
			}
			run(b, ValueOf(m))
		})
	}
}
