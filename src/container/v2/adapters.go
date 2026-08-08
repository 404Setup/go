// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import "iter"

// Queue is a first-in, first-out container. The zero Queue is empty and ready
// for use.
type Queue[E any] struct {
	deque Deque[E]
}

// NewQueue returns a Queue containing values in dequeue order.
func NewQueue[E any](values ...E) *Queue[E] {
	q := new(Queue[E])
	for _, value := range values {
		q.Enqueue(value)
	}
	return q
}

// Len returns the number of elements in q.
func (q *Queue[E]) Len() int { return q.deque.Len() }

// Cap returns the capacity of q's backing storage.
func (q *Queue[E]) Cap() int { return q.deque.Cap() }

// Enqueue adds value to the back of q.
func (q *Queue[E]) Enqueue(value E) { q.deque.PushBack(value) }

// Dequeue removes and returns the front element. The ok result is false if q is
// empty.
func (q *Queue[E]) Dequeue() (value E, ok bool) { return q.deque.PopFront() }

// Peek returns the front element without removing it. The ok result is false if
// q is empty.
func (q *Queue[E]) Peek() (value E, ok bool) { return q.deque.Front() }

// Grow ensures space for another n elements without allocation.
func (q *Queue[E]) Grow(n int) { q.deque.Grow(n) }

// Clear removes all elements while retaining backing storage for reuse.
func (q *Queue[E]) Clear() { q.deque.Clear() }

// Shrink reallocates q's backing storage to exactly its current length.
func (q *Queue[E]) Shrink() { q.deque.Shrink() }

// Slice returns a copy of q's elements in dequeue order.
func (q *Queue[E]) Slice() []E { return q.deque.Slice() }

// Values returns an iterator over q in dequeue order. Mutating q during
// iteration invalidates the iterator.
func (q *Queue[E]) Values() iter.Seq[E] { return q.deque.Values() }

// Stack is a last-in, first-out container. The zero Stack is empty and ready
// for use.
type Stack[E any] struct {
	vector Vector[E]
}

// NewStack returns a Stack containing values from bottom to top.
func NewStack[E any](values ...E) *Stack[E] {
	return &Stack[E]{vector: *NewVector(values...)}
}

// Len returns the number of elements in s.
func (s *Stack[E]) Len() int { return s.vector.Len() }

// Cap returns the capacity of s's backing storage.
func (s *Stack[E]) Cap() int { return s.vector.Cap() }

// Push adds value to the top of s.
func (s *Stack[E]) Push(value E) { s.vector.Append(value) }

// Pop removes and returns the top element. The ok result is false if s is
// empty.
func (s *Stack[E]) Pop() (value E, ok bool) { return s.vector.Pop() }

// Peek returns the top element without removing it. The ok result is false if s
// is empty.
func (s *Stack[E]) Peek() (value E, ok bool) {
	if s.vector.Len() == 0 {
		return value, false
	}
	return s.vector.At(s.vector.Len() - 1), true
}

// Grow ensures space for another n elements without allocation.
func (s *Stack[E]) Grow(n int) { s.vector.Grow(n) }

// Clear removes all elements while retaining backing storage for reuse.
func (s *Stack[E]) Clear() { s.vector.Clear() }

// Shrink reallocates s's backing storage to exactly its current length.
func (s *Stack[E]) Shrink() { s.vector.Shrink() }

// Slice returns a copy of s's elements from bottom to top.
func (s *Stack[E]) Slice() []E { return s.vector.Slice() }

// Values returns an iterator over s from top to bottom. Mutating s during
// iteration invalidates the iterator.
func (s *Stack[E]) Values() iter.Seq[E] {
	return func(yield func(E) bool) {
		for i := s.vector.Len() - 1; i >= 0; i-- {
			if !yield(s.vector.At(i)) {
				return
			}
		}
	}
}
