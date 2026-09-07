// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import "iter"

// TreeMap is a red-black tree whose entries are ordered by a comparison
// function. Lookups, stores, deletes, and boundary queries take O(log n) time.
// Keys that compare equal identify the same entry; replacing a value preserves
// the originally stored key.
//
// A TreeMap must be created with NewTreeMap. It is not safe for concurrent
// writes and must not be copied after first use.
type TreeMap[K, V any] struct {
	_       noCopy
	compare func(K, K) int
	root    *treeMapNode[K, V]
	length  int
	version uint64
}

type treeMapNode[K, V any] struct {
	key         K
	value       V
	left, right *treeMapNode[K, V]
	red         bool
}

// NewTreeMap returns an empty map ordered by compare, which must define a
// consistent total ordering and must not modify the map. It returns a negative
// number, zero, or a positive number when its first argument is less than, equal
// to, or greater than its second. For ordered Go types, use cmp.Compare[K].
// NewTreeMap panics if compare is nil.
func NewTreeMap[K, V any](compare func(K, K) int) *TreeMap[K, V] {
	if compare == nil {
		panic("sync/v2/container: nil TreeMap comparator")
	}
	return &TreeMap[K, V]{compare: compare}
}

// Len returns the number of entries.
func (m *TreeMap[K, V]) Len() int { return m.length }

func (m *TreeMap[K, V]) find(key K) *treeMapNode[K, V] {
	for n := m.root; n != nil; {
		switch c := m.compare(key, n.key); {
		case c < 0:
			n = n.left
		case c > 0:
			n = n.right
		default:
			return n
		}
	}
	return nil
}

// Load returns the value for key and whether it was present.
func (m *TreeMap[K, V]) Load(key K) (value V, ok bool) {
	if n := m.find(key); n != nil {
		return n.value, true
	}
	return value, false
}

// Store sets the value for key.
func (m *TreeMap[K, V]) Store(key K, value V) { m.Swap(key, value) }

// Swap sets the value for key and returns its previous value, if any.
func (m *TreeMap[K, V]) Swap(key K, value V) (previous V, loaded bool) {
	return m.store(key, value, true)
}

