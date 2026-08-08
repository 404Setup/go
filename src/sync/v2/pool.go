// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	rtatomic "internal/runtime/atomic"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// A Pool is a set of temporary values that may be individually saved and
// retrieved.
//
// Any value stored in the Pool may be removed automatically at any time without
// notification. If the Pool holds the only reference when this happens, the
// value might be deallocated.
//
// A Pool is safe for use by multiple goroutines simultaneously.
//
// Pool's purpose is to cache allocated but unused values for later reuse,
// relieving pressure on the garbage collector. It is not suitable for free
// lists embedded in short-lived objects, where the overhead does not amortize.
//
// A Pool must not be copied after first use.
//
// In the terminology of the Go memory model, a call to Put(x) synchronizes
// before a call to Get returning that same value x. Similarly, a call to New
// returning x synchronizes before a call to Get returning that same value x.
//
// [the Go memory model]: https://go.dev/ref/mem
type Pool[T any] struct {
	_ noCopy

	local     unsafe.Pointer // local fixed-size per-P pool, actual type is [P]poolLocal[T]
	localSize uintptr        // size of the local array

	victim     unsafe.Pointer // local from previous cycle
	victimSize uintptr        // size of victims array

	// New optionally specifies a function to generate a value when Get would
	// otherwise find the pool empty. It may not be changed concurrently with
	// calls to Get.
	New func() T
}

type poolItem[T any] struct {
	value T
	race  uint8
}

type poolLocalInternal[T any] struct {
	private    poolItem[T] // Can be used only by the respective P.
	privateSet bool
	shared     poolChain[T] // Local P can pushHead/popHead; any P can popTail.
}

type poolLocal[T any] struct {
	poolLocalInternal[T]

	// Keep local shards on separate cache lines. The size of T is not a
	// compile-time constant, so use a full cache line instead of rounding the
	// generic structure size up to one.
	pad [128]byte
}

// from runtime
//
//go:linkname runtime_randn runtime.randn
func runtime_randn(n uint32) uint32

var poolRaceHash [128]uint64

func poolRaceAddr(i uint8) unsafe.Pointer {
	return unsafe.Pointer(&poolRaceHash[i])
}

// Put adds x to the pool.
func (p *Pool[T]) Put(x T) {
	item := poolItem[T]{value: x}
	if race.Enabled {
		if runtime_randn(4) == 0 {
			// Match sync.Pool's deliberate random drop under the race detector.
			return
		}
		item.race = uint8(runtime_randn(uint32(len(poolRaceHash))))
		race.ReleaseMerge(poolRaceAddr(item.race))
		race.Disable()
	}
	l, _ := p.pin()
	if !l.privateSet {
		l.private = item
		l.privateSet = true
	} else {
		l.shared.pushHead(item)
	}
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
	}
}

// Get selects an arbitrary value from the Pool, removes it from the Pool, and
// returns it to the caller. Get may choose to ignore the pool and treat it as
// empty. Callers should not assume any relation between values passed to Put
// and the values returned by Get.
//
// If the pool is empty and p.New is non-nil, Get returns the result of calling
// p.New. Otherwise, Get returns the zero value of T.
func (p *Pool[T]) Get() T {
	if race.Enabled {
		race.Disable()
	}
	l, pid := p.pin()
	item, ok := l.private, l.privateSet
	if ok {
		var zero poolItem[T]
		l.private = zero
		l.privateSet = false
	} else {
		item, ok = l.shared.popHead()
		if !ok {
			item, ok = p.getSlow(pid)
		}
	}
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
		if ok {
			race.Acquire(poolRaceAddr(item.race))
		}
	}
	if ok {
		return item.value
	}
	if p.New != nil {
		return p.New()
	}
	var zero T
	return zero
}

