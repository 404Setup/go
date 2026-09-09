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

func TestO3LoopReorder(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(loopReorderSource), 0o666); err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, out []byte, enabled bool) {
		t.Helper()
		want := 0
		if enabled {
			want = 1
		}
		for _, msg := range []string{"Interchanged loops", "Tiled loops (32x32)"} {
			if got := bytes.Count(out, []byte(msg)); got != want {
				t.Fatalf("%s: got %d, want %d\n%s", msg, got, want, out)
			}
		}
	}
	for _, tt := range []struct {
		name, gcflags, ldflags string
		enabled                bool
	}{
		{"default", "", "", false},
		{"o2", "-o2", "", false},
		{"o3", "-o3", "", true},
		{"linker", "", "-o3", true},
		{"disabled", "-o3 -o3=false", "", false},
		{"override", "-o3=false", "-o3", false},
		{"no-opt", "-o3 -N", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			exe := filepath.Join(dir, tt.name+".exe")
			flags := "-d=loopreorder=1,ssa/check/on " + tt.gcflags
			out, err := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+flags, "-ldflags="+tt.ldflags, "-o", exe, src).CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			check(t, out, tt.enabled)
			if out, err := testenv.Command(t, exe).CombinedOutput(); err != nil || string(out) != "ok\n" {
				t.Fatalf("execution: %v\n%s", err, out)
			}
		})
	}
	for _, arch := range []string{"amd64", "386", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			cmd := testenv.Command(t, testenv.GoToolPath(t), "tool", "compile", "-o3", "-d=loopreorder=1,ssa/check/on", "-o", filepath.Join(dir, arch+".o"), src)
			cmd.Env = append(cmd.Environ(), "GOARCH="+arch)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("compile: %v\n%s", err, out)
			}
			check(t, out, true)
		})
	}
}

const loopReorderSource = `package main

//go:noinline
func interchange() (a [65][67]int) {
	for j := 3; j < 67; j++ {
		for i := 2; i < 65; i++ {
			a[i][j] = i*100 + j
			a[i][j] += a[i][j]
		}
	}
	return
}

//go:noinline
func transpose(b [67][65]int) (a [65][67]int) {
	for i := 1; i < 65; i++ {
		for j := 2; j < 67; j++ { a[i][j] = b[j][i] }
	}
	return
}

//go:noinline
func dependent(a [65][65]int) [65][65]int {
	for j := 0; j < 65; j++ {
		for i := 0; i < 65; i++ { a[i][j] = a[j][i] + 1 }
	}
	return a
}

//go:noinline
func pointers(a, b *[65][65]int) {
	for j := 0; j < 65; j++ {
		for i := 0; i < 65; i++ { a[i][j] = b[j][i] + 1 }
	}
}

//go:noinline
func addressed() (a [65][65]int, p *[65][65]int) {
	p = &a
	for j := 0; j < 65; j++ {
		for i := 0; i < 65; i++ { a[i][j] = i+j }
	}
	return
}

//go:noinline
func offset(a [65][65]int) [65][65]int {
	for j := 0; j < 65; j++ {
		for i := 1; i < 65; i++ { a[i][j] = a[i-1][j] + 1 }
	}
	return a
}

//go:noinline
func reduction() (a [65][65]int, sum int) {
	for j := 0; j < 65; j++ {
		for i := 0; i < 65; i++ { sum++; a[i][j] = sum }
	}
	return
}

//go:noinline
func bounds() (a [65][65]int) {
	for j := 0; j < 66; j++ {
		for i := 0; i < 65; i++ { a[i][j] = i+j }
	}
	return
}

//go:noinline
func division() (a [65][65]int) {
	for j := 0; j < 65; j++ {
		for i := 0; i < 65; i++ { a[i][j] = i/(j-1) }
	}
	return
}

//go:noinline
func observableIndex() (a [65][65]int, j int) {
	for j = 0; j < 65; j++ {
		for i := 0; i < 65; i++ { a[i][j] = i+j }
	}
	return
}

//go:noinline
func triangular() (a [65][65]int) {
	for j := 0; j < 65; j++ {
		for i := 0; i < j; i++ { a[i][j] = i+j }
	}
	return
}

var calls int
//go:noinline
func effect(i, j int) int { calls++; return i+j }
//go:noinline
func call() (a [65][65]int) {
	for j := 0; j < 65; j++ {
		for i := 0; i < 65; i++ { a[i][j] = effect(i,j) }
	}
	return
}

//go:noinline
func empty() (a [65][65]int) {
	for j := 0; j < 0; j++ {
		for i := 0; i < 65; i++ { a[i][j] = i+j }
	}
	return
}

//go:noinline
func captured() (a [65][65]int, last func() int) {
	for j := 0; j < 65; j++ {
		for i := 0; i < 65; i++ { a[i][j] = i+j; last = func() int { return i+j } }
	}
	return
}

func mustPanic(f func()) {
	defer func() { if recover() == nil { panic("missing panic") } }()
	f()
}

func main() {
	a := interchange()
	var b [67][65]int
	for j := range b { for i := range b[j] { b[j][i] = i*100+j } }
	c := transpose(b)
	for i := range a { for j := range a[i] {
		want := 0
		if i >= 2 && j >= 3 { want = 2*(i*100+j) }
		if a[i][j] != want { panic("interchange") }
		want = 0
		if i >= 1 && j >= 2 { want = i*100+j }
		if c[i][j] != want { panic("tile tail") }
	} }
	var d [65][65]int
	e := dependent(d)
	pointers(&d, &d)
	for j := range d { for i := range d {
		want := 1
		if i < j { want = 2 }
		if d[i][j] != want || e[i][j] != want { panic("dependence") }
	} }
	e = offset([65][65]int{})
	r, sum := reduction()
	o, index := observableIndex()
	u := triangular()
	v := call()
	w, last := captured()
	x, ptr := addressed()
	if x != *ptr { panic("addressed array") }
	if sum != 65*65 || index != 65 || calls != 65*65 || last() != 128 { panic("side effect") }
	if empty() != [65][65]int{} { panic("empty") }
	for i := range e { for j := range e[i] {
		if e[i][j] != i || r[i][j] != j*65+i+1 || o[i][j] != i+j || v[i][j] != i+j || w[i][j] != i+j || x[i][j] != i+j { panic("order") }
		want := 0
		if i < j { want = i+j }
		if u[i][j] != want { panic("triangular") }
	} }
	mustPanic(func() { bounds() })
	mustPanic(func() { division() })
	println("ok")
}
`

// Run with -gcflags=cmd/compile/internal/test=-o2 and then -o3 to compare.
var loopReorderSink [512][512]int

//go:noinline
func loopReorderColumns() (a [512][512]int) {
	for j := 0; j < 512; j++ {
		for i := 0; i < 512; i++ {
			a[i][j] = i + j
		}
	}
	return
}

//go:noinline
func loopReorderTranspose(b [512][512]int) (a [512][512]int) {
	for i := 0; i < 512; i++ {
		for j := 0; j < 512; j++ {
			a[i][j] = b[j][i]
		}
	}
	return
}

func BenchmarkO3LoopInterchange(b *testing.B) {
	for b.Loop() {
		loopReorderSink = loopReorderColumns()
	}
}

func BenchmarkO3LoopTiling(b *testing.B) {
	for b.Loop() {
		loopReorderSink = loopReorderTranspose(loopReorderSink)
	}
}
