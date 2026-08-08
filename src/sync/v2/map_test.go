// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync_test

import (
	"reflect"
	"sync"
	syncv2 "sync/v2"
	"testing"
)

func TestMap(t *testing.T) {
	var m syncv2.Map[string, int]
	if got, ok := m.Load("missing"); got != 0 || ok {
		t.Fatalf("Load(missing) = %v, %v; want 0, false", got, ok)
	}

	m.Store("one", 1)
	if got, ok := m.Load("one"); got != 1 || !ok {
		t.Fatalf("Load(one) = %v, %v; want 1, true", got, ok)
	}
	if got, loaded := m.LoadOrStore("one", 2); got != 1 || !loaded {
		t.Fatalf("LoadOrStore(existing) = %v, %v; want 1, true", got, loaded)
	}
	if got, loaded := m.LoadOrStore("two", 2); got != 2 || loaded {
		t.Fatalf("LoadOrStore(new) = %v, %v; want 2, false", got, loaded)
	}
	if !m.CompareAndSwap("two", 2, 3) {
		t.Fatal("CompareAndSwap did not swap matching value")
	}
	if m.CompareAndSwap("two", 2, 4) {
		t.Fatal("CompareAndSwap swapped non-matching value")
	}
	if !m.CompareAndDelete("two", 3) {
		t.Fatal("CompareAndDelete did not delete matching value")
	}
	if got, loaded := m.Swap("one", 10); got != 1 || !loaded {
		t.Fatalf("Swap(existing) = %v, %v; want 1, true", got, loaded)
	}
	if got, loaded := m.LoadAndDelete("one"); got != 10 || !loaded {
		t.Fatalf("LoadAndDelete = %v, %v; want 10, true", got, loaded)
	}

	m.Store("a", 1)
	m.Store("b", 2)
	seen := make(map[string]int)
	for key, value := range m.All() {
		seen[key] = value
	}
	if len(seen) != 2 || seen["a"] != 1 || seen["b"] != 2 {
		t.Fatalf("All visited %v", seen)
	}
	seenRange := make(map[string]int)
	m.Range(func(key string, value int) bool {
		seenRange[key] = value
		return true
	})
	if !reflect.DeepEqual(seenRange, seen) {
		t.Fatalf("Range visited %v; All visited %v", seenRange, seen)
	}

	visits := 0
	for key := range m.All() {
		visits++
		if _, ok := m.Load(key); !ok {
			t.Fatalf("Load(%q) failed during All", key)
		}
		break
	}
	if visits != 1 {
		t.Fatalf("early-stop All visited %v entries; want 1", visits)
	}
	m.Clear()
	for key := range m.All() {
		t.Fatalf("All visited %q after Clear", key)
	}
}

func TestMapConcurrent(t *testing.T) {
	var m syncv2.Map[int, int]
	const goroutines = 32
	const keysPerGoroutine = 128

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		g := g
		wg.Go(func() {
			base := g * keysPerGoroutine
			for i := 0; i < keysPerGoroutine; i++ {
				key := base + i
				m.Store(key, key)
				if got, ok := m.Load(key); !ok || got != key {
					t.Errorf("Load(%v) = %v, %v", key, got, ok)
					return
				}
			}
		})
	}
	wg.Wait()

	count := 0
	m.Range(func(key, value int) bool {
		if key != value {
			t.Errorf("Map contains %v: %v", key, value)
		}
		count++
		return true
	})
	if count != goroutines*keysPerGoroutine {
		t.Fatalf("Range visited %v entries; want %v", count, goroutines*keysPerGoroutine)
	}
}

func TestMapComparePanicsForNonComparableValue(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(*syncv2.Map[string, []int])
	}{
		{"CompareAndSwap", func(m *syncv2.Map[string, []int]) {
			m.CompareAndSwap("key", []int{1}, []int{2})
		}},
		{"CompareAndDelete", func(m *syncv2.Map[string, []int]) {
			m.CompareAndDelete("key", []int{1})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var m syncv2.Map[string, []int]
			m.Store("key", []int{1})
			defer func() {
				if recover() == nil {
					t.Fatalf("%s did not panic for a non-comparable value type", test.name)
				}
			}()
			test.call(&m)
		})
	}
}
