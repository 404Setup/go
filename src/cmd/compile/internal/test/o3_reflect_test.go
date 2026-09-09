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

func TestO3ReflectMethodName(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	const valueAlias = `name := "Keep"; check(reflect.ValueOf(T{}).MethodByName(name).Call(nil), 1)`
	for _, tt := range []struct {
		name, flags, body string
		wantDrop          bool
	}{
		{"value", "-o3", valueAlias, false},
		{"o2", "-o2", valueAlias, true},
		{"no-opt", "-o3 -N", valueAlias, true},
		{"no-inline", "-o3 -l", valueAlias, false},
		{"type", "-o3", `name := "Keep"; m, ok := reflect.TypeOf(T{}).MethodByName(name); if !ok { panic("missing method") }; check(m.Func.Call([]reflect.Value{reflect.ValueOf(T{})}), 1)`, false},
		{"method-expression", "-o3", `name := "Keep"; check(reflect.Value.MethodByName(reflect.ValueOf(T{}), name).Call(nil), 1)`, false},
		{"aliases", "-o3", `first, unused := "Keep", 0; name := first; _ = unused; check(reflect.ValueOf(T{}).MethodByName(name).Call(nil), 1)`, false},
		{"converted", "-o3", `type Name string; name := Name("Keep"); check(reflect.ValueOf(T{}).MethodByName(string(name)).Call(nil), 1)`, false},
		{"reassigned", "-o3", `name := "Keep"; name = "Drop"; check(reflect.ValueOf(T{}).MethodByName(name).Call(nil), 2)`, true},
		{"address-taken", "-o3", `name := "Keep"; change(&name); check(reflect.ValueOf(T{}).MethodByName(name).Call(nil), 2)`, true},
		{"closure-write", "-o3", `name := "Keep"; f := func() { name = "Drop" }; f(); check(reflect.ValueOf(T{}).MethodByName(name).Call(nil), 2)`, true},
		{"mixed-dynamic", "-o3", valueAlias + `; name2 := dynamicName; check(reflect.ValueOf(T{}).MethodByName(name2).Call(nil), 2)`, true},
		{"index", "-o3", `check(reflect.ValueOf(T{}).Method(0).Call(nil), 2)`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "main.go"), filepath.Join(dir, "main.exe")
			if err := os.WriteFile(src, []byte(o3ReflectSource+"\nfunc main() { "+tt.body+"; println(\"ok\") }\n"), 0o666); err != nil {
				t.Fatal(err)
			}
			goTool := testenv.GoToolPath(t)
			if out, err := testenv.Command(t, goTool, "build", "-gcflags="+tt.flags+" -d=ssa/check/on", "-o", dst, src).CombinedOutput(); err != nil {
				t.Fatalf("build failed: %v\n%s", err, out)
			}
			out, err := testenv.Command(t, goTool, "tool", "nm", dst).CombinedOutput()
			if err != nil {
				t.Fatalf("nm failed: %v\n%s", err, out)
			}
			if got := bytes.Contains(out, []byte(" main.T.Drop")); got != tt.wantDrop {
				t.Errorf("Drop retained = %v; want %v", got, tt.wantDrop)
			}
			if out, err := testenv.Command(t, dst).CombinedOutput(); err != nil || string(out) != "ok\n" {
				t.Fatalf("incorrect execution: %v\n%s", err, out)
			}
		})
	}
}

const o3ReflectSource = `package main

import "reflect"

type T struct{}

//go:noinline
func (T) Keep() int { return 1 }

//go:noinline
func (T) Drop() int { return 2 }

var dynamicName = "Drop"

//go:noinline
func change(name *string) { *name = "Drop" }

func check(results []reflect.Value, want int) {
	if len(results) != 1 || results[0].Int() != int64(want) { panic("wrong method result") }
}
`
