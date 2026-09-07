// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import (
	"cmp"
	"iter"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testMap interface {
	Load(int) (int, bool)
	Store(int, int)
	LoadOrStore(int, int) (int, bool)
	Swap(int, int) (int, bool)
	Delete(int)
	LoadAndDelete(int) (int, bool)
	CompareAndSwap(int, int, int) bool
	CompareAndDelete(int, int) bool
	Clear()
	All() iter.Seq2[int, int]
}

type testSortedMap interface {
	testMap
	Len() int
	First() (int, int, bool)
	Last() (int, int, bool)
	PollFirst() (int, int, bool)
	PollLast() (int, int, bool)
	Floor(int) (int, int, bool)
	Lower(int) (int, int, bool)
	Ceiling(int) (int, int, bool)
	Higher(int) (int, int, bool)
	Backward() iter.Seq2[int, int]
	RangeBetween(int, int) iter.Seq2[int, int]
}

// Compare operation results and iteration against a plain Go map, independently
// tracking insertion order. Also validate the actual balancing and list links.
func TestMapsAgainstReference(t *testing.T) {
	for _, test := range []struct {
		name   string
		m      testMap
		sorted bool
	}{
		{"ordered", new(OrderedMap[int, int]), false},
		{"linked", new(LinkedHashMap[int, int]), false},
		{"tree", NewTreeMap[int, int](cmp.Compare[int]), true},
		{"skip", NewConcurrentSkipListMap[int, int](cmp.Compare[int]), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := test.m
			ref := make(map[int]int)
			var order []int
			rng := rand.New(rand.NewPCG(1, 2))
			for step := range 6000 {
				key, value := rng.IntN(256), rng.IntN(10000)
				previous, exists := ref[key]
				old := previous
				if rng.IntN(2) == 0 {
					old = -1
				}
				switch rng.IntN(8) {
				case 0:
					m.Store(key, value)
					ref[key] = value
				case 1:
					want := value
					if exists {
						want = previous
					}
					if got, loaded := m.LoadOrStore(key, value); got != want || loaded != exists {
						t.Fatalf("step %d LoadOrStore = %d, %v; want %d, %v", step, got, loaded, want, exists)
					}
					ref[key] = want
				case 2:
					if got, loaded := m.Swap(key, value); got != previous || loaded != exists {
						t.Fatalf("step %d Swap = %d, %v; want %d, %v", step, got, loaded, previous, exists)
					}
					ref[key] = value
				case 3:
					m.Delete(key)
					delete(ref, key)
				case 4:
					if got, loaded := m.LoadAndDelete(key); got != previous || loaded != exists {
						t.Fatalf("step %d LoadAndDelete = %d, %v; want %d, %v", step, got, loaded, previous, exists)
					}
					delete(ref, key)
				case 5:
					want := exists && previous == old
					if got := m.CompareAndSwap(key, old, value); got != want {
						t.Fatalf("step %d CompareAndSwap = %v; want %v", step, got, want)
					}
					if want {
						ref[key] = value
					}
				case 6:
					want := exists && previous == old
					if got := m.CompareAndDelete(key, old); got != want {
						t.Fatalf("step %d CompareAndDelete = %v; want %v", step, got, want)
					}
					if want {
						delete(ref, key)
					}
				case 7:
					if got, ok := m.Load(key); got != previous || ok != exists {
						t.Fatalf("step %d Load = %d, %v; want %d, %v", step, got, ok, previous, exists)
					}
				}
				_, present := ref[key]
				if exists && !present {
					order = slices.DeleteFunc(order, func(k int) bool { return k == key })
				} else if !exists && present {
					order = append(order, key)
				}
				if step%997 == 996 {
					m.Clear()
					clear(ref)
					order = nil
				}
				want := slices.Clone(order)
				if test.sorted {
					slices.Sort(want)
				}
				var got []int
				for k, v := range m.All() {
					got = append(got, k)
					if expected, ok := ref[k]; !ok || v != expected {
						t.Fatalf("step %d unexpected entry %d: %d", step, k, v)
					}
				}
				if !slices.Equal(got, want) {
					t.Fatalf("step %d keys = %v; want %v", step, got, want)
				}
				switch m := m.(type) {
				case *TreeMap[int, int]:
					checkTreeMap(t, m)
				case *ConcurrentSkipListMap[int, int]:
					checkSkipListMap(t, m)
				case *LinkedHashMap[int, int]:
					if m.Len() != len(ref) || m.order.Len() != len(ref) {
						t.Fatal("linked map index/list lengths differ")
					}
				}
			}
		})
	}
}

