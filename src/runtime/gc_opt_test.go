// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

type gcOptNode struct {
	id   uint64
	next *gcOptNode
	prev *gcOptNode
	aux  *uint64
	pad  [3]uint64
}

type gcOptWideRecord struct {
	s0  uint64
	s1  uint64
	p0  *gcOptNode
	s2  uint64
	s3  uint64
	s4  uint64
	p1  *gcOptNode
	s5  uint64
	s6  uint64
	p2  *uint64
	s7  uint64
	s8  uint64
	s9  uint64
	p3  *gcOptNode
	s10 uint64
	s11 uint64
}

var (
	gcOptNodeSink   []*gcOptNode
	gcOptRecordSink []gcOptWideRecord
	gcOptByteSink   [][]byte
)

func TestGCSweepZombieCheckAndCountAlloc(t *testing.T) {
	// Exercise span sweeping across size classes with partial survival so that
	// s.freeindex < s.nelems and non-64-bit-aligned freeindex offsets are tested.
	sizes := []int{8, 16, 24, 32, 48, 64, 96, 128, 256, 512, 1024}
	for _, sz := range sizes {
		const count = 300
		objs := make([][]byte, count)
		for i := range objs {
			b := make([]byte, sz)
			b[0] = byte(i + 1)
			b[sz-1] = byte((i * 7) ^ 0x5A)
			objs[i] = b
		}
		// Drop specific indices across byte and 64-bit word boundaries (0, 1, 7, 8, 63, 64, 65, 127, 128).
		for i := range objs {
			if i%3 == 0 || i == 63 || i == 64 || i == 65 || i == 127 || i == 128 {
				objs[i] = nil
			}
		}
		runtime.GC()
		// Allocate a few new objects so freeindex advances into the middle of partially swept spans.
		for i := 0; i < 37; i++ {
			b := make([]byte, sz)
			b[0] = 0xEE
			objs[i*2%count] = b
		}
		runtime.GC()
		for i, b := range objs {
			if b == nil {
				continue
			}
			if b[0] == 0xEE {
				continue
			}
			if b[0] != byte(i+1) || b[sz-1] != byte((i*7)^0x5A) {
				t.Fatalf("size %d index %d corrupted: got (%d, %d)", sz, i, b[0], b[sz-1])
			}
		}
		gcOptByteSink = objs
	}
	gcOptByteSink = nil
}

func TestGCBulkBarrierNilFilterAndNextFast(t *testing.T) {
	const (
		sliceLen = 256
		iters    = 40
	)
	sharedVal := uint64(0xCAFEBABE12345678)
	sharedNode := &gcOptNode{id: 42, aux: &sharedVal}

	src := make([]gcOptWideRecord, sliceLen)
	dst := make([]gcOptWideRecord, sliceLen)
	for i := range src {
		if i%4 == 0 {
			src[i].p0 = &gcOptNode{id: uint64(i + 100), aux: &sharedVal}
			src[i].p1 = sharedNode
		}
		if i%7 == 0 {
			v := uint64(i * 9)
			src[i].p2 = &v
			src[i].p3 = sharedNode
		}
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			runtime.GC()
		}
	}()

	for it := 0; it < iters; it++ {
		copy(dst, src)
		cloned := append([]gcOptWideRecord(nil), dst...)
		// Assign individual wide structs to exercise typeBitsBulkBarrier.
		for j := 0; j < len(dst); j += 16 {
			dst[j] = cloned[j]
		}
		if it%2 == 1 {
			clear(dst)
		}
		gcOptRecordSink = cloned
	}

	stop.Store(true)
	wg.Wait()
	runtime.GC()

	for i, r := range gcOptRecordSink {
		if i%4 == 0 {
			if r.p0 == nil || r.p0.id != uint64(i+100) || *r.p0.aux != sharedVal {
				t.Fatalf("corrupted p0 at %d", i)
			}
			if r.p1 != sharedNode || r.p1.id != 42 {
				t.Fatalf("corrupted p1 at %d", i)
			}
		}
		if i%7 == 0 {
			if r.p2 == nil || *r.p2 != uint64(i*9) {
				t.Fatalf("corrupted p2 at %d", i)
			}
		}
	}
	gcOptRecordSink = nil
}

func TestGCWbBufFlushDeduplication(t *testing.T) {
	buf := make([]byte, 32)
	buf[0] = 0x7F
	runtime.WbBufFlushDedupForTest(uintptr(unsafe.Pointer(&buf[0])))
	if buf[0] != 0x7F {
		t.Fatalf("unexpected mutation after WbBufFlushDedupForTest: %d", buf[0])
	}
	runtime.KeepAlive(buf)
}

