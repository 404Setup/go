// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package atomic_test

import (
	"math"
	"runtime"
	"sync"
	atomicv2 "sync/atomic/v2"
	"testing"
)

type largeValue struct {
	sequence uint64
	words    [15]uint64
}

func largeValueFor(sequence uint64) largeValue {
	v := largeValue{sequence: sequence}
	for i := range v.words {
		v.words[i] = sequence
	}
	return v
}

func validLargeValue(v largeValue) bool {
	for _, word := range v.words {
		if word != v.sequence {
			return false
		}
	}
	return true
}

func TestValue(t *testing.T) {
	var integer atomicv2.Value[int]
	if got := integer.Load(); got != 0 {
		t.Fatalf("zero Value[int] = %v, want 0", got)
	}
	integer.Store(42)
	if got := integer.Load(); got != 42 {
		t.Fatalf("Value[int].Load() = %v, want 42", got)
	}

	var pointer atomicv2.Value[*int]
	if got := pointer.Load(); got != nil {
		t.Fatalf("zero Value[*int] = %v, want nil", got)
	}
	x := 1
	pointer.Store(&x)
	if got := pointer.Load(); got != &x {
		t.Fatalf("Value[*int].Load() = %p, want %p", got, &x)
	}
	pointer.Store(nil)
	if got := pointer.Load(); got != nil {
		t.Fatalf("Value[*int] after Store(nil) = %v, want nil", got)
	}

	var text atomicv2.Value[string]
	text.Store("typed atomic value")
	if got := text.Load(); got != "typed atomic value" {
		t.Fatalf("Value[string].Load() = %q", got)
	}

	var slice atomicv2.Value[[]int]
	wantSlice := []int{1, 2, 3}
	slice.Store(wantSlice)
	gotSlice := slice.Load()
	if len(gotSlice) != len(wantSlice) || &gotSlice[0] != &wantSlice[0] {
		t.Fatalf("Value[[]int].Load() = %v, want %v", gotSlice, wantSlice)
	}

	var mapping atomicv2.Value[map[string]int]
	wantMap := map[string]int{"answer": 42}
	mapping.Store(wantMap)
	if got := mapping.Load(); got["answer"] != 42 {
		t.Fatalf("Value[map[string]int].Load() = %v, want %v", got, wantMap)
	}

	var dynamic atomicv2.Value[any]
	dynamic.Store(1)
	if got := dynamic.Load(); got != 1 {
		t.Fatalf("Value[any].Load() = %v, want 1", got)
	}
	dynamic.Store("different dynamic type")
	if got := dynamic.Load(); got != "different dynamic type" {
		t.Fatalf("Value[any].Load() = %v, want different dynamic type", got)
	}
	if !dynamic.CompareAndSwap("different dynamic type", 2) {
		t.Fatal("Value[any].CompareAndSwap did not permit a new dynamic type")
	}
}

func TestValueSwap(t *testing.T) {
	var integer atomicv2.Value[int]
	if old := integer.Swap(1); old != 0 {
		t.Fatalf("first Swap returned %v, want 0", old)
	}
	if old := integer.Swap(2); old != 1 {
		t.Fatalf("second Swap returned %v, want 1", old)
	}
	if got := integer.Load(); got != 2 {
		t.Fatalf("Load after Swap = %v, want 2", got)
	}

	var slice atomicv2.Value[[]int]
	first := []int{1}
	second := []int{2}
	if old := slice.Swap(first); old != nil {
		t.Fatalf("first slice Swap returned %v, want nil", old)
	}
	if old := slice.Swap(second); len(old) != 1 || old[0] != 1 {
		t.Fatalf("second slice Swap returned %v, want [1]", old)
	}
}

