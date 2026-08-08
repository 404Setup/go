// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import (
	"cmp"
	"iter"
)

// Set is a non-concurrent collection of unique values that iterates in sorted
// order. Use NewSet or NewOrderedSet to initialize a Set before adding values.
type Set[E any] struct {
	values Map[E, struct{}]
}

// NewSet returns an empty Set ordered by compare.
func NewSet[E any](compare Compare[E]) *Set[E] {
	return &Set[E]{values: *NewMap[E, struct{}](compare)}
}

// NewOrderedSet returns an empty Set ordered by cmp.Compare.
func NewOrderedSet[E cmp.Ordered]() *Set[E] {
	return NewSet[E](cmp.Compare[E])
}

// Len returns the number of values in s.
func (s *Set[E]) Len() int { return s.values.Len() }

// Cap returns the capacity of s's value storage.
func (s *Set[E]) Cap() int { return s.values.Cap() }

// Contains reports whether value is in s.
func (s *Set[E]) Contains(value E) bool {
	_, ok := s.values.Load(value)
	return ok
}

// Add inserts value. It reports whether value was newly added.
func (s *Set[E]) Add(value E) bool {
	_, loaded := s.values.LoadOrStore(value, struct{}{})
	return !loaded
}

// Delete removes value. It reports whether value was present.
func (s *Set[E]) Delete(value E) bool {
	_, loaded := s.values.LoadAndDelete(value)
	return loaded
}

// First returns the lowest value. The ok result is false if s is empty.
func (s *Set[E]) First() (value E, ok bool) {
	value, _, ok = s.values.First()
	return value, ok
}

// Last returns the highest value. The ok result is false if s is empty.
func (s *Set[E]) Last() (value E, ok bool) {
	value, _, ok = s.values.Last()
	return value, ok
}

// LowerBound returns the first stored value that does not sort before value.
func (s *Set[E]) LowerBound(value E) (found E, ok bool) {
	found, _, ok = s.values.LowerBound(value)
	return found, ok
}

// UpperBound returns the first stored value that sorts after value.
func (s *Set[E]) UpperBound(value E) (found E, ok bool) {
	found, _, ok = s.values.UpperBound(value)
	return found, ok
}

// Grow ensures space for another n values without allocation.
func (s *Set[E]) Grow(n int) { s.values.Grow(n) }

// Clear removes all values while retaining storage for reuse.
func (s *Set[E]) Clear() { s.values.Clear() }

// Shrink reallocates s's value storage to exactly its current length.
func (s *Set[E]) Shrink() { s.values.Shrink() }

// Clone returns an independent copy of s.
func (s *Set[E]) Clone() *Set[E] {
	return &Set[E]{values: *s.values.Clone()}
}

// All returns an iterator over values in ascending order. Mutating s during
// iteration invalidates the iterator.
func (s *Set[E]) All() iter.Seq[E] { return s.values.Keys() }

// Backward returns an iterator over values in descending order. Mutating s
// during iteration invalidates the iterator.
func (s *Set[E]) Backward() iter.Seq[E] {
	return func(yield func(E) bool) {
		for value := range s.values.Backward() {
			if !yield(value) {
				return
			}
		}
	}
}
