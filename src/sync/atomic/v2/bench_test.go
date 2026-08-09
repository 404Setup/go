// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package atomic_test

import (
	atomicv1 "sync/atomic"
	atomicv2 "sync/atomic/v2"
	"testing"
)

var (
	benchmarkIntSink     int
	benchmarkPointerSink *int
	benchmarkLargeSink   largeValue
	benchmarkBoolSink    bool
)

// These benchmarks intentionally use identical workloads for sync/atomic and
// sync/atomic/v2. Run with -bench=Value -benchmem to compare them.

func BenchmarkValueLoadInt(b *testing.B) {
	b.Run("v1", func(b *testing.B) {
		var value atomicv1.Value
		value.Store(42)
		b.ReportAllocs()
		for range b.N {
			benchmarkIntSink = value.Load().(int)
		}
	})
	b.Run("v2", func(b *testing.B) {
		var value atomicv2.Value[int]
		value.Store(42)
		b.ReportAllocs()
		for range b.N {
			benchmarkIntSink = value.Load()
		}
	})
}

func BenchmarkValueLoadPointerParallel(b *testing.B) {
	p := new(int)
	b.Run("v1", func(b *testing.B) {
		var value atomicv1.Value
		value.Store(p)
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				benchmarkPointerSink = value.Load().(*int)
			}
		})
	})
	b.Run("v2", func(b *testing.B) {
		var value atomicv2.Value[*int]
		value.Store(p)
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				benchmarkPointerSink = value.Load()
			}
		})
	})
}

func BenchmarkValueLoadLarge(b *testing.B) {
	large := largeValueFor(1)
	b.Run("v1", func(b *testing.B) {
		var value atomicv1.Value
		value.Store(large)
		b.ReportAllocs()
		for range b.N {
			benchmarkLargeSink = value.Load().(largeValue)
		}
	})
	b.Run("v2", func(b *testing.B) {
		var value atomicv2.Value[largeValue]
		value.Store(large)
		b.ReportAllocs()
		for range b.N {
			benchmarkLargeSink = value.Load()
		}
	})
}

func BenchmarkValueStoreInt(b *testing.B) {
	b.Run("v1", func(b *testing.B) {
		var value atomicv1.Value
		value.Store(0)
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			value.Store(i)
		}
	})
	b.Run("v2", func(b *testing.B) {
		var value atomicv2.Value[int]
		b.ReportAllocs()
		for i := range b.N {
			value.Store(i)
		}
	})
}

func BenchmarkValueStorePointer(b *testing.B) {
	values := [...]*int{new(int), new(int)}
	b.Run("v1", func(b *testing.B) {
		var value atomicv1.Value
		value.Store(values[0])
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			value.Store(values[i&1])
		}
	})
	b.Run("v2", func(b *testing.B) {
		var value atomicv2.Value[*int]
		b.ReportAllocs()
		for i := range b.N {
			value.Store(values[i&1])
		}
	})
}

func BenchmarkValueStoreLarge(b *testing.B) {
	large := largeValueFor(1)
	b.Run("v1", func(b *testing.B) {
		var value atomicv1.Value
		value.Store(large)
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			large.sequence = uint64(i)
			value.Store(large)
		}
	})
	b.Run("v2", func(b *testing.B) {
		var value atomicv2.Value[largeValue]
		b.ReportAllocs()
		for i := range b.N {
			large.sequence = uint64(i)
			value.Store(large)
		}
	})
}

func BenchmarkValueSwapInt(b *testing.B) {
	b.Run("v1", func(b *testing.B) {
		var value atomicv1.Value
		value.Store(0)
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			benchmarkIntSink = value.Swap(i).(int)
		}
	})
	b.Run("v2", func(b *testing.B) {
		var value atomicv2.Value[int]
		b.ReportAllocs()
		for i := range b.N {
			benchmarkIntSink = value.Swap(i)
		}
	})
}

func BenchmarkValueSwapLarge(b *testing.B) {
	large := largeValueFor(1)
	b.Run("v1", func(b *testing.B) {
		var value atomicv1.Value
		value.Store(large)
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			large.sequence = uint64(i)
			benchmarkLargeSink = value.Swap(large).(largeValue)
		}
	})
	b.Run("v2", func(b *testing.B) {
		var value atomicv2.Value[largeValue]
		value.Store(large)
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			large.sequence = uint64(i)
			benchmarkLargeSink = value.Swap(large)
		}
	})
}

func BenchmarkValueCompareAndSwapLarge(b *testing.B) {
	first := largeValueFor(1)
	second := largeValueFor(2)
	b.Run("v1", func(b *testing.B) {
		var value atomicv1.Value
		value.Store(first)
		old, new := first, second
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if !value.CompareAndSwap(old, new) {
				b.Fatal("CompareAndSwap failed")
			}
			old, new = new, old
		}
	})
	b.Run("v2", func(b *testing.B) {
		var value atomicv2.Value[largeValue]
		value.Store(first)
		old, new := first, second
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if !value.CompareAndSwap(old, new) {
				b.Fatal("CompareAndSwap failed")
			}
			old, new = new, old
		}
	})
}

func BenchmarkValueCompareAndSwapMismatchLarge(b *testing.B) {
	stored := largeValueFor(1)
	old := largeValueFor(2)
	new := largeValueFor(3)
	b.Run("v1", func(b *testing.B) {
		var value atomicv1.Value
		value.Store(stored)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			benchmarkBoolSink = value.CompareAndSwap(old, new)
		}
	})
	b.Run("v2", func(b *testing.B) {
		var value atomicv2.Value[largeValue]
		value.Store(stored)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			benchmarkBoolSink = value.CompareAndSwap(old, new)
		}
	})
}
