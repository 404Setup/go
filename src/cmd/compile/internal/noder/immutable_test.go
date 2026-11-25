// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder_test

import (
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testGoBin(t *testing.T) string {
	t.Helper()
	testenv.MustHaveGoBuild(t)
	return testenv.GoToolPath(t)
}

func TestImmutableDirectiveRuntimeAndValid(t *testing.T) {
	goBin := testGoBin(t)
	dir := t.TempDir()

	writeFile := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("go.mod", "module example.com/immtest\n\ngo 1.26\n")

	writeFile("immdef/defs.go", `package immdef

//go:immutable
var Allowed = []string{"GET", "POST"}

//go:immutable
var Ints = []int64{10, 20, 30}

//go:immutable
var Codes map[string]int

//go:immutable
var Arr = [3]int{1, 2, 3}

type NamedSlice []string
type NamedMap map[string]int64

//go:immutable
var NamedS = NamedSlice{"x", "y"}

//go:immutable
var NamedM = NamedMap{"k": 7}

func (s NamedSlice) Get(i int) string { return s[i] }

func init() {
	Codes = map[string]int{"ok": 200}
	Codes["notfound"] = 404
	delete(Codes, "tmp")
}
`)

	writeFile("main.go", `package main

import (
	"fmt"
	"maps"
	"reflect"
	"slices"

	"example.com/immtest/immdef"
)

type User struct {
	Name string
}

type Inner struct {
	X int
}

func (in *Inner) Set(v int) { in.X = v }

type OuterPtr struct {
	*Inner
}

//go:immutable
var userPtrs = []*User{{Name: "alice"}}

//go:immutable
var outerPtrs = []OuterPtr{{Inner: &Inner{X: 1}}}

//go:immutable
var matrix = [][]int{{1, 2}, {3, 4}}

func mustPanic(name string, fn func()) {
	defer func() {
		if r := recover(); r == nil {
			panic(fmt.Sprintf("%s: expected panic", name))
		}
	}()
	fn()
}

func mutateSlice(s []string) { s[0] = "hacked" }
func mutateInts(s []int64)   { s[0] = 999 }
func mutateMap(m map[string]int) {
	m["ok"] = 500
}
func deleteMap(m map[string]int) {
	delete(m, "ok")
}

func genericImmutable[T comparable](a, b T) []T {
	//go:immutable
	s := []T{a, b}
	if !slices.IsImmutable(s) {
		panic("generic local slice not immutable")
	}
	return s
}

func genericImmutableMap[K comparable, V any](k K, v V) map[K]V {
	//go:immutable
	m := map[K]V{k: v}
	if !maps.IsImmutable(m) {
		panic("generic local map not immutable")
	}
	return m
}

func main() {
	if !slices.IsImmutable(immdef.Allowed) || immdef.Allowed[0] != "GET" {
		panic("immdef.Allowed not immutable")
	}
	if !slices.IsImmutable(immdef.Ints) || immdef.Ints[2] != 30 {
		panic("immdef.Ints not immutable")
	}
	if !maps.IsImmutable(immdef.Codes) || immdef.Codes["ok"] != 200 || immdef.Codes["notfound"] != 404 {
		panic("immdef.Codes not immutable")
	}
	if !slices.IsImmutable(immdef.NamedS) || immdef.NamedS.Get(1) != "y" {
		panic("immdef.NamedS not immutable")
	}
	if !maps.IsImmutable(immdef.NamedM) || immdef.NamedM["k"] != 7 {
		panic("immdef.NamedM not immutable")
	}
	if immdef.Arr[1] != 2 {
		panic("immdef.Arr mismatch")
	}

	// Reading via copy(dst, src) and range and append-copy are allowed.
	dst := make([]string, 2)
	copy(dst, immdef.Allowed)
	if dst[1] != "POST" {
		panic("copy from immutable failed")
	}
	extended := append(immdef.Allowed, "PUT")
	if len(extended) != 3 || len(immdef.Allowed) != 2 {
		panic("append from immutable misbehaved")
	}

	// Allowed non-collection-backing writes: pointer element target, embedded pointer, & inner slice.
	userPtrs[0].Name = "bob"
	if userPtrs[0].Name != "bob" {
		panic("userPtrs target update failed")
	}
	outerPtrs[0].X = 7
	outerPtrs[0].Set(9)
	if outerPtrs[0].X != 9 {
		panic("outerPtrs embedded pointer update failed")
	}
	matrix[0][1] = 99
	if matrix[0][1] != 99 {
		panic("matrix inner slice update failed")
	}

	// Cross-package alias / function-call / reflect mutation attempts must panic at runtime.
	mustPanic("alias slice", func() { mutateSlice(immdef.Allowed) })
	mustPanic("alias ints", func() { mutateInts(immdef.Ints) })
	mustPanic("alias map", func() { mutateMap(immdef.Codes) })
	mustPanic("delete map", func() { deleteMap(immdef.Codes) })
	mustPanic("reflect slice", func() { reflect.ValueOf(immdef.Allowed).Index(0).SetString("hacked") })
	mustPanic("reflect slice SetZero", func() { reflect.ValueOf(immdef.Allowed).Index(0).SetZero() })
	mustPanic("reflect map", func() { reflect.ValueOf(immdef.Codes).SetMapIndex(reflect.ValueOf("ok"), reflect.ValueOf(1)) })

	// Local //go:immutable var & :=
	//go:immutable
	localS := []string{"a", "b"}
	//go:immutable
	var localVarS = []string{"c", "d"}
	//go:immutable
	var localM = map[string]int{"x": 1}
	//go:immutable
	pairA, pairB := []int64{1, 2}, map[int]string{1: "one"}

	if !slices.IsImmutable(localS) || !slices.IsImmutable(localVarS) || !maps.IsImmutable(localM) || !slices.IsImmutable(pairA) || !maps.IsImmutable(pairB) {
		panic("local //go:immutable not runtime-frozen")
	}
	mustPanic("local slice via fn", func() { mutateSlice(localS) })
	mustPanic("local var slice via fn", func() { mutateSlice(localVarS) })
	mustPanic("local map via fn", func() { mutateMap(localM) })
	mustPanic("local pair slice via fn", func() { mutateInts(pairA) })

	// Generic local //go:immutable
	gs := genericImmutable("hello", "world")
	mustPanic("generic local slice", func() { gs[0] = "hacked" })
	gm := genericImmutableMap("k", 42)
	mustPanic("generic local map", func() { gm["k"] = 99 })

	fmt.Println("PASS")
}
`)

	cmd := exec.Command(goBin, "run", ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run . failed: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "PASS" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestImmutableDirectiveCompileErrors(t *testing.T) {
	goBin := testGoBin(t)

	cases := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			name: "reassign_slice",
			files: map[string]string{
				"main.go": `package main
//go:immutable
var s = []string{"a"}
func main() { s = nil }
`,
			},
			wantErr: "cannot assign to //go:immutable variable s",
		},
		{
			name: "assign_elem_slice",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	s := []string{"a", "b"}
	s[0] = "c"
}
`,
			},
			wantErr: "cannot assign to //go:immutable variable s",
		},
		{
			name: "inc_elem_int64",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	s := []int64{1, 2}
	s[0]++
}
`,
			},
			wantErr: "cannot assign to //go:immutable variable s",
		},
		{
			name: "assign_map_key",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	m := map[string]int{"a": 1}
	m["b"] = 2
}
`,
			},
			wantErr: "cannot assign to //go:immutable variable m",
		},
		{
			name: "delete_map",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	m := map[string]int{"a": 1}
	delete(m, "a")
}
`,
			},
			wantErr: "cannot delete from //go:immutable map m",
		},
		{
			name: "clear_slice",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	s := []string{"a"}
	clear(s)
}
`,
			},
			wantErr: "cannot clear //go:immutable variable s",
		},
		{
			name: "copy_into_slice",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	s := []string{"a"}
	copy(s, []string{"b"})
}
`,
			},
			wantErr: "cannot copy into //go:immutable slice s",
		},
		{
			name: "take_addr_var",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	s := []string{"a"}
	_ = &s
}
`,
			},
			wantErr: "cannot take address of //go:immutable variable s",
		},
		{
			name: "take_addr_elem",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	s := []string{"a"}
	_ = &s[0]
}
`,
			},
			wantErr: "cannot take address of //go:immutable variable s",
		},
		{
			name: "struct_value_field_write",
			files: map[string]string{
				"main.go": `package main
type User struct { Name string }
func main() {
	//go:immutable
	users := []User{{Name: "a"}}
	users[0].Name = "b"
}
`,
			},
			wantErr: "cannot assign to //go:immutable variable users",
		},
		{
			name: "ptr_receiver_method_on_slice",
			files: map[string]string{
				"main.go": `package main
type MySlice []string
func (s *MySlice) Push(v string) { *s = append(*s, v) }
func main() {
	//go:immutable
	s := MySlice{"a"}
	s.Push("b")
}
`,
			},
			wantErr: "cannot call pointer-receiver method on //go:immutable variable s",
		},
		{
			name: "ptr_receiver_method_on_elem",
			files: map[string]string{
				"main.go": `package main
type User struct { Name string }
func (u *User) Set(n string) { u.Name = n }
func main() {
	//go:immutable
	users := []User{{Name: "a"}}
	users[0].Set("b")
}
`,
			},
			wantErr: "cannot call pointer-receiver method on //go:immutable variable users",
		},
		{
			name: "slice_immutable_array",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	a := [3]int{1, 2, 3}
	_ = a[:]
}
`,
			},
			wantErr: "cannot slice //go:immutable array a",
		},
		{
			name: "range_reassign",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	s := []int{1}
	src := [][]int{{2}}
	for _, s = range src {
		_ = s
	}
}
`,
			},
			wantErr: "cannot assign to //go:immutable variable s",
		},
		{
			name: "closure_in_init",
			files: map[string]string{
				"main.go": `package main
//go:immutable
var s = []string{"a"}
var fn func()
func init() {
	fn = func() { s[0] = "b" }
}
func main() { fn() }
`,
			},
			wantErr: "cannot assign to //go:immutable variable s",
		},
		{
			name: "other_global_initializer",
			files: map[string]string{
				"main.go": `package main
//go:immutable
var a = []string{"x"}
var b = copy(a, []string{"y"})
func main() { _ = b }
`,
			},
			wantErr: "cannot copy into //go:immutable slice a",
		},
		{
			name: "cross_package_mutation",
			files: map[string]string{
				"sub/sub.go": `package sub
//go:immutable
var Allowed = []string{"GET"}
`,
				"main.go": `package main
import "example.com/bad/sub"
func main() { sub.Allowed[0] = "POST" }
`,
			},
			wantErr: "cannot assign to //go:immutable variable Allowed",
		},
		{
			name: "invalid_non_collection_type",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	x := 42
	_ = x
}
`,
			},
			wantErr: "//go:immutable only applies to slice, array, or map variables",
		},
		{
			name: "local_var_without_initializer",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	var s []string
	_ = s
}
`,
			},
			wantErr: "//go:immutable local variable must have an initializer",
		},
		{
			name: "misplaced_on_func",
			files: map[string]string{
				"main.go": `package main
//go:immutable
func foo() {}
func main() { foo() }
`,
			},
			wantErr: "misplaced compiler directive",
		},
		{
			name: "misplaced_on_assign",
			files: map[string]string{
				"main.go": `package main
func main() {
	s := []int{1}
	//go:immutable
	s = []int{2}
	_ = s
}
`,
			},
			wantErr: "misplaced compiler directive",
		},
		{
			name: "redeclare_short_var",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	s := []int{1}
	s, ok := []int{2}, true
	_, _ = s, ok
}
`,
			},
			wantErr: "cannot assign to //go:immutable variable s",
		},
		{
			name: "cross_package_map_mutation",
			files: map[string]string{
				"def/def.go": `package def
