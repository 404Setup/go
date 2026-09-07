// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import (
	"iter"
	"math/bits"
	"math/rand/v2"
	"sync/atomic"
	"sync/v2"
)

// ConcurrentSkipListMap is a concurrent map ordered by a comparison function.
// It uses a randomized skip list with expected O(log n) lookups, stores,
// deletes, and boundary queries. Comparator-equal keys identify the same entry;
// replacing a value preserves the originally stored key.
//
// Loads and ascending iteration do not acquire locks. Writes and boundary
// queries serialize on a mutex. Iteration is weakly consistent: it may observe
// concurrent changes, but visits keys in order without duplicates. Callbacks
// may call any method on the map.
//
// A ConcurrentSkipListMap must be created with NewConcurrentSkipListMap and
// must not be copied after first use. Unlike Java's ConcurrentSkipListMap,
// it permits nil keys and values when the comparator and Go types permit them.
type ConcurrentSkipListMap[K, V any] struct {
	_       noCopy
	compare func(K, K) int
	mu      sync.Mutex
	state   atomic.Pointer[skipListState[K, V]]
	length  int // guarded by mu
}

const skipListLevels = 32

type skipListState[K, V any] struct {
	head skipListNode[K, V]
}

type skipListNode[K, V any] struct {
	key   K                                // immutable after publication
	value atomic.Pointer[skipListValue[V]] // nil marks a deleted entry
	next  []atomic.Pointer[skipListNode[K, V]]
}

type skipListValue[V any] struct{ value V }

// NewConcurrentSkipListMap returns an empty map ordered by compare. The
// comparator must define a consistent total ordering, be safe for concurrent
// calls, and not call methods on the map. For ordered Go types, use cmp.Compare[K].
// It returns a negative number, zero, or a positive number for less, equal, or
// greater keys respectively. NewConcurrentSkipListMap panics if compare is nil.
func NewConcurrentSkipListMap[K, V any](compare func(K, K) int) *ConcurrentSkipListMap[K, V] {
	if compare == nil {
		panic("sync/v2/container: nil ConcurrentSkipListMap comparator")
	}
	return &ConcurrentSkipListMap[K, V]{compare: compare}
}

// Len returns the number of entries. It waits for an in-progress write.
func (m *ConcurrentSkipListMap[K, V]) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.length
}

// find returns the first node whose key is at least key. If predecessors is
// non-nil, it also records the predecessor at every level for a writer.
func (m *ConcurrentSkipListMap[K, V]) find(s *skipListState[K, V], key K, predecessors *[skipListLevels]*skipListNode[K, V]) *skipListNode[K, V] {
	n := &s.head
	for level := skipListLevels - 1; level >= 0; level-- {
		for next := n.next[level].Load(); next != nil && m.compare(next.key, key) < 0; next = n.next[level].Load() {
			n = next
		}
		if predecessors != nil {
			predecessors[level] = n
		}
	}
	return n.next[0].Load()
}

// Load returns the value for key and whether it was present.
func (m *ConcurrentSkipListMap[K, V]) Load(key K) (value V, ok bool) {
	if s := m.state.Load(); s != nil {
		if n := m.find(s, key, nil); n != nil && m.compare(n.key, key) == 0 {
			if current := n.value.Load(); current != nil {
				return current.value, true
			}
		}
	}
	return value, false
}

// Store sets the value for key.
func (m *ConcurrentSkipListMap[K, V]) Store(key K, value V) { m.Swap(key, value) }

// Swap sets the value for key and returns its previous value, if any.
func (m *ConcurrentSkipListMap[K, V]) Swap(key K, value V) (previous V, loaded bool) {
	return m.store(key, value, true)
}

