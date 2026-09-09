// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"bytes"
	"internal/testenv"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFastMathLibrary(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "math.go")
	if err := os.WriteFile(src, []byte(fastMathLibrarySource), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, ldflags, gcflags string
		fast                   bool
	}{
		{"default", "", "", false},
		{"enabled", "-fmth", "", true},
		{"disabled", "-fmth=false", "", false},
		{"no-opt", "-fmth", "-N", false},
		{"override", "-fmth", "-fmth=false", false},
		{"dependencies", "all=-fmth", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dst := filepath.Join(dir, tt.name+".exe")
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-ldflags="+tt.ldflags, "-gcflags=-S "+tt.gcflags, "-o", dst, src)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			for _, name := range []string{"Exp", "Exp2", "Log", "Log2", "Log10", "Pow", "Sin", "Cos", "Tan"} {
				if got := bytes.Contains(out, []byte("math.fast"+name+"(SB)")); got != tt.fast {
					t.Errorf("approximate %s call present = %v; want %v", name, got, tt.fast)
				}
			}
			out, err = testenv.Command(t, dst).CombinedOutput()
			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			values := strings.Fields(string(out))
			want := []float64{math.Exp(0.5), math.Exp2(0.5), math.Log(0.5), math.Log2(0.5), math.Log10(0.5), math.Pow(0.5, 1.3), math.Sin(0.5), math.Cos(0.5), math.Tan(0.5)}
			if len(values) != len(want) {
				t.Fatalf("unexpected output: %s", out)
			}
			for i, w := range want {
				got, err := strconv.ParseFloat(values[i], 64)
				if err != nil || math.IsNaN(got) || math.Abs(got-w) > 1e-6*math.Abs(w) {
					t.Errorf("value %d: got %s, want %g (parse: %v)", i, values[i], w, err)
				}
			}
		})
	}
}

const fastMathLibrarySource = `package main
import "math"
//go:noinline
func values(x float64) {
	println(math.Exp(x), math.Exp2(x), math.Log(x), math.Log2(x), math.Log10(x),
		math.Pow(x, 1.3), math.Sin(x), math.Cos(x), math.Tan(x))
}
func main() { values(0.5) }
`
