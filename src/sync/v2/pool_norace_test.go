// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !race

package sync

import (
	"runtime"
	"runtime/debug"
	syncv1 "sync"
	"testing"
	"unsafe"
)

func TestPoolItemLayout(t *testing.T) {
	if got, want := unsafe.Sizeof(poolItem[uintptr]{}), unsafe.Sizeof(uintptr(0)); got != want {
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

	// Pinning makes the local Put/Get order deterministic.
	runtime_procPin()
	p.Put(0)
	if got := p.Get(); got != 0 {
		t.Fatalf("Get() = %v; want stored zero value", got)
	}
	runtime_procUnpin()
}

func TestPoolStoresNil(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	p := Pool[*int]{New: func() *int { return new(int) }}

	runtime_procPin()
	p.Put(nil)
	if got := p.Get(); got != nil {
		t.Fatalf("Get() = %v; want stored nil", got)
	}
	runtime_procUnpin()
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
	p1 := syncv1.Pool{New: func() any { return "new-v1" }}
	p2 := Pool[string]{New: func() string { return "new-v2" }}
	p1.Put("v1")
	p2.Put("v2")

	runtime.GC()
	// Pool values may be discarded at any GC, but each implementation must
	// remain usable when both cleanup callbacks are registered.
	if got := p1.Get(); got != "v1" && got != "new-v1" {
		t.Fatalf("sync.Pool.Get() after GC = %v; want cached or New value", got)
	}
	if got := p2.Get(); got != "v2" && got != "new-v2" {
		t.Fatalf("sync/v2.Pool.Get() after GC = %q; want cached or New value", got)
	}
}
