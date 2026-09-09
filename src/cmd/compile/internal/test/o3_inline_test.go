// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"bytes"
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestO3Inlining(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	// The plain body costs 149: only a loop/specialization credit should
	// bring it below the O3 threshold. The interface body also pays for a call.
	body := strings.Repeat("x = x*3 + 1\n", 21)
	source := strings.NewReplacer("BODY", body, "INTERFACE_BODY", strings.Repeat("x = x*3 + 1\n", 18),
		"GROW_CALLS", strings.Repeat("x = growHelper(x)\n", 30)).Replace(o3InlineSource)
	if err := os.WriteFile(src, []byte(source), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, ldflags, gcflags string
		advanced               bool
	}{
		{"default", "", "", false},
		{"o2", "-o2", "", false},
		{"o3", "-o3", "", true},
		{"compiler", "", "-o3", true},
		{"disabled", "-o3", "-o3=false", false},
		{"no-opt", "-o3", "-N", false},
		{"no-inline", "-o3", "-l", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dst := filepath.Join(dir, tt.name+".exe")
			out, err := testenv.Command(t, testenv.GoToolPath(t), "build", "-ldflags="+tt.ldflags,
				"-gcflags=-m=2 -d=ssa/check/on "+tt.gcflags, "-o", dst, src).CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, out)
			}
			for _, name := range []string{"medium", "specialize", "callback", "interfaceCall"} {
				want := 0
				if tt.advanced {
					want = 1
				}
				if got := bytes.Count(out, []byte("inlining call to "+name)); got != want {
					t.Errorf("%s: got %d inlines, want %d\n%s", name, got, want, out)
				}
			}
			growth := bytes.Count(out, []byte("inlining call to growHelper"))
			if tt.advanced && (growth == 0 || growth >= 30) || !tt.advanced && growth != 0 {
				t.Errorf("growth budget: got %d of 30 inlines\n%s", growth, out)
			}
			if bytes.Contains(out, []byte("inlining call to blocked")) {
				t.Errorf("ignored go:noinline\n%s", out)
			}
			if out, err := testenv.Command(t, dst).CombinedOutput(); err != nil || string(out) != "ok\n" {
				t.Fatalf("incorrect execution: %v\n%s", err, out)
			}
		})
	}
}

func TestO3InliningImport(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	dep := filepath.Join(dir, "dep.go")
	archive := filepath.Join(dir, "dep.a")
	middle := filepath.Join(dir, "middle.go")
	middleArchive := filepath.Join(dir, "middle.a")
	src := filepath.Join(dir, "caller.go")
	cfg := filepath.Join(dir, "importcfg")
	for path, contents := range map[string]string{
		dep: "package dep\nfunc Choose(b bool, x int) int { if b {\n" +
			strings.Repeat("x = x*3 + 1\n", 21) + "}; return x }\nfunc Inc(x int) int { return x+1 }\n",
		middle: "package middle\nimport \"dep\"\nfunc Wrap(x int) int { return dep.Choose(false, x) }\n",
		src: "package caller\nimport (\"dep\"; \"middle\")\n" +
			"func Caller(x int) int { return dep.Inc(dep.Choose(false, x)) + middle.Wrap(x) }\n",
		cfg: "packagefile dep=" + archive + "\npackagefile middle=" + middleArchive + "\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	for _, depO3 := range []string{"false", "true"} {
		for _, callerO3 := range []string{"false", "true"} {
			t.Run("dep="+depO3+"/caller="+callerO3, func(t *testing.T) {
				goTool := testenv.GoToolPath(t)
				out, err := testenv.Command(t, goTool, "tool", "compile", "-p", "dep", "-pack",
					"-d=syncframes=1", "-o3="+depO3, "-o", archive, dep).CombinedOutput()
				if err != nil {
					t.Fatalf("compile dependency: %v\n%s", err, out)
				}
				out, err = testenv.Command(t, goTool, "tool", "compile", "-p", "middle", "-pack",
					"-d=syncframes=1", "-o3=false", "-importcfg", cfg, "-o", middleArchive, middle).CombinedOutput()
				if err != nil {
					t.Fatalf("compile re-export: %v\n%s", err, out)
				}
				out, err = testenv.Command(t, goTool, "tool", "compile", "-p", "caller", "-m=2",
					"-d=syncframes=1", "-o3="+callerO3, "-importcfg", cfg, "-o", filepath.Join(dir, "caller.o"), src).CombinedOutput()
				if err != nil {
					t.Fatalf("compile caller: %v\n%s", err, out)
				}
				want := 0
				if depO3 == "true" && callerO3 == "true" {
					want = 2 // direct call and call exposed by inlining middle.Wrap
				}
				if got := bytes.Count(out, []byte("inlining call to dep.Choose")); got != want {
					t.Errorf("constant specialization: got %d inlines, want %d\n%s", got, want, out)
				}
				if !bytes.Contains(out, []byte("inlining call to dep.Inc")) {
					t.Errorf("small imported helper did not inline\n%s", out)
				}
			})
		}
	}
}

func TestO3InliningRuntime(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	out, err := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags=all=-o3", "runtime").CombinedOutput()
	if err != nil {
		t.Fatalf("runtime write-barrier checks failed: %v\n%s", err, out)
	}
}

const o3InlineSource = `package main

func medium(x int) int { BODY; return x }
func growHelper(x int) int { BODY; return x }
func specialize(b bool, x int) int { if b { BODY }; return x }
func callback(f func(int) int, x int) int { BODY; return f(x) }
type adder interface { add(int) int }
type value int
func (v value) add(x int) int { return x + int(v) }
func interfaceCall(v adder, x int) int { INTERFACE_BODY; return v.add(x) }
func increment(x int) int { return x+1 }
//go:noinline
func blocked(x int) int { return x+1 }
//go:noinline
func plain(x int) int { return medium(x) }
//go:noinline
func hot(n, x int) int {
 for i := 0; i < n; i++ { x = medium(x) }
 return x
}
//go:noinline
func cold(x int) { panic(medium(x)) }
var initial int
func init() { initial = medium(0) }
//go:noinline
func constant(x int) int { return specialize(false, x) }
//go:noinline
func dynamic(b bool, x int) int { return specialize(b, x) }
//go:noinline
func indirect(x int) int { return callback(increment, x) }
//go:noinline
func concrete(x int) int { return interfaceCall(value(1), x) }
//go:noinline
func growth(n, x int) int {
 for i := 0; i < n; i++ { GROW_CALLS }
 return increment(x)
}
//go:noinline
func reference(n, x int) int {
 for i := 0; i < n; i++ { x = x*3 + 1 }
 return x
}
func checkPanic(x int) {
 defer func() { if recover() != reference(21, x) { panic("panic value") } }()
 cold(x)
}
func main() {
 if initial != reference(21, 0) { panic("init") }
 for _, x := range []int{-17, 0, 1, 42} {
  want := reference(21, x)
  if plain(x) != want || hot(2, x) != reference(42, x) { panic("loop") }
  if constant(x) != x || dynamic(false, x) != x || dynamic(true, x) != want { panic("constant") }
  if indirect(x) != want+1 || concrete(x) != reference(18, x)+1 { panic("devirtualization") }
  if growth(1, x) != reference(21*30, x)+1 || blocked(x) != x+1 { panic("growth") }
  checkPanic(x)
 }
 println("ok")
}
`
