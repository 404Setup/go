// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"bytes"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestFastMath(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(fastMathSource), 0o666); err != nil {
		t.Fatal(err)
	}
	goTool := testenv.GoToolPath(t)
	for _, tt := range []struct {
		name, ldflags, gcflags string
		fast                   bool
	}{
		{"default", "", "", false},
		{"enabled", "-fmth", "", true},
		{"disabled", "-fmth=false", "", false},
		{"last-wins", "-fmth -fmth=false", "", false},
		{"double-dash", "--fmth=true", "", true},
		{"no-opt", "-fmth", "-N", false},
		{"compiler-override", "-fmth", "-fmth=false", false},
		{"combined", "-o2 -fmth", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dst := filepath.Join(dir, tt.name+".exe")
			cmd := testenv.Command(t, goTool, "build", "-ldflags="+tt.ldflags, "-gcflags=-d=ssa/check/on "+tt.gcflags, "-o", dst, src)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			flush := (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") && (tt.fast || tt.ldflags == "-fmth")
			if out, err := testenv.Command(t, dst, strconv.FormatBool(tt.fast), strconv.FormatBool(flush)).CombinedOutput(); err != nil || string(out) != "ok\n" {
				t.Fatalf("execution: %v\n%s", err, out)
			}
			if runtime.GOARCH != "amd64" || (tt.name != "default" && tt.name != "enabled") {
				return
			}
			for _, fn := range []string{"quotient32", "quotient64", "shared32", "shared64", "squareRoot64"} {
				out, err := testenv.Command(t, goTool, "tool", "objdump", "-s", "main\\."+fn+"$", dst).CombinedOutput()
				if err != nil {
					t.Fatalf("objdump: %v\n%s", err, out)
				}
				want := 1
				if strings.HasPrefix(fn, "shared") {
					want = 2
				}
				if tt.fast {
					want--
				}
				instruction := "DIVS"
				if fn == "squareRoot64" {
					instruction = "SQRT"
				}
				if got := bytes.Count(out, []byte(instruction)); len(out) == 0 || got != want {
					t.Errorf("%s has %d %s instructions; want %d\n%s", fn, got, instruction, want, out)
				}
			}
		})
	}
}

func TestFastMathContraction(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "fma.go")
	const source = `package probe
func fused64(a, b, c float64) float64 { return float64(a*b) + c }
func fused32(a, b, c float32) float32 { return float32(a*b) + c }
`
	if err := os.WriteFile(src, []byte(source), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		cmd := testenv.Command(t, testenv.GoToolPath(t), "tool", "compile", "-S", "-fmth="+strconv.FormatBool(enabled), "-o", filepath.Join(dir, "fma.o"), src)
		cmd.Env = append(cmd.Environ(), "GOARCH=amd64", "GOAMD64=v3")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("compile: %v\n%s", err, out)
		}
		for _, instruction := range []string{"VFMADD231SD", "VFMADD231SS"} {
			if bytes.Contains(out, []byte(instruction)) != enabled {
				t.Errorf("%s present != %v\n%s", instruction, enabled, out)
			}
		}
	}
}

const fastMathSource = `package main

import (
	"math"
	"os"
)

//go:noinline
func add32(x float32) float32 { return x + 0 }
//go:noinline
func add64(x float64) float64 { return x + 0 }
//go:noinline
func self32(x float32) float32 { return x / x }
//go:noinline
func self64(x float64) float64 { return x / x }
//go:noinline
func sub32(x float32) float32 { return x - x }
//go:noinline
func sub64(x float64) float64 { return x - x }
//go:noinline
func mul32(x float32) float32 { return x * 0 }
//go:noinline
func mul64(x float64) float64 { return x * 0 }
//go:noinline
func equal32(x float32) bool { return x == x }
//go:noinline
func equal64(x float64) bool { return x == x }
//go:noinline
func reassoc32(x float32) float32 { return (x + 1e20) + -1e20 }
//go:noinline
func reassoc64(x float64) float64 { return (x + 1e20) + -1e20 }
//go:noinline
func cancel32(x, y float32) float32 { return (x + y) - y }
//go:noinline
func cancel64(x, y float64) float64 { return (x + y) - y }
//go:noinline
func quotient32(x float32) float32 { return x / 3 }
//go:noinline
func quotient64(x float64) float64 { return x / 3 }
//go:noinline
func shared32(a, b, d float32) (float32, float32) { return a / d, b / d }
//go:noinline
func shared64(a, b, d float64) (float64, float64) { return a / d, b / d }
//go:noinline
func tinyDivisor(x float64) float64 { return x / 1e-320 }
//go:noinline
func rootSquare64(x float64) float64 { return math.Sqrt(x*x) }
//go:noinline
func squareRoot64(x float64) float64 { s := math.Sqrt(x); return s*s }
//go:noinline
func sideEffect(x float64) float64 { calls++; return x }

var calls int

func check(ok bool) { if !ok { panic("incorrect fast-math result") } }

func main() {
	fast := os.Args[1] == "true"
	negzero := math.Copysign(0, -1)
	check(math.Signbit(add64(negzero)) == fast)
	check((math.Float32bits(add32(float32(negzero))) >> 31 != 0) == fast)
	check((self64(0) == 1) == fast)
	check((self32(0) == 1) == fast)
	check((sub64(math.Inf(1)) == 0) == fast)
	check((sub32(float32(math.Inf(1))) == 0) == fast)
	check((mul64(math.Inf(1)) == 0) == fast)
	check((mul32(float32(math.Inf(1))) == 0) == fast)
	check(equal64(math.NaN()) == fast)
	check(equal32(float32(math.NaN())) == fast)
	check((reassoc64(3) == 3) == fast)
	check((reassoc32(3) == 3) == fast)
	check((cancel64(3, 1e20) == 3) == fast)
	check((cancel32(3, 1e20) == 3) == fast)
	check(quotient32(6) == 2 && quotient64(6) == 2)
	a32, b32 := shared32(6, 12, 3)
	a64, b64 := shared64(6, 12, 3)
	check(a32 == 2 && b32 == 4 && a64 == 2 && b64 == 4)
	if os.Args[2] == "true" {
		bits := math.Float64bits(tinyDivisor(1e-320))
		check(bits&0x7ff0000000000000 == 0x7ff0000000000000 && bits&0xfffffffffffff != 0)
	} else {
		check(tinyDivisor(1e-320) == 1)
	}
	check(rootSquare64(-3) == 3 && squareRoot64(4) == 4)
	check(mul64(sideEffect(7)) == 0 && calls == 1)
	println("ok")
}
`
