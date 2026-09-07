// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import (
	"sync/atomic"
	"testing"
)

var benchmarkSink atomic.Uint64

func BenchmarkMapLoadMostlyHits(b *testing.B) {
	const entries = 1024
	b.Run("ordered-v2", func(b *testing.B) {
		var m OrderedMap[int, int]
		for i := range entries {
			m.Store(i, i)
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			key := 0
			sum := 0
			for pb.Next() {
				if value, ok := m.Load(key); ok {
					sum += value
				}
				key = (key + 1) % entries
			}
			benchmarkSink.Add(uint64(sum))
		})
	})
}

func BenchmarkMapMixed(b *testing.B) {
	const entries = 256
	b.Run("ordered-v2", func(b *testing.B) {
		var m OrderedMap[int, int]
		for i := range entries {
			m.Store(i, i)
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			key := 0
			sum := 0
			for pb.Next() {
				if key&7 == 0 {
					m.Store(key, key)
				} else if value, ok := m.Load(key); ok {
					sum += value
				}
				key = (key + 1) % entries
			}
			benchmarkSink.Add(uint64(sum))
		})
	})
}

func BenchmarkOrderedMapAll(b *testing.B) {
	const entries = 1024
	var m OrderedMap[int, int]
	for i := range entries {
		m.Store(i, i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		sum := 0
		for _, value := range m.All() {
			sum += value
		}
		benchmarkSink.Add(uint64(sum))
	}
}
