// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import (
	"iter"
	"slices"
)

// PriorityQueue is a min-priority queue ordered by a comparison function. A
// negative comparison result gives a value higher priority. Use
// NewPriorityQueue to construct one.
type PriorityQueue[E any] struct {
	compare func(E, E) int
	values  []E
}

// NewPriorityQueue returns a PriorityQueue containing values. Compare must not
// be nil.
func NewPriorityQueue[E any](compare func(E, E) int, values ...E) *PriorityQueue[E] {
	if compare == nil {
		panic("container: nil PriorityQueue comparison")
	}
	q := &PriorityQueue[E]{compare: compare, values: slices.Clone(values)}
	for i := len(q.values)/2 - 1; i >= 0; i-- {
		q.down(i)
	}
	return q
}

// Len returns the number of elements in q.
func (q *PriorityQueue[E]) Len() int { return len(q.values) }

// Cap returns the capacity of q's backing storage.
func (q *PriorityQueue[E]) Cap() int { return cap(q.values) }

// Push adds value to q.
func (q *PriorityQueue[E]) Push(value E) {
	q.checkCompare()
	q.values = append(q.values, value)
	q.up(len(q.values) - 1)
}

// Peek returns the highest-priority element without removing it. The ok result
// is false if q is empty.
func (q *PriorityQueue[E]) Peek() (value E, ok bool) {
	if len(q.values) == 0 {
		return value, false
	}
	return q.values[0], true
}

// Pop removes and returns the highest-priority element. The ok result is false
// if q is empty.
func (q *PriorityQueue[E]) Pop() (value E, ok bool) {
	if len(q.values) == 0 {
		return value, false
	}
	q.checkCompare()
	last := len(q.values) - 1
	value = q.values[0]
	q.values[0] = q.values[last]
	var zero E
	q.values[last] = zero
	q.values = q.values[:last]
	if last != 0 {
		q.down(0)
	}
	return value, true
}

// Remove removes and returns the element at heap index i. It panics if i is out
// of range. Heap indices are unstable across mutations.
func (q *PriorityQueue[E]) Remove(i int) E {
	q.checkCompare()
	if i < 0 || i >= len(q.values) {
		panic("container: PriorityQueue.Remove index out of range")
	}
	last := len(q.values) - 1
	value := q.values[i]
	if i != last {
		q.values[i] = q.values[last]
	}
	var zero E
	q.values[last] = zero
	q.values = q.values[:last]
	if i != last && !q.down(i) {
		q.up(i)
	}
	return value
}

// Update replaces the element at heap index i and restores heap order. It
// panics if i is out of range. Heap indices are unstable across mutations.
func (q *PriorityQueue[E]) Update(i int, value E) {
	q.checkCompare()
	if i < 0 || i >= len(q.values) {
		panic("container: PriorityQueue.Update index out of range")
	}
	q.values[i] = value
	if !q.down(i) {
		q.up(i)
	}
}

// Grow ensures space for another n elements without allocation.
func (q *PriorityQueue[E]) Grow(n int) {
	q.values = slices.Grow(q.values, n)
}

// Clear removes all elements while retaining backing storage for reuse.
func (q *PriorityQueue[E]) Clear() {
	clear(q.values)
	q.values = q.values[:0]
}

// Shrink reallocates q's backing storage to exactly its current length. If q is
// empty, Shrink releases the backing storage entirely.
func (q *PriorityQueue[E]) Shrink() {
	if len(q.values) == cap(q.values) {
		return
	}
	if len(q.values) == 0 {
		q.values = nil
		return
	}
	values := make([]E, len(q.values))
	copy(values, q.values)
	q.values = values
}

// Slice returns a copy of q's elements in priority order. It does not mutate q.
func (q *PriorityQueue[E]) Slice() []E {
	values := make([]E, 0, len(q.values))
	for value := range q.Values() {
		values = append(values, value)
	}
	return values
}

// Values returns an iterator over a snapshot of q in priority order. It does
// not mutate q.
func (q *PriorityQueue[E]) Values() iter.Seq[E] {
	return func(yield func(E) bool) {
		copy := &PriorityQueue[E]{compare: q.compare, values: slices.Clone(q.values)}
		for copy.Len() != 0 {
			value, _ := copy.Pop()
			if !yield(value) {
				return
			}
		}
	}
}

func (q *PriorityQueue[E]) checkCompare() {
	if q.compare == nil {
		panic("container: PriorityQueue must be initialized with NewPriorityQueue")
	}
}

func (q *PriorityQueue[E]) up(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if q.compare(q.values[parent], q.values[i]) <= 0 {
			return
		}
		q.values[parent], q.values[i] = q.values[i], q.values[parent]
		i = parent
	}
}

func (q *PriorityQueue[E]) down(i int) bool {
	start := i
	n := len(q.values)
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		child := left
		right := left + 1
		if right < n && q.compare(q.values[right], q.values[left]) < 0 {
			child = right
		}
		if q.compare(q.values[i], q.values[child]) <= 0 {
			break
		}
		q.values[i], q.values[child] = q.values[child], q.values[i]
		i = child
	}
	return i > start
}