func (p *Pool[T]) getSlow(pid int) (poolItem[T], bool) {
	// See the comment in pin regarding ordering of the loads.
	size := rtatomic.LoadAcquintptr(&p.localSize) // load-acquire
	locals := p.local                             // load-consume
	for i := 0; i < int(size); i++ {
		l := indexLocal[T](locals, (pid+i+1)%int(size))
		if item, ok := l.shared.popTail(); ok {
			return item, true
		}
	}

	// Try the victim cache after all primary shards so that victim values age
	// out when the primary cache has enough traffic.
	size = atomic.LoadUintptr(&p.victimSize)
	if uintptr(pid) >= size {
		return poolItem[T]{}, false
	}
	locals = p.victim
	l := indexLocal[T](locals, pid)
	if l.privateSet {
		item := l.private
		var zero poolItem[T]
		l.private = zero
		l.privateSet = false
		return item, true
	}
	for i := 0; i < int(size); i++ {
		l := indexLocal[T](locals, (pid+i)%int(size))
		if item, ok := l.shared.popTail(); ok {
			return item, true
		}
	}

	atomic.StoreUintptr(&p.victimSize, 0)
	return poolItem[T]{}, false
}

// pin pins the current goroutine to P, disables preemption, and returns the
// local shard for that P. The caller must call runtime_procUnpin when done.
func (p *Pool[T]) pin() (*poolLocal[T], int) {
	if p == nil {
		panic("nil Pool")
	}

	pid := runtime_procPin()
	// pinSlow stores local before localSize; load them in the opposite order.
	// With preemption disabled, GC cannot run between these loads.
	s := rtatomic.LoadAcquintptr(&p.localSize) // load-acquire
	l := p.local                               // load-consume
	if uintptr(pid) < s {
		return indexLocal[T](l, pid), pid
	}
	return p.pinSlow()
}

func (p *Pool[T]) pinSlow() (*poolLocal[T], int) {
	// A mutex cannot be acquired while pinned.
	runtime_procUnpin()
	allPoolsMu.Lock()
	defer allPoolsMu.Unlock()
	pid := runtime_procPin()

	s := p.localSize
	l := p.local
	if uintptr(pid) < s {
		return indexLocal[T](l, pid), pid
	}
	if p.local == nil {
		allPools = append(allPools, p)
	}
	size := runtime.GOMAXPROCS(0)
	local := make([]poolLocal[T], size)
	atomic.StorePointer(&p.local, unsafe.Pointer(&local[0])) // store-release
	rtatomic.StoreReluintptr(&p.localSize, uintptr(size))    // store-release
	return &local[pid], pid
}

// poolCleaner is the type-erased boundary used only by the stop-the-world GC
// callback. Values themselves remain typed in every Pool and local queue.
type poolCleaner interface {
	dropVictim()
	movePrimaryToVictim()
}

func (p *Pool[T]) dropVictim() {
	p.victim = nil
	p.victimSize = 0
}

func (p *Pool[T]) movePrimaryToVictim() {
	p.victim = p.local
	p.victimSize = p.localSize
	p.local = nil
	p.localSize = 0
}

func poolCleanup() {
	// This function runs with the world stopped at the beginning of a garbage
	// collection. It must not allocate or call into the runtime.
	for _, p := range oldPools {
		p.dropVictim()
	}
	for _, p := range allPools {
		p.movePrimaryToVictim()
	}
	oldPools, allPools = allPools, nil
}

var (
	allPoolsMu Mutex

	// Protected by allPoolsMu plus pinning, or by STW.
	allPools []poolCleaner
	oldPools []poolCleaner
)

func init() {
	runtime_registerPoolCleanup(poolCleanup)
}

func indexLocal[T any](l unsafe.Pointer, i int) *poolLocal[T] {
	lp := unsafe.Pointer(uintptr(l) + uintptr(i)*unsafe.Sizeof(poolLocal[T]{}))
	return (*poolLocal[T])(lp)
}

// Implemented in runtime and exported under sync's established ABI names.
//
//go:linkname runtime_registerPoolCleanup sync.runtime_registerPoolCleanup
func runtime_registerPoolCleanup(cleanup func())

//go:linkname runtime_procPin sync.runtime_procPin
func runtime_procPin() int

//go:linkname runtime_procUnpin sync.runtime_procUnpin
func runtime_procUnpin()
