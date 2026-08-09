// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package atomic provides a generic atomic value useful for implementing
// synchronization algorithms.
//
// Except for special, low-level applications, synchronization is better done
// with channels or the facilities of the [sync] package. Share memory by
// communicating; don't communicate by sharing memory.
//
// In the terminology of [the Go memory model], if the effect of an atomic
// operation A is observed by atomic operation B, then A "synchronizes before"
// B. All atomic operations executed in a program behave as though executed in
// some sequentially consistent order.
//
// [the Go memory model]: https://go.dev/ref/mem
package atomic
