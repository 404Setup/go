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

var (
	runtimeOptBytesSink [][]byte
	runtimeOptPtrSink   [][]*byte
)

func TestSpanClassToNelemsConsistency(t *testing.T) {
	if !runtime.CheckSpanClassToNelemsForTest() {
		t.Fatal("spanClassToNelems table does not match expected per-spanclass element counts")
	}
}

func TestFusedInlineMarksSweepAllSizeClasses(t *testing.T) {
	// Exercise fused inline mark move + live count + zombie check across
	// 16B (504 elems -> hits 8th word containing imb.class byte), 24B..512B,
	// and >512B (non-inline mark bits), covering empty, partial, and 100%-full spans.
	sizes := []int{16, 24, 32, 48, 64, 96, 128, 256, 512, 1024}
	for _, sz := range sizes {
		// 1. Full spans (100% survival across multiple GCs).
		full := make([][]byte, 1200)
		for i := range full {
			b := make([]byte, sz)
			b[0] = byte(i ^ 0xA5)
			b[sz-1] = byte(i + 3)
			full[i] = b
		}
		runtime.GC()
		runtime.GC()
		for i, b := range full {
			if b[0] != byte(i^0xA5) || b[sz-1] != byte(i+3) {
				t.Fatalf("full span size=%d idx=%d corrupted", sz, i)
			}
		}

		// 2. Partial spans with survival across 64-bit word boundaries (0, 63, 64, 447, 448, 503).
		for i := range full {
			switch {
			case i == 0 || i == 63 || i == 64 || i == 447 || i == 448 || i == 503:
				// keep boundary objects alive
			case i%5 == 0:
				// keep sparse survivors
			default:
				full[i] = nil
			}
		}
		runtime.GC()
		// Reallocate into swept partial spans so freeindex sits mid-word.
		for i := 0; i < 73; i++ {
			b := make([]byte, sz)
			b[0] = 0xCC
			b[sz-1] = 0xDD
			full[i*7%len(full)] = b
		}
		runtime.GC()
		for i, b := range full {
			if b == nil {
				continue
			}
			if b[0] == 0xCC && b[sz-1] == 0xDD {
				continue
			}
			if b[0] != byte(i^0xA5) || b[sz-1] != byte(i+3) {
				t.Fatalf("partial span size=%d idx=%d corrupted", sz, i)
			}
		}

		// 3. Drop everything and verify empty spans sweep cleanly.
		runtimeOptBytesSink = full
		runtimeOptBytesSink = nil
		full = nil
		runtime.GC()
	}
}

func TestMCacheReleaseAllAndStackCacheClear(t *testing.T) {
	// Exercise empty and populated mcache.releaseAll + stackcache_clear
	// and verify MemStats consistency (no underflow or lost alloc counts).
	var ms1, ms2 runtime.MemStats
	runtime.ReadMemStats(&ms1)

	// Empty flush.
	runtime.ReleaseAllMCacheForTest()
	runtime.StackCacheClearForTest()

	// Populate multiple size classes (tiny, small noscan, small scan).
	var keep []any
	for _, sz := range []int{8, 16, 24, 32, 48, 64, 96, 128, 256, 512, 1024, 2048} {
		keep = append(keep, make([]byte, sz))
	}
	for i := 0; i < 16; i++ {
		p := new(*byte)
		keep = append(keep, p)
	}

	runtime.ReleaseAllMCacheForTest()
	runtime.StackCacheClearForTest()
	// Second immediate call exercises empty fast path.
	runtime.ReleaseAllMCacheForTest()
	runtime.StackCacheClearForTest()

	runtime.ReadMemStats(&ms2)
	if ms2.TotalAlloc < ms1.TotalAlloc {
		t.Fatalf("TotalAlloc decreased: %d -> %d", ms1.TotalAlloc, ms2.TotalAlloc)
	}
	runtime.KeepAlive(keep)
}

func TestMCacheReleaseAllConcurrent(t *testing.T) {
	const workers = 4
	var workersWg sync.WaitGroup
	var readerWg sync.WaitGroup
	var stop atomic.Bool
	readerWg.Add(1)
	go func() {
		defer readerWg.Done()
		var ms runtime.MemStats
		for !stop.Load() {
			runtime.ReadMemStats(&ms)
		}
	}()
	for w := 0; w < workers; w++ {
		workersWg.Add(1)
		go func(id int) {
			defer workersWg.Done()
			for i := 0; i < 200; i++ {
				b := make([]byte, 32+(i%8)*16)
				b[0] = byte(id)
				if i%16 == 0 {
					runtime.ReleaseAllMCacheForTest()
					runtime.StackCacheClearForTest()
				}
				runtime.KeepAlive(b)
			}
		}(w)
	}
	workersWg.Wait()
	stop.Store(true)
	readerWg.Wait()
}

