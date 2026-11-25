// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container_test

import (
	"cmp"
	"runtime"
	"sync"
	"sync/v2/container"
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

func TestToImmutableLinkedHashMap(t *testing.T) {
	var nilMap *container.LinkedHashMap[string, int]
	if got := container.ToImmutable(nilMap); got != nil {
		t.Fatalf("ToImmutable(nil) = %v, want nil", got)
	}
	if container.IsImmutable(nilMap) || nilMap.IsImmutable() {
		t.Fatal("IsImmutable(nil) = true, want false")
	}

	// Access-order map: freezing preserves current order and disables access-order mutation on Load.
	src := container.NewLinkedHashMap[string, int](true)
	src.Store("a", 1)
	src.Store("b", 2)
	src.Store("c", 3)
	_, _ = src.Load("a") // order is now b, c, a

	imm := container.ToImmutable(src)
	if !imm.IsImmutable() || !container.IsImmutable(imm) || imm.Len() != 3 {
		t.Fatalf("unexpected imm state: imm=%v len=%d", imm.IsImmutable(), imm.Len())
	}

	// Concurrent Loads on frozen LinkedHashMap must be race-free and preserve order.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if v, ok := imm.Load("b"); !ok || v != 2 {
					panic("unexpected Load")
				}
			}
		}()
	}
	wg.Wait()

	k, v, ok := imm.First()
	if !ok || k != "b" || v != 2 {
		t.Fatalf("First() = (%q, %d, %v), want (\"b\", 2, true)", k, v, ok)
	}

	src.Store("b", 999)
	if v, _ := imm.Load("b"); v != 2 {
		t.Fatalf("imm mutated via src: got %d", v)
	}

	if allocs := testing.AllocsPerRun(50, func() {
		_ = container.ToImmutable(imm)
	}); allocs != 0 {
		t.Fatalf("idempotent ToImmutable allocs = %v, want 0", allocs)
	}

	mustPanic(t, "Store", func() { imm.Store("x", 9) })
	mustPanic(t, "Swap", func() { imm.Swap("b", 9) })
	mustPanic(t, "LoadOrStore hit", func() { imm.LoadOrStore("b", 9) })
	mustPanic(t, "LoadOrStore miss", func() { imm.LoadOrStore("z", 9) })
	mustPanic(t, "LoadAndDelete", func() { imm.LoadAndDelete("b") })
	mustPanic(t, "Delete", func() { imm.Delete("b") })
	mustPanic(t, "CompareAndSwap", func() { imm.CompareAndSwap("b", 2, 3) })
	mustPanic(t, "CompareAndDelete", func() { imm.CompareAndDelete("b", 2) })
	mustPanic(t, "Clear", func() { imm.Clear() })
	mustPanic(t, "Shrink", func() { imm.Shrink() })
	mustPanic(t, "PollFirst", func() { imm.PollFirst() })
	mustPanic(t, "PollLast", func() { imm.PollLast() })
}

func TestToImmutableOrderedMap(t *testing.T) {
	var nilMap *container.OrderedMap[string, int]
	if got := container.ToImmutable(nilMap); got != nil {
		t.Fatalf("ToImmutable(nil) = %v, want nil", got)
	}
	if container.IsImmutable(nilMap) {
		t.Fatal("IsImmutable(nil) = true, want false")
	}

	src := new(container.OrderedMap[string, int])
	src.Store("x", 10)
	src.Store("y", 20)
	src.Delete("x")
	src.Store("z", 30)

	imm := container.ToImmutable(src)
	if !container.IsImmutable(imm) {
		t.Fatal("OrderedMap not immutable")
	}
	if _, ok := imm.Load("x"); ok {
		t.Fatal("deleted key present in immutable snapshot")
	}
	if v, ok := imm.Load("y"); !ok || v != 20 {
		t.Fatalf("Load(y) = %d, %v", v, ok)
	}

	src.Store("y", 999)
	if v, _ := imm.Load("y"); v != 20 {
		t.Fatalf("imm mutated via src: %d", v)
	}

	if allocs := testing.AllocsPerRun(50, func() {
		_ = container.ToImmutable(imm)
	}); allocs != 0 {
		t.Fatalf("idempotent ToImmutable allocs = %v, want 0", allocs)
	}

	mustPanic(t, "Store", func() { imm.Store("a", 1) })
	mustPanic(t, "Swap", func() { imm.Swap("y", 1) })
	mustPanic(t, "LoadOrStore hit", func() { imm.LoadOrStore("y", 1) })
	mustPanic(t, "LoadAndDelete", func() { imm.LoadAndDelete("y") })
	mustPanic(t, "Delete", func() { imm.Delete("y") })
	mustPanic(t, "CompareAndSwap", func() { imm.CompareAndSwap("y", 20, 21) })
	mustPanic(t, "CompareAndDelete", func() { imm.CompareAndDelete("y", 20) })
	mustPanic(t, "Clear", func() { imm.Clear() })
	mustPanic(t, "Shrink", func() { imm.Shrink() })
}

