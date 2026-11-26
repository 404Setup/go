// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"context"
	"fmt"
	"internal/race"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestStreamContextLifecycle(t *testing.T) {
	type ctxKey struct{}
	deadline := time.Now().Add(time.Minute)
	base, baseCancel := context.WithDeadline(context.WithValue(context.Background(), ctxKey{}, "val"), deadline)
	defer baseCancel()

	// Case 1: Done() never called before cancel.
	sc1 := &streamContext{base: base}
	if d, ok := sc1.Deadline(); !ok || !d.Equal(deadline) {
		t.Fatalf("Deadline() = (%v, %v), want (%v, true)", d, ok, deadline)
	}
	if v := sc1.Value(ctxKey{}); v != "val" {
		t.Fatalf("Value() = %v, want val", v)
	}
	if err := sc1.Err(); err != nil {
		t.Fatalf("Err() before cancel = %v, want nil", err)
	}
	sc1.cancel()
	sc1.cancel() // idempotent
	if err := sc1.Err(); err != context.Canceled {
		t.Fatalf("Err() after cancel = %v, want context.Canceled", err)
	}
	ch1 := sc1.Done()
	ch2 := sc1.Done()
	if ch1 != ch2 {
		t.Fatalf("successive Done() channels differ after cancel")
	}
	select {
	case <-ch1:
	default:
		t.Fatal("Done() channel not closed after cancel")
	}
	if allocs := testing.AllocsPerRun(100, func() {
		_ = sc1.Done()
		_ = sc1.Err()
	}); allocs != 0 {
		t.Fatalf("post-cancel Done()/Err() allocs = %v, want 0", allocs)
	}

	// Case 2: Done() called before cancel.
	sc2 := &streamContext{base: base}
	chBefore := sc2.Done()
	select {
	case <-chBefore:
		t.Fatal("Done() closed before cancel")
	default:
	}
	sc2.cancel()
	select {
	case <-chBefore:
	default:
		t.Fatal("Done() channel not closed by cancel")
	}
	if sc2.Done() != chBefore {
		t.Fatal("Done() returned a different channel after cancel")
	}

	// Case 3: Parent base context canceled.
	pBase, pCancel := context.WithCancel(context.Background())
	sc3 := &streamContext{base: pBase}
	chParent := sc3.Done()
	pCancel()
	select {
	case <-chParent:
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not close streamContext.Done()")
	}
	if sc3.Err() == nil {
		t.Fatal("Err() = nil after parent cancel")
	}
}

func TestStreamContextConcurrentRace(t *testing.T) {
	base, baseCancel := context.WithCancel(context.Background())
	defer baseCancel()

	for range 100 {
		sc := &streamContext{base: base}
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				switch id % 3 {
				case 0:
					<-sc.Done()
				case 1:
					_ = sc.Err()
					_ = sc.Done()
				case 2:
					sc.cancel()
				}
			}(i)
		}
		wg.Wait()
	}
}

func TestH2WorkerPoolAndSynctest(t *testing.T) {
	const total = h2WorkerPoolShards*h2WorkerPoolMaxPerShard + 64
	var done atomic.Int32
	var wg sync.WaitGroup
	wg.Add(total)
	for range total {
		workerPool.Go(h2Task{
			kind: h2TaskFunc,
			fn: func() {
				defer wg.Done()
				done.Add(1)
			},
		})
	}
	wg.Wait()
	if got := done.Load(); got != total {
		t.Fatalf("done = %d, want %d", got, total)
	}

	synctest.Test(t, func(t *testing.T) {
		var ran atomic.Bool
		ch := make(chan struct{})
		workerPool.Go(h2Task{
			kind: h2TaskFunc,
			fn: func() {
				ran.Store(true)
				close(ch)
			},
		})
		<-ch
		if !ran.Load() {
			t.Fatal("goroutine did not run in synctest bubble")
		}
	})
}