//go:immutable
var Codes = map[string]int{"ok": 1}
`,
				"main.go": `package main
import "example.com/bad/def"
func main() { def.Codes["ok"] = 2 }
`,
			},
			wantErr: "cannot assign to //go:immutable variable Codes",
		},
		{
			name: "cross_package_reassign",
			files: map[string]string{
				"def/def.go": `package def
//go:immutable
var S = []string{"a"}
`,
				"main.go": `package main
import "example.com/bad/def"
func main() { def.S = nil }
`,
			},
			wantErr: "cannot assign to //go:immutable variable S",
		},
		{
			name: "cross_package_take_addr",
			files: map[string]string{
				"def/def.go": `package def
//go:immutable
var S = []int64{1, 2}
`,
				"main.go": `package main
import "example.com/bad/def"
func main() { _ = &def.S[0] }
`,
			},
			wantErr: "cannot take address of //go:immutable variable S",
		},
		{
			name: "slice_nested_array",
			files: map[string]string{
				"main.go": `package main
//go:immutable
var a = [2][3]int{{1, 2, 3}, {4, 5, 6}}
func main() { _ = a[0][:] }
`,
			},
			wantErr: "cannot slice //go:immutable variable a",
		},
		{
			name: "slice_struct_array_field",
			files: map[string]string{
				"main.go": `package main
