// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container

import (
	"cmp"
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

func BenchmarkOrderedMapLoadOrStoreHit(b *testing.B) {
	var m OrderedMap[int, int]
	for i := range 1024 {
		m.Store(i, i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		key := 0
		for pb.Next() {
			m.LoadOrStore(key, key)
			key = (key + 1) % 1024
		}
	})
}

func BenchmarkOrderedMapShrink(b *testing.B) {
	var m OrderedMap[int, int]
	for i := range 1024 {
		m.Store(i, i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		m.Shrink()
	}
}

func BenchmarkMapImplementations(b *testing.B) {
	for _, test := range []struct {
		name   string
		newMap func() testMap
	}{
		{"ordered", func() testMap { return new(OrderedMap[int, int]) }},
		{"linked", func() testMap { return new(LinkedHashMap[int, int]) }},
		{"tree", func() testMap { return NewTreeMap[int, int](cmp.Compare[int]) }},
		{"skip", func() testMap { return NewConcurrentSkipListMap[int, int](cmp.Compare[int]) }},
	} {
		b.Run(test.name, func(b *testing.B) {
			for _, operation := range []string{"Load", "Store", "Churn", "All"} {
				b.Run(operation, func(b *testing.B) {
					m := test.newMap()
					for i := range 1024 {
						m.Store(i, i)
					}
					b.ReportAllocs()
					b.ResetTimer()
					sum := 0
					switch operation {
					case "Load":
						for i := range b.N {
							value, _ := m.Load(i % 1024)
							sum += value
						}
					case "Store":
						for i := range b.N {
							m.Store(i%1024, i)
						}
					case "Churn":
						for i := range b.N {
							m.Delete(i % 1024)
							m.Store(i%1024, i)
						}
					case "All":
						for range b.N {
							for _, value := range m.All() {
								sum += value
							}
						}
					}
					benchmarkSink.Add(uint64(sum))
				})
			}
		})
	}
}

func BenchmarkConcurrentSkipListMapMixed(b *testing.B) {
	m := NewConcurrentSkipListMap[int, int](cmp.Compare[int])
	for i := range 256 {
		m.Store(i, i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		key, sum := 0, 0
		for pb.Next() {
			if key&7 == 0 {
				m.Store(key, key)
			} else if value, ok := m.Load(key); ok {
				sum += value
			}
			key = (key + 1) % 256
		}
		benchmarkSink.Add(uint64(sum))
	})
}
