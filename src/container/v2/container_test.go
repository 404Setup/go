// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container_test

import (
	"cmp"
	"container/v2"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
)

func TestVector(t *testing.T) {
	v := container.NewVector(1, 2, 4)
	v.Insert(2, 3)
	v.Insert(0, -1, 0)
	if got, want := v.Slice(), []int{-1, 0, 1, 2, 3, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Vector after Insert = %v; want %v", got, want)
	}
	if got := v.Remove(0); got != -1 {
		t.Fatalf("Remove(0) = %v; want -1", got)
	}
	v.Delete(1, 3)
	v.Set(0, 10)
	if got, want := v.Slice(), []int{10, 3, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Vector after Remove/Delete/Set = %v; want %v", got, want)
	}
	if got, ok := v.Pop(); !ok || got != 4 {
		t.Fatalf("Pop() = %v, %v; want 4, true", got, ok)
	}
	if got := slices.Collect(v.Values()); !reflect.DeepEqual(got, []int{10, 3}) {
		t.Fatalf("Values() = %v; want [10 3]", got)
	}

	v.Grow(100)
	if v.Cap() < v.Len()+100 {
		t.Fatalf("Cap after Grow = %v; want at least %v", v.Cap(), v.Len()+100)
	}
	v.Shrink()
	if v.Cap() != v.Len() {
		t.Fatalf("Cap after Shrink = %v; want Len %v", v.Cap(), v.Len())
	}
	v.Clear()
	if v.Len() != 0 || v.Cap() == 0 {
		t.Fatalf("after Clear Len/Cap = %v/%v; want 0/nonzero", v.Len(), v.Cap())
	}
	v.Shrink()
	if v.Cap() != 0 {
		t.Fatalf("Cap after empty Shrink = %v; want 0", v.Cap())
	}
}

func TestDeque(t *testing.T) {
	var d container.Deque[int]
	var want []int
	rng := rand.New(rand.NewPCG(1, 2))
	for step := range 5000 {
		switch rng.IntN(6) {
		case 0:
			value := int(rng.Uint32())
			d.PushFront(value)
			want = append([]int{value}, want...)
		case 1:
			value := int(rng.Uint32())
			d.PushBack(value)
			want = append(want, value)
		case 2:
			got, ok := d.PopFront()
			if len(want) == 0 {
				if ok {
					t.Fatalf("step %v: PopFront() = %v, true on empty Deque", step, got)
				}
			} else {
				if !ok || got != want[0] {
					t.Fatalf("step %v: PopFront() = %v, %v; want %v, true", step, got, ok, want[0])
				}
				want = want[1:]
			}
		case 3:
			got, ok := d.PopBack()
			if len(want) == 0 {
				if ok {
					t.Fatalf("step %v: PopBack() = %v, true on empty Deque", step, got)
				}
			} else {
				last := len(want) - 1
				if !ok || got != want[last] {
					t.Fatalf("step %v: PopBack() = %v, %v; want %v, true", step, got, ok, want[last])
				}
				want = want[:last]
			}
		case 4:
			value := int(rng.Uint32())
			i := rng.IntN(len(want) + 1)
			d.Insert(i, value)
			want = slices.Insert(want, i, value)
		case 5:
			if len(want) != 0 {
				i := rng.IntN(len(want))
				got := d.Remove(i)
				if got != want[i] {
					t.Fatalf("step %v: Remove(%v) = %v; want %v", step, i, got, want[i])
				}
				want = slices.Delete(want, i, i+1)
			}
		}
		if got := d.Slice(); !reflect.DeepEqual(got, want) {
			t.Fatalf("step %v: Deque = %v; want %v", step, got, want)
		}
	}

	d.Grow(100)
	d.Shrink()
	if d.Cap() != d.Len() {
		t.Fatalf("Cap after Shrink = %v; want Len %v", d.Cap(), d.Len())
	}
	if got := slices.Collect(d.Values()); !reflect.DeepEqual(got, want) {
		t.Fatalf("Values() = %v; want %v", got, want)
	}
	d.Clear()
	d.Shrink()
	if d.Len() != 0 || d.Cap() != 0 {
		t.Fatalf("empty Deque Len/Cap = %v/%v; want 0/0", d.Len(), d.Cap())
	}
}

func TestQueueAndStack(t *testing.T) {
	q := container.NewQueue("a", "b")
	q.Enqueue("c")
	for _, want := range []string{"a", "b", "c"} {
		if got, ok := q.Dequeue(); !ok || got != want {
			t.Fatalf("Queue.Dequeue() = %q, %v; want %q, true", got, ok, want)
		}
	}
	if _, ok := q.Dequeue(); ok {
		t.Fatal("Queue.Dequeue succeeded on empty Queue")
	}
	q.Grow(32)
	q.Shrink()
	if q.Cap() != 0 {
		t.Fatalf("empty Queue Cap after Shrink = %v; want 0", q.Cap())
	}

	s := container.NewStack(1, 2, 3)
	if got := slices.Collect(s.Values()); !reflect.DeepEqual(got, []int{3, 2, 1}) {
		t.Fatalf("Stack.Values() = %v; want [3 2 1]", got)
	}
	s.Push(4)
	if got, ok := s.Peek(); !ok || got != 4 {
		t.Fatalf("Stack.Peek() = %v, %v; want 4, true", got, ok)
	}
	if got, ok := s.Pop(); !ok || got != 4 {
		t.Fatalf("Stack.Pop() = %v, %v; want 4, true", got, ok)
	}
	s.Grow(32)
	s.Shrink()
	if s.Cap() != s.Len() {
		t.Fatalf("Stack Cap after Shrink = %v; want Len %v", s.Cap(), s.Len())
	}
}

