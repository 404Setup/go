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

func TestO2DeadCode(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(o2Source+o2LoopSource), 0o666); err != nil {
		t.Fatal(err)
	}
	goTool := testenv.GoToolPath(t)
	for _, tt := range []struct {
		name       string
		ldflags    string
		gcflags    string
		wantSymbol bool
	}{
		{"default", "", "", true},
		{"enabled", "-o2", "", false},
		{"disabled", "-o2=false", "", true},
		{"last-wins", "-o2 -o2=false", "", true},
		{"no-opt", "-o2", "-N", true},
		{"compiler-override", "-o2", "-o2=false", true},
		{"compiler-enabled", "", "-o2", false},
		{"no-inline", "-o2", "-l", false},
		{"combined", "-o2 -fmth", "", false},
		{"o3-inherits", "-o3", "", false},
		{"o3-inherits-disabled-o2", "-o3 -o2=false", "", false},
		{"o3-compiler-enabled", "", "-o3", false},
		{"o3-disabled-keeps-o2", "-o2 -o3=false", "", false},
		{"o3-no-opt", "-o3", "-N", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dst := filepath.Join(dir, tt.name+".exe")
			flags := "-d=ssa/check/on,ssa/o2_loop_unroll/debug=1 " + tt.gcflags
			cmd := testenv.Command(t, goTool, "build", "-ldflags="+tt.ldflags, "-gcflags="+flags, "-o", dst, src)
			buildOut, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, buildOut)
			}
			for _, diagnostic := range []string{"Unrolled loop 8 times (full=true)", "Unrolled loop 4 times (full=false)", "Unrolled loop 2 times (full=false)"} {
				if got := bytes.Contains(buildOut, []byte(diagnostic)); got == tt.wantSymbol {
					t.Errorf("%q present = %v; want %v\n%s", diagnostic, got, !tt.wantSymbol, buildOut)
				}
			}
			out, err := testenv.Command(t, goTool, "tool", "nm", dst).CombinedOutput()
			if err != nil {
				t.Fatalf("nm failed: %v\n%s", err, out)
			}
			if hasSymbol := bytes.Contains(out, []byte(" main.o2Unreachable")); hasSymbol != tt.wantSymbol {
				t.Errorf("unreachable function present = %v; want %v", hasSymbol, tt.wantSymbol)
			}
			if out, err := testenv.Command(t, dst).CombinedOutput(); err != nil || string(out) != "ok\n" {
				t.Fatalf("incorrect execution: %v\n%s", err, out)
			}
		})
	}
}

const o2Source = `package main

var sink int

//go:noinline
func o2Unreachable() { panic("unreachable") }

//go:noinline
func sideEffect() { sink++ }

//go:noinline
func lateConstants(n, x int) {
	a := 1
	for i := 0; i < n; i++ {
		a = 2 - a
	}
	// SCCP discovers that a is always 1 after the first prove pass.
	if x < a {
		sideEffect()
		if x >= 1 {
			o2Unreachable()
		}
	}
}

//go:noinline
func read(p *int) { _ = *p }

//go:noinline
func divide(x, y int) { _ = x / y }

//go:noinline
func index(s []int, i int) { _ = s[i] }

func mustPanic(f func()) {
	defer func() {
		if recover() == nil {
			panic("missing panic")
		}
	}()
	f()
}

func main() {
	checkLoops()
	for _, n := range []int{-1, 0, 1, 2, 8} {
		for _, x := range []int{-2, -1, 0, 1, 2, 100} {
			lateConstants(n, x)
		}
	}
	if sink != 15 {
		panic("incorrect side effects")
	}
	mustPanic(func() { read(nil) })
	mustPanic(func() { divide(1, 0) })
	mustPanic(func() { index(nil, 0) })
	println("ok")
}
`

const o2LoopSource = `
//go:noinline
func fixed(x int) (int, int, int) {
	a, b := x, 3
	i := 0
	for ; i < 8; i++ {
		a, b = b, a + i
	}
	return a, b, i
}

//go:noinline
func referenceFixed(x, n int) (int, int) {
	a, b := x, 3
	for i := 0; i < n; i++ { a, b = b, a + i }
	return a, b
}

//go:noinline
func partial4(x int) int {
	for i := 0; i < 100; i++ { x = x*3 + i }
	return x
}

//go:noinline
func partial2(x int) int {
	for i := 0; i < 34; i++ { x = x*3 + i }
	return x
}

//go:noinline
func reference(x, n int) int {
	for i := 0; i < n; i++ { x = x*3 + i }
	return x
}

//go:noinline
func downward(x int) int {
	for i := int8(20); i >= -1; i -= 3 { x = x*3 + int(i) }
	return x
}

//go:noinline
func referenceDownward(x, n int) int {
	for i := n; i >= -1; i -= 3 { x = x*3 + i }
	return x
}

//go:noinline
func stores(p *[8]int, x int) {
	for i := 0; i < 8; i++ { p[i] = x + i }
}

//go:noinline
func mutate(p *int) {
	for i := 0; i < 100; i++ { *p = *p*3 + i }
}

//go:noinline
func swap(a, b int) (int, int) {
	for i := 0; i < 7; i++ { a, b = b, a }
	return a, b
}

//go:noinline
func empty(x int) int {
	for i := 4; i < 0; i++ { x++ }
	return x
}

//go:noinline
func wrapping(x int) int {
	// This loop relies on int8 wrapping before its condition becomes false.
	for i := int8(120); i > 0; i += 3 { x = x*3 + int(i) }
	return x
}

func checkLoops() {
	for x := -10; x <= 10; x++ {
		a, b, i := fixed(x)
		wantA, wantB := referenceFixed(x, 8)
		if a != wantA || b != wantB || i != 8 { panic("full unroll") }
		if partial4(x) != reference(x, 100) || partial2(x) != reference(x, 34) { panic("partial unroll") }
		if downward(x) != referenceDownward(x, 20) { panic("downward unroll") }
		var buf [8]int
		stores(&buf, x)
		for j, v := range buf { if v != x+j { panic("unrolled store") } }
		v := x
		mutate(&v)
		if v != reference(x, 100) { panic("partial memory chain") }
		a, b = swap(x, 3)
		if a != 3 || b != x { panic("swapped phis") }
		if empty(x) != x { panic("empty loop") }
		if wrapping(x) != ((x*3+120)*3+123)*3+126 { panic("wrapping loop") }
	}
	mustPanic(func() { stores(nil, 1) })
}
`