type Box struct { Arr [3]int }
//go:immutable
var boxes = []Box{{Arr: [3]int{1, 2, 3}}}
func main() { _ = boxes[0].Arr[:] }
`,
			},
			wantErr: "cannot slice //go:immutable variable boxes",
		},
		{
			name: "embedded_value_field_write",
			files: map[string]string{
				"main.go": `package main
type Inner struct { X int }
type Outer struct { Inner }
//go:immutable
var list = []Outer{{Inner: Inner{X: 1}}}
func main() { list[0].X = 2 }
`,
			},
			wantErr: "cannot assign to //go:immutable variable list",
		},
		{
			name: "embedded_value_ptr_method",
			files: map[string]string{
				"main.go": `package main
type Inner struct { X int }
func (in *Inner) Set(v int) { in.X = v }
type Outer struct { Inner }
//go:immutable
var list = []Outer{{Inner: Inner{X: 1}}}
func main() { list[0].Set(2) }
`,
			},
			wantErr: "cannot call pointer-receiver method on //go:immutable variable list",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/bad\n\ngo 1.26\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for rel, content := range tc.files {
				p := filepath.Join(dir, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(goBin, "build", ".")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("%s: expected compile error %q, but build succeeded", tc.name, tc.wantErr)
			}
			if !strings.Contains(string(out), tc.wantErr) {
				t.Fatalf("%s: output %q does not contain %q", tc.name, string(out), tc.wantErr)
			}
		})
	}
}
