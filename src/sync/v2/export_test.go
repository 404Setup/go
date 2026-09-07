// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import "unsafe"

// Export internals without importing testing, which depends on sync/v2.
type PoolItem[T any] = poolItem[T]

func MapStorage[K comparable, V any](m *Map[K, V]) unsafe.Pointer {
	return unsafe.Pointer(m.current.Load())
}

type PoolDequeue[T any] interface {
	PushHead(T) bool
	PopHead() (T, bool)
	PopTail() (T, bool)
}

func NewPoolDequeue[T any](n int) PoolDequeue[T] {
	d := &poolDequeue[T]{vals: make([]poolSlot[T], n)}
	// Exercise index wraparound as well as slot reuse.
	d.headTail.Store(d.pack(1<<dequeueBits-500, 1<<dequeueBits-500))
	return d
}

func (d *poolDequeue[T]) PushHead(value T) bool {
	return d.pushHead(poolItem[T]{value: value})
}

func (d *poolDequeue[T]) PopHead() (T, bool) {
	item, ok := d.popHead()
	return item.value, ok
}

func (d *poolDequeue[T]) PopTail() (T, bool) {
	item, ok := d.popTail()
	return item.value, ok
}

func NewPoolChain[T any]() PoolDequeue[T] { return new(poolChain[T]) }

func (c *poolChain[T]) PushHead(value T) bool {
	c.pushHead(poolItem[T]{value: value})
	return true
}

func (c *poolChain[T]) PopHead() (T, bool) {
	item, ok := c.popHead()
	return item.value, ok
}

func (c *poolChain[T]) PopTail() (T, bool) {
	item, ok := c.popTail()
	return item.value, ok
}
