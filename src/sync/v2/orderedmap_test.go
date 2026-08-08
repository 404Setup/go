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

func TestOrderedMap(t *testing.T) {
	var m syncv2.OrderedMap[string, int]
	if got, ok := m.Load("missing"); got != 0 || ok {
		t.Fatalf("Load(missing) = %v, %v; want 0, false", got, ok)
	}

	m.Store("b", 2)
	m.Store("a", 1)
	m.Store("c", 3)
	m.Store("a", 10)
	if got, loaded := m.LoadOrStore("b", 20); got != 2 || !loaded {
		t.Fatalf("LoadOrStore(existing) = %v, %v; want 2, true", got, loaded)
	}
	if got, loaded := m.LoadOrStore("d", 4); got != 4 || loaded {
		t.Fatalf("LoadOrStore(new) = %v, %v; want 4, false", got, loaded)
	}
	if !m.CompareAndSwap("c", 3, 30) {
		t.Fatal("CompareAndSwap did not swap matching value")
	}
	if m.CompareAndSwap("c", 3, 300) {
		t.Fatal("CompareAndSwap swapped non-matching value")
	}
	if got, loaded := m.Swap("d", 40); got != 4 || !loaded {
		t.Fatalf("Swap(existing) = %v, %v; want 4, true", got, loaded)
	}

	want := []orderedPair{{"b", 2}, {"a", 10}, {"c", 30}, {"d", 40}}
	if got := orderedMapPairs(&m); !reflect.DeepEqual(got, want) {
		t.Fatalf("All() = %v; want %v", got, want)
	}

	if !m.CompareAndDelete("b", 2) {
		t.Fatal("CompareAndDelete did not delete matching value")
	}
	if m.CompareAndDelete("a", 1) {
		t.Fatal("CompareAndDelete deleted non-matching value")
	}
	if got, loaded := m.LoadAndDelete("a"); got != 10 || !loaded {
		t.Fatalf("LoadAndDelete(a) = %v, %v; want 10, true", got, loaded)
	}
	m.Store("a", 100)
	want = []orderedPair{{"c", 30}, {"d", 40}, {"a", 100}}
	if got := orderedMapPairs(&m); !reflect.DeepEqual(got, want) {
		t.Fatalf("All() after delete and reinsert = %v; want %v", got, want)
	}

	visits := 0
	m.Range(func(key string, value int) bool {
		visits++
		m.Store("from-range", 50)
		return false
	})
	if visits != 1 {
		t.Fatalf("early-stop Range visited %v entries; want 1", visits)
	}
	if got, ok := m.Load("from-range"); !ok || got != 50 {
		t.Fatalf("Load(from-range) = %v, %v; want 50, true", got, ok)
	}

	m.Clear()
	if got := orderedMapPairs(&m); len(got) != 0 {
		t.Fatalf("All() after Clear = %v; want empty", got)
	}
}

