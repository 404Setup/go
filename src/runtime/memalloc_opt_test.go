// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"bytes"
	"internal/testenv"
	"runtime"
	"sync"
	"testing"
)

//go:noinline
func sinkBytesNoEscape(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0] ^ b[len(b)-1]
}

//go:noinline
func sinkIntsNoEscape(s []int) int {
	if len(s) == 0 {
		return 0
	}
	return s[0] + s[len(s)-1]
}

//go:noinline
func sinkPtrsNoEscape(s []*byte) *byte {
	if len(s) == 0 {
		return nil
	}
	return s[0]
}

//go:noinline
func sinkStringsNoEscape(s []string) int {
	if len(s) == 0 {
		return 0
	}
	return len(s[0]) + len(s[len(s)-1])
}

var escapingBytesSink []byte

//go:noinline
func makeEscapingBytes(n int) []byte {
	s := make([]byte, n)
	escapingBytesSink = s
	return s
}

//go:noinline
func makeCopyEscapingBytes(n int, src []byte) []byte {
	s := make([]byte, n)
	copy(s, src)
	escapingBytesSink = s
	return s
}

func TestReclaimBeforeGrow(t *testing.T) {
	// Deterministic check of ReclaimBeforeGrowForTest state transitions.
	runtime.GC()
	runtime.ResetReclaimIndexForTest()
	if idx := runtime.ReclaimIndexForTest(); idx >= 1<<63 {
		t.Fatalf("expected reclaimIndex < 1<<63 after reset, got %#x", idx)
	}
	runtime.ReclaimBeforeGrowForTest(1)
	runtime.ReclaimBeforeGrowForTest(1 << 20)
	if idx := runtime.ReclaimIndexForTest(); idx < 1<<63 {
		t.Fatalf("expected reclaimIndex >= 1<<63 after full reclaimBeforeGrow, got %#x", idx)
	}

	// Concurrent large-span allocation + GC churn exercising alloc -> reclaimBeforeGrow.
	const (
		workers = 4
		iters   = 6
		spanSz  = 64 << 10
	)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				batch := make([][]byte, 16)
				for j := range batch {
					batch[j] = make([]byte, spanSz)
					batch[j][0] = byte(j + 1)
					batch[j][spanSz-1] = byte(i + 1)
				}
				runtime.GC()
				runtime.ResetReclaimIndexForTest()
				for j := range batch {
					if batch[j][0] != byte(j+1) || batch[j][spanSz-1] != byte(i+1) {
						t.Errorf("corrupted span contents at (%d, %d)", i, j)
					}
				}
			}
		}()
	}
	wg.Wait()
}

func TestMakeSliceZeroAndEmptySourceFastPaths(t *testing.T) {
	testenv.SkipIfOptimizationOff(t)

	// 1. Escaping dynamic make([]byte, 0) should not allocate on the heap.
	zeroN := 0
	if allocs := testing.AllocsPerRun(20, func() {
		s := makeEscapingBytes(zeroN)
		if s == nil || len(s) != 0 || cap(s) != 0 {
			t.Fatalf("unexpected slice for n=0: nil=%v len=%d cap=%d", s == nil, len(s), cap(s))
		}
	}); allocs != 0 {
		t.Fatalf("makeEscapingBytes(0) allocs = %v, want 0", allocs)
	}

	// 2. Escaping dynamic make+copy with tolen == 0 should not allocate.
	nonEmptySrc := []byte{1, 2, 3, 4}
	if allocs := testing.AllocsPerRun(20, func() {
		s := makeCopyEscapingBytes(zeroN, nonEmptySrc)
		if s == nil || len(s) != 0 {
			t.Fatalf("unexpected slice for tolen=0: nil=%v len=%d", s == nil, len(s))
		}
	}); allocs != 0 {
		t.Fatalf("makeCopyEscapingBytes(0, nonEmptySrc) allocs = %v, want 0", allocs)
	}

	// 3. Escaping dynamic make+copy with empty source (copymem == 0) must still zero target.
	var emptySrc []byte
	s := makeCopyEscapingBytes(32, emptySrc)
	for i, b := range s {
		if b != 0 {
			t.Fatalf("s[%d] = %d, want 0", i, b)
		}
	}

	// 4. rawbyteslice(0) and rawruneslice(0) should not allocate.
	if allocs := testing.AllocsPerRun(20, func() {
		b := runtime.RawByteSliceForTest(0)
		r := runtime.RawRuneSliceForTest(0)
		if b == nil || len(b) != 0 || r == nil || len(r) != 0 {
			t.Fatalf("unexpected raw empty slice")
		}
	}); allocs != 0 {
		t.Fatalf("RawByteSliceForTest(0)/RawRuneSliceForTest(0) allocs = %v, want 0", allocs)
	}
}