func TestPriorityQueue(t *testing.T) {
	q := container.NewPriorityQueue(cmp.Compare[int], 5, 1, 4, 2, 3)
	if got := q.Slice(); !reflect.DeepEqual(got, []int{1, 2, 3, 4, 5}) {
		t.Fatalf("Slice() = %v; want [1 2 3 4 5]", got)
	}
	if q.Len() != 5 {
		t.Fatalf("Slice mutated PriorityQueue: Len = %v; want 5", q.Len())
	}
	q.Push(0)
	if got, ok := q.Peek(); !ok || got != 0 {
		t.Fatalf("Peek() = %v, %v; want 0, true", got, ok)
	}
	var got []int
	for q.Len() != 0 {
		value, _ := q.Pop()
		got = append(got, value)
	}
	if want := []int{0, 1, 2, 3, 4, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Pop order = %v; want %v", got, want)
	}
	q.Grow(64)
	q.Shrink()
	if q.Cap() != 0 {
		t.Fatalf("empty PriorityQueue Cap after Shrink = %v; want 0", q.Cap())
	}
}

func TestMap(t *testing.T) {
	m := container.NewOrderedMap[int, string]()
	for _, key := range []int{5, 1, 4, 2, 3} {
		m.Store(key, string(rune('a'+key)))
	}
	m.Store(3, "updated")
	if got, ok := m.Load(3); !ok || got != "updated" {
		t.Fatalf("Load(3) = %q, %v; want updated, true", got, ok)
	}
	if got, loaded := m.LoadOrStore(3, "ignored"); !loaded || got != "updated" {
		t.Fatalf("LoadOrStore(3) = %q, %v; want updated, true", got, loaded)
	}
	if got, loaded := m.Swap(6, "g"); loaded || got != "" {
		t.Fatalf("Swap(6) = %q, %v; want empty, false", got, loaded)
	}

	var keys []int
	for key := range m.All() {
		keys = append(keys, key)
	}
	if want := []int{1, 2, 3, 4, 5, 6}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("All keys = %v; want %v", keys, want)
	}
	keys = keys[:0]
	for key := range m.Backward() {
		keys = append(keys, key)
	}
	if want := []int{6, 5, 4, 3, 2, 1}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("Backward keys = %v; want %v", keys, want)
	}
	if key, _, ok := m.LowerBound(3); !ok || key != 3 {
		t.Fatalf("LowerBound(3) key = %v, %v; want 3, true", key, ok)
	}
	if key, _, ok := m.UpperBound(3); !ok || key != 4 {
		t.Fatalf("UpperBound(3) key = %v, %v; want 4, true", key, ok)
	}
	if key, _, ok := m.First(); !ok || key != 1 {
		t.Fatalf("First key = %v, %v; want 1, true", key, ok)
	}
	if key, _, ok := m.Last(); !ok || key != 6 {
		t.Fatalf("Last key = %v, %v; want 6, true", key, ok)
	}

	m.Grow(100)
	m.Delete(2)
	if got, loaded := m.LoadAndDelete(5); !loaded || got != "f" {
		t.Fatalf("LoadAndDelete(5) = %q, %v; want f, true", got, loaded)
	}
	m.Shrink()
	if m.Cap() != m.Len() {
		t.Fatalf("Map Cap after Shrink = %v; want Len %v", m.Cap(), m.Len())
	}
	clone := m.Clone()
	clone.Store(100, "clone")
	if _, ok := m.Load(100); ok {
		t.Fatal("mutating Clone changed original Map")
	}
}

func TestMapCustomComparison(t *testing.T) {
	type key struct {
		major int
		minor int
	}
	compare := func(a, b key) int {
		if c := cmp.Compare(a.major, b.major); c != 0 {
			return c
		}
		return cmp.Compare(a.minor, b.minor)
	}
	m := container.NewMap[key, string](compare)
	m.Store(key{2, 0}, "two")
	m.Store(key{1, 1}, "one-one")
	m.Store(key{1, 0}, "one-zero")
	var got []key
	for k := range m.All() {
		got = append(got, k)
	}
	want := []key{{1, 0}, {1, 1}, {2, 0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("custom-comparison Map keys = %v; want %v", got, want)
	}
}

func TestSet(t *testing.T) {
	s := container.NewOrderedSet[int]()
	for _, value := range []int{4, 1, 3, 2, 3} {
		s.Add(value)
	}
	if got := slices.Collect(s.All()); !reflect.DeepEqual(got, []int{1, 2, 3, 4}) {
		t.Fatalf("Set.All() = %v; want [1 2 3 4]", got)
	}
	if !s.Contains(3) || s.Contains(5) {
		t.Fatalf("Set.Contains results incorrect")
	}
	if !s.Delete(3) || s.Delete(3) {
		t.Fatalf("Set.Delete results incorrect")
	}
	if got := slices.Collect(s.Backward()); !reflect.DeepEqual(got, []int{4, 2, 1}) {
		t.Fatalf("Set.Backward() = %v; want [4 2 1]", got)
	}
	s.Grow(100)
	s.Shrink()
	if s.Cap() != s.Len() {
		t.Fatalf("Set Cap after Shrink = %v; want Len %v", s.Cap(), s.Len())
	}
}
