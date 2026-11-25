// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package list_test

import (
	"container/list"
	"runtime"
	"testing"
	"weak"
)

func mustPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("%s: expected panic, got none", name)
		}
	}()
	fn()
}

func TestToImmutableList(t *testing.T) {
	var nilList *list.List
	if got := list.ToImmutable(nilList); got != nil {
		t.Fatalf("ToImmutable(nil) = %v, want nil", got)
	}
	if list.IsImmutable(nilList) || nilList.IsImmutable() {
		t.Fatal("IsImmutable(nil) = true, want false")
	}

	empty := list.New()
	immEmpty := list.ToImmutable(empty)
	if !list.IsImmutable(immEmpty) || immEmpty.Len() != 0 || immEmpty.Front() != nil || immEmpty.Back() != nil {
		t.Fatalf("unexpected empty immutable list: len=%d", immEmpty.Len())
	}
	if list.IsImmutable(empty) {
		t.Fatal("original empty list became immutable")
	}

	src := list.New()
	src.PushBack("a")
	src.PushBack("b")
	src.PushBack("c")

	imm := list.ToImmutable(src)
	if !imm.IsImmutable() || !list.IsImmutable(imm) || imm.Len() != 3 {
		t.Fatalf("imm invalid: imm=%v len=%d", imm.IsImmutable(), imm.Len())
	}

	// Mutating original must not affect frozen copy.
	src.Front().Value = "mutated"
	src.PushBack("d")
	if imm.Len() != 3 || imm.Front().Value != "a" || imm.Back().Value != "c" {
		t.Fatalf("immutable list changed after mutating source")
	}

	// Forward and backward traversal.
	var got []string
	for e := imm.Front(); e != nil; e = e.Next() {
		got = append(got, e.Value.(string))
	}
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("forward traversal = %v", got)
	}

	// Idempotent and zero-alloc on already-immutable list.
	if imm.ToImmutable() != imm || list.ToImmutable(imm) != imm {
		t.Fatal("ToImmutable not idempotent")
	}
	if allocs := testing.AllocsPerRun(50, func() {
		_ = list.ToImmutable(imm)
	}); allocs != 0 {
		t.Fatalf("ToImmutable(imm) allocs = %v, want 0", allocs)
	}

	// Reading into a mutable list via PushBackList is allowed.
	dst := list.New()
	dst.PushBackList(imm)
	if dst.Len() != 3 || dst.IsImmutable() {
		t.Fatalf("PushBackList from immutable failed: len=%d", dst.Len())
	}

	front := imm.Front()
	back := imm.Back()
	mustPanic(t, "Init", func() { imm.Init() })
	mustPanic(t, "Remove", func() { imm.Remove(front) })
	mustPanic(t, "PushFront", func() { imm.PushFront("x") })
	mustPanic(t, "PushBack", func() { imm.PushBack("x") })
	mustPanic(t, "InsertBefore", func() { imm.InsertBefore("x", front) })
	mustPanic(t, "InsertAfter", func() { imm.InsertAfter("x", front) })
	mustPanic(t, "MoveToFront", func() { imm.MoveToFront(back) })
	mustPanic(t, "MoveToBack", func() { imm.MoveToBack(front) })
	mustPanic(t, "MoveBefore", func() { imm.MoveBefore(back, front) })
	mustPanic(t, "MoveAfter", func() { imm.MoveAfter(front, back) })
	mustPanic(t, "PushBackList", func() { imm.PushBackList(src) })
	mustPanic(t, "PushFrontList", func() { imm.PushFrontList(src) })
}

func TestImmutableListGCReclaimed(t *testing.T) {
	var wp weak.Pointer[list.List]
	func() {
		l := list.New()
		for i := range 64 {
			l.PushBack(i)
		}
		imm := list.ToImmutable(l)
		wp = weak.Make(imm)
	}()
	for range 4 {
		runtime.GC()
		if wp.Value() == nil {
			return
		}
	}
	t.Fatal("immutable *list.List was not reclaimed by GC")
}
