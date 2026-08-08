// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync_test

import (
	"sync"
	"sync/atomic"
	syncv2 "sync/v2"
	"testing"
)

var benchmarkSink atomic.Uint64

func BenchmarkMapLoadFreshEmpty(b *testing.B) {
	b.Run("v1", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var m sync.Map
			if _, ok := m.Load(i); ok {
				b.Fatal("unexpected value")
			}
		}
	})
	b.Run("v2", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var m syncv2.Map[int, int]
			if _, ok := m.Load(i); ok {
				b.Fatal("unexpected value")
			}
		}
	})
}

// These benchmarks intentionally use identical workloads for sync and sync/v2.
// Run with `-bench='(Map|Pool)' -benchmem` to compare throughput and allocation
// behavior on the same machine.
func BenchmarkMapLoadMostlyHits(b *testing.B) {
	const entries = 1024
	b.Run("v1", func(b *testing.B) {
		var m sync.Map
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
					sum += value.(int)
				}
				key = (key + 1) % entries
			}
			benchmarkSink.Add(uint64(sum))
		})
	})
	b.Run("v2", func(b *testing.B) {
		var m syncv2.Map[int, int]
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
	b.Run("ordered-v2", func(b *testing.B) {
		var m syncv2.OrderedMap[int, int]
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
	b.Run("v1", func(b *testing.B) {
		var m sync.Map
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
					sum += value.(int)
				}
				key = (key + 1) % entries
			}
			benchmarkSink.Add(uint64(sum))
		})
	})
	b.Run("v2", func(b *testing.B) {
		var m syncv2.Map[int, int]
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
	b.Run("ordered-v2", func(b *testing.B) {
		var m syncv2.OrderedMap[int, int]
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
	var m syncv2.OrderedMap[int, int]
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

type poolBenchValue struct {
	a, b, c, d uint64
}

func BenchmarkPoolPointer(b *testing.B) {
	b.Run("v1", func(b *testing.B) {
		var p sync.Pool
		p.New = func() any { return new(poolBenchValue) }
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			x := p.Get().(*poolBenchValue)
			for pb.Next() {
				x.a++
				p.Put(x)
				x = p.Get().(*poolBenchValue)
			}
			benchmarkSink.Add(x.a)
		})
	})
	b.Run("v2", func(b *testing.B) {
		var p syncv2.Pool[*poolBenchValue]
		p.New = func() *poolBenchValue { return new(poolBenchValue) }
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			x := p.Get()
			for pb.Next() {
				x.a++
				p.Put(x)
				x = p.Get()
			}
			benchmarkSink.Add(x.a)
		})
	})
}

func BenchmarkPoolValue(b *testing.B) {
	b.Run("v1", func(b *testing.B) {
		var p sync.Pool
		p.New = func() any { return poolBenchValue{} }
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			value := poolBenchValue{}
			for pb.Next() {
				value.a++
				p.Put(value)
				value = p.Get().(poolBenchValue)
			}
			benchmarkSink.Add(value.a)
		})
	})
	b.Run("v2", func(b *testing.B) {
		var p syncv2.Pool[poolBenchValue]
		p.New = func() poolBenchValue { return poolBenchValue{} }
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			value := poolBenchValue{}
			for pb.Next() {
				value.a++
				p.Put(value)
				value = p.Get()
			}
			benchmarkSink.Add(value.a)
		})
	})
}