// LoadOrStore returns the existing value, or stores and returns value if absent.
// The loaded result reports whether the key was already present.
func (m *ConcurrentSkipListMap[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	if actual, loaded = m.Load(key); loaded {
		return actual, true
	}
	if actual, loaded = m.store(key, value, false); loaded {
		return actual, true
	}
	return value, false
}

func (m *ConcurrentSkipListMap[K, V]) store(key K, value V, overwrite bool) (previous V, loaded bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.compare == nil {
		panic("sync/v2/container: uninitialized ConcurrentSkipListMap")
	}
	s := m.state.Load()
	if s == nil {
		s = &skipListState[K, V]{head: skipListNode[K, V]{next: make([]atomic.Pointer[skipListNode[K, V]], skipListLevels)}}
		m.state.Store(s)
	}
	var predecessors [skipListLevels]*skipListNode[K, V]
	n := m.find(s, key, &predecessors)
	if n != nil && m.compare(n.key, key) == 0 {
		previous = n.value.Load().value
		if overwrite {
			n.value.Store(&skipListValue[V]{value: value})
		}
		return previous, true
	}
	// Each additional level is chosen with probability 1/4. The cap covers
	// a 64-bit key space without allocating a full tower for every node.
	height := min(1+bits.TrailingZeros64(rand.Uint64())/2, skipListLevels)
	n = &skipListNode[K, V]{key: key, next: make([]atomic.Pointer[skipListNode[K, V]], height)}
	n.value.Store(&skipListValue[V]{value: value})
	for level := range height {
		n.next[level].Store(predecessors[level].next[level].Load())
	}
	// Publish the base link first; higher links only accelerate searches.
	for level := range height {
		predecessors[level].next[level].Store(n)
	}
	m.length++
	return previous, false
}

// LoadAndDelete removes key and returns its previous value, if any.
func (m *ConcurrentSkipListMap[K, V]) LoadAndDelete(key K) (value V, loaded bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.remove(key)
}

// remove requires mu. Unlinked nodes retain their next pointers so an iterator
// already visiting one can continue; the GC reclaims them when readers finish.
func (m *ConcurrentSkipListMap[K, V]) remove(key K) (value V, loaded bool) {
	s := m.state.Load()
	if s == nil {
		return value, false
	}
	var predecessors [skipListLevels]*skipListNode[K, V]
	n := m.find(s, key, &predecessors)
	if n == nil || m.compare(n.key, key) != 0 {
		return value, false
	}
	value = n.value.Load().value
	n.value.Store(nil)
	for level := len(n.next) - 1; level >= 0; level-- {
		predecessors[level].next[level].Store(n.next[level].Load())
	}
	m.length--
	return value, true
}

// Delete removes key if present.
func (m *ConcurrentSkipListMap[K, V]) Delete(key K) { m.LoadAndDelete(key) }

// CompareAndSwap atomically replaces the value for key if it equals old. It
// panics if V is not comparable, or if the comparison involves an uncomparable
// dynamic value.
func (m *ConcurrentSkipListMap[K, V]) CompareAndSwap(key K, old, new V) bool {
	mapCheckComparable[V]("CompareAndSwap")
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.state.Load(); s != nil {
		if n := m.find(s, key, nil); n != nil && m.compare(n.key, key) == 0 && any(n.value.Load().value) == any(old) {
			n.value.Store(&skipListValue[V]{value: new})
			return true
		}
	}
	return false
}

// CompareAndDelete atomically removes key if its value equals old. It has the
// same comparability requirements as CompareAndSwap.
func (m *ConcurrentSkipListMap[K, V]) CompareAndDelete(key K, old V) bool {
	mapCheckComparable[V]("CompareAndDelete")
	m.mu.Lock()
	defer m.mu.Unlock()
	if value, ok := m.Load(key); ok && any(value) == any(old) {
		m.remove(key)
		return true
	}
	return false
}

// Clear atomically removes all entries. Readers already using the old list may
// finish on it; its storage is reclaimed when they finish.
func (m *ConcurrentSkipListMap[K, V]) Clear() {
	m.mu.Lock()
	m.state.Store(nil)
	m.length = 0
	m.mu.Unlock()
}

// skipListPair is called with mu held, so n is either nil or live.
func skipListPair[K, V any](n *skipListNode[K, V]) (key K, value V, ok bool) {
	if n == nil {
		return key, value, false
	}
	return n.key, n.value.Load().value, true
}

func (m *ConcurrentSkipListMap[K, V]) edge(last, remove bool) (key K, value V, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state.Load()
	if s == nil {
		return key, value, false
	}
	n := s.head.next[0].Load()
	if last {
		n = &s.head
		for level := skipListLevels - 1; level >= 0; level-- {
			for next := n.next[level].Load(); next != nil; next = n.next[level].Load() {
				n = next
			}
		}
		if n == &s.head {
			n = nil
		}
	}
	key, value, ok = skipListPair(n)
	if ok && remove {
		m.remove(key)
	}
	return
}

// First returns the least entry according to the comparator.
func (m *ConcurrentSkipListMap[K, V]) First() (key K, value V, ok bool) { return m.edge(false, false) }

// Last returns the greatest entry according to the comparator.
func (m *ConcurrentSkipListMap[K, V]) Last() (key K, value V, ok bool) { return m.edge(true, false) }

// PollFirst atomically removes and returns the least entry.
func (m *ConcurrentSkipListMap[K, V]) PollFirst() (key K, value V, ok bool) {
	return m.edge(false, true)
}

// PollLast atomically removes and returns the greatest entry.
func (m *ConcurrentSkipListMap[K, V]) PollLast() (key K, value V, ok bool) { return m.edge(true, true) }

// Floor returns the greatest entry whose key is less than or equal to key.
func (m *ConcurrentSkipListMap[K, V]) Floor(key K) (K, V, bool) { return m.bound(key, false, true) }

// Lower returns the greatest entry whose key is strictly less than key.
func (m *ConcurrentSkipListMap[K, V]) Lower(key K) (K, V, bool) { return m.bound(key, false, false) }

// Ceiling returns the least entry whose key is greater than or equal to key.
func (m *ConcurrentSkipListMap[K, V]) Ceiling(key K) (K, V, bool) { return m.bound(key, true, true) }

// Higher returns the least entry whose key is strictly greater than key.
func (m *ConcurrentSkipListMap[K, V]) Higher(key K) (K, V, bool) { return m.bound(key, true, false) }

func (m *ConcurrentSkipListMap[K, V]) bound(key K, upper, inclusive bool) (k K, value V, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state.Load()
	if s == nil {
		return k, value, false
	}
	n := &s.head
	for level := skipListLevels - 1; level >= 0; level-- {
		for next := n.next[level].Load(); next != nil; next = n.next[level].Load() {
			c := m.compare(next.key, key)
			if c > 0 || (c == 0 && upper == inclusive) {
				break
			}
			n = next
		}
	}
	if upper {
		n = n.next[0].Load()
	} else if n == &s.head {
		n = nil
	}
	return skipListPair(n)
}

// All returns a weakly consistent iterator in ascending key order, taking
// O(n) time for n traversed nodes without locking or copying the whole map.
// Concurrent inserts may be visited and concurrent deletes may be skipped.
// Yield may call any method on m.
func (m *ConcurrentSkipListMap[K, V]) All() iter.Seq2[K, V] { return m.iterate(nil, nil) }

// RangeBetween returns an ascending iterator over keys in [lower, upper),
// according to the comparator. It takes expected O(log n + k) time for k
// traversed nodes and has the same concurrency guarantees as All.
func (m *ConcurrentSkipListMap[K, V]) RangeBetween(lower, upper K) iter.Seq2[K, V] {
	return m.iterate(&lower, &upper)
}

func (m *ConcurrentSkipListMap[K, V]) iterate(lower, upper *K) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		s := m.state.Load()
		if s == nil {
			return
		}
		n := s.head.next[0].Load()
		if lower != nil {
			n = m.find(s, *lower, nil)
		}
		for ; n != nil; n = n.next[0].Load() {
			if upper != nil && m.compare(n.key, *upper) >= 0 {
				return
			}
			if current := n.value.Load(); current != nil && !yield(n.key, current.value) {
				return
			}
		}
	}
}

// Backward returns a weakly consistent iterator in descending key order.
// It uses successive boundary queries, taking O(n log n) expected time and
// briefly acquiring the writer mutex for each entry. Yield runs without locks.
func (m *ConcurrentSkipListMap[K, V]) Backward() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		for key, value, ok := m.Last(); ok; key, value, ok = m.Lower(key) {
			if !yield(key, value) {
				return
			}
		}
	}
}

// Range calls f in ascending key order, stopping when f returns false.
// It has the same concurrency guarantees as All.
func (m *ConcurrentSkipListMap[K, V]) Range(f func(K, V) bool) { m.All()(f) }
