// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"bytes"
	"internal/testenv"
	"os"
	"path/filepath"
	"testing"
)

func TestO3DeadCode(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(o3Source), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, ldflags, gcflags string
		removed                bool
	}{
		{"o2", "-o2", "", false},
		{"o3", "-o3", "", true},
		{"compiler", "", "-o3", true},
		{"disabled", "-o3 -o3=false", "", false},
		{"override", "-o3", "-o3=false", false},
		{"no-opt", "-o3", "-N", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dst := filepath.Join(dir, tt.name+".exe")
			flags := "-d=ssa/check/on,ssa/o3_deadcode/debug=1 " + tt.gcflags
			out, err := testenv.Command(t, testenv.GoToolPath(t), "build", "-ldflags="+tt.ldflags, "-gcflags="+flags, "-o", dst, src).CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, out)
			}
			count := bytes.Count(out, []byte("Removed dead loop"))
			if tt.removed && count < 5 || !tt.removed && count != 0 {
				t.Fatalf("removed %d loops; optimization enabled = %v\n%s", count, tt.removed, out)
			}
			if out, err := testenv.Command(t, dst).CombinedOutput(); err != nil || string(out) != "ok\n" {
				t.Fatalf("incorrect execution: %v\n%s", err, out)
			}
		})
	}
}

const o3Source = `package main

var sink int

//go:noinline
func dead(n, x int) int {
	for i := 0; i < n; i++ { x = x*3 + i }
	return n
}

//go:noinline
func branches(n, x int) int {
	for i := 0; i < n; i++ {
		if x & 1 == 0 { x += i } else { x = x*3 - i }
	}
	return n
}

//go:noinline
func nested(n, x int) int {
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ { x = x*3 + j }
	}
	return n
}

//go:noinline
func down(n, x int) int {
	for i := n; i > 0; i-- { x += i }
	return n
}

//go:noinline
func live(n int) int {
	x := 0
	for i := 0; i < n; i++ { x += i }
	return x
}

//go:noinline
func stores(n int) {
	for i := 0; i < n; i++ { sink += i }
}

//go:noinline
func calls(n int) {
	for i := 0; i < n; i++ { stores(i) }
}

//go:noinline
func read(n int, p *int) {
	for i := 0; i < n; i++ { _ = *p }
}

//go:noinline
func divide(n, d int) {
	for i := 0; i < n; i++ { _ = i / d }
}

//go:noinline
func index(n int, s []int) {
	for i := 0; i < n; i++ { _ = s[i] }
}

//go:noinline
func changing(n int) int {
	for i := 0; i < n; i++ { n-- }
	return n
}

func mustPanic(f func()) {
	defer func() { if recover() == nil { panic("missing panic") } }()
	f()
}

func main() {
	for _, n := range []int{-1, 0, 1, 2, 17, 100} {
		if dead(n, 7) != n || branches(n, 7) != n || nested(n, 7) != n || down(n, 7) != n {
			panic("dead loop result")
		}
		want := 0
		if n > 0 { want = n*(n-1)/2 }
		if live(n) != want { panic("live loop result") }
		sink = 0
		stores(n)
		if sink != want { panic("lost store") }
	}
	sink = 0
	calls(4)
	if sink != 4 { panic("lost call") }
	if changing(10) != 5 { panic("changing bound") }
	read(0, nil)
	divide(0, 0)
	index(0, nil)
	mustPanic(func() { read(1, nil) })
	mustPanic(func() { divide(1, 0) })
	mustPanic(func() { index(1, nil) })
	println("ok")
}
`
