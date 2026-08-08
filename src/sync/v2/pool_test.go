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

func TestPool(t *testing.T) {
	var zero syncv2.Pool[int]
	if got := zero.Get(); got != 0 {
		t.Fatalf("zero Pool.Get() = %v; want 0", got)
	}

	var calls atomic.Int32
	p := syncv2.Pool[*int]{
		New: func() *int {
			calls.Add(1)
			return new(int)
		},
	}
	x := p.Get()
	if x == nil || calls.Load() != 1 {
		t.Fatalf("Get() = %v, New calls = %v; want non-nil, 1", x, calls.Load())
	}
	*x = 42
	p.Put(x)
	if got := p.Get(); got == nil {
		t.Fatal("Get returned nil")
	}
}

func TestPoolConcurrent(t *testing.T) {
	p := syncv2.Pool[[]byte]{New: func() []byte { return make([]byte, 32) }}
	const goroutines = 32
	const iterations = 1000

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range iterations {
				buf := p.Get()
				if len(buf) != 32 {
					t.Errorf("Get returned buffer with length %v", len(buf))
					return
				}
				p.Put(buf)
			}
		})
	}
	wg.Wait()
}

func TestSynchronizationAliases(t *testing.T) {
	var mu syncv2.Mutex
	var wg syncv2.WaitGroup
	n := 0
	for range 10 {
		wg.Go(func() {
			mu.Lock()
			n++
			mu.Unlock()
		})
	}
	wg.Wait()
	if n != 10 {
		t.Fatalf("n = %v; want 10", n)
	}

	calls := 0
	f := syncv2.OnceValue(func() int {
		calls++
		return 42
	})
	if f() != 42 || f() != 42 || calls != 1 {
		t.Fatalf("OnceValue result/calls = %v/%v; want 42/1", f(), calls)
	}
}