//go:noinline
func allocDynamicBytesSBO(t *testing.T, n int) {
	b := make([]byte, n)
	for i := range b {
		if b[i] != 0 {
			t.Fatalf("unzeroed byte at %d for n=%d", i, n)
		}
		b[i] = 0xAA
	}
	sinkBytesNoEscape(b)
}

//go:noinline
func allocDynamicIntsSBO(n int) {
	s := make([]int, n)
	sinkIntsNoEscape(s)
}

//go:noinline
func allocDynamicPtrsSBO(n int) {
	s := make([]*byte, n)
	sinkPtrsNoEscape(s)
}

//go:noinline
func allocDynamicStringsSBO(n int) {
	s := make([]string, n)
	sinkStringsNoEscape(s)
}

//go:noinline
func allocDynamicMakeCopySBO(t *testing.T, sub []byte) {
	dst := make([]byte, len(sub))
	copy(dst, sub)
	if !bytes.Equal(dst, sub) {
		t.Fatalf("make+copy mismatch for len=%d", len(sub))
	}
	sinkBytesNoEscape(dst)
}

func TestDynamicMakeSliceSBO(t *testing.T) {
	testenv.SkipIfOptimizationOff(t)

	// 1. Pointerless []byte up to 128 bytes: 0 allocs; 129 bytes: 1 alloc.
	for _, n := range []int{0, 1, 32, 33, 64, 96, 128} {
		allocs := testing.AllocsPerRun(20, func() {
			allocDynamicBytesSBO(t, n)
		})
		if allocs != 0 {
			t.Fatalf("make([]byte, %d) allocs = %v, want 0", n, allocs)
		}
	}
	if allocs := testing.AllocsPerRun(20, func() {
		allocDynamicBytesSBO(t, 129)
	}); allocs != 1 {
		t.Fatalf("make([]byte, 129) allocs = %v, want 1", allocs)
	}

	// 2. Pointerless []int up to 16 elements (128 bytes): 0 allocs; 17 elements: 1 alloc.
	if allocs := testing.AllocsPerRun(20, func() {
		allocDynamicIntsSBO(16)
	}); allocs != 0 {
		t.Fatalf("make([]int, 16) allocs = %v, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(20, func() {
		allocDynamicIntsSBO(17)
	}); allocs != 1 {
		t.Fatalf("make([]int, 17) allocs = %v, want 1", allocs)
	}

	// 3. Pointer-containing []*byte up to 8 elements (64 bytes): 0 allocs; 9 elements: 1 alloc.
	if allocs := testing.AllocsPerRun(20, func() {
		allocDynamicPtrsSBO(8)
	}); allocs != 0 {
		t.Fatalf("make([]*byte, 8) allocs = %v, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(20, func() {
		allocDynamicPtrsSBO(9)
	}); allocs != 1 {
		t.Fatalf("make([]*byte, 9) allocs = %v, want 1", allocs)
	}

	// 4. Pointer-containing []string up to 4 elements (64 bytes): 0 allocs; 5 elements: 1 alloc.
	if allocs := testing.AllocsPerRun(20, func() {
		allocDynamicStringsSBO(4)
	}); allocs != 0 {
		t.Fatalf("make([]string, 4) allocs = %v, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(20, func() {
		allocDynamicStringsSBO(5)
	}); allocs != 1 {
		t.Fatalf("make([]string, 5) allocs = %v, want 1", allocs)
	}

	// 5. Non-escaping bounded make+copy (OMAKESLICECOPY) SBO up to 128 bytes.
	src := make([]byte, 256)
	for i := range src {
		src[i] = byte(i + 1)
	}
	for _, n := range []int{1, 32, 64, 128} {
		sub := src[:n]
		allocs := testing.AllocsPerRun(20, func() {
			allocDynamicMakeCopySBO(t, sub)
		})
		if allocs != 0 {
			t.Fatalf("non-escaping make+copy(n=%d) allocs = %v, want 0", n, allocs)
		}
	}
	if allocs := testing.AllocsPerRun(20, func() {
		allocDynamicMakeCopySBO(t, src[:129])
	}); allocs != 1 {
		t.Fatalf("non-escaping make+copy(n=129) allocs = %v, want 1", allocs)
	}
}

// dynamicMakeHelper has an AST inlining hairyness score above the standard
// budget (80) but within the dynamic-make boosted budget (120), allowing it
// to be inlined so its dynamic make([]byte, n) uses stack SBO.
func dynamicMakeHelper(src []byte, mask byte) uint64 {
	n := len(src)
	buf := make([]byte, n)
	for i := 0; i < n; i++ {
		v := src[i] ^ mask
		v = (v << 1) | (v >> 7)
		buf[i] = v + byte(i)
	}
	var acc uint64
	for i := 0; i < len(buf); i++ {
		acc = (acc * 131) + uint64(buf[i])
	}
	if acc == 0 {
		return 1
	}
	return acc
}