func checkTreeMap(t *testing.T, m *TreeMap[int, int]) {
	t.Helper()
	if treeMapRed(m.root) {
		t.Fatal("red root")
	}
	count := 0
	var check func(*treeMapNode[int, int], *int, *int) int
	check = func(n *treeMapNode[int, int], low, high *int) int {
		if n == nil {
			return 1
		}
		count++
		if (low != nil && m.compare(n.key, *low) <= 0) || (high != nil && m.compare(n.key, *high) >= 0) {
			t.Fatal("tree keys out of order")
		}
		if treeMapRed(n.right) || (n.red && treeMapRed(n.left)) {
			t.Fatal("invalid red links")
		}
		left, right := check(n.left, low, &n.key), check(n.right, &n.key, high)
		if left != right {
			t.Fatalf("black heights at key %d = %d, %d", n.key, left, right)
		}
		if !n.red {
			left++
		}
		return left
	}
	check(m.root, nil, nil)
	if count != m.Len() {
		t.Fatalf("tree has %d nodes; Len = %d", count, m.Len())
	}
}

func checkSkipListMap(t *testing.T, m *ConcurrentSkipListMap[int, int]) {
	t.Helper()
	s := m.state.Load()
	if s == nil {
		if m.Len() != 0 {
			t.Fatal("nil skip list has entries")
		}
		return
	}
	base := make(map[*skipListNode[int, int]]bool)
	for level := range skipListLevels {
		var previous *skipListNode[int, int]
		for n := s.head.next[level].Load(); n != nil; n = n.next[level].Load() {
			if previous != nil && m.compare(previous.key, n.key) >= 0 {
				t.Fatal("unordered skip list level")
			}
			if n.value.Load() == nil {
				t.Fatal("deleted node remains linked")
			}
			if level == 0 {
				base[n] = true
			} else if !base[n] {
				t.Fatal("index node absent from base list")
			}
			previous = n
		}
	}
	if len(base) != m.Len() {
		t.Fatalf("skip list has %d nodes; Len = %d", len(base), m.Len())
	}
}

func TestSortedMapNavigation(t *testing.T) {
	for _, direction := range []int{1, -1} {
		compare := func(a, b int) int { return direction * cmp.Compare(a, b) }
		for _, m := range []testSortedMap{NewTreeMap[int, int](compare), NewConcurrentSkipListMap[int, int](compare)} {
			var keys []int
			for i := 0; i < 64; i += 2 {
				m.Store(i, i*10)
				keys = append(keys, i)
			}
			slices.SortFunc(keys, compare)
			for query := -1; query <= 65; query++ {
				for _, bound := range []struct {
					call   func(int) (int, int, bool)
					accept func(int) bool
					last   bool
				}{
					{m.Floor, func(c int) bool { return c <= 0 }, true},
					{m.Lower, func(c int) bool { return c < 0 }, true},
					{m.Ceiling, func(c int) bool { return c >= 0 }, false},
					{m.Higher, func(c int) bool { return c > 0 }, false},
				} {
					var want int
					found := false
					for _, k := range keys {
						if bound.accept(compare(k, query)) && (!found || bound.last) {
							want, found = k, true
						}
					}
					if k, v, ok := bound.call(query); ok != found || (ok && (k != want || v != want*10)) {
						t.Fatalf("%T boundary(%d), direction %d = %d, %d, %v; want %d, %d, %v", m, query, direction, k, v, ok, want, want*10, found)
					}
				}
			}
			for _, low := range []int{-1, 0, 16, 63, 80} {
				for _, high := range []int{-1, 0, 16, 63, 80} {
					var want, got []int
					for _, k := range keys {
						if compare(k, low) >= 0 && compare(k, high) < 0 {
							want = append(want, k)
						}
					}
					for k := range m.RangeBetween(low, high) {
						got = append(got, k)
					}
					if !slices.Equal(got, want) {
						t.Fatalf("%T RangeBetween(%d, %d) = %v; want %v", m, low, high, got, want)
					}
				}
			}
			var backward []int
			for k := range m.Backward() {
				backward = append(backward, k)
			}
			want := slices.Clone(keys)
			slices.Reverse(want)
			if !slices.Equal(backward, want) {
				t.Fatalf("%T Backward = %v; want %v", m, backward, want)
			}
			for len(keys) > 0 {
				first, _, ok := m.First()
				last, _, lastOK := m.Last()
				if !ok || !lastOK || first != keys[0] || last != keys[len(keys)-1] {
					t.Fatal("incorrect endpoints")
				}
				poll, want := m.PollFirst, keys[0]
				if len(keys)%2 == 0 {
					poll, want = m.PollLast, keys[len(keys)-1]
					keys = keys[:len(keys)-1]
				} else {
					keys = keys[1:]
				}
				if k, v, ok := poll(); !ok || k != want || v != want*10 {
					t.Fatal("incorrect polled entry")
				}
			}
			if _, _, ok := m.PollFirst(); ok {
				t.Fatal("empty PollFirst succeeded")
			}
			if _, _, ok := m.PollLast(); ok {
				t.Fatal("empty PollLast succeeded")
			}
			if m.Len() != 0 {
				t.Fatal("drained map is nonempty")
			}
		}
	}
}