func TestValueCompareAndSwap(t *testing.T) {
	var integer atomicv2.Value[int]
	if !integer.CompareAndSwap(0, 1) {
		t.Fatal("CompareAndSwap did not replace the initial zero value")
	}
	if integer.CompareAndSwap(0, 2) {
		t.Fatal("CompareAndSwap replaced a non-matching value")
	}
	if !integer.CompareAndSwap(1, 2) || integer.Load() != 2 {
		t.Fatal("CompareAndSwap did not replace a matching value")
	}

	first := 1
	second := 2
	var pointer atomicv2.Value[*int]
	if !pointer.CompareAndSwap(nil, &first) {
		t.Fatal("pointer CompareAndSwap did not replace nil")
	}
	if !pointer.CompareAndSwap(&first, &second) || pointer.Load() != &second {
		t.Fatal("pointer CompareAndSwap did not replace a matching pointer")
	}

	oldLarge := largeValueFor(1)
	newLarge := largeValueFor(2)
	var large atomicv2.Value[largeValue]
	large.Store(oldLarge)
	if !large.CompareAndSwap(largeValueFor(1), newLarge) {
		t.Fatal("CompareAndSwap compared boxed values by address")
	}
	if got := large.Load(); got != newLarge {
		t.Fatalf("large Load after CompareAndSwap = %v, want %v", got, newLarge)
	}
}

func TestValueCompareAndSwapFloat(t *testing.T) {
	negativeZero := math.Copysign(0, -1)
	var value atomicv2.Value[float64]
	value.Store(negativeZero)
	if !value.CompareAndSwap(0, 1) {
		t.Fatal("CompareAndSwap did not use Go equality for signed zero")
	}

	nan := math.NaN()
	value.Store(nan)
	if value.CompareAndSwap(nan, 1) {
		t.Fatal("CompareAndSwap matched NaN")
	}
}

func TestValueCompareAndSwapPanics(t *testing.T) {
	tests := []struct {
		name string
		call func()
	}{
		{
			name: "static incomparable type",
			call: func() {
				var value atomicv2.Value[[]int]
				value.CompareAndSwap(nil, []int{1})
			},
		},
		{
			name: "dynamic incomparable type",
			call: func() {
				var value atomicv2.Value[any]
				value.Store([]int{1})
				value.CompareAndSwap([]int{1}, []int{2})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("CompareAndSwap did not panic")
				}
			}()
			test.call()
		})
	}
}

func TestValueConcurrentStoreLoad(t *testing.T) {
	values := [...]largeValue{largeValueFor(1), largeValueFor(2)}
	var value atomicv2.Value[largeValue]
	value.Store(values[0])

	const goroutines = 8
	iterations := 100000
	if testing.Short() {
		iterations = 1000
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for id := range goroutines {
		wg.Go(func() {
			<-start
			for i := range iterations {
				value.Store(values[(i+id)&1])
				if got := value.Load(); !validLargeValue(got) {
					t.Errorf("observed torn value: %+v", got)
					return
				}
			}
		})
	}
	close(start)
	wg.Wait()
}

func TestValueConcurrentSwap(t *testing.T) {
	var value atomicv2.Value[uint64]
	const goroutines = 20
	iterations := uint64(10000)
	if testing.Short() {
		iterations = 1000
	}
	var sum uint64
	var sumMu sync.Mutex
	var wg sync.WaitGroup
	for id := range uint64(goroutines) {
		wg.Go(func() {
			var local uint64
			base := id * iterations
			for next := base; next < base+iterations; next++ {
				local += value.Swap(next)
			}
			sumMu.Lock()
			sum += local
			sumMu.Unlock()
		})
	}
	wg.Wait()
	n := uint64(goroutines) * iterations
	if got, want := sum+value.Load(), (n-1)*n/2; got != want {
		t.Fatalf("sum of swapped values = %v, want %v", got, want)
	}
}

func TestValueConcurrentCompareAndSwap(t *testing.T) {
	var value atomicv2.Value[int]
	const goroutines = 16
	increments := 10000
	if testing.Short() {
		increments = 1000
	}
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range increments {
				for {
					old := value.Load()
					if value.CompareAndSwap(old, old+1) {
						break
					}
					runtime.Gosched()
				}
			}
		})
	}
	wg.Wait()
	if got, want := value.Load(), goroutines*increments; got != want {
		t.Fatalf("final value = %v, want %v", got, want)
	}
}

