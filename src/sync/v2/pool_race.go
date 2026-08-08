// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build race

package sync

import (
	"internal/race"
	"unsafe"
)

type poolRaceToken uint8

// from runtime
//
//go:linkname runtime_randn runtime.randn
func runtime_randn(n uint32) uint32

var poolRaceHash [128]uint64

func poolRaceAddr(token poolRaceToken) unsafe.Pointer {
	return unsafe.Pointer(&poolRaceHash[token])
}

func poolRacePut[T any](item *poolItem[T]) bool {
	if runtime_randn(4) == 0 {
		// Match sync.Pool's deliberate random drop under the race detector.
		return false
	}
	item.race = poolRaceToken(runtime_randn(uint32(len(poolRaceHash))))
	race.ReleaseMerge(poolRaceAddr(item.race))
	race.Disable()
	return true
}

func poolRacePutDone() {
	race.Enable()
}

func poolRaceGet() {
	race.Disable()
}

func poolRaceGetDone[T any](item *poolItem[T], ok bool) {
	race.Enable()
	if ok {
		race.Acquire(poolRaceAddr(item.race))
	}
}