func TestLinkedHashMapAccessOrder(t *testing.T) {
	m := NewLinkedHashMap[int, int](true)
	for i := range 4 {
		m.Store(i, i)
	}
	m.Load(0)
	m.Peek(1)
	m.LoadOrStore(1, 100)
	m.Swap(2, 20)
	if m.CompareAndSwap(3, 30, 300) {
		t.Fatal("nonmatching compare swapped")
	}
	if !m.CompareAndSwap(3, 3, 30) {
		t.Fatal("matching compare failed")
	}
	m.Load(1)
	m.Shrink()
	var got []int
	for k := range m.All() {
		got = append(got, k)
	}
	if want := []int{0, 2, 3, 1}; !slices.Equal(got, want) {
		t.Fatalf("access order = %v; want %v", got, want)
	}
	got = nil
	for k := range m.Backward() {
		got = append(got, k)
	}
	if want := []int{1, 3, 2, 0}; !slices.Equal(got, want) {
		t.Fatalf("backward = %v; want %v", got, want)
	}
	if k, v, ok := m.PollFirst(); !ok || k != 0 || v != 0 {
		t.Fatal("incorrect LRU entry")
	}
	if k, v, ok := m.PollLast(); !ok || k != 1 || v != 1 {
		t.Fatal("incorrect MRU entry")
	}
	m.Clear()
	m.Store(1, 1)
	m.Store(2, 2)
	m.Load(1)
	if k, _, _ := m.First(); k != 2 {
		t.Fatal("Clear lost access order configuration")
	}
}

func TestMapSpecialKeys(t *testing.T) {
	type floatMap interface {
		Store(float64, int)
		Load(float64) (int, bool)
		Len() int
		Delete(float64)
		All() iter.Seq2[float64, int]
	}
	for _, m := range []floatMap{NewTreeMap[float64, int](cmp.Compare[float64]), NewConcurrentSkipListMap[float64, int](cmp.Compare[float64])} {
		m.Store(1, 1)
		m.Store(math.NaN(), 2)
		m.Store(math.NaN(), 3)
		m.Store(0, 4)
		m.Store(math.Copysign(0, -1), 5)
		if v, ok := m.Load(math.NaN()); !ok || v != 3 || m.Len() != 3 {
			t.Fatal("NaN key equality failed")
		}
		first := true
		for k := range m.All() {
			if first && !math.IsNaN(k) {
				t.Fatal("NaN not ordered first")
			}
			first = false
		}
		m.Delete(math.NaN())
		if m.Len() != 2 {
			t.Fatal("NaN deletion failed")
		}
	}
	type sliceMap interface {
		Store([]int, int)
		Load([]int) (int, bool)
		First() ([]int, int, bool)
		Len() int
	}
	for _, m := range []sliceMap{NewTreeMap[[]int, int](slices.Compare[[]int]), NewConcurrentSkipListMap[[]int, int](slices.Compare[[]int])} {
		key := []int{1, 2}
		m.Store(key, 1)
		m.Store([]int{1, 2}, 2)
		if v, ok := m.Load([]int{1, 2}); !ok || v != 2 || m.Len() != 1 {
			t.Fatal("comparator key equality failed")
		}
		if stored, _, _ := m.First(); &stored[0] != &key[0] {
			t.Fatal("original key was replaced")
		}
	}
	var linked LinkedHashMap[float64, int]
	linked.Store(math.NaN(), 1)
	linked.Store(1, 2)
	linked.Store(math.NaN(), 3)
	linked.PollFirst()
	linked.PollLast()
	if linked.Len() != 1 || linked.order.Len() != 1 {
		t.Fatal("polled NaN retained in linked index")
	}
	if v, ok := linked.Load(1); !ok || v != 2 {
		t.Fatal("NaN polling lost another entry")
	}
}

func mustPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("expected panic")
		}
	}()
	f()
}

func TestMapIterationMutation(t *testing.T) {
	for _, m := range []testMap{new(LinkedHashMap[int, int]), NewLinkedHashMap[int, int](true), NewTreeMap[int, int](cmp.Compare[int])} {
		m.Store(1, 1)
		m.Store(2, 2)
		mustPanic(t, func() {
			for k := range m.All() {
				m.Delete(k)
			}
		})
		m.Clear()
		m.Store(1, 1)
		for k := range m.All() {
			m.Delete(k)
			break
		} // Stopping after mutation is permitted.
		m.Store(1, 1)
		for k, v := range m.All() {
			m.Store(k, v+1)
		}
	}
	access := NewLinkedHashMap[int, int](true)
	access.Store(1, 1)
	access.Store(2, 2)
	mustPanic(t, func() {
		for k := range access.All() {
			access.Load(k)
		}
	})
	for _, call := range []func(){
		func() { NewTreeMap[int, int](nil) },
		func() { NewConcurrentSkipListMap[int, int](nil) },
		func() { new(TreeMap[int, int]).Store(1, 1) },
		func() { new(ConcurrentSkipListMap[int, int]).Store(1, 1) },
	} {
		mustPanic(t, call)
	}
}

func TestMapComparisonPanicUnlocks(t *testing.T) {
	type anyMap interface {
		Store(int, any)
		Load(int) (any, bool)
		CompareAndSwap(int, any, any) bool
		CompareAndDelete(int, any) bool
	}
	for _, m := range []anyMap{new(LinkedHashMap[int, any]), NewTreeMap[int, any](cmp.Compare[int]), NewConcurrentSkipListMap[int, any](cmp.Compare[int])} {
		m.Store(1, []int{1})
		mustPanic(t, func() { m.CompareAndSwap(1, []int{1}, 2) })
		mustPanic(t, func() { m.CompareAndDelete(1, []int{1}) })
		m.Store(1, 2)
		if v, _ := m.Load(1); v != 2 {
			t.Fatal("map unusable after comparison panic")
		}
		m.Store(2, nil)
		if v, ok := m.Load(2); v != nil || !ok || !m.CompareAndSwap(2, nil, 3) {
			t.Fatal("nil value was confused with an absent entry")
		}
	}
	type sliceValueMap interface {
		CompareAndSwap(int, []int, []int) bool
		CompareAndDelete(int, []int) bool
	}
	for _, m := range []sliceValueMap{new(LinkedHashMap[int, []int]), NewTreeMap[int, []int](cmp.Compare[int]), NewConcurrentSkipListMap[int, []int](cmp.Compare[int])} {
		mustPanic(t, func() { m.CompareAndSwap(0, nil, nil) })
		mustPanic(t, func() { m.CompareAndDelete(0, nil) })
	}
}

func TestLinkedHashMapInvalidKey(t *testing.T) {
	var m LinkedHashMap[any, int]
	for range 2 {
		mustPanic(t, func() { m.Store([]int{1}, 1) })
		if m.Len() != m.order.Len() {
			t.Fatal("invalid key left an orphan list entry")
		}
		m.Store(1, 1)
	}
}

func TestTreeMapMonotone(t *testing.T) {
	for _, direction := range []int{1, -1} {
		m := NewTreeMap[int, int](cmp.Compare[int])
		for i := range 4096 {
			m.Store(direction*i, i)
		}
		checkTreeMap(t, m)
		for i := range 4096 {
			m.Delete(direction * i)
			if i%32 == 0 {
				checkTreeMap(t, m)
			}
		}
		if m.Len() != 0 || m.root != nil {
			t.Fatal("monotone deletion did not empty tree")
		}
	}
}

