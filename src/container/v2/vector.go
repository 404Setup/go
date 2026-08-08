// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import (
	"iter"
	"slices"
)

// Vector is a growable contiguous sequence. The zero Vector is empty and ready
// for use.
type Vector[E any] struct {
	values []E
}

// NewVector returns a Vector containing a copy of values.
func NewVector[E any](values ...E) *Vector[E] {
	return &Vector[E]{values: slices.Clone(values)}
}

// Len returns the number of elements in v.
func (v *Vector[E]) Len() int {
	return len(v.values)
}

// Cap returns the capacity of v's backing storage.
func (v *Vector[E]) Cap() int {
	return cap(v.values)
}

// At returns the element at index i. It panics if i is out of range.
func (v *Vector[E]) At(i int) E {
	return v.values[i]
}

// Set replaces the element at index i. It panics if i is out of range.
func (v *Vector[E]) Set(i int, value E) {
	v.values[i] = value
}

// Append adds values to the end of v.
func (v *Vector[E]) Append(values ...E) {
	v.values = append(v.values, values...)
}

// Insert adds values before index i. It panics if i is outside [0, v.Len()].
func (v *Vector[E]) Insert(i int, values ...E) {
	if i < 0 || i > len(v.values) {
		panic("container: Vector.Insert index out of range")
	}
	v.values = slices.Insert(v.values, i, values...)
}

// Remove deletes and returns the element at index i. It panics if i is out of
// range.
func (v *Vector[E]) Remove(i int) E {
	value := v.values[i]
	v.Delete(i, i+1)
	return value
}

// Delete removes the elements v[i:j]. It panics if the range is invalid.
func (v *Vector[E]) Delete(i, j int) {
	if i < 0 || i > j || j > len(v.values) {
		panic("container: Vector.Delete range out of bounds")
	}
	v.values = slices.Delete(v.values, i, j)
}

// Pop removes and returns the last element. The ok result is false if v is
// empty.
func (v *Vector[E]) Pop() (value E, ok bool) {
	if len(v.values) == 0 {
		return value, false
	}
	i := len(v.values) - 1
	value = v.values[i]
	var zero E
	v.values[i] = zero
	v.values = v.values[:i]
	return value, true
}

// Grow ensures space for another n elements without allocation. It panics if n
// is negative or too large.
func (v *Vector[E]) Grow(n int) {
	v.values = slices.Grow(v.values, n)
}

// Clear removes all elements while retaining backing storage for reuse.
func (v *Vector[E]) Clear() {
	clear(v.values)
	v.values = v.values[:0]
}

// Shrink reallocates v's backing storage to exactly its current length. If v
// is empty, Shrink releases the backing storage entirely.
func (v *Vector[E]) Shrink() {
	if len(v.values) == cap(v.values) {
		return
	}
	if len(v.values) == 0 {
		v.values = nil
		return
	}
	values := make([]E, len(v.values))
	copy(values, v.values)
	v.values = values
}

// Slice returns a copy of the elements in v.
func (v *Vector[E]) Slice() []E {
	return slices.Clone(v.values)
}

// All returns an iterator over index and value pairs in increasing index order.
// Mutating v during iteration invalidates the iterator.
func (v *Vector[E]) All() iter.Seq2[int, E] {
	return func(yield func(int, E) bool) {
		for i, value := range v.values {
			if !yield(i, value) {
				return
			}
		}
	}
}

// Values returns an iterator over the elements in v from first to last.
// Mutating v during iteration invalidates the iterator.
func (v *Vector[E]) Values() iter.Seq[E] {
	return func(yield func(E) bool) {
		for _, value := range v.values {
			if !yield(value) {
				return
			}
		}
	}
}
