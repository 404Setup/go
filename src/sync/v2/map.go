// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	isync "internal/sync"
	"iter"
	"sync/atomic"
)

// Map is like a Go map[K]V but is safe for concurrent use by multiple
// goroutines without additional locking or coordination. Loads, stores, and
// deletes run in amortized constant time.
//
// The Map type is specialized. Most code should use a plain Go map instead,
// with separate locking or coordination, to make it easier to maintain other
// invariants along with the map content.
//
// The Map type is optimized for two common use cases: (1) when the entry for a
// given key is only ever written once but read many times, as in caches that
// only grow, or (2) when multiple goroutines read, write, and overwrite entries
// for disjoint sets of keys. In these two cases, use of a Map may significantly
// reduce lock contention compared to a Go map paired with a separate Mutex or
// RWMutex.
//
// The zero Map is empty and ready for use. A Map must not be copied after first
// use.
//
// In the terminology of the Go memory model, Map arranges that a write
// operation synchronizes before any read operation that observes the effect of
// the write. Load, LoadAndDelete, LoadOrStore, Swap, CompareAndSwap, and
// CompareAndDelete are read operations. Delete, LoadAndDelete, Store, and Swap
// are write operations. LoadOrStore is a write operation when it returns loaded
// set to false. CompareAndSwap is a write operation when it returns swapped set
// to true. CompareAndDelete is a write operation when it returns deleted set to
// true.
//
// [the Go memory model]: https://go.dev/ref/mem
type Map[K comparable, V any] struct {
	_ noCopy

	shrinkMu RWMutex
	current  atomic.Pointer[isync.HashTrieMap[K, V]]
}

func (m *Map[K, V]) trieForWrite() *isync.HashTrieMap[K, V] {
	if current := m.current.Load(); current != nil {
		return current
	}
	current := new(isync.HashTrieMap[K, V])
	if m.current.CompareAndSwap(nil, current) {
		return current
	}
	return m.current.Load()
}

// Load returns the value stored in the map for a key. The ok result indicates
// whether the value was found in the map.
func (m *Map[K, V]) Load(key K) (value V, ok bool) {
	if current := m.current.Load(); current != nil {
		return current.Load(key)
	}
	return value, false
}

// Store sets the value for a key.
func (m *Map[K, V]) Store(key K, value V) {
	m.shrinkMu.RLock()
	defer m.shrinkMu.RUnlock()
	m.trieForWrite().Store(key, value)
}

// Clear deletes all the entries, resulting in an empty Map.
func (m *Map[K, V]) Clear() {
	m.shrinkMu.RLock()
	defer m.shrinkMu.RUnlock()
	if current := m.current.Load(); current != nil {
		current.Clear()
	}
}

// Shrink rebuilds the Map's underlying storage to fit its current entries.
// It does not change the entries in the Map. Storage made unnecessary by past
// entries becomes eligible for garbage collection when Shrink returns.
//
// Write operations block while Shrink runs. Loads and iterations may continue
// concurrently and may use either the old or rebuilt storage.
func (m *Map[K, V]) Shrink() {
	m.shrinkMu.Lock()
	defer m.shrinkMu.Unlock()

	current := m.current.Load()
	if current == nil {
		return
	}
	var next *isync.HashTrieMap[K, V]
	current.Range(func(key K, value V) bool {
		if next == nil {
			next = new(isync.HashTrieMap[K, V])
		}
		next.Store(key, value)
		return true
	})
	m.current.Store(next)
}

// LoadOrStore returns the existing value for the key if present. Otherwise, it
// stores and returns the given value. The loaded result is true if the value was
// loaded, false if stored.
func (m *Map[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	m.shrinkMu.RLock()
	defer m.shrinkMu.RUnlock()
	return m.trieForWrite().LoadOrStore(key, value)
}

// LoadAndDelete deletes the value for a key, returning the previous value if
// any. The loaded result reports whether the key was present.
func (m *Map[K, V]) LoadAndDelete(key K) (value V, loaded bool) {
	m.shrinkMu.RLock()
	defer m.shrinkMu.RUnlock()
	if current := m.current.Load(); current != nil {
		return current.LoadAndDelete(key)
	}
	return value, false
}

// Delete deletes the value for a key. If the key is not in the map, Delete does
// nothing.
func (m *Map[K, V]) Delete(key K) {
	m.shrinkMu.RLock()
	defer m.shrinkMu.RUnlock()
	if current := m.current.Load(); current != nil {
		current.Delete(key)
	}
}

// Swap swaps the value for a key and returns the previous value if any. The
// loaded result reports whether the key was present.
func (m *Map[K, V]) Swap(key K, value V) (previous V, loaded bool) {
	m.shrinkMu.RLock()
	defer m.shrinkMu.RUnlock()
	return m.trieForWrite().Swap(key, value)
}

// CompareAndSwap swaps the old and new values for key if the value stored in
// the map is equal to old. It panics if V is not a comparable type.
func (m *Map[K, V]) CompareAndSwap(key K, old, new V) (swapped bool) {
	m.shrinkMu.RLock()
	defer m.shrinkMu.RUnlock()
	return m.trieForWrite().CompareAndSwap(key, old, new)
}

// CompareAndDelete deletes the entry for key if its value is equal to old. It
// panics if V is not a comparable type.
//
// If there is no current value for key, CompareAndDelete returns false.
func (m *Map[K, V]) CompareAndDelete(key K, old V) (deleted bool) {
	m.shrinkMu.RLock()
	defer m.shrinkMu.RUnlock()
	return m.trieForWrite().CompareAndDelete(key, old)
}

// All returns an iterator over each key and value present in the map.
//
// All does not necessarily correspond to any consistent snapshot of the Map's
// contents: no key will be visited more than once, but if the value for any key
// is stored or deleted concurrently, the iterator may reflect any mapping for
// that key from any point during iteration. The iterator does not block other
// methods on the receiver; even yield itself may call any method on m.
func (m *Map[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if current := m.current.Load(); current != nil {
			current.Range(yield)
		}
	}
}

// Range calls f sequentially for each key and value present in the map. If f
// returns false, Range stops the iteration.
//
// Range does not necessarily correspond to any consistent snapshot of the
// Map's contents: no key will be visited more than once, but if the value for
// any key is stored or deleted concurrently (including by f), Range may reflect
// any mapping for that key from any point during the Range call. Range does not
// block other methods on the receiver; even f itself may call any method on m.
//
// Range may be O(N) with the number of elements in the map even if f returns
// false after a constant number of calls.
func (m *Map[K, V]) Range(f func(key K, value V) bool) {
	if current := m.current.Load(); current != nil {
		current.Range(f)
	}
}
