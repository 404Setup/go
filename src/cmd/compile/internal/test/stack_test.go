// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"internal/testenv"
	"testing"
	"unsafe"
)

// Stack allocation size thresholds for variable-sized allocations.
// Matches makeSliceMaxStackSize in ../walk/builtin.go.
const (
	maxStackSizeNoScan = 128
	maxStackSizeScan   = 64
)

//go:noinline
func genericUse[T any](s []T) {
	// Doesn't escape s.
}

func TestStackAllocation(t *testing.T) {
	testenv.SkipIfOptimizationOff(t)

	type testCase struct {
		f            func(int)
		elemSize     uintptr
		maxStackSize int
	}

	srcBytes := make([]byte, maxStackSizeNoScan+16)

	for _, tc := range []testCase{
		{
			f: func(n int) {
				genericUse(make([]byte, n))
			},
			elemSize:     unsafe.Sizeof(byte(0)),
			maxStackSize: maxStackSizeNoScan,
		},
		{
			f: func(n int) {
				genericUse(make([]int, n))
			},
			elemSize:     unsafe.Sizeof(int(0)),
			maxStackSize: maxStackSizeNoScan,
		},
		{
			f: func(n int) {
				b := make([]byte, n)
				copy(b, srcBytes[:n])
				genericUse(b)
			},
			elemSize:     unsafe.Sizeof(byte(0)),
			maxStackSize: maxStackSizeNoScan,
		},
		{
			f: func(n int) {
				genericUse(make([]*byte, n))
			},
			elemSize:     unsafe.Sizeof((*byte)(nil)),
			maxStackSize: maxStackSizeScan,
		},
		{
			f: func(n int) {
				genericUse(make([]string, n))
			},
			elemSize:     unsafe.Sizeof(""),
			maxStackSize: maxStackSizeScan,
		},
	} {
		max := tc.maxStackSize / int(tc.elemSize)
		if n := testing.AllocsPerRun(10, func() {
			tc.f(max)
		}); n != 0 {
			t.Fatalf("unexpected allocation: %f", n)
		}
		if n := testing.AllocsPerRun(10, func() {
			tc.f(max + 1)
		}); n != 1 {
			t.Fatalf("unexpected allocation: %f", n)
		}
	}
}