func TestToImmutableTreeMapAndSkipListMap(t *testing.T) {
	var nilTree *container.TreeMap[int, string]
	var nilSkip *container.ConcurrentSkipListMap[int, string]
	if container.ToImmutable(nilTree) != nil || container.IsImmutable(nilTree) {
		t.Fatal("nil TreeMap failed")
	}
	if container.ToImmutable(nilSkip) != nil || container.IsImmutable(nilSkip) {
		t.Fatal("nil ConcurrentSkipListMap failed")
	}

	tm := container.NewTreeMap[int, string](cmp.Compare[int])
	sm := container.NewConcurrentSkipListMap[int, string](cmp.Compare[int])
	for i := 10; i >= 1; i-- {
		tm.Store(i*10, "v")
		sm.Store(i*10, "v")
	}

	immTM := container.ToImmutable(tm)
	immSM := container.ToImmutable(sm)
	if !immTM.IsImmutable() || immTM.Len() != 10 {
		t.Fatalf("immTM invalid: len=%d", immTM.Len())
	}
	if !immSM.IsImmutable() || immSM.Len() != 10 {
		t.Fatalf("immSM invalid: len=%d", immSM.Len())
	}

	if k, _, ok := immTM.First(); !ok || k != 10 {
		t.Fatalf("immTM.First = %d, %v", k, ok)
	}
	if k, _, ok := immSM.Last(); !ok || k != 100 {
		t.Fatalf("immSM.Last = %d, %v", k, ok)
	}
	if k, _, ok := immSM.Floor(55); !ok || k != 50 {
		t.Fatalf("immSM.Floor(55) = %d, %v", k, ok)
	}

	if allocs := testing.AllocsPerRun(50, func() {
		_ = container.ToImmutable(immTM)
		_ = container.ToImmutable(immSM)
	}); allocs != 0 {
		t.Fatalf("idempotent ToImmutable allocs = %v, want 0", allocs)
	}

	mustPanic(t, "tm Store", func() { immTM.Store(1, "x") })
	mustPanic(t, "tm LoadOrStore hit", func() { immTM.LoadOrStore(10, "x") })
	mustPanic(t, "tm Delete", func() { immTM.Delete(10) })
	mustPanic(t, "tm CompareAndSwap", func() { immTM.CompareAndSwap(10, "v", "x") })
	mustPanic(t, "tm CompareAndDelete", func() { immTM.CompareAndDelete(10, "v") })
	mustPanic(t, "tm Clear", func() { immTM.Clear() })
	mustPanic(t, "tm PollFirst", func() { immTM.PollFirst() })
	mustPanic(t, "tm PollLast", func() { immTM.PollLast() })

	mustPanic(t, "sm Store", func() { immSM.Store(1, "x") })
	mustPanic(t, "sm LoadOrStore hit", func() { immSM.LoadOrStore(10, "x") })
	mustPanic(t, "sm Delete", func() { immSM.Delete(10) })
	mustPanic(t, "sm CompareAndSwap", func() { immSM.CompareAndSwap(10, "v", "x") })
	mustPanic(t, "sm CompareAndDelete", func() { immSM.CompareAndDelete(10, "v") })
	mustPanic(t, "sm Clear", func() { immSM.Clear() })
	mustPanic(t, "sm PollFirst", func() { immSM.PollFirst() })
	mustPanic(t, "sm PollLast", func() { immSM.PollLast() })
}

func TestImmutableContainersGCReclaimed(t *testing.T) {
	var (
		wpLHM  weak.Pointer[container.LinkedHashMap[int, int]]
		wpOM   weak.Pointer[container.OrderedMap[int, int]]
		wpTM   weak.Pointer[container.TreeMap[int, int]]
		wpCSLM weak.Pointer[container.ConcurrentSkipListMap[int, int]]
	)
	func() {
		lhm := container.NewLinkedHashMap[int, int](false)
		om := new(container.OrderedMap[int, int])
		tm := container.NewTreeMap[int, int](cmp.Compare[int])
		cslm := container.NewConcurrentSkipListMap[int, int](cmp.Compare[int])
		for i := range 64 {
			lhm.Store(i, i)
			om.Store(i, i)
			tm.Store(i, i)
			cslm.Store(i, i)
		}
		wpLHM = weak.Make(container.ToImmutable(lhm))
		wpOM = weak.Make(container.ToImmutable(om))
		wpTM = weak.Make(container.ToImmutable(tm))
		wpCSLM = weak.Make(container.ToImmutable(cslm))
	}()
	for range 4 {
		runtime.GC()
		if wpLHM.Value() == nil && wpOM.Value() == nil && wpTM.Value() == nil && wpCSLM.Value() == nil {
			return
		}
	}
	t.Fatalf("immutable containers retained across GC: lhm=%v om=%v tm=%v cslm=%v",
		wpLHM.Value() != nil, wpOM.Value() != nil, wpTM.Value() != nil, wpCSLM.Value() != nil)
}

func BenchmarkImmutableContainerLoad(b *testing.B) {
	const entries = 256
	om := new(container.OrderedMap[int, int])
	sm := container.NewConcurrentSkipListMap[int, int](cmp.Compare[int])
	for i := range entries {
		om.Store(i, i)
		sm.Store(i, i)
	}
	immOM := container.ToImmutable(om)
	immSM := container.ToImmutable(sm)

	b.Run("OrderedMap/Mutable", func(b *testing.B) {
		b.ReportAllocs()
		for i := range b.N {
			_, _ = om.Load(i & (entries - 1))
		}
	})
	b.Run("OrderedMap/Immutable", func(b *testing.B) {
		b.ReportAllocs()
		for i := range b.N {
			_, _ = immOM.Load(i & (entries - 1))
		}
	})
	b.Run("SkipListMap/Mutable", func(b *testing.B) {
		b.ReportAllocs()
		for i := range b.N {
			_, _ = sm.Load(i & (entries - 1))
		}
	})
	b.Run("SkipListMap/Immutable", func(b *testing.B) {
		b.ReportAllocs()
		for i := range b.N {
			_, _ = immSM.Load(i & (entries - 1))
		}
	})
	b.Run("ToImmutableIdempotent/OrderedMap", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = container.ToImmutable(immOM)
		}
	})
}
