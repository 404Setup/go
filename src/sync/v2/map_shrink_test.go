// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import "testing"

func TestMapShrinkReplacesStorage(t *testing.T) {
	var m Map[int, int]
	for i := range 256 {
		m.Store(i, i)
	}
	for i := range 240 {
		m.Delete(i)
	}
	before := m.current.Load()
	if before == nil {
		t.Fatal("Map has no storage after Store")
	}

	m.Shrink()
	after := m.current.Load()
	if after == nil || after == before {
		t.Fatalf("Map storage after Shrink = %p; want a non-nil replacement for %p", after, before)
	}

	m.Clear()
	m.Shrink()
	if current := m.current.Load(); current != nil {
		t.Fatalf("empty Map storage after Shrink = %p; want nil", current)
	}
}
