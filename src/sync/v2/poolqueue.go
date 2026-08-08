// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import "sync/atomic"

// poolDequeue is a lock-free fixed-size single-producer, multi-consumer queue.
// The producer can push and pop at the head; consumers pop at the tail.
type poolDequeue[T any] struct {
	// headTail packs a 32-bit head index and a 32-bit tail index. The head is
	// stored in the most-significant bits so incrementing it cannot disturb the
	// tail on overflow.
	headTail atomic.Uint64

	// The length of vals must be a power of two.
	vals []poolSlot[T]
}

type poolSlot[T any] struct {
	// state is zero when the producer may reuse this slot. A consumer clears
	// value before publishing state=0.
	state atomic.Uint32
	value poolItem[T]
}

const dequeueBits = 32

// dequeueLimit must be at most (1<<dequeueBits)/2 for full detection. Dividing
// by four also keeps it representable as an int on 32-bit systems.
const dequeueLimit = (1 << dequeueBits) / 4

func (d *poolDequeue[T]) unpack(ptrs uint64) (head, tail uint32) {
	const mask = 1<<dequeueBits - 1
	head = uint32((ptrs >> dequeueBits) & mask)
	tail = uint32(ptrs & mask)
	return
}

func (d *poolDequeue[T]) pack(head, tail uint32) uint64 {
	const mask = 1<<dequeueBits - 1
	return uint64(head)<<dequeueBits | uint64(tail&mask)
}

func (d *poolDequeue[T]) pushHead(value poolItem[T]) bool {
	ptrs := d.headTail.Load()
	head, tail := d.unpack(ptrs)
	if (tail+uint32(len(d.vals)))&(1<<dequeueBits-1) == head {
		return false
	}
	slot := &d.vals[head&uint32(len(d.vals)-1)]
	if slot.state.Load() != 0 {
		// A consumer claimed the slot but has not finished clearing it.
		return false
	}

	slot.value = value
	slot.state.Store(1)
	// Publishing the new head makes value visible to consumers.
	d.headTail.Add(1 << dequeueBits)
	return true
}

func (d *poolDequeue[T]) popHead() (poolItem[T], bool) {
	var slot *poolSlot[T]
	for {
		ptrs := d.headTail.Load()
		head, tail := d.unpack(ptrs)
		if tail == head {
			return poolItem[T]{}, false
		}
		head--
		if d.headTail.CompareAndSwap(ptrs, d.pack(head, tail)) {
			slot = &d.vals[head&uint32(len(d.vals)-1)]
			break
		}
	}

	value := slot.value
	var zero poolItem[T]
	slot.value = zero
	slot.state.Store(0)
	return value, true
}

func (d *poolDequeue[T]) popTail() (poolItem[T], bool) {
	var slot *poolSlot[T]
	for {
		ptrs := d.headTail.Load()
		head, tail := d.unpack(ptrs)
		if tail == head {
			return poolItem[T]{}, false
		}
		if d.headTail.CompareAndSwap(ptrs, d.pack(head, tail+1)) {
			slot = &d.vals[tail&uint32(len(d.vals)-1)]
			break
		}
	}

	value := slot.value
	var zero poolItem[T]
	slot.value = zero
	// Publish the cleared value last so the producer cannot reuse this slot
	// while it still retains references.
	slot.state.Store(0)
	return value, true
}

// poolChain grows by linking fixed-size dequeues. Only the producer writes head;
// any consumer may advance tail.
type poolChain[T any] struct {
	head *poolChainElt[T]
	tail atomic.Pointer[poolChainElt[T]]
}

type poolChainElt[T any] struct {
	poolDequeue[T]
	next, prev atomic.Pointer[poolChainElt[T]]
}

func (c *poolChain[T]) pushHead(value poolItem[T]) {
	d := c.head
	if d == nil {
		const initSize = 8
		d = new(poolChainElt[T])
		d.vals = make([]poolSlot[T], initSize)
		c.head = d
		c.tail.Store(d)
	}
	if d.pushHead(value) {
		return
	}

	newSize := len(d.vals) * 2
	if newSize >= dequeueLimit {
		newSize = dequeueLimit
	}
	d2 := new(poolChainElt[T])
	d2.prev.Store(d)
	d2.vals = make([]poolSlot[T], newSize)
	c.head = d2
	d.next.Store(d2)
	d2.pushHead(value)
}

func (c *poolChain[T]) popHead() (poolItem[T], bool) {
	for d := c.head; d != nil; d = d.prev.Load() {
		if value, ok := d.popHead(); ok {
			return value, true
		}
	}
	return poolItem[T]{}, false
}

func (c *poolChain[T]) popTail() (poolItem[T], bool) {
	d := c.tail.Load()
	if d == nil {
		return poolItem[T]{}, false
	}

	for {
		// Load next before popping. If next is non-nil and the pop fails, d is
		// permanently empty and can be removed from the chain.
		d2 := d.next.Load()
		if value, ok := d.popTail(); ok {
			return value, true
		}
		if d2 == nil {
			return poolItem[T]{}, false
		}
		if c.tail.CompareAndSwap(d, d2) {
			d2.prev.Store(nil)
		}
		d = d2
	}
}
