// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import (
	"cmp"
	"sync"
	"sync/atomic"
	"testing"
)

type casBenchVal struct {
	a, b uint64
}

func TestContainerCompareAndSwapNoAllocs(t *testing.T) {
	t.Run("OrderedMap", func(t *testing.T) {
		var m OrderedMap[int, casBenchVal]
		m.Store(1, casBenchVal{1, 2})
		wrong := casBenchVal{9, 9}
		allocs := testing.AllocsPerRun(100, func() {
			if m.CompareAndSwap(1, wrong, casBenchVal{3, 4}) {
				t.Fatal("unexpected CAS success")
			}
			if m.CompareAndDelete(1, wrong) {
				t.Fatal("unexpected CAD success")
			}
		})
		if allocs != 0 {
			t.Fatalf("OrderedMap CAS/CAD miss allocs = %v, want 0", allocs)
		}
	})

	t.Run("LinkedHashMap", func(t *testing.T) {
		var m LinkedHashMap[int, casBenchVal]
		m.Store(1, casBenchVal{1, 2})
		v1 := casBenchVal{1, 2}
		v2 := casBenchVal{3, 4}
		wrong := casBenchVal{9, 9}
		allocs := testing.AllocsPerRun(100, func() {
			if m.CompareAndSwap(1, wrong, v2) {
				t.Fatal("unexpected CAS success")
			}
			if !m.CompareAndSwap(1, v1, v2) || !m.CompareAndSwap(1, v2, v1) {
				t.Fatal("CAS toggle failed")
			}
			if m.CompareAndDelete(1, wrong) {
				t.Fatal("unexpected CAD success")
			}
		})
		if allocs != 0 {
			t.Fatalf("LinkedHashMap CAS/CAD allocs = %v, want 0", allocs)
		}
	})

	t.Run("TreeMap", func(t *testing.T) {
		m := NewTreeMap[int, casBenchVal](cmp.Compare[int])
		m.Store(1, casBenchVal{1, 2})
		v1 := casBenchVal{1, 2}
		v2 := casBenchVal{3, 4}
		wrong := casBenchVal{9, 9}
		allocs := testing.AllocsPerRun(100, func() {
			if m.CompareAndSwap(1, wrong, v2) {
				t.Fatal("unexpected CAS success")
			}
			if !m.CompareAndSwap(1, v1, v2) || !m.CompareAndSwap(1, v2, v1) {
				t.Fatal("CAS toggle failed")
			}
			if m.CompareAndDelete(1, wrong) {
				t.Fatal("unexpected CAD success")
			}
		})
		if allocs != 0 {
			t.Fatalf("TreeMap CAS/CAD allocs = %v, want 0", allocs)
		}
	})

	t.Run("ConcurrentSkipListMap", func(t *testing.T) {
		m := NewConcurrentSkipListMap[int, casBenchVal](cmp.Compare[int])
		m.Store(1, casBenchVal{1, 2})
		wrong := casBenchVal{9, 9}
		allocs := testing.AllocsPerRun(100, func() {
			if m.CompareAndSwap(1, wrong, casBenchVal{3, 4}) {
				t.Fatal("unexpected CAS success")
			}
			if m.CompareAndDelete(1, wrong) {
				t.Fatal("unexpected CAD success")
			}
		})
		if allocs != 0 {
			t.Fatalf("ConcurrentSkipListMap CAS/CAD miss allocs = %v, want 0", allocs)
		}
	})
}

func TestContainerCompareAndSwapUncomparablePanics(t *testing.T) {
	assertPanics := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s did not panic", name)
			}
		}()
		fn()
	}

	var om OrderedMap[int, []int]
	assertPanics("OrderedMap slice CAS", func() { om.CompareAndSwap(1, nil, nil) })
	var lm LinkedHashMap[int, []int]
	assertPanics("LinkedHashMap slice CAD", func() { lm.CompareAndDelete(1, nil) })
	tm := NewTreeMap[int, []int](cmp.Compare[int])
	assertPanics("TreeMap slice CAS", func() { tm.CompareAndSwap(1, nil, nil) })
	sm := NewConcurrentSkipListMap[int, []int](cmp.Compare[int])
	assertPanics("ConcurrentSkipListMap slice CAD", func() { sm.CompareAndDelete(1, nil) })

	var omAny OrderedMap[int, any]
	omAny.Store(1, []int{1})
	assertPanics("OrderedMap any dynamic uncomparable", func() { omAny.CompareAndSwap(1, []int{1}, 2) })
}

