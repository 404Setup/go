// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	isync "internal/sync"
	"iter"
	"reflect"
	"runtime"
	"sync/atomic"
)

// OrderedMap is like a Go map[K]V but is safe for concurrent use and iterates
// in insertion order. Storing a value for an existing key does not change its
// position. A key that is deleted and later stored again is placed at the end.
//
// Loads use a concurrent hash-trie and an atomic pointer to an immutable value.
// Updates to existing keys use compare-and-swap. New keys hold the order lock
// only while linking one entry at the end of the iteration chain.
//
// The zero OrderedMap is empty and ready for use. An OrderedMap must not be
// copied after first use.
type OrderedMap[K comparable, V any] struct {
	_ noCopy

	shrinkMu RWMutex
	state    atomic.Pointer[orderedMapState[K, V]]
}

type orderedMapState[K comparable, V any] struct {
	index isync.HashTrieMap[K, *orderedMapEntry[K, V]]

	appendMu Mutex
	head     atomic.Pointer[orderedMapEntry[K, V]]
	tail     atomic.Pointer[orderedMapEntry[K, V]]
}

type orderedMapEntry[K comparable, V any] struct {
	value atomic.Pointer[orderedMapValue[K, V]]
	next  atomic.Pointer[orderedMapEntry[K, V]]
}

// orderedMapValue is immutable after publication.
type orderedMapValue[K comparable, V any] struct {
	key   K
	value V
}

func newOrderedMapEntry[K comparable, V any](key K, value V) (*orderedMapEntry[K, V], *orderedMapValue[K, V]) {
	return new(orderedMapEntry[K, V]), &orderedMapValue[K, V]{key: key, value: value}
}

func (m *OrderedMap[K, V]) stateForWrite() *orderedMapState[K, V] {
	if state := m.state.Load(); state != nil {
		return state
	}
	state := new(orderedMapState[K, V])
	if m.state.CompareAndSwap(nil, state) {
		return state
	}
	return m.state.Load()
}

// loadOrInsert returns an existing entry or links and publishes candidate. All
// insertions pass through appendMu, so checking the index before modifying the
// chain is sufficient to prevent duplicate live entries.
func (s *orderedMapState[K, V]) loadOrInsert(key K, candidate *orderedMapEntry[K, V], initial *orderedMapValue[K, V]) (actual *orderedMapEntry[K, V], loaded bool) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if actual, ok := s.index.Load(key); ok {
		return actual, true
	}
	if tail := s.tail.Load(); tail != nil {
		tail.next.Store(candidate)
	} else {
		s.head.Store(candidate)
	}
	s.tail.Store(candidate)
	s.index.Store(key, candidate)
	// Publish the value last. Lookups and iterators that observe the entry
	// before this point treat it as an insertion still in progress.
	candidate.value.Store(initial)
	return candidate, false
}

// Load returns the value stored in the map for a key. The ok result indicates
// whether the value was found in the map.
func (m *OrderedMap[K, V]) Load(key K) (value V, ok bool) {
	state := m.state.Load()
	if state == nil {
		return value, false
	}
	for {
		entry, ok := state.index.Load(key)
		if !ok {
			return value, false
		}
		if current := entry.value.Load(); current != nil {
			return current.value, true
		}
		// A nil value is either an insertion about to be published or a
		// deletion about to be removed from the index.
		runtime.Gosched()
	}
}

// Store sets the value for a key.
func (m *OrderedMap[K, V]) Store(key K, value V) {
	_, _ = m.Swap(key, value)
}

// Clear deletes all the entries, resulting in an empty OrderedMap and releasing
// its index and order chain for garbage collection once concurrent readers
// finish with them.
func (m *OrderedMap[K, V]) Clear() {
	m.shrinkMu.Lock()
	m.state.Store(nil)
	m.shrinkMu.Unlock()
}

// Shrink rebuilds the OrderedMap's underlying index and order chain from its
// current entries. It preserves both the entries and their insertion order.
// Storage made unnecessary by deleted entries and old value versions becomes
// eligible for garbage collection when Shrink returns and concurrent readers
// finish with it.
//
// Mutating operations block while Shrink runs. Loads and iterations may
// continue concurrently on the old storage.
func (m *OrderedMap[K, V]) Shrink() {
	m.shrinkMu.Lock()
	defer m.shrinkMu.Unlock()

	current := m.state.Load()
	if current == nil {
		return
	}
	next := new(orderedMapState[K, V])
	for entry := current.head.Load(); entry != nil; entry = entry.next.Load() {
		if value := entry.value.Load(); value != nil {
			candidate, initial := newOrderedMapEntry(value.key, value.value)
			next.loadOrInsert(value.key, candidate, initial)
		}
	}
	if next.head.Load() == nil {
		m.state.Store(nil)
	} else {
		m.state.Store(next)
	}
}

