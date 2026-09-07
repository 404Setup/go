// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !race

package sync_test

import (
	"runtime"
	"runtime/debug"
	syncv1 "sync"
	. "sync/v2"
	"testing"
	"unsafe"
	"weak"
)

func TestPoolItemLayout(t *testing.T) {
	if got, want := unsafe.Sizeof(PoolItem[uintptr]{}), unsafe.Sizeof(uintptr(0)); got != want {
		t.Fatalf("sizeof(poolItem[uintptr]) = %v; want %v", got, want)
	}
}

func TestPoolSteadyStateDoesNotAllocate(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	var p Pool[[4]uintptr]
	p.Put([4]uintptr{})
	if allocs := testing.AllocsPerRun(1000, func() {
		value := p.Get()
		p.Put(value)
	}); allocs != 0 {
		t.Fatalf("Pool Get/Put allocated %v times per iteration; want 0", allocs)
	}
}

func TestPoolStoresZeroValue(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	p := Pool[int]{New: func() int { return 1 }}

	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	p.Put(0)
	if got := p.Get(); got != 0 {
		t.Fatalf("Get() = %v; want stored zero value", got)
	}
}

func TestPoolStoresNil(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	p := Pool[*int]{New: func() *int { return new(int) }}

	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	p.Put(nil)
	if got := p.Get(); got != nil {
		t.Fatalf("Get() = %v; want stored nil", got)
	}
}

func TestPoolGetOKStoredZero(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	testPoolGetOKZero[int](t)
	testPoolGetOKZero[*int](t)
	testPoolGetOKZero[[]byte](t)
	testPoolGetOKZero[map[int]int](t)
	testPoolGetOKZero[any](t)
	testPoolGetOKZero[struct{}](t)
}

func testPoolGetOKZero[T any](t *testing.T) {
	t.Helper()
	for _, victim := range []bool{false, true} {
		var p Pool[T]
		var zero T
		// Exercise both the private slot and a growing shared queue.
		for range 32 {
			p.Put(zero)
		}
		if victim {
			runtime.GC()
		}
		for i := range 32 {
			if _, ok := p.GetOK(); !ok {
				t.Fatalf("Pool[%T].GetOK() missed stored zero %d (victim=%v)", zero, i, victim)
			}
		}
		if _, ok := p.GetOK(); ok {
			t.Fatalf("Pool[%T].GetOK() succeeded on empty pool", zero)
		}
	}
}

type poolGCObject [64]byte

type poolGCValue struct {
	padding [1024]byte
	pointer *poolGCObject
	slice   []*poolGCObject
	iface   any
}

func TestPoolGC(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	testPoolGC(t, func(p *poolGCObject) *poolGCObject { return p })
	testPoolGC(t, func(p *poolGCObject) []*poolGCObject { return []*poolGCObject{p} })
	testPoolGC(t, func(p *poolGCObject) any { return p })
	testPoolGC(t, func(p *poolGCObject) poolGCValue {
		return poolGCValue{pointer: p, slice: []*poolGCObject{p}, iface: p}
	})
}

//go:noinline
func fillPoolGC[T any](wrap func(*poolGCObject) T) (*Pool[T], []weak.Pointer[poolGCObject]) {
	p := new(Pool[T])
	refs := make([]weak.Pointer[poolGCObject], 100)
	for i := range refs {
		value := new(poolGCObject)
		refs[i] = weak.Make(value)
		p.Put(wrap(value))
	}
	return p, refs
}

func testPoolGC[T any](t *testing.T, wrap func(*poolGCObject) T) {
	t.Helper()
	for _, mode := range []string{"primary", "victim", "expire"} {
		p, refs := fillPoolGC(wrap)
		if mode != "primary" {
			runtime.GC()
			for i, ref := range refs {
				if ref.Value() == nil {
					t.Fatalf("%T: victim lost object %d after one GC", p, i)
				}
			}
		}
		if mode != "expire" {
			for range refs {
				if _, ok := p.GetOK(); !ok {
					t.Fatalf("%T: %s cache lost a value", p, mode)
				}
			}
		}
		runtime.GC()
		for i, ref := range refs {
			if ref.Value() != nil {
				t.Fatalf("%T: %s cache retained object %d", p, mode, i)
			}
		}
		// Verify reclamation while the pool and its empty queues remain live.
		runtime.KeepAlive(p)
	}
}

func TestPoolVictimCache(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	var p Pool[string]

	for range 100 {
		p.Put("cached")
	}
	runtime.GC()
	if got := p.Get(); got != "cached" {
		t.Fatalf("Get() after one GC = %q; want cached", got)
	}
	runtime.GC()
	runtime.GC()
	if got := p.Get(); got != "" {
		t.Fatalf("Get() after victim expiry = %q; want empty", got)
	}
}

func TestPoolCoexistsWithV1(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	p1 := syncv1.Pool{New: func() any { return "new-v1" }}
	p2 := Pool[string]{New: func() string { return "new-v2" }}
	p1.Put("v1")
	p2.Put("v2")

	runtime.GC()
	if got := p1.Get(); got != "v1" {
		t.Fatalf("sync.Pool.Get() after GC = %v; want cached value", got)
	}
	if got := p2.Get(); got != "v2" {
		t.Fatalf("sync/v2.Pool.Get() after GC = %q; want cached value", got)
	}
	p1.Put("v1")
	p2.Put("v2")
	runtime.GC()
	runtime.GC()
	if got := p1.Get(); got != "new-v1" {
		t.Fatalf("sync.Pool retained expired victim: %v", got)
	}
	if got := p2.Get(); got != "new-v2" {
		t.Fatalf("sync/v2.Pool retained expired victim: %v", got)
	}
}

func TestPoolQueueReleasesReferences(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	for _, d := range []PoolDequeue[poolGCValue]{NewPoolDequeue[poolGCValue](16), NewPoolChain[poolGCValue]()} {
		for _, pop := range []func() (poolGCValue, bool){d.PopHead, d.PopTail} {
			ref := fillPoolQueueGC(d)
			runtime.GC()
			if ref.Value() == nil {
				t.Fatal("queue lost a live value")
			}
			if _, ok := pop(); !ok {
				t.Fatal("queue pop failed")
			}
			runtime.GC()
			if ref.Value() != nil {
				t.Fatal("queue retained a popped value")
			}
			runtime.KeepAlive(d)
		}
	}
}

//go:noinline
func fillPoolQueueGC(d PoolDequeue[poolGCValue]) weak.Pointer[poolGCObject] {
	p := new(poolGCObject)
	d.PushHead(poolGCValue{pointer: p, slice: []*poolGCObject{p}, iface: p})
	return weak.Make(p)
}