type finalizableValue struct {
	data byte
}

type boxedFinalizableValue struct {
	object *finalizableValue
	label  string
}

func testValueKeepsObjectAlive[T any](t *testing.T, wrap func(*finalizableValue) T, unwrap func(T) *finalizableValue) {
	t.Helper()
	finalized := make(chan struct{}, 1)
	object := new(finalizableValue)
	runtime.SetFinalizer(object, func(*finalizableValue) {
		finalized <- struct{}{}
	})

	var value atomicv2.Value[T]
	stored := wrap(object)
	value.Store(stored)
	var zero T
	stored = zero
	object = nil
	for range 3 {
		runtime.GC()
	}
	select {
	case <-finalized:
		t.Fatal("stored pointer was finalized")
	default:
	}
	if unwrap(value.Load()) == nil {
		t.Fatal("stored pointer was lost")
	}
	value.Store(zero)
	runtime.KeepAlive(&value)
}

func TestValueKeepsPointersAlive(t *testing.T) {
	t.Run("direct", func(t *testing.T) {
		testValueKeepsObjectAlive(t,
			func(object *finalizableValue) *finalizableValue { return object },
			func(object *finalizableValue) *finalizableValue { return object },
		)
	})
	t.Run("boxed", func(t *testing.T) {
		testValueKeepsObjectAlive(t,
			func(object *finalizableValue) boxedFinalizableValue {
				return boxedFinalizableValue{object: object, label: "boxed"}
			},
			func(value boxedFinalizableValue) *finalizableValue { return value.object },
		)
	})
	t.Run("interface", func(t *testing.T) {
		testValueKeepsObjectAlive(t,
			func(object *finalizableValue) any { return object },
			func(value any) *finalizableValue { return value.(*finalizableValue) },
		)
	})
}

func TestValueAllocations(t *testing.T) {
	var integer atomicv2.Value[int]
	n := 256
	if got := testing.AllocsPerRun(1000, func() {
		integer.Store(n)
		n++
	}); got != 0 {
		t.Fatalf("Value[int].Store allocated %v times, want 0", got)
	}

	x := 1
	var pointer atomicv2.Value[*int]
	if got := testing.AllocsPerRun(1000, func() {
		pointer.Store(&x)
	}); got != 0 {
		t.Fatalf("Value[*int].Store allocated %v times, want 0", got)
	}
	pointer.Store(nil)
	if got := testing.AllocsPerRun(1000, func() {
		if !pointer.CompareAndSwap(nil, &x) || !pointer.CompareAndSwap(&x, nil) {
			panic("unexpected pointer CompareAndSwap failure")
		}
	}); got != 0 {
		t.Fatalf("Value[*int].CompareAndSwap allocated %v times, want 0", got)
	}

	large := largeValueFor(1)
	var boxed atomicv2.Value[largeValue]
	if got := testing.AllocsPerRun(1000, func() {
		boxed.Store(large)
	}); got != 1 {
		t.Fatalf("Value[largeValue].Store allocated %v times, want 1", got)
	}
	if got := testing.AllocsPerRun(1000, func() {
		_ = boxed.Load()
	}); got != 0 {
		t.Fatalf("Value[largeValue].Load allocated %v times, want 0", got)
	}
	if got := testing.AllocsPerRun(1000, func() {
		_ = boxed.Swap(large)
	}); got != 1 {
		t.Fatalf("Value[largeValue].Swap allocated %v times, want 1", got)
	}
	if got := testing.AllocsPerRun(1000, func() {
		if boxed.CompareAndSwap(largeValueFor(2), largeValueFor(3)) {
			panic("unexpected swap")
		}
	}); got != 0 {
		t.Fatalf("failed Value[largeValue].CompareAndSwap allocated %v times, want 0", got)
	}
}
