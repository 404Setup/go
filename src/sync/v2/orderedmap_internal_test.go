// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import "testing"

func TestOrderedMapShrinkCompactsStorage(t *testing.T) {
	var m OrderedMap[int, *int]
	for i := range 256 {
		value := i
		m.Store(i, &value)
	}
	for i := range 240 {
		m.Delete(i)
	}
	before := m.state.Load()
	if before == nil {
		t.Fatal("OrderedMap has no storage after Store")
	}
	entries := 0
	deleted := 0
	for entry := before.head.Load(); entry != nil; entry = entry.next.Load() {
		entries++
		if entry.value.Load() == nil {
			deleted++
		}
	}
	if entries != 256 || deleted != 240 {
		t.Fatalf("order chain before Shrink has %v entries (%v deleted); want 256 (240 deleted)", entries, deleted)
	}

	m.Shrink()
	after := m.state.Load()
	if after == nil || after == before {
		t.Fatalf("OrderedMap storage after Shrink = %p; want a non-nil replacement for %p", after, before)
	}
	entries = 0
	for entry := after.head.Load(); entry != nil; entry = entry.next.Load() {
		want := 240 + entries
		current := entry.value.Load()
		if current == nil || current.key != want || current.value == nil || *current.value != want {
			t.Fatalf("entry %v after Shrink has value %v; want key/value %v/%v", entries, current, want, want)
		}
		entries++
	}
	if entries != 16 {
		t.Fatalf("order chain after Shrink has %v entries; want 16", entries)
	}
}

func TestOrderedMapDeleteDropsKeyAndValueRecord(t *testing.T) {
	type key struct{ p *int }
	keyPointer := new(int)
	valuePointer := new(int)
	var m OrderedMap[key, *int]
	m.Store(key{keyPointer}, valuePointer)
	state := m.state.Load()
	entry, ok := state.index.Load(key{keyPointer})
	if !ok || entry.value.Load() == nil {
		t.Fatal("OrderedMap entry was not published")
	}

	m.Delete(key{keyPointer})
	if current := entry.value.Load(); current != nil {
		t.Fatalf("deleted entry retains key/value record %v", current)
	}
	if _, ok := state.index.Load(key{keyPointer}); ok {
		t.Fatal("deleted entry remains in hash index")
	}
}