// LoadOrStore returns the existing value for the key if present. Otherwise, it
// stores and returns the given value. The loaded result is true if the value was
// loaded, false if stored.
func (m *OrderedMap[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	m.shrinkMu.RLock()
	defer m.shrinkMu.RUnlock()
	state := m.stateForWrite()
	var candidate *orderedMapEntry[K, V]
	var initial *orderedMapValue[K, V]
	for {
		entry, ok := state.index.Load(key)
		if !ok {
			if candidate == nil {
				candidate, initial = newOrderedMapEntry(key, value)
			}
			entry, loaded = state.loadOrInsert(key, candidate, initial)
			if !loaded {
				return value, false
			}
		}
		if current := entry.value.Load(); current != nil {
			return current.value, true
		}
		runtime.Gosched()
	}
}

// LoadAndDelete deletes the value for a key, returning the previous value if
// any. The loaded result reports whether the key was present.
func (m *OrderedMap[K, V]) LoadAndDelete(key K) (value V, loaded bool) {
	m.shrinkMu.RLock()
	defer m.shrinkMu.RUnlock()
	state := m.state.Load()
	if state == nil {
		return value, false
	}
	for {
		entry, ok := state.index.Load(key)
		if !ok {
			return value, false
		}
		current := entry.value.Load()
		if current == nil {
			runtime.Gosched()
			continue
		}
		if !entry.value.CompareAndSwap(current, nil) {
			continue
		}
		if !state.index.CompareAndDelete(key, entry) {
			panic("sync/v2: OrderedMap index changed during delete")
		}
		return current.value, true
	}
}

// Delete deletes the value for a key. If the key is not in the map, Delete does
// nothing.
func (m *OrderedMap[K, V]) Delete(key K) {
	_, _ = m.LoadAndDelete(key)
}

// Swap swaps the value for a key and returns the previous value if any. The
// loaded result reports whether the key was present.
func (m *OrderedMap[K, V]) Swap(key K, value V) (previous V, loaded bool) {
	m.shrinkMu.RLock()
	defer m.shrinkMu.RUnlock()
	state := m.stateForWrite()
	var candidate *orderedMapEntry[K, V]
	var initial *orderedMapValue[K, V]
	var replacement *orderedMapValue[K, V]
	for {
		entry, ok := state.index.Load(key)
		if !ok {
			if candidate == nil {
				candidate, initial = newOrderedMapEntry(key, value)
			}
			entry, loaded = state.loadOrInsert(key, candidate, initial)
			if !loaded {
				return previous, false
			}
		}
		current := entry.value.Load()
		if current == nil {
			runtime.Gosched()
			continue
		}
		if replacement == nil {
			replacement = &orderedMapValue[K, V]{key: key, value: value}
		}
		if entry.value.CompareAndSwap(current, replacement) {
			return current.value, true
		}
	}
}

// CompareAndSwap swaps the old and new values for key if the value stored in
// the map is equal to old. It panics if V is not a comparable type.
func (m *OrderedMap[K, V]) CompareAndSwap(key K, old, new V) (swapped bool) {
	orderedMapCheckComparable[V]("CompareAndSwap")
	m.shrinkMu.RLock()
	defer m.shrinkMu.RUnlock()
	state := m.state.Load()
	if state == nil {
		return false
	}
	var replacement *orderedMapValue[K, V]
	for {
		entry, ok := state.index.Load(key)
		if !ok {
			return false
		}
		current := entry.value.Load()
		if current == nil {
			runtime.Gosched()
			continue
		}
		if any(current.value) != any(old) {
			return false
		}
		if replacement == nil {
			replacement = &orderedMapValue[K, V]{key: key, value: new}
		}
		if entry.value.CompareAndSwap(current, replacement) {
			return true
		}
	}
}

// CompareAndDelete deletes the entry for key if its value is equal to old. It
// panics if V is not a comparable type.
//
// If there is no current value for key, CompareAndDelete returns false.
func (m *OrderedMap[K, V]) CompareAndDelete(key K, old V) (deleted bool) {
	orderedMapCheckComparable[V]("CompareAndDelete")
	m.shrinkMu.RLock()
	defer m.shrinkMu.RUnlock()
	state := m.state.Load()
	if state == nil {
		return false
	}
	for {
		entry, ok := state.index.Load(key)
		if !ok {
			return false
		}
		current := entry.value.Load()
		if current == nil {
			runtime.Gosched()
			continue
		}
		if any(current.value) != any(old) {
			return false
		}
		if !entry.value.CompareAndSwap(current, nil) {
			continue
		}
		if !state.index.CompareAndDelete(key, entry) {
			panic("sync/v2: OrderedMap index changed during compare-and-delete")
		}
		return true
	}
}

// All returns an iterator over each key and value present in the map, in
// insertion order. The iterator does not include keys first inserted after
// iteration begins. Concurrent updates and deletes of keys not yet visited may
// be reflected by the iterator. Yield may call any method on m.
func (m *OrderedMap[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		state := m.state.Load()
		if state == nil {
			return
		}
		stop := state.tail.Load()
		if stop == nil {
			return
		}
		for entry := state.head.Load(); entry != nil; entry = entry.next.Load() {
			if current := entry.value.Load(); current != nil {
				if !yield(current.key, current.value) {
					return
				}
			}
			if entry == stop {
				return
			}
		}
	}
}

// Range calls f sequentially for each key and value present in the map, in
// insertion order. If f returns false, Range stops the iteration. It has the
// same concurrent-iteration guarantees as All, and f may call any method on m.
func (m *OrderedMap[K, V]) Range(f func(key K, value V) bool) {
	m.All()(f)
}

func orderedMapCheckComparable[V any](operation string) {
	if !reflect.TypeFor[V]().Comparable() {
		panic("called " + operation + " when value is not of comparable type")
	}
}
