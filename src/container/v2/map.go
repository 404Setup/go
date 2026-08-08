// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import (
	"cmp"
	"iter"
	"slices"
	"sort"
)

// Compare orders two values. It returns a negative number when a sorts before
// b, zero when they are equivalent, and a positive number when a sorts after b.
type Compare[E any] func(a, b E) int

// Map is a non-concurrent map that iterates in key order. It uses binary search
// over contiguous storage, favoring compact storage, lookups, and iteration.
// Insertions and deletions may move later entries.
//
// Use NewMap or NewOrderedMap to initialize a Map before storing entries.
type Map[K, V any] struct {
	compare Compare[K]
	entries []mapEntry[K, V]
}

type mapEntry[K, V any] struct {
	key   K
	value V
}

// NewMap returns an empty Map ordered by compare. Compare must define a strict
// total order and must not be nil.
func NewMap[K, V any](compare Compare[K]) *Map[K, V] {
	if compare == nil {
		panic("container: nil Map comparison")
	}
	return &Map[K, V]{compare: compare}
}

// NewOrderedMap returns an empty Map ordered by cmp.Compare.
func NewOrderedMap[K cmp.Ordered, V any]() *Map[K, V] {
	return NewMap[K, V](cmp.Compare[K])
}

// Len returns the number of entries in m.
func (m *Map[K, V]) Len() int { return len(m.entries) }

// Cap returns the capacity of m's entry storage.
func (m *Map[K, V]) Cap() int { return cap(m.entries) }

// Load returns the value stored for key. The ok result reports whether key was
// present.
func (m *Map[K, V]) Load(key K) (value V, ok bool) {
	if len(m.entries) == 0 {
		return value, false
	}
	i, ok := m.search(key)
	if !ok {
		return value, false
	}
	return m.entries[i].value, true
}

// Store sets the value for key.
func (m *Map[K, V]) Store(key K, value V) {
	_, _ = m.Swap(key, value)
}

// Swap sets the value for key and returns the previous value, if any. The
// loaded result reports whether key was already present.
func (m *Map[K, V]) Swap(key K, value V) (previous V, loaded bool) {
	i, loaded := m.search(key)
	if loaded {
		previous = m.entries[i].value
		m.entries[i].value = value
		return previous, true
	}
	m.entries = append(m.entries, mapEntry[K, V]{})
	copy(m.entries[i+1:], m.entries[i:])
	m.entries[i] = mapEntry[K, V]{key: key, value: value}
	return previous, false
}

// LoadOrStore returns the existing value for key if present. Otherwise, it
// stores and returns value. The loaded result is true if the value was loaded.
func (m *Map[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	i, loaded := m.search(key)
	if loaded {
		return m.entries[i].value, true
	}
	m.entries = append(m.entries, mapEntry[K, V]{})
	copy(m.entries[i+1:], m.entries[i:])
	m.entries[i] = mapEntry[K, V]{key: key, value: value}
	return value, false
}

// LoadAndDelete removes the entry for key and returns its value, if any.
func (m *Map[K, V]) LoadAndDelete(key K) (value V, loaded bool) {
	if len(m.entries) == 0 {
		return value, false
	}
	i, loaded := m.search(key)
	if !loaded {
		return value, false
	}
	value = m.entries[i].value
	copy(m.entries[i:], m.entries[i+1:])
	last := len(m.entries) - 1
	var zero mapEntry[K, V]
	m.entries[last] = zero
	m.entries = m.entries[:last]
	return value, true
}

// Delete removes the entry for key. It does nothing if key is absent.
func (m *Map[K, V]) Delete(key K) {
	_, _ = m.LoadAndDelete(key)
}

// First returns the lowest key and its value. The ok result is false if m is
// empty.
func (m *Map[K, V]) First() (key K, value V, ok bool) {
	if len(m.entries) == 0 {
		return key, value, false
	}
	entry := m.entries[0]
	return entry.key, entry.value, true
}

// Last returns the highest key and its value. The ok result is false if m is
// empty.
func (m *Map[K, V]) Last() (key K, value V, ok bool) {
	if len(m.entries) == 0 {
		return key, value, false
	}
	entry := m.entries[len(m.entries)-1]
	return entry.key, entry.value, true
}

// LowerBound returns the first entry whose key does not sort before key. The ok
// result is false if no such entry exists.
func (m *Map[K, V]) LowerBound(key K) (foundKey K, value V, ok bool) {
	i, _ := m.search(key)
	if i == len(m.entries) {
		return foundKey, value, false
	}
	entry := m.entries[i]
	return entry.key, entry.value, true
}

// UpperBound returns the first entry whose key sorts after key. The ok result
// is false if no such entry exists.
func (m *Map[K, V]) UpperBound(key K) (foundKey K, value V, ok bool) {
	m.checkCompare()
	i := sort.Search(len(m.entries), func(i int) bool {
		return m.compare(m.entries[i].key, key) > 0
	})
	if i == len(m.entries) {
		return foundKey, value, false
	}
	entry := m.entries[i]
	return entry.key, entry.value, true
}

// Grow ensures space for another n entries without allocation.
func (m *Map[K, V]) Grow(n int) {
	m.entries = slices.Grow(m.entries, n)
}

// Clear removes all entries while retaining entry storage for reuse.
func (m *Map[K, V]) Clear() {
	clear(m.entries)
	m.entries = m.entries[:0]
}

// Shrink reallocates m's entry storage to exactly its current length. If m is
// empty, Shrink releases the storage entirely.
func (m *Map[K, V]) Shrink() {
	if len(m.entries) == cap(m.entries) {
		return
	}
	if len(m.entries) == 0 {
		m.entries = nil
		return
	}
	entries := make([]mapEntry[K, V], len(m.entries))
	copy(entries, m.entries)
	m.entries = entries
}

// Clone returns an independent copy of m.
func (m *Map[K, V]) Clone() *Map[K, V] {
	return &Map[K, V]{compare: m.compare, entries: slices.Clone(m.entries)}
}

// All returns an iterator over key and value pairs in ascending key order.
// Mutating m during iteration invalidates the iterator.
func (m *Map[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		for _, entry := range m.entries {
			if !yield(entry.key, entry.value) {
				return
			}
		}
	}
}

// Backward returns an iterator over key and value pairs in descending key
// order. Mutating m during iteration invalidates the iterator.
func (m *Map[K, V]) Backward() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		for _, entry := range slices.Backward(m.entries) {
			if !yield(entry.key, entry.value) {
				return
			}
		}
	}
}

// Keys returns an iterator over keys in ascending order.
func (m *Map[K, V]) Keys() iter.Seq[K] {
	return func(yield func(K) bool) {
		for _, entry := range m.entries {
			if !yield(entry.key) {
				return
			}
		}
	}
}

// Values returns an iterator over values in ascending key order.
func (m *Map[K, V]) Values() iter.Seq[V] {
	return func(yield func(V) bool) {
		for _, entry := range m.entries {
			if !yield(entry.value) {
				return
			}
		}
	}
}

func (m *Map[K, V]) search(key K) (int, bool) {
	m.checkCompare()
	i := sort.Search(len(m.entries), func(i int) bool {
		return m.compare(m.entries[i].key, key) >= 0
	})
	return i, i < len(m.entries) && m.compare(m.entries[i].key, key) == 0
}

func (m *Map[K, V]) checkCompare() {
	if m.compare == nil {
		panic("container: Map must be initialized with NewMap or NewOrderedMap")
	}
}
