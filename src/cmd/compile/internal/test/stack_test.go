// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"internal/race"
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

var (
	sharedMakeLen   int
	sharedMakeSlice []byte
)

//go:noinline
func useSharedMake(p *int) {
	var guard [4]uint64 = [4]uint64{0xdeadbeef, 0xdeadbeef, 0xdeadbeef, 0xdeadbeef}
	b := make([]byte, sharedMakeLen)
	for i := range b {
		b[i] = 0x42
	}
	b2 := make([]byte, *p, *p)
	for i := range b2 {
		b2[i] = 0x43
	}
	b3 := make([]byte, len(sharedMakeSlice))
	copy(b3, sharedMakeSlice)
	for _, g := range guard {
		if g != 0xdeadbeef {
			panic("stack corruption")
		}
	}
	genericUse(b)
	genericUse(b2)
	genericUse(b3)
}

func TestStackAllocationSharedBounds(t *testing.T) {
	testenv.SkipIfOptimizationOff(t)
	if testing.Short() || race.Enabled {
		t.Skip("skipping concurrent race-safety stress in short or race mode")
	}
	var v int = 4
	small := make([]byte, 8)
	large := make([]byte, 512)
	sharedMakeLen = 4
	sharedMakeSlice = small

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50000; i++ {
			if i&1 == 0 {
				sharedMakeLen = 4
				v = 4
				sharedMakeSlice = small
			} else {
				sharedMakeLen = 512
				v = 512
				sharedMakeSlice = large
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
			useSharedMake(&v)
		}
	}
}