func TestConcurrentSkipListMapLockFreeLenAndFirstRace(t *testing.T) {
	m := NewConcurrentSkipListMap[int, int](cmp.Compare[int])
	var stop atomic.Bool
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				n := m.Len()
				if n < 0 {
					t.Errorf("negative Len: %d", n)
					return
				}
				if k, v, ok := m.First(); ok && v != k*10 {
					t.Errorf("First() inconsistent: (%d, %d)", k, v)
					return
				}
			}
		}()
	}
	for round := 0; round < 64; round++ {
		for k := 0; k < 32; k++ {
			m.Store(k, k*10)
		}
		for k := 0; k < 16; k++ {
			m.PollFirst()
		}
		for k := 16; k < 32; k++ {
			m.Delete(k)
		}
		m.Clear()
	}
	stop.Store(true)
	wg.Wait()
	if got := m.Len(); got != 0 {
		t.Fatalf("final Len() = %d, want 0", got)
	}
	if _, _, ok := m.First(); ok {
		t.Fatal("First() on empty map returned ok=true")
	}
}

func TestTreeMapAndLinkedHashMapRecycleCap(t *testing.T) {
	tm := NewTreeMap[int, int](cmp.Compare[int])
	for i := 0; i < 256; i++ {
		tm.Store(i, i)
	}
	for i := 0; i < 256; i++ {
		tm.Delete(i)
	}
	if tm.freeLen > 64 || tm.freeLen == 0 {
		t.Fatalf("TreeMap freeLen = %d, want in (0, 64]", tm.freeLen)
	}
	tm.Clear()
	if tm.freeLen != 0 || tm.free != nil {
		t.Fatalf("TreeMap after Clear: freeLen=%d free=%v", tm.freeLen, tm.free)
	}

	var lm LinkedHashMap[int, int]
	for i := 0; i < 256; i++ {
		lm.Store(i, i)
	}
	for i := 0; i < 256; i++ {
		lm.Delete(i)
	}
	if lm.freeLen > 64 || lm.freeLen == 0 {
		t.Fatalf("LinkedHashMap freeLen = %d, want in (0, 64]", lm.freeLen)
	}
	lm.Shrink()
	if lm.index != nil || lm.freeLen != 0 || lm.free != nil {
		t.Fatalf("LinkedHashMap empty Shrink did not release storage")
	}
}

func BenchmarkContainerCompareAndSwapMiss(b *testing.B) {
	b.Run("ordered", func(b *testing.B) {
		var m OrderedMap[int, casBenchVal]
		m.Store(1, casBenchVal{1, 2})
		wrong := casBenchVal{9, 9}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			m.CompareAndSwap(1, wrong, casBenchVal{3, 4})
		}
	})
	b.Run("linked", func(b *testing.B) {
		var m LinkedHashMap[int, casBenchVal]
		m.Store(1, casBenchVal{1, 2})
		wrong := casBenchVal{9, 9}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			m.CompareAndSwap(1, wrong, casBenchVal{3, 4})
		}
	})
	b.Run("tree", func(b *testing.B) {
		m := NewTreeMap[int, casBenchVal](cmp.Compare[int])
		m.Store(1, casBenchVal{1, 2})
		wrong := casBenchVal{9, 9}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			m.CompareAndSwap(1, wrong, casBenchVal{3, 4})
		}
	})
	b.Run("skip", func(b *testing.B) {
		m := NewConcurrentSkipListMap[int, casBenchVal](cmp.Compare[int])
		m.Store(1, casBenchVal{1, 2})
		wrong := casBenchVal{9, 9}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			m.CompareAndSwap(1, wrong, casBenchVal{3, 4})
		}
	})
}

func BenchmarkConcurrentSkipListMapFirst(b *testing.B) {
	m := NewConcurrentSkipListMap[int, int](cmp.Compare[int])
	for i := 0; i < 1024; i++ {
		m.Store(i, i)
	}
	b.Run("FirstParallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if k, _, ok := m.First(); !ok || k != 0 {
					b.Fatal("unexpected First")
				}
			}
		})
	})
}