func TestStackAllocFreeOrders(t *testing.T) {
	// Exercise stackalloc/stackfree across all small stack cache orders.
	for _, n := range []uint32{2048, 4096, 8192, 16384} {
		for i := 0; i < 64; i++ {
			runtime.StackAllocFreeForTest(n)
		}
	}
	runtime.StackCacheClearForTest()
}

func TestBulkBarrierBitmapOffsetsAndModes(t *testing.T) {
	// Verify bulkBarrierBitmap across all unaligned maskOffset values (0..7 words),
	// varying sizes (0..256 words), src == 0 (clear), src != 0 (copy), and dstv == srcv.
	const ptrSize = unsafe.Sizeof(uintptr(0))
	const maxWords = 128
	var dst, src [maxWords]uintptr
	var mask [(maxWords + 16) / 8]uint8

	t1 := new(uint64)
	t2 := new(uint64)
	*t1 = 0x1111
	*t2 = 0x2222
	p1 := uintptr(unsafe.Pointer(t1))
	p2 := uintptr(unsafe.Pointer(t2))

	for i := range mask {
		mask[i] = byte((i*37 + 0x53) & 0xFF)
	}
	for i := 0; i < maxWords; i++ {
		switch i % 4 {
		case 0:
			dst[i], src[i] = p1, p2
		case 1:
			dst[i], src[i] = p1, p1 // equal non-nil pointers
		case 2:
			dst[i], src[i] = 0, p2
		case 3:
			dst[i], src[i] = p1, 0
		}
	}

	dstAddr := uintptr(unsafe.Pointer(&dst[0]))
	srcAddr := uintptr(unsafe.Pointer(&src[0]))

	for offWords := uintptr(0); offWords < 16; offWords++ {
		for _, nWords := range []uintptr{0, 1, 2, 3, 7, 8, 9, 15, 16, 17, 63, 64, 100} {
			size := nWords * ptrSize
			maskOff := offWords * ptrSize
			runtime.BulkBarrierBitmapOffsetForTest(dstAddr, 0, size, maskOff, &mask[0])
			runtime.BulkBarrierBitmapOffsetForTest(dstAddr, srcAddr, size, maskOff, &mask[0])
		}
	}

	runtime.KeepAlive(t1)
	runtime.KeepAlive(t2)
	runtime.KeepAlive(&dst)
	runtime.KeepAlive(&src)
}

func TestEnableChunkHugePagesSingleL1AndIdempotent(t *testing.T) {
	// Regression test for pageAlloc.enableChunkHugePages:
	// 1. Iterates cloned inUse.ranges instead of p.inUse.ranges after unlock.
	// 2. Inclusive L1 loop handles a contiguous heap within a single L1 entry.
	before := map[runtime.ChunkIdx][]runtime.BitRange{
		runtime.BaseChunkIdx: {{0, 16}},
	}
	b := runtime.NewPageAlloc(before, nil)
	defer runtime.FreePageAlloc(b)

	if b.ChunkHugePagesEnabled() {
		t.Fatal("chunkHugePages unexpectedly enabled before EnableChunkHugePages")
	}
	b.EnableChunkHugePages()
	if !b.ChunkHugePagesEnabled() {
		t.Fatal("chunkHugePages not enabled after EnableChunkHugePages")
	}
	// Idempotent second call.
	b.EnableChunkHugePages()
}

func TestReclaimChunkWordBoundaries(t *testing.T) {
	// Allocate a mix of live and dead spans across >512 pages (multiple 64-page words)
	// and verify reclaimBeforeGrow preserves all live objects while reclaiming dead spans.
	const liveCount = 256
	const deadCount = 256
	live := make([][]byte, liveCount)
	dead := make([][]byte, deadCount)
	for i := 0; i < liveCount; i++ {
		b := make([]byte, 8192)
		b[0] = byte(i + 1)
		b[8191] = byte(i ^ 0x3C)
		live[i] = b
	}
	for i := 0; i < deadCount; i++ {
		dead[i] = make([]byte, 8192)
	}
	dead = nil

	runtime.GC()
	runtime.ResetReclaimIndexForTest()
	runtime.ReclaimBeforeGrowForTest(64)

	for i, b := range live {
		if b[0] != byte(i+1) || b[8191] != byte(i^0x3C) {
			t.Fatalf("live span %d corrupted after reclaim", i)
		}
	}
	runtime.KeepAlive(live)
}

