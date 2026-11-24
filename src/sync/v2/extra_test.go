// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync_test

import (
	"runtime/debug"
	"sync/atomic"
	syncv2 "sync/v2"
	"testing"
)

func TestMapLoadBeforeInitAndAfterShrinkRace(t *testing.T) {
	for iter := 0; iter < 32; iter++ {
		var m syncv2.Map[int, int]
		var stop atomic.Bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; !stop.Load(); i++ {
				if v, ok := m.Load(i & 31); ok && v < 0 {
					t.Errorf("unexpected negative value %d", v)
					return
				}
			}
		}()
		for i := 0; i < 64; i++ {
			m.Store(i&31, i)
		}
		m.Clear()
		m.Shrink()
		if v, ok := m.Load(1); ok || v != 0 {
			stop.Store(true)
			<-done
			t.Fatalf("Load after Clear+Shrink = (%d, %v), want (0, false)", v, ok)
		}
		m.Store(7, 42)
		if v, ok := m.Load(7); !ok || v != 42 {
			stop.Store(true)
			<-done
			t.Fatalf("Load after re-insert = (%d, %v), want (42, true)", v, ok)
		}
		stop.Store(true)
		<-done
	}
}

func TestPoolGetOKZeroValue(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	var p syncv2.Pool[int]
	if v, ok := p.GetOK(); ok || v != 0 {
		t.Fatalf("empty GetOK() = (%d, %v), want (0, false)", v, ok)
	}
	gotHit := false
	for i := 0; i < 64; i++ {
		p.Put(0)
		if v, ok := p.GetOK(); ok {
			if v != 0 {
				t.Fatalf("GetOK() after Put(0) = (%d, true), want 0", v)
			}
			gotHit = true
			break
		}
	}
	if !gotHit {
		t.Fatal("GetOK() never observed Put(0)")
	}
	p.New = func() int { return 0 }
	if v, ok := p.GetOK(); !ok || v != 0 {
		t.Fatalf("GetOK() with New returning 0 = (%d, %v), want (0, true)", v, ok)
	}
}

func BenchmarkPoolSharedChain(b *testing.B) {
	p := syncv2.NewPool(func() *int { return new(int) })
	var batch [16]*int
	for i := range batch {
		batch[i] = new(int)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range batch {
			p.Put(batch[j])
		}
		for j := range batch {
			batch[j] = p.Get()
		}
	}
}

func BenchmarkMapCompareAndSwapStruct(b *testing.B) {
	type val struct{ a, b, c, d uint64 }
	var m syncv2.Map[int, val]
	m.Store(1, val{1, 2, 3, 4})
	want := val{5, 6, 7, 8}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.CompareAndSwap(1, want, want)
	}
}