func TestDynamicMakeSliceInlinePromotion(t *testing.T) {
	testenv.SkipIfOptimizationOff(t)

	input := make([]byte, 96)
	for i := range input {
		input[i] = byte(i * 3)
	}
	if allocs := testing.AllocsPerRun(20, func() {
		if got := dynamicMakeHelper(input, 0x5A); got == 0 {
			t.Fatal("unexpected zero result")
		}
	}); allocs != 0 {
		t.Fatalf("dynamicMakeHelper(96B) allocs = %v, want 0", allocs)
	}
}

func TestEscapeLiteralMakeAndStr2Bytes(t *testing.T) {
	testenv.SkipIfOptimizationOff(t)

	if allocs := testing.AllocsPerRun(20, func() {
		// 1. make([]byte, cap(sliceLit))
		lit := []byte{1, 2, 3, 4, 5, 6, 7, 8}
		b1 := make([]byte, cap(lit))
		sinkBytesNoEscape(b1)

		// 2. make([]byte, len(stringLitVar))
		s := "0123456789abcdef0123456789abcdef0123456789abcdef"
		b2 := make([]byte, len(s))
		sinkBytesNoEscape(b2)

		// 3. []byte(stringLitVar) where len(s) > 32
		b3 := []byte(s)
		b3[0] ^= 1
		sinkBytesNoEscape(b3)

		// 4. make([]int, staticLenVar, constCap)
		n := 6
		ints := make([]int, n, 12)
		sinkIntsNoEscape(ints)
	}); allocs != 0 {
		t.Fatalf("TestEscapeLiteralMakeAndStr2Bytes allocs = %v, want 0", allocs)
	}
}

var (
	benchDynamicN64  = 64
	benchDynamicN128 = 128
	benchDynamicN16  = 16
	benchDynamicN8   = 8
	benchDynamicZero = 0
)

func BenchmarkDynamicMakeSliceSBO_Byte64(b *testing.B) {
	n := benchDynamicN64
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := make([]byte, n)
		buf[0] = byte(i)
		sinkBytesNoEscape(buf)
	}
}

func BenchmarkDynamicMakeSliceSBO_Byte128(b *testing.B) {
	n := benchDynamicN128
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := make([]byte, n)
		buf[0] = byte(i)
		sinkBytesNoEscape(buf)
	}
}

func BenchmarkDynamicMakeSliceSBO_Int16(b *testing.B) {
	n := benchDynamicN16
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := make([]int, n)
		buf[0] = i
		sinkIntsNoEscape(buf)
	}
}

func BenchmarkDynamicMakeSliceSBO_Ptr8(b *testing.B) {
	n := benchDynamicN8
	var x byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := make([]*byte, n)
		buf[0] = &x
		sinkPtrsNoEscape(buf)
	}
}

func BenchmarkDynamicMakeSliceCopySBO_Byte64(b *testing.B) {
	src := bytes.Repeat([]byte("abcd"), 16) // 64 bytes
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := make([]byte, len(src))
		copy(buf, src)
		sinkBytesNoEscape(buf)
	}
}

func BenchmarkDynamicMakeSliceCopySBO_Byte128(b *testing.B) {
	src := bytes.Repeat([]byte("abcd"), 32) // 128 bytes
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := make([]byte, len(src))
		copy(buf, src)
		sinkBytesNoEscape(buf)
	}
}

func BenchmarkDynamicMakeInlinePromotion(b *testing.B) {
	src := bytes.Repeat([]byte("01234567"), 12) // 96 bytes
	b.ReportAllocs()
	var sum uint64
	for i := 0; i < b.N; i++ {
		sum += dynamicMakeHelper(src, byte(i))
	}
	if sum == 0 {
		b.Fatal("unexpected zero sum")
	}
}

func BenchmarkStaticStringVarToBytes48(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := "0123456789abcdef0123456789abcdef0123456789abcdef"
		buf := []byte(s)
		buf[0] = byte(i)
		sinkBytesNoEscape(buf)
	}
}

func BenchmarkMakeSliceZeroDynamic(b *testing.B) {
	n := benchDynamicZero
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = makeEscapingBytes(n)
	}
}

func BenchmarkMakeSliceCopyZeroDynamic(b *testing.B) {
	n := benchDynamicZero
	src := []byte("0123456789abcdef")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = makeCopyEscapingBytes(n, src)
	}
}

func BenchmarkSpanReclaimChurn(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := make([]byte, 32<<10)
		s[0] = byte(i)
		escapingBytesSink = s
		if i&63 == 63 {
			runtime.GC()
		}
	}
}