func TestGCScanBlockAndSpecialsBitScan(t *testing.T) {
	// Test ScanBlockForTest with sparse pointer bitmaps.
	const words = 64
	var block [words]uintptr
	var mask [words / 8]uint8
	anchor := new(uint64)
	*anchor = 0x1122334455667788
	block[3] = uintptr(unsafe.Pointer(anchor))
	block[19] = uintptr(unsafe.Pointer(anchor))
	block[52] = uintptr(unsafe.Pointer(anchor))
	mask[3/8] |= 1 << (3 % 8)
	mask[19/8] |= 1 << (19 % 8)
	mask[52/8] |= 1 << (52 % 8)
	runtime.ScanBlockForTest(uintptr(unsafe.Pointer(&block[0])), uintptr(words)*unsafe.Sizeof(uintptr(0)), &mask[0])
	runtime.KeepAlive(anchor)

	// Test markrootSpans (TrailingZeros8 on pageSpecials) via AddCleanup.
	const numSpecials = 64
	var cleaned atomic.Int32
	liveKeep := make([]*gcOptNode, 0, numSpecials/2)
	for i := 0; i < numSpecials; i++ {
		n := &gcOptNode{id: uint64(i + 1)}
		runtime.AddCleanup(n, func(id uint64) {
			if id == 0 {
				panic("zero cleanup id")
			}
			cleaned.Add(1)
		}, uint64(i+1))
		if i%2 == 0 {
			liveKeep = append(liveKeep, n)
		}
	}
	runtime.GC()
	runtime.GC()
	for _, n := range liveKeep {
		if n.id == 0 {
			t.Fatal("live object corrupted")
		}
	}
	runtime.KeepAlive(liveKeep)
}

func TestGCSpanSetResetFastPath(t *testing.T) {
	runtime.ResetEmptySpanSetsForTest()
	runtime.GC()
	runtime.ResetEmptySpanSetsForTest()
}

func BenchmarkGCSpanSetResetEmpty(b *testing.B) {
	for i := 0; i < b.N; i++ {
		runtime.ResetEmptySpanSetsForTest()
	}
}

func BenchmarkGCScanBlockSparse(b *testing.B) {
	const words = 256
	var block [words]uintptr
	var mask [words / 8]uint8
	target := make([]byte, 32)
	ptr := uintptr(unsafe.Pointer(&target[0]))
	for i := 0; i < words; i += 8 {
		block[i] = ptr
		mask[i/8] = 1
	}
	bAddr := uintptr(unsafe.Pointer(&block[0]))
	bSize := uintptr(words) * unsafe.Sizeof(uintptr(0))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runtime.ScanBlockForTest(bAddr, bSize, &mask[0])
	}
	runtime.KeepAlive(target)
	runtime.KeepAlive(&block)
}

func BenchmarkGCWbBufFlushDedup(b *testing.B) {
	target := make([]byte, 32)
	ptr := uintptr(unsafe.Pointer(&target[0]))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runtime.WbBufFlushDedupForTest(ptr)
	}
	runtime.KeepAlive(target)
}

func BenchmarkGCBulkBarrierBitmapSparseNil(b *testing.B) {
	const words = 256
	var dst, src [words]uintptr
	var mask [words / 8]uint8
	target := make([]byte, 32)
	ptr := uintptr(unsafe.Pointer(&target[0]))
	for i := 0; i < words; i++ {
		mask[i/8] |= 1 << (i % 8)
		if i%8 == 0 {
			src[i] = ptr
		}
	}
	dstAddr := uintptr(unsafe.Pointer(&dst[0]))
	srcAddr := uintptr(unsafe.Pointer(&src[0]))
	byteSize := uintptr(words) * unsafe.Sizeof(uintptr(0))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runtime.BulkBarrierBitmapForTest(dstAddr, srcAddr, byteSize, &mask[0])
	}
	runtime.KeepAlive(target)
	runtime.KeepAlive(&dst)
	runtime.KeepAlive(&src)
}

func BenchmarkGCSweepSmallObjects(b *testing.B) {
	b.ReportAllocs()
	const batchSize = 512
	survivors := make([][32]byte, batchSize/4)
	for i := 0; i < b.N; i++ {
		p := new([32]byte)
		p[0] = byte(i)
		if i&3 == 0 {
			survivors[(i>>2)%len(survivors)] = *p
		}
		if i&(batchSize-1) == batchSize-1 {
			runtime.GC()
		}
	}
	if survivors[0][0] == 0xff && len(survivors) == 0 {
		b.Fatal("unreachable")
	}
}

func BenchmarkGCScanSharedPointers(b *testing.B) {
	sharedA := &gcOptNode{id: 1}
	sharedB := &gcOptNode{id: 2}
	ring := make([]*gcOptNode, 4096)
	for i := range ring {
		switch i & 3 {
		case 0, 1:
			ring[i] = sharedA
		case 2:
			ring[i] = sharedB
		default:
			ring[i] = nil
		}
	}
	gcOptNodeSink = ring
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runtime.GC()
	}
	gcOptNodeSink = nil
}