func TestOrderedMapIteratorSnapshot(t *testing.T) {
	var m syncv2.OrderedMap[int, int]
	for i := range 4 {
		m.Store(i, i)
	}
	var got []orderedIntPair
	for key, value := range m.All() {
		got = append(got, orderedIntPair{key, value})
		m.Delete(key)
		m.Store(key+10, value+10)
	}
	want := []orderedIntPair{{0, 0}, {1, 1}, {2, 2}, {3, 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("All() snapshot = %v; want %v", got, want)
	}
}

func TestOrderedMapShrink(t *testing.T) {
	var m syncv2.OrderedMap[int, int]
	for i := range 1024 {
		m.Store(i, i)
	}
	for i := range 1000 {
		m.Delete(i)
	}
	m.Shrink()

	var got []orderedIntPair
	for key, value := range m.All() {
		got = append(got, orderedIntPair{key, value})
	}
	if len(got) != 24 {
		t.Fatalf("All() after Shrink visited %v entries; want 24", len(got))
	}
	for i, pair := range got {
		want := 1000 + i
		if pair != (orderedIntPair{want, want}) {
			t.Fatalf("entry %v after Shrink = %v; want {%v %v}", i, pair, want, want)
		}
	}

	m.Clear()
	m.Shrink()
	m.Store(1, 2)
	if got, ok := m.Load(1); !ok || got != 2 {
		t.Fatalf("reused OrderedMap Load(1) = %v, %v; want 2, true", got, ok)
	}
}

func TestOrderedMapConcurrent(t *testing.T) {
	var m syncv2.OrderedMap[int, int]
	const writers = 8
	const entriesPerWriter = 128
	var wg sync.WaitGroup
	for writer := range writers {
		writer := writer
		wg.Go(func() {
			base := writer * entriesPerWriter
			for i := range entriesPerWriter {
				key := base + i
				m.Store(key, key)
				if i%16 == 0 {
					m.Shrink()
				}
			}
		})
	}
	wg.Wait()

	seen := make(map[int]bool)
	positions := make([]int, writers)
	for key, value := range m.All() {
		if key != value {
			t.Fatalf("OrderedMap contains %v: %v", key, value)
		}
		if seen[key] {
			t.Fatalf("All() visited key %v more than once", key)
		}
		seen[key] = true
		writer := key / entriesPerWriter
		offset := key % entriesPerWriter
		if offset != positions[writer] {
			t.Fatalf("writer %v entry offset = %v; want %v", writer, offset, positions[writer])
		}
		positions[writer]++
	}
	if len(seen) != writers*entriesPerWriter {
		t.Fatalf("All() visited %v entries; want %v", len(seen), writers*entriesPerWriter)
	}
}

func TestOrderedMapConcurrentSameKey(t *testing.T) {
	var m syncv2.OrderedMap[int, int]
	const goroutines = 8
	const iterations = 2000
	var wg sync.WaitGroup
	for id := range goroutines {
		id := id
		wg.Go(func() {
			for i := range iterations {
				value := id*iterations + i
				switch (id + i) % 5 {
				case 0:
					m.Store(1, value)
				case 1:
					m.LoadOrStore(1, value)
				case 2:
					m.LoadAndDelete(1)
				case 3:
					if old, ok := m.Load(1); ok {
						m.CompareAndSwap(1, old, value)
					}
				case 4:
					if old, ok := m.Load(1); ok {
						m.CompareAndDelete(1, old)
					}
				}
				if i%64 == 0 {
					m.Shrink()
				}
			}
		})
	}
	wg.Wait()

	m.Store(1, 42)
	visits := 0
	for key, value := range m.All() {
		if key == 1 {
			visits++
			if value != 42 {
				t.Fatalf("final value = %v; want 42", value)
			}
		}
	}
	if visits != 1 {
		t.Fatalf("All() visited the final key %v times; want once", visits)
	}
}

func TestOrderedMapComparePanicsForNonComparableValue(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(*syncv2.OrderedMap[string, []int])
	}{
		{"CompareAndSwap", func(m *syncv2.OrderedMap[string, []int]) {
			m.CompareAndSwap("missing", nil, nil)
		}},
		{"CompareAndDelete", func(m *syncv2.OrderedMap[string, []int]) {
			m.CompareAndDelete("missing", nil)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var m syncv2.OrderedMap[string, []int]
			defer func() {
				if recover() == nil {
					t.Fatalf("%s did not panic for a non-comparable value type", test.name)
				}
			}()
			test.call(&m)
		})
	}
}

type orderedPair struct {
	key   string
	value int
}

type orderedIntPair struct {
	key   int
	value int
}

func orderedMapPairs(m *syncv2.OrderedMap[string, int]) []orderedPair {
	var pairs []orderedPair
	for key, value := range m.All() {
		pairs = append(pairs, orderedPair{key, value})
	}
	return pairs
}
