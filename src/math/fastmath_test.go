// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package math_test

import (
	"math"
	"testing"
)

func TestFastMathApproximation(t *testing.T) {
	for _, tt := range []struct {
		name       string
		fast, full func(float64) float64
		lo, hi     float64
		tolerance  float64
		relative   bool
	}{
		{"Exp", math.FastExp, math.Exp, -700, 700, 8e-9, true},
		{"Exp2", math.FastExp2, math.Exp2, -1000, 1000, 8e-9, true},
		{"Sin", math.FastSin, math.Sin, -65536, 65536, 3e-9, false},
		{"Cos", math.FastCos, math.Cos, -65536, 65536, 3e-9, false},
		{"Tan", math.FastTan, math.Tan, -65536, 65536, 1e-7, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i <= 16384; i++ {
				x := tt.lo + (tt.hi-tt.lo)*float64(i)/16384
				got, want := tt.fast(x), tt.full(x)
				scale := 1 + math.Abs(want)
				if tt.relative {
					scale = math.Abs(want)
				}
				if math.IsNaN(got) || math.Abs(got-want) > tt.tolerance*scale {
					t.Fatalf("x=%g: got %.17g, want %.17g", x, got, want)
				}
			}
			for _, x := range []float64{math.NaN(), math.Inf(-1), math.Inf(1), -1e10, 1e10} {
				got, want := tt.fast(x), tt.full(x)
				if !(math.IsNaN(got) && math.IsNaN(want)) && got != want {
					t.Fatalf("fallback x=%g: got %g, want %g", x, got, want)
				}
			}
		})
	}
	for k := -1021; k <= 1023; k += 11 {
		for i := 0; i <= 64; i++ {
			x := math.Ldexp(0.5+float64(i)/128, k)
			for _, pair := range [][2]float64{
				{math.FastLog(x), math.Log(x)},
				{math.FastLog2(x), math.Log2(x)},
				{math.FastLog10(x), math.Log10(x)},
			} {
				if math.IsNaN(pair[0]) || math.Abs(pair[0]-pair[1]) > 2e-9 {
					t.Fatalf("log x=%g: got %.17g, want %.17g", x, pair[0], pair[1])
				}
			}
		}
	}
	for _, x := range []float64{-3, -0.5, 0, 0.01, 0.5, 1, 1.5, 3, 100} {
		for _, y := range []float64{-20, -16, -3, -0.5, 0, 0.5, 3, 16, 20} {
			got, want := math.FastPow(x, y), math.Pow(x, y)
			if got == want || math.IsNaN(got) && math.IsNaN(want) {
				continue
			}
			if math.IsNaN(got) || math.Abs(got-want) > 4e-8*math.Abs(want) {
				t.Fatalf("pow(%g,%g): got %.17g, want %.17g", x, y, got, want)
			}
		}
	}
}

var fastMathBenchResult float64

func BenchmarkFastMath(b *testing.B) {
	for _, tt := range []struct {
		name       string
		fast, full func(float64) float64
	}{
		{"Exp", math.FastExp, math.Exp},
		{"Log", math.FastLog, math.Log},
		{"Sin", math.FastSin, math.Sin},
		{"Cos", math.FastCos, math.Cos},
		{"Tan", math.FastTan, math.Tan},
	} {
		b.Run(tt.name, func(b *testing.B) {
			for _, mode := range []struct {
				name string
				fn   func(float64) float64
			}{{"fast", tt.fast}, {"full", tt.full}} {
				b.Run(mode.name, func(b *testing.B) {
					for i := 0; i < b.N; i++ {
						fastMathBenchResult = mode.fn(0.5 + float64(i&1023)/1024)
					}
				})
			}
		})
	}
}
