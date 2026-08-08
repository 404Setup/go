// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"runtime"
	"sync/atomic"
	"testing"
)

func TestPoolDequeue(t *testing.T) {
	d := poolDequeue[int]{vals: make([]poolSlot[int], 16)}
	testPoolQueue(t, d.pushHead, d.popHead, d.popTail)
}

func TestPoolChain(t *testing.T) {
	var c poolChain[int]
	testPoolQueue(t, func(value poolItem[int]) bool {
		c.pushHead(value)
		return true
	}, c.popHead, c.popTail)
}

func testPoolQueue(t *testing.T, push func(poolItem[int]) bool, popHead, popTail func() (poolItem[int], bool)) {
	const consumers = 8
	n := 100_000
	if testing.Short() {
		n = 1_000
	}
	have := make([]atomic.Uint32, n)
	var stop atomic.Bool
	var wg WaitGroup
	record := func(value int) {
		if value < 0 || value >= n {
			t.Errorf("queue returned out-of-range value %v", value)
			return
		}
		have[value].Add(1)
		if value == n-1 {
			stop.Store(true)
		}
	}

	for range consumers {
		wg.Go(func() {
			failures := 0
			for !stop.Load() {
				if value, ok := popTail(); ok {
					failures = 0
					record(value.value)
				} else if failures++; failures%100 == 0 {
					runtime.Gosched()
				}
			}
		})
	}

	for value := 0; value < n; value++ {
		for !push(poolItem[int]{value: value}) {
			runtime.Gosched()
		}
		if value%10 == 0 {
			if item, ok := popHead(); ok {
				record(item.value)
			}
		}
	}
	wg.Wait()

	for value := range have {
		if got := have[value].Load(); got != 1 {
			t.Errorf("value %v observed %v times; want once", value, got)
		}
	}
}

func TestNilPool(t *testing.T) {
	var p *Pool[int]
	for _, test := range []struct {
		name string
		f    func()
	}{
		{"Get", func() { p.Get() }},
		{"Put", func() { p.Put(1) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("operation on nil Pool did not panic")
				}
			}()
			test.f()
		})
	}
}
