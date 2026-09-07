// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import (
	"container/list"
	"iter"
)

// LinkedHashMap is a hash map with a doubly linked iteration order. The zero
// value is ready for use and iterates in insertion order. Updating a key keeps
// its position; deleting and reinserting it places it at the end.
// NewLinkedHashMap can instead select access order, from least to most recently
// accessed, for use in an LRU cache.
//
// Lookups, stores, and deletes take amortized O(1) time; iteration takes O(n).
// LinkedHashMap is not safe for concurrent writes and must not be copied after
// first use. In access order, successful loads also mutate the map and require
// synchronization. Deleted entries are unlinked immediately.
type LinkedHashMap[K comparable, V any] struct {
	_           noCopy
	index       map[K]*list.Element
	order       list.List
	accessOrder bool
	version     uint64
}

type linkedHashMapEntry[K comparable, V any] struct {
	key   K
	value V
}

// NewLinkedHashMap returns an empty map. If accessOrder is true, successful
// Load, LoadOrStore, Store, Swap, and CompareAndSwap calls move the entry to the
// end. Peek, First, Last, and iteration do not change the order.
func NewLinkedHashMap[K comparable, V any](accessOrder bool) *LinkedHashMap[K, V] {
	return &LinkedHashMap[K, V]{accessOrder: accessOrder}
}

// Len returns the number of entries.
func (m *LinkedHashMap[K, V]) Len() int { return len(m.index) }

// Peek returns the value for key without changing its position.
func (m *LinkedHashMap[K, V]) Peek(key K) (value V, ok bool) {
	if e := m.index[key]; e != nil {
		return e.Value.(*linkedHashMapEntry[K, V]).value, true
	}
	return value, false
}

// Load returns the value for key. In access order it moves the entry to the end.
func (m *LinkedHashMap[K, V]) Load(key K) (value V, ok bool) {
	if e := m.index[key]; e != nil {
		m.touch(e)
		return e.Value.(*linkedHashMapEntry[K, V]).value, true
	}
	return value, false
}

func (m *LinkedHashMap[K, V]) touch(e *list.Element) {
	if m.accessOrder && e != m.order.Back() {
		m.order.MoveToBack(e)
		m.version++
	}
}

// Store sets the value for key.
func (m *LinkedHashMap[K, V]) Store(key K, value V) { m.Swap(key, value) }

// Swap sets the value for key and returns its previous value, if any.
func (m *LinkedHashMap[K, V]) Swap(key K, value V) (previous V, loaded bool) {
	if e := m.index[key]; e != nil {
		entry := e.Value.(*linkedHashMapEntry[K, V])
		previous = entry.value
		entry.value = value
		m.touch(e)
		return previous, true
	}
	if m.index == nil {
		m.index = make(map[K]*list.Element)
	}
	m.index[key] = m.order.PushBack(&linkedHashMapEntry[K, V]{key: key, value: value})
	m.version++
	return previous, false
}

