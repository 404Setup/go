// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container_test

import (
	"container/v2"
	"strconv"
	"testing"
)

var containerBenchmarkSink int

func BenchmarkVectorAppend(b *testing.B) {
	var v container.Vector[int]
	v.Grow(b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		v.Append(i)
	}
	containerBenchmarkSink = v.Len()
}

func BenchmarkDequeCycle(b *testing.B) {
	var d container.Deque[int]
	d.Grow(1024)
	for i := range 1024 {
		d.PushBack(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		value, _ := d.PopFront()
		d.PushBack(value + i)
	}
	containerBenchmarkSink = d.Len()
}

func BenchmarkMapLoad(b *testing.B) {
	for _, size := range []int{32, 1024, 65536} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			m := container.NewOrderedMap[int, int]()
			m.Grow(size)
			for i := range size {
				m.Store(i, i)
			}
			mask := size - 1
			sum := 0
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				value, _ := m.Load(i & mask)
				sum += value
			}
			containerBenchmarkSink = sum
		})
	}
}

func BenchmarkMapBuild(b *testing.B) {
	const size = 1024
	keys := make([]int, size)
	for i := range keys {
		keys[i] = (i * 33) & (size - 1)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		m := container.NewOrderedMap[int, int]()
		m.Grow(size)
		for _, key := range keys {
			m.Store(key, key)
		}
		containerBenchmarkSink = m.Len()
	}
}

func BenchmarkPriorityQueueCycle(b *testing.B) {
	compare := func(a, b int) int { return a - b }
	q := container.NewPriorityQueue(compare)
	q.Grow(1024)
	for i := range 1024 {
		q.Push(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		q.Pop()
		q.Push(i)
	}
	containerBenchmarkSink = q.Len()
}
