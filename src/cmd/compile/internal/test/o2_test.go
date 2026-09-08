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
	if err := os.WriteFile(src, []byte(o2Source), 0o666); err != nil {
		t.Fatal(err)
	}
	goTool := testenv.GoToolPath(t)
	for _, tt := range []struct {
		name       string
		flags      string
		wantSymbol bool
	}{
		{"default", "", true},
		{"enabled", "-o2", false},
		{"disabled", "-o2=false", true},
		{"no-opt-first", "-N -o2", true},
		{"no-opt-last", "-o2 -N", true},
		{"no-inline", "-o2 -l", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dst := filepath.Join(dir, tt.name+".exe")
			flags := "-d=ssa/check/on " + tt.flags
			cmd := testenv.Command(t, goTool, "build", "-o2=false", "-gcflags="+flags, "-o", dst, src)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build failed: %v\n%s", err, out)
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