// LoadOrStore returns the existing value, or stores and returns value if absent.
// The loaded result reports whether the key was already present.
func (m *TreeMap[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	if actual, loaded = m.store(key, value, false); loaded {
		return actual, true
	}
	return value, false
}

func (m *TreeMap[K, V]) store(key K, value V, overwrite bool) (previous V, loaded bool) {
	if m.compare == nil {
		panic("sync/v2/container: uninitialized TreeMap")
	}
	m.root, previous, loaded = m.put(m.root, key, value, overwrite)
	m.root.red = false
	if !loaded {
		m.length++
		m.version++
	}
	return
}

func (m *TreeMap[K, V]) put(n *treeMapNode[K, V], key K, value V, overwrite bool) (root *treeMapNode[K, V], previous V, loaded bool) {
	if n == nil {
		return &treeMapNode[K, V]{key: key, value: value, red: true}, previous, false
	}
	switch c := m.compare(key, n.key); {
	case c < 0:
		n.left, previous, loaded = m.put(n.left, key, value, overwrite)
	case c > 0:
		n.right, previous, loaded = m.put(n.right, key, value, overwrite)
	default:
		previous, loaded = n.value, true
		if overwrite {
			n.value = value
		}
	}
	return treeMapBalance(n), previous, loaded
}

// LoadAndDelete removes key and returns its previous value, if any.
func (m *TreeMap[K, V]) LoadAndDelete(key K) (value V, loaded bool) {
	if value, loaded = m.Load(key); !loaded {
		return value, false
	}
	if !treeMapRed(m.root.left) && !treeMapRed(m.root.right) {
		m.root.red = true
	}
	m.root = m.remove(m.root, key)
	if m.root != nil {
		m.root.red = false
	}
	m.length--
	m.version++
	return value, true
}

// Delete removes key if present.
func (m *TreeMap[K, V]) Delete(key K) { m.LoadAndDelete(key) }

// CompareAndSwap replaces the value for key if it equals old. It panics if V
// is not comparable, or if the comparison involves an uncomparable dynamic value.
func (m *TreeMap[K, V]) CompareAndSwap(key K, old, new V) bool {
	mapCheckComparable[V]("CompareAndSwap")
	if n := m.find(key); n != nil && any(n.value) == any(old) {
		n.value = new
		return true
	}
	return false
}

// CompareAndDelete removes key if its value equals old. It has the same
// comparability requirements as CompareAndSwap.
func (m *TreeMap[K, V]) CompareAndDelete(key K, old V) bool {
	mapCheckComparable[V]("CompareAndDelete")
	if n := m.find(key); n != nil && any(n.value) == any(old) {
		m.Delete(key)
		return true
	}
	return false
}

// Clear removes all entries, releasing the tree storage and retaining the comparator.
func (m *TreeMap[K, V]) Clear() {
	m.root = nil
	m.length = 0
	m.version++
}

func treeMapPair[K, V any](n *treeMapNode[K, V]) (key K, value V, ok bool) {
	if n == nil {
		return key, value, false
	}
	return n.key, n.value, true
}

func (m *TreeMap[K, V]) edge(last bool) *treeMapNode[K, V] {
	n := m.root
	for n != nil {
		next := n.left
		if last {
			next = n.right
		}
		if next == nil {
			break
		}
		n = next
	}
	return n
}

// First returns the least entry according to the comparator.
func (m *TreeMap[K, V]) First() (key K, value V, ok bool) {
	return treeMapPair(m.edge(false))
}

// Last returns the greatest entry according to the comparator.
func (m *TreeMap[K, V]) Last() (key K, value V, ok bool) {
	return treeMapPair(m.edge(true))
}

// PollFirst removes and returns the least entry.
func (m *TreeMap[K, V]) PollFirst() (key K, value V, ok bool) {
	if key, value, ok = m.First(); ok {
		m.Delete(key)
	}
	return
}

// PollLast removes and returns the greatest entry.
func (m *TreeMap[K, V]) PollLast() (key K, value V, ok bool) {
	if key, value, ok = m.Last(); ok {
		m.Delete(key)
	}
	return
}

// Floor returns the greatest entry whose key is less than or equal to key.
func (m *TreeMap[K, V]) Floor(key K) (K, V, bool) {
	return treeMapPair(m.bound(key, false, true))
}

// Lower returns the greatest entry whose key is strictly less than key.
func (m *TreeMap[K, V]) Lower(key K) (K, V, bool) {
	return treeMapPair(m.bound(key, false, false))
}

// Ceiling returns the least entry whose key is greater than or equal to key.
func (m *TreeMap[K, V]) Ceiling(key K) (K, V, bool) {
	return treeMapPair(m.bound(key, true, true))
}

// Higher returns the least entry whose key is strictly greater than key.
func (m *TreeMap[K, V]) Higher(key K) (K, V, bool) {
	return treeMapPair(m.bound(key, true, false))
}

func (m *TreeMap[K, V]) bound(key K, upper, inclusive bool) (candidate *treeMapNode[K, V]) {
	for n := m.root; n != nil; {
		c := m.compare(n.key, key)
		if c == 0 && inclusive {
			return n
		}
		if upper {
			if c > 0 {
				candidate, n = n, n.left
			} else {
				n = n.right
			}
		} else if c < 0 {
			candidate, n = n, n.right
		} else {
			n = n.left
		}
	}
	return
}

// All returns an iterator in ascending key order. Adding or removing entries
// during iteration panics if iteration continues afterward. Replacing values
// is permitted. Iteration takes O(n) time and O(log n) stack space.
func (m *TreeMap[K, V]) All() iter.Seq2[K, V] { return m.iterate(false, nil, nil) }

// Backward returns an iterator in descending key order, with the same rules as All.
func (m *TreeMap[K, V]) Backward() iter.Seq2[K, V] { return m.iterate(true, nil, nil) }

// RangeBetween returns an iterator over keys in [lower, upper), in ascending
// comparator order. It is empty if lower is not less than upper. It takes
// O(log n + k) time for k visited entries and has the same mutation rules as All.
func (m *TreeMap[K, V]) RangeBetween(lower, upper K) iter.Seq2[K, V] {
	return m.iterate(false, &lower, &upper)
}

func (m *TreeMap[K, V]) iterate(reverse bool, lower, upper *K) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		version := m.version
		var visit func(*treeMapNode[K, V]) bool
		visit = func(n *treeMapNode[K, V]) bool {
			if n == nil {
				return true
			}
			if lower != nil && m.compare(n.key, *lower) < 0 {
				return visit(n.right)
			}
			if upper != nil && m.compare(n.key, *upper) >= 0 {
				return visit(n.left)
			}
			first, second := n.left, n.right
			if reverse {
				first, second = second, first
			}
			if !visit(first) || !yield(n.key, n.value) {
				return false
			}
			if m.version != version {
				panic("sync/v2/container: TreeMap modified during iteration")
			}
			return visit(second)
		}
		visit(m.root)
	}
}