func TestConcurrentMapsAtomicOperations(t *testing.T) {
	for _, m := range []testMap{new(OrderedMap[int, int]), NewConcurrentSkipListMap[int, int](cmp.Compare[int])} {
		const workers, iterations = 8, 500
		var wg sync.WaitGroup
		var winners atomic.Int32
		for range workers {
			wg.Go(func() {
				if _, loaded := m.LoadOrStore(1, 0); !loaded {
					winners.Add(1)
				}
				for range iterations {
					for {
						value, _ := m.Load(1)
						if m.CompareAndSwap(1, value, value+1) {
							break
						}
					}
				}
			})
		}
		wg.Wait()
		if value, ok := m.Load(1); !ok || value != workers*iterations || winners.Load() != 1 {
			t.Fatalf("%T lost an atomic update: %d, %v; winners %d", m, value, ok, winners.Load())
		}
		winners.Store(0)
		for range workers {
			wg.Go(func() {
				if m.CompareAndDelete(1, workers*iterations) {
					winners.Add(1)
				}
			})
		}
		wg.Wait()
		if winners.Load() != 1 {
			t.Fatal("multiple successful conditional deletions")
		}
	}
}

func TestConcurrentSkipListMapIteration(t *testing.T) {
	m := NewConcurrentSkipListMap[int, int](cmp.Compare[int])
	var wg sync.WaitGroup
	for id := range 8 {
		wg.Go(func() {
			for i := range 1500 {
				key := (i*17 + id*31) % 256
				switch (i + id) % 8 {
				case 0:
					m.Store(key, key)
				case 1:
					m.LoadOrStore(key, key)
				case 2:
					m.Delete(key)
				case 3:
					m.CompareAndSwap(key, key, key)
				case 4:
					m.CompareAndDelete(key, key)
				case 5:
					m.PollFirst()
				case 6:
					m.PollLast()
				case 7:
					if i%127 == 0 {
						m.Clear()
					}
				}
				for _, seq := range []iter.Seq2[int, int]{m.All(), m.RangeBetween(32, 224)} {
					last := -1
					for k, v := range seq {
						if k <= last || k != v {
							t.Errorf("unordered or invalid entry after %d: %d, %d", last, k, v)
							return
						}
						last = k
					}
				}
				last := 256
				for k, v := range m.Backward() {
					if k >= last || k != v {
						t.Errorf("invalid descending entry %d, %d", k, v)
						return
					}
					last = k
				}
			}
		})
	}
	wg.Wait()
	checkSkipListMap(t, m)
	m.Clear()
	for i := range 32 {
		m.Store(i, i)
	}
	visits := 0
	for k := range m.All() {
		m.Delete(k)
		m.Store(k, k)
		visits++
	}
	if visits != 32 {
		t.Fatalf("reentrant iteration visited %d entries", visits)
	}
	visits = 0
	for k := range m.Backward() {
		m.Delete(k)
		visits++
	}
	if visits != 32 || m.Len() != 0 {
		t.Fatal("reentrant descending iteration failed")
	}
}

func TestOrderedMapLoadOrStoreHitDuringShrink(t *testing.T) {
	var m OrderedMap[int, int]
	m.Store(1, 10)
	m.shrinkMu.Lock()
	defer m.shrinkMu.Unlock()
	done := make(chan bool, 1)
	go func() { value, loaded := m.LoadOrStore(1, 20); done <- loaded && value == 10 }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("LoadOrStore did not return existing value")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadOrStore hit blocked on Shrink")
	}
}

func TestOrderedMapShrinkSharesImmutableValues(t *testing.T) {
	var m OrderedMap[int, int]
	m.Store(1, 10)
	before := m.state.Load().head.Load().value.Load()
	m.Shrink()
	if got := m.state.Load().head.Load().value.Load(); got != before {
		t.Fatal("Shrink copied an immutable value")
	}
	m.Store(1, 20)
	m.Delete(1)
	if before.key != 1 || before.value != 10 {
		t.Fatal("update changed an old value record")
	}
}
