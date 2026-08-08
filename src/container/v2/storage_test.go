// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import "testing"

func TestRemovedReferencesAreCleared(t *testing.T) {
	t.Run("Vector", func(t *testing.T) {
		v := NewVector(new(int), new(int), new(int))
		backing := v.values[:cap(v.values)]
		v.Delete(0, 2)
		for i := v.Len(); i < len(backing); i++ {
			if backing[i] != nil {
				t.Fatalf("backing[%v] retains deleted pointer", i)
			}
		}
	})

	t.Run("Deque", func(t *testing.T) {
		var d Deque[*int]
		for range 16 {
			d.PushBack(new(int))
		}
		for range 8 {
			d.PopFront()
			d.PopBack()
		}
		for i, value := range d.buf {
			if value != nil {
				t.Fatalf("empty Deque buffer[%v] retains pointer", i)
			}
		}
	})

	t.Run("Map", func(t *testing.T) {
		m := NewOrderedMap[int, *int]()
		for i := range 16 {
			m.Store(i, new(int))
		}
		m.Clear()
		for i, entry := range m.entries[:cap(m.entries)] {
			if entry.value != nil {
				t.Fatalf("cleared Map entry %v retains pointer", i)
			}
		}
	})

	t.Run("PriorityQueue", func(t *testing.T) {
		compare := func(a, b *int) int { return *a - *b }
		q := NewPriorityQueue(compare)
		for i := range 16 {
			value := i
			q.Push(&value)
		}
		q.Clear()
		for i, value := range q.values[:cap(q.values)] {
			if value != nil {
				t.Fatalf("cleared PriorityQueue slot %v retains pointer", i)
			}
		}
	})
}