// Range calls f for each entry in ascending order, stopping when f returns false.
// It has the same mutation rules as All.
func (m *TreeMap[K, V]) Range(f func(K, V) bool) { m.All()(f) }

// The tree is left-leaning: red links lean left, no two red links are adjacent,
// and every path to a nil child has the same number of black links.
func treeMapRed[K, V any](n *treeMapNode[K, V]) bool { return n != nil && n.red }

func treeMapRotateLeft[K, V any](n *treeMapNode[K, V]) *treeMapNode[K, V] {
	r := n.right
	n.right, r.left = r.left, n
	r.red, n.red = n.red, true
	return r
}

func treeMapRotateRight[K, V any](n *treeMapNode[K, V]) *treeMapNode[K, V] {
	l := n.left
	n.left, l.right = l.right, n
	l.red, n.red = n.red, true
	return l
}

func treeMapFlip[K, V any](n *treeMapNode[K, V]) {
	n.red = !n.red
	n.left.red = !n.left.red
	n.right.red = !n.right.red
}

func treeMapBalance[K, V any](n *treeMapNode[K, V]) *treeMapNode[K, V] {
	if treeMapRed(n.right) {
		n = treeMapRotateLeft(n)
	}
	if treeMapRed(n.left) && treeMapRed(n.left.left) {
		n = treeMapRotateRight(n)
	}
	if treeMapRed(n.left) && treeMapRed(n.right) {
		treeMapFlip(n)
	}
	return n
}

func treeMapMoveRedLeft[K, V any](n *treeMapNode[K, V]) *treeMapNode[K, V] {
	treeMapFlip(n)
	if treeMapRed(n.right.left) {
		n.right = treeMapRotateRight(n.right)
		n = treeMapRotateLeft(n)
		treeMapFlip(n)
	}
	return n
}

func treeMapMoveRedRight[K, V any](n *treeMapNode[K, V]) *treeMapNode[K, V] {
	treeMapFlip(n)
	if treeMapRed(n.left.left) {
		n = treeMapRotateRight(n)
		treeMapFlip(n)
	}
	return n
}

func treeMapDeleteMin[K, V any](n *treeMapNode[K, V]) *treeMapNode[K, V] {
	if n.left == nil {
		return nil
	}
	if !treeMapRed(n.left) && !treeMapRed(n.left.left) {
		n = treeMapMoveRedLeft(n)
	}
	n.left = treeMapDeleteMin(n.left)
	return treeMapBalance(n)
}

func (m *TreeMap[K, V]) remove(n *treeMapNode[K, V], key K) *treeMapNode[K, V] {
	if m.compare(key, n.key) < 0 {
		if !treeMapRed(n.left) && !treeMapRed(n.left.left) {
			n = treeMapMoveRedLeft(n)
		}
		n.left = m.remove(n.left, key)
	} else {
		if treeMapRed(n.left) {
			n = treeMapRotateRight(n)
		}
		if m.compare(key, n.key) == 0 && n.right == nil {
			return nil
		}
		if !treeMapRed(n.right) && !treeMapRed(n.right.left) {
			n = treeMapMoveRedRight(n)
		}
		if m.compare(key, n.key) == 0 {
			successor := n.right
			for successor.left != nil {
				successor = successor.left
			}
			n.key, n.value = successor.key, successor.value
			n.right = treeMapDeleteMin(n.right)
		} else {
			n.right = m.remove(n.right, key)
		}
	}
	return treeMapBalance(n)
}
