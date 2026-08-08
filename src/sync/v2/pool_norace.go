// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !race

package sync

type poolRaceToken struct{}

func poolRacePut[T any](*poolItem[T]) bool {
	return true
}

func poolRacePutDone() {}

func poolRaceGet() {}

func poolRaceGetDone[T any](*poolItem[T], bool) {}
