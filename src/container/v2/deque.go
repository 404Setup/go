// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import (
	"iter"
	"slices"
)

// Deque is a double-ended queue backed by a growable ring buffer. The zero
// Deque is empty and ready for use.
type Deque[E any] struct {
	buf  []E
	head int
	len  int
}

// NewDeque returns a Deque containing values from front to back.
func NewDeque[E any](values ...E) *Deque[E] {
	d := new(Deque[E])
	if len(values) != 0 {
		d.buf = slices.Clone(values)
		d.len = len(values)
	}
	return d
}

// Len returns the number of elements in d.
func (d *Deque[E]) Len() int {
	return d.len
}

// Cap returns the capacity of d's ring buffer.
func (d *Deque[E]) Cap() int {
	return len(d.buf)
}

// At returns the element at index i from the front. It panics if i is out of
// range.
func (d *Deque[E]) At(i int) E {
	if i < 0 || i >= d.len {
		panic("container: Deque.At index out of range")
	}
	return d.buf[d.physical(i)]
}

// Set replaces the element at index i from the front. It panics if i is out of
// range.
func (d *Deque[E]) Set(i int, value E) {
	if i < 0 || i >= d.len {
		panic("container: Deque.Set index out of range")
	}
	d.buf[d.physical(i)] = value
}

// Front returns the first element. The ok result is false if d is empty.
func (d *Deque[E]) Front() (value E, ok bool) {
	if d.len == 0 {
		return value, false
	}
	return d.buf[d.head], true
}

// Back returns the last element. The ok result is false if d is empty.
func (d *Deque[E]) Back() (value E, ok bool) {
	if d.len == 0 {
		return value, false
	}
	return d.buf[d.physical(d.len-1)], true
}

// PushFront adds value to the front of d.
func (d *Deque[E]) PushFront(value E) {
	d.ensure(1)
	if d.head == 0 {
		d.head = len(d.buf) - 1
	} else {
		d.head--
	}
	d.buf[d.head] = value
	d.len++
}

// PushBack adds value to the back of d.
func (d *Deque[E]) PushBack(value E) {
	d.ensure(1)
	d.buf[d.physical(d.len)] = value
	d.len++
}

// PopFront removes and returns the first element. The ok result is false if d
// is empty.
func (d *Deque[E]) PopFront() (value E, ok bool) {
	if d.len == 0 {
		return value, false
	}
	value = d.buf[d.head]
	var zero E
	d.buf[d.head] = zero
	d.head++
	if d.head == len(d.buf) {
		d.head = 0
	}
	d.len--
	if d.len == 0 {
		d.head = 0
	}
	return value, true
}

// PopBack removes and returns the last element. The ok result is false if d is
// empty.
func (d *Deque[E]) PopBack() (value E, ok bool) {
	if d.len == 0 {
		return value, false
	}
	i := d.physical(d.len - 1)
	value = d.buf[i]
	var zero E
	d.buf[i] = zero
	d.len--
	if d.len == 0 {
		d.head = 0
	}
	return value, true
}

// Insert adds value before index i from the front. It panics if i is outside
// [0, d.Len()].
func (d *Deque[E]) Insert(i int, value E) {
	if i < 0 || i > d.len {
		panic("container: Deque.Insert index out of range")
	}
	if i == 0 {
		d.PushFront(value)
		return
	}
	if i == d.len {
		d.PushBack(value)
		return
	}
	d.ensure(1)
	d.len++
	if i < d.len/2 {
		if d.head == 0 {
			d.head = len(d.buf) - 1
		} else {
			d.head--
		}
		for j := 0; j < i; j++ {
			d.setPhysical(j, d.getPhysical(j+1))
		}
	} else {
		for j := d.len - 1; j > i; j-- {
			d.setPhysical(j, d.getPhysical(j-1))
		}
	}
	d.setPhysical(i, value)
}

// Remove deletes and returns the element at index i from the front. It panics
// if i is out of range.
func (d *Deque[E]) Remove(i int) E {
	if i < 0 || i >= d.len {
		panic("container: Deque.Remove index out of range")
	}
	value := d.getPhysical(i)
	var zero E
	if i < d.len/2 {
		for j := i; j > 0; j-- {
			d.setPhysical(j, d.getPhysical(j-1))
		}
		d.buf[d.head] = zero
		d.head++
		if d.head == len(d.buf) {
			d.head = 0
		}
	} else {
		for j := i; j < d.len-1; j++ {
			d.setPhysical(j, d.getPhysical(j+1))
		}
		d.setPhysical(d.len-1, zero)
	}
	d.len--
	if d.len == 0 {
		d.head = 0
	}
	return value
}

// Grow ensures space for another n elements without allocation. It panics if n
// is negative or too large.
func (d *Deque[E]) Grow(n int) {
	if n < 0 {
		panic("container: negative Deque.Grow count")
	}
	d.ensure(n)
}

// Clear removes all elements while retaining the ring buffer for reuse.
func (d *Deque[E]) Clear() {
	for i := 0; i < d.len; i++ {
		var zero E
		d.setPhysical(i, zero)
	}
	d.head = 0
	d.len = 0
}

// Shrink reallocates d's ring buffer to exactly its current length and makes
// the elements contiguous. If d is empty, Shrink releases the buffer entirely.
func (d *Deque[E]) Shrink() {
	if d.len == 0 {
		d.buf = nil
		d.head = 0
		return
	}
	if d.len == len(d.buf) && d.head == 0 {
		return
	}
	buf := make([]E, d.len)
	d.copyTo(buf)
	d.buf = buf
	d.head = 0
}

// Slice returns a copy of d's elements from front to back.
func (d *Deque[E]) Slice() []E {
	values := make([]E, d.len)
	d.copyTo(values)
	return values
}

// All returns an iterator over index and value pairs from front to back.
// Mutating d during iteration invalidates the iterator.
func (d *Deque[E]) All() iter.Seq2[int, E] {
	return func(yield func(int, E) bool) {
		for i := 0; i < d.len; i++ {
			if !yield(i, d.getPhysical(i)) {
				return
			}
		}
	}
}

// Values returns an iterator over d from front to back. Mutating d during
// iteration invalidates the iterator.
func (d *Deque[E]) Values() iter.Seq[E] {
	return func(yield func(E) bool) {
		for i := 0; i < d.len; i++ {
			if !yield(d.getPhysical(i)) {
				return
			}
		}
	}
}

func (d *Deque[E]) physical(i int) int {
	return (d.head + i) % len(d.buf)
}

func (d *Deque[E]) getPhysical(i int) E {
	return d.buf[d.physical(i)]
}

func (d *Deque[E]) setPhysical(i int, value E) {
	d.buf[d.physical(i)] = value
}

func (d *Deque[E]) ensure(n int) {
	if n <= len(d.buf)-d.len {
		return
	}
	if n > int(^uint(0)>>1)-d.len {
		panic("container: Deque too large")
	}
	want := d.len + n
	capacity := len(d.buf) * 2
	if capacity < 8 {
		capacity = 8
	}
	if capacity < want {
		capacity = want
	}
	buf := make([]E, capacity)
	d.copyTo(buf)
	d.buf = buf
	d.head = 0
}

func (d *Deque[E]) copyTo(dst []E) {
	if d.len == 0 {
		return
	}
	first := min(d.len, len(d.buf)-d.head)
	copy(dst, d.buf[d.head:d.head+first])
	copy(dst[first:], d.buf[:d.len-first])
}