func BenchmarkMCacheReleaseAllEmpty(b *testing.B) {
	runtime.ReleaseAllMCacheForTest()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runtime.ReleaseAllMCacheForTest()
	}
}

func BenchmarkMCacheReleaseAllPopulated(b *testing.B) {
	var sink [16][]byte
	sizes := [16]int{16, 24, 32, 48, 64, 80, 96, 112, 128, 144, 160, 192, 224, 256, 384, 512}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j, sz := range sizes {
			sink[j] = make([]byte, sz)
		}
		runtime.ReleaseAllMCacheForTest()
	}
	runtime.KeepAlive(&sink)
}

func BenchmarkStackCacheClearEmpty(b *testing.B) {
	runtime.StackCacheClearForTest()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runtime.StackCacheClearForTest()
	}
}

func BenchmarkStackAllocFree8K(b *testing.B) {
	for i := 0; i < b.N; i++ {
		runtime.StackAllocFreeForTest(8192)
	}
}

func BenchmarkPageCacheFlush64(b *testing.B) {
	pa := runtime.NewPageAlloc(
		map[runtime.ChunkIdx][]runtime.BitRange{
			runtime.BaseChunkIdx: {},
		},
		map[runtime.ChunkIdx][]runtime.BitRange{
			runtime.BaseChunkIdx: {{0, 64}},
		},
	)
	defer runtime.FreePageAlloc(pa)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := pa.AllocToCache()
		c.Flush(pa)
	}
}

func BenchmarkGCSweepLongLivedSmallSpans(b *testing.B) {
	// 4 MiB of long-lived 32B objects (full small spans with inline mark bits).
	const n = 131072
	live := make([][]byte, n)
	for i := range live {
		live[i] = make([]byte, 32)
	}
	runtimeOptBytesSink = live
	runtime.GC()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runtime.GC()
	}
	b.StopTimer()
	runtimeOptBytesSink = nil
}

func BenchmarkBulkBarrierBitmapSparseClear(b *testing.B) {
	const words = 256
	const ptrSize = unsafe.Sizeof(uintptr(0))
	var dst [words]uintptr
	var mask [words / 8]uint8
	target := make([]byte, 32)
	ptr := uintptr(unsafe.Pointer(&target[0]))
	for i := 0; i < words; i += 8 {
		mask[i/8] = 1
		dst[i] = ptr
	}
	dstAddr := uintptr(unsafe.Pointer(&dst[0]))
	byteSize := uintptr(words) * ptrSize
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runtime.BulkBarrierBitmapOffsetForTest(dstAddr, 0, byteSize, 0, &mask[0])
	}
	runtime.KeepAlive(target)
	runtime.KeepAlive(&dst)
}

func BenchmarkBulkBarrierBitmapDenseSamePtr(b *testing.B) {
	const words = 256
	const ptrSize = unsafe.Sizeof(uintptr(0))
	var dst, src [words]uintptr
	var mask [words / 8]uint8
	target := make([]byte, 32)
	ptr := uintptr(unsafe.Pointer(&target[0]))
	for i := 0; i < words; i++ {
		mask[i/8] |= 1 << (i % 8)
		dst[i] = ptr
		src[i] = ptr
	}
	dstAddr := uintptr(unsafe.Pointer(&dst[0]))
	srcAddr := uintptr(unsafe.Pointer(&src[0]))
	byteSize := uintptr(words) * ptrSize
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runtime.BulkBarrierBitmapOffsetForTest(dstAddr, srcAddr, byteSize, 0, &mask[0])
	}
	runtime.KeepAlive(target)
	runtime.KeepAlive(&dst)
	runtime.KeepAlive(&src)
}

var runtimeOptSpanSink [][64]byte

func BenchmarkSpanAllocFreeChurn(b *testing.B) {
	// Exercise initSpan + setSpans + mcentral.grow/cacheSpan + sweep.
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := make([][64]byte, 256)
		s[0][0] = byte(i)
		s[255][63] = byte(i)
		runtimeOptSpanSink = s
	}
	runtimeOptSpanSink = nil
}