func TestClientConnReadLoopCanonHeaderCacheBounded(t *testing.T) {
	rl := &clientConnReadLoop{}
	for i := range 500 {
		wire := fmt.Sprintf("x-custom-header-%d", i)
		got := rl.canonicalHeader(wire)
		want := fmt.Sprintf("X-Custom-Header-%d", i)
		if got != want {
			t.Fatalf("canonicalHeader(%q) = %q, want %q", wire, got, want)
		}
	}
	if rl.canonHeaderKeysSize > maxClientCachedCanonicalHeadersKeysSize {
		t.Fatalf("canonHeaderKeysSize = %d, exceeds limit %d", rl.canonHeaderKeysSize, maxClientCachedCanonicalHeadersKeysSize)
	}
	// Cached custom header lookup must not allocate.
	if allocs := testing.AllocsPerRun(100, func() {
		if got := rl.canonicalHeader("x-custom-header-0"); got != "X-Custom-Header-0" {
			t.Fatalf("unexpected cached canonical header %q", got)
		}
	}); allocs != 0 {
		t.Fatalf("cached canonicalHeader allocs = %v, want 0", allocs)
	}
}

func BenchmarkStreamContextNoDone(b *testing.B) {
	base := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		sc := &streamContext{base: base}
		sc.cancel()
	}
}

func BenchmarkClientConnReadLoopCanonHeaderCached(b *testing.B) {
	rl := &clientConnReadLoop{}
	keys := make([]string, 32)
	for i := range keys {
		keys[i] = fmt.Sprintf("x-bench-header-%d", i)
		_ = rl.canonicalHeader(keys[i])
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, k := range keys {
			_ = rl.canonicalHeader(k)
		}
	}
}

func TestLazySignalLifecycleAndRace(t *testing.T) {
	// 1. Close before chan_(): never allocates a channel and returns closedStreamChan.
	var s1 lazySignal
	if s1.isClosed() {
		t.Fatal("fresh lazySignal reported closed")
	}
	s1.close()
	s1.close() // idempotent
	if !s1.isClosed() {
		t.Fatal("closed lazySignal reported not closed")
	}
	if s1.ch != nil {
		t.Fatal("close() allocated channel unnecessarily")
	}
	select {
	case <-s1.chan_():
	default:
		t.Fatal("chan_() after close() not immediately readable")
	}
	if allocs := testing.AllocsPerRun(100, func() {
		if !s1.isClosed() {
			t.Fatal("not closed")
		}
		select {
		case <-s1.chan_():
		default:
			t.Fatal("not readable")
		}
	}); allocs != 0 {
		t.Fatalf("closed lazySignal check allocs = %v, want 0", allocs)
	}
	if !race.Enabled {
		var s lazySignal
		if allocs := testing.AllocsPerRun(100, func() {
			s.closed.Store(false)
			s.close()
			if !s.isClosed() {
				t.Fatal("not closed")
			}
			select {
			case <-s.chan_():
			default:
				t.Fatal("not readable")
			}
		}); allocs != 0 {
			t.Fatalf("lazySignal close+check allocs = %v, want 0", allocs)
		}
	}

	// 2. chan_() before close(): unblocks waiters once closed.
	var s2 lazySignal
	ch := s2.chan_()
	select {
	case <-ch:
		t.Fatal("open lazySignal channel already closed")
	default:
	}
	s2.close()
	select {
	case <-ch:
	default:
		t.Fatal("lazySignal channel not closed after close()")
	}

	// 3. Concurrent close()/chan_()/isClosed() under -race.
	for range 200 {
		var s lazySignal
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				switch id % 3 {
				case 0:
					s.close()
				case 1:
					<-s.chan_()
				case 2:
					_ = s.isClosed()
				}
			}(i)
		}
		wg.Wait()
	}
}

func BenchmarkLazySignalFastClose(b *testing.B) {
	var s lazySignal
	b.ReportAllocs()
	for b.Loop() {
		s.closed.Store(false)
		s.close()
		if !s.isClosed() {
			b.Fatal("expected closed")
		}
	}
}
