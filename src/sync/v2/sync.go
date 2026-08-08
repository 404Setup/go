// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import syncv1 "sync"

// A Locker represents an object that can be locked and unlocked.
type Locker = syncv1.Locker

// Mutex is a mutual exclusion lock.
type Mutex = syncv1.Mutex

// RWMutex is a reader/writer mutual exclusion lock.
type RWMutex = syncv1.RWMutex

// Cond implements a condition variable, a rendezvous point for goroutines
// waiting for or announcing the occurrence of an event.
type Cond = syncv1.Cond

// NewCond returns a new Cond with Locker l.
func NewCond(l Locker) *Cond {
	return syncv1.NewCond(l)
}

// Once is an object that will perform exactly one action.
type Once = syncv1.Once

// WaitGroup waits for a collection of tasks to finish.
type WaitGroup = syncv1.WaitGroup

// OnceFunc returns a function that invokes f only once. The returned function
// may be called concurrently.
//
// If f panics, the returned function will panic with the same value on every call.
func OnceFunc(f func()) func() {
	return syncv1.OnceFunc(f)
}

// OnceValue returns a function that invokes f only once and returns the value
// returned by f. The returned function may be called concurrently.
//
// If f panics, the returned function will panic with the same value on every call.
func OnceValue[T any](f func() T) func() T {
	return syncv1.OnceValue(f)
}

// OnceValues returns a function that invokes f only once and returns the values
// returned by f. The returned function may be called concurrently.
//
// If f panics, the returned function will panic with the same value on every call.
func OnceValues[T1, T2 any](f func() (T1, T2)) func() (T1, T2) {
	return syncv1.OnceValues(f)
}