// LoadOrStore returns the existing value, or stores and returns value if absent.
// The loaded result reports whether the key was already present.
func (m *LinkedHashMap[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	if actual, loaded = m.Load(key); loaded {
		return actual, true
	}
	m.Store(key, value)
	return value, false
}

// LoadAndDelete removes key and returns its previous value, if any.
func (m *LinkedHashMap[K, V]) LoadAndDelete(key K) (value V, loaded bool) {
	if e := m.index[key]; e != nil {
		_, value, _ = m.remove(e)
		return value, true
	}
	return value, false
}

// Delete removes key if present.
func (m *LinkedHashMap[K, V]) Delete(key K) { m.LoadAndDelete(key) }

// CompareAndSwap replaces the value for key if it equals old. It panics if V
// is not comparable, or if the comparison involves an uncomparable dynamic value.
func (m *LinkedHashMap[K, V]) CompareAndSwap(key K, old, new V) bool {
	mapCheckComparable[V]("CompareAndSwap")
	if e := m.index[key]; e != nil {
		entry := e.Value.(*linkedHashMapEntry[K, V])
		if any(entry.value) == any(old) {
			entry.value = new
			m.touch(e)
			return true
		}
	}
	return false
}

// CompareAndDelete removes key if its value equals old. It has the same
// comparability requirements as CompareAndSwap.
func (m *LinkedHashMap[K, V]) CompareAndDelete(key K, old V) bool {
	mapCheckComparable[V]("CompareAndDelete")
	if value, ok := m.Peek(key); ok && any(value) == any(old) {
		m.Delete(key)
		return true
	}
	return false
}

// Clear removes all entries and releases the hash index and list storage.
func (m *LinkedHashMap[K, V]) Clear() {
	m.index = nil
	m.order.Init()
	m.version++
}

// Shrink rebuilds the hash index to fit the current entries, preserving order.
func (m *LinkedHashMap[K, V]) Shrink() {
	m.index = make(map[K]*list.Element, m.order.Len())
	for e := m.order.Front(); e != nil; e = e.Next() {
		m.index[e.Value.(*linkedHashMapEntry[K, V]).key] = e
	}
}

func linkedHashMapPair[K comparable, V any](e *list.Element) (key K, value V, ok bool) {
	if e == nil {
		return key, value, false
	}
	entry := e.Value.(*linkedHashMapEntry[K, V])
	return entry.key, entry.value, true
}

// First returns the first entry without changing its position.
func (m *LinkedHashMap[K, V]) First() (key K, value V, ok bool) {
	return linkedHashMapPair[K, V](m.order.Front())
}

// Last returns the last entry without changing its position.
func (m *LinkedHashMap[K, V]) Last() (key K, value V, ok bool) {
	return linkedHashMapPair[K, V](m.order.Back())
}

// PollFirst removes and returns the first entry. In access order this is the
// least recently accessed entry. It takes O(n) time for a key containing NaN,
// which cannot be removed from a Go map by key; otherwise it takes O(1) time.
func (m *LinkedHashMap[K, V]) PollFirst() (key K, value V, ok bool) {
	return m.remove(m.order.Front())
}

// PollLast removes and returns the last entry. Its complexity is as for PollFirst.
func (m *LinkedHashMap[K, V]) PollLast() (key K, value V, ok bool) {
	return m.remove(m.order.Back())
}

func (m *LinkedHashMap[K, V]) remove(e *list.Element) (key K, value V, ok bool) {
	key, value, ok = linkedHashMapPair[K, V](e)
	if ok {
		m.order.Remove(e)
		if key != key {
			m.Shrink()
		} else {
			delete(m.index, key)
		}
		m.version++
	}
	return
}

// All returns an iterator in insertion or access order. Adding, removing, or
// reordering entries during iteration panics if iteration continues afterward.
// Values may be replaced without reordering entries. In access order, use Peek
// to read values without reordering them.
func (m *LinkedHashMap[K, V]) All() iter.Seq2[K, V] { return m.iterate(false) }

// Backward returns an iterator in reverse order, with the same rules as All.
func (m *LinkedHashMap[K, V]) Backward() iter.Seq2[K, V] { return m.iterate(true) }

func (m *LinkedHashMap[K, V]) iterate(reverse bool) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		version := m.version
		e := m.order.Front()
		if reverse {
			e = m.order.Back()
		}
		for e != nil {
			entry := e.Value.(*linkedHashMapEntry[K, V])
			if !yield(entry.key, entry.value) {
				return
			}
			if m.version != version {
				panic("sync/v2/container: LinkedHashMap modified during iteration")
			}
			if reverse {
				e = e.Prev()
			} else {
				e = e.Next()
			}
		}
	}
}

// Range calls f for each entry in order, stopping when f returns false.
// It has the same mutation rules as All.
func (m *LinkedHashMap[K, V]) Range(f func(K, V) bool) { m.All()(f) }
