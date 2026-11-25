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

import (
	"cmp"
	"container/list"
	"container/ring"
	"maps"
	"slices"
	"sync/v2/container"
)

//go:immutable
var Allowed = []string{"GET", "POST"}

//go:immutable
var Ints = []int64{10, 20, 30}

//go:immutable
var Codes map[string]int

//go:immutable
var Arr = [3]int{1, 2, 3}

//go:immutable
var GlobalList = list.New()

//go:immutable
var GlobalRing = ring.New(2)

//go:immutable
var GlobalOrdered = new(container.OrderedMap[string, int])

//go:immutable
var GlobalTree = container.NewTreeMap[string, int](cmp.Compare[string])

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

	GlobalList.PushBack("first")
	GlobalList.PushBack("second")

	GlobalRing.Value = 10
	GlobalRing.Next().Value = 20

	GlobalOrdered.Store("alpha", 1)
	GlobalTree.Store("beta", 2)
}

//go:immutable
func ReadAllSupported(s []string, scale int, m map[string]int, a [3]int, extra ...any) int {
	_ = extra
	return (len(s) + m["ok"] + a[0]) * scale
}

//go:immutable args0 r0
func ForwardSlice(s []string, tag string) ([]string, string) {
	return s, tag
}

//go:immutable r
func FreezeCopy(mut []string) []string {
	return slices.ToImmutable(mut)
}

//go:immutable r0 r1
func MakeFrozenPair(k string, v int) ([]string, map[string]int, error) {
	return slices.ToImmutable([]string{k}), maps.ToImmutable(map[string]int{k: v}), nil
}

//go:immutable args0 args1
func ConsumePair(s []string, m map[string]int, err error) int {
	if err != nil {
		return -1
	}
	return len(s) + len(m)
}

//go:immutable args0 r
func GenericClone[S ~[]E, E comparable](s S) S {
	return s
}

type Box[T any] struct {
	Label string
}

//go:immutable args0 r
func (b *Box[T]) Echo(s []T, note string) []T {
	_ = note
	return s
}

func (b Box[T]) String() string { return b.Label }
`)

	writeFile("main.go", `package main

import (
	"cmp"
	"container/list"
	"container/ring"
	"fmt"
	"maps"
	"reflect"
	"runtime"
	"slices"
	"sync/v2/container"
	"weak"

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

func mutateSlice(s []string)   { s[0] = "hacked" }
func mutateInts(s []int64)     { s[0] = 999 }
func mutateMap(m map[string]int) { m["ok"] = 500 }
func deleteMap(m map[string]int) { delete(m, "ok") }
func mutateList(l *list.List)    { l.PushBack("hacked") }
func mutateRing(r *ring.Ring)    { r.Unlink(1) }
func mutateOM(m *container.OrderedMap[string, int]) { m.Store("k", 9) }

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

func genericImmutableContainer[K comparable, V any](k K, v V) *container.OrderedMap[K, V] {
	raw := new(container.OrderedMap[K, V])
	raw.Store(k, v)
	//go:immutable
	frozen := raw
	if !frozen.IsImmutable() {
		panic("generic local container not immutable")
	}
	return frozen
}

//go:immutable args0 args2 r0
func selectiveAndReturn(immS []string, mutScratch []byte, immL *list.List) []string {
	mutScratch[0] = 'Z'
	if immL != nil && immL.Len() == 0 {
		return nil
	}
	return immS[:1]
}

//go:immutable r0
func namedResultFunc(useDefault bool) (out []string) {
	defer func() {
		if out == nil {
			out = immdef.Allowed
		}
	}()
	if useDefault {
		return
	}
	//go:immutable
	custom := []string{"CUSTOM"}
	out = custom
	return
}

//go:immutable args1
func variadicImm(prefix string, items ...string) int {
	_ = prefix
	return len(items)
}

//go:immutable
func inspectContainers(
	l *list.List,
	r *ring.Ring,
	lhm *container.LinkedHashMap[string, int],
	om *container.OrderedMap[string, int],
	tm *container.TreeMap[string, int],
	sm *container.ConcurrentSkipListMap[string, int],
) int {
	total := 0
	if l != nil {
		total += l.Len()
	}
	if r != nil {
		total += r.Len()
	}
	if lhm != nil {
		total += lhm.Len()
	}
	if om != nil {
		if v, ok := om.Load("alpha"); ok {
			total += v
		}
	}
	if tm != nil {
		total += tm.Len()
	}
	if sm != nil {
		total += sm.Len()
	}
	return total
}

//go:immutable r
func passRingNext(r *ring.Ring) *ring.Ring {
	//go:immutable
	frozen := r
	return frozen.Next()
}

type Handler struct {
	name string
}

//go:immutable args0 r
func (h *Handler) Process(in []string) []string {
	return in
}

func (h Handler) String() string { return h.name }

func verifyGCReclaim() {
	var (
		wpSlice weak.Pointer[int64]
		wpList  weak.Pointer[list.List]
		wpOM    weak.Pointer[container.OrderedMap[string, int]]
	)
	func() {
		//go:immutable
		s := []int64{100, 200, 300, 400}
		//go:immutable
		l := list.New()
		_ = l
		rawL := list.New()
		rawL.PushBack("ephemeral")
		//go:immutable
		frozenL := rawL

		om := genericImmutableContainer("temp", 123)
		echoBox := &immdef.Box[int64]{Label: "gc"}
		s2 := echoBox.Echo(s, "check")

		wpSlice = weak.Make(&s2[:1:1][0])
		wpList = weak.Make(frozenL)
		wpOM = weak.Make(om)
	}()
	for range 5 {
		runtime.GC()
		if wpSlice.Value() == nil && wpList.Value() == nil && wpOM.Value() == nil {
			return
		}
	}
	panic("immutable values were retained by GC")
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
	if !immdef.GlobalList.IsImmutable() || immdef.GlobalList.Len() != 2 {
		panic("immdef.GlobalList not immutable")
	}
	if !immdef.GlobalRing.IsImmutable() || !immdef.GlobalRing.Next().IsImmutable() {
		panic("immdef.GlobalRing not immutable")
	}
	if !immdef.GlobalOrdered.IsImmutable() || !immdef.GlobalTree.IsImmutable() {
		panic("immdef global containers not immutable")
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

	// Function //go:immutable calls: bare, selective, return-shorthand, tuple-forwarding, nil, conversions.
	if got := immdef.ReadAllSupported(immdef.Allowed, 2, immdef.Codes, immdef.Arr, "ignored", 123); got != (2+200+1)*2 {
		panic(fmt.Sprintf("ReadAllSupported got %d", got))
	}
	if got := immdef.ReadAllSupported(nil, 1, nil, immdef.Arr); got != 1 {
		panic("ReadAllSupported with nil failed")
	}
	fwd, tag := immdef.ForwardSlice(immdef.FreezeCopy([]string{"a", "b"}), "ok")
	if len(fwd) != 2 || tag != "ok" || !slices.IsImmutable(fwd) {
		panic("ForwardSlice + FreezeCopy failed")
	}
	// Return-only //go:immutable function MAY be used as a first-class function value.
	freezeFn := immdef.FreezeCopy
	if res := freezeFn([]string{"fnval"}); !slices.IsImmutable(res) {
		panic("return-only func value failed")
	}
	if n := immdef.ConsumePair(immdef.MakeFrozenPair("k", 9)); n != 2 {
		panic("tuple forwarding failed")
	}

	scratch := []byte{'A', 'B'}
	sub := selectiveAndReturn(immdef.Allowed, scratch, immdef.GlobalList)
	if scratch[0] != 'Z' || len(sub) != 1 || sub[0] != "GET" {
		panic("selectiveAndReturn failed")
	}
	if nr1 := namedResultFunc(true); nr1[0] != "GET" {
		panic("namedResultFunc(true) failed")
	}
	if nr2 := namedResultFunc(false); nr2[0] != "CUSTOM" || !slices.IsImmutable(nr2) {
		panic("namedResultFunc(false) failed")
	}
	if variadicImm("empty") != 0 || variadicImm("spread", immdef.Allowed...) != 2 {
		panic("variadicImm failed")
	}

	// Generic //go:immutable functions and methods + MethodExpr + non-constrained interface satisfaction.
	clonedNamed := immdef.GenericClone(immdef.NamedS)
	if !slices.IsImmutable(clonedNamed) || clonedNamed[0] != "x" {
		panic("GenericClone failed")
	}
	box := &immdef.Box[string]{Label: "mybox"}
	if out := box.Echo(immdef.Allowed, "hi"); len(out) != 2 {
		panic("Box.Echo failed")
	}
	if out := (*immdef.Box[string]).Echo(box, immdef.Allowed, "mexpr"); len(out) != 2 {
		panic("Box.Echo MethodExpr failed")
	}
	var stringer fmt.Stringer = *box
	if stringer.String() != "mybox" {
		panic("unconstrained method String() should still satisfy fmt.Stringer")
	}

	h := &Handler{name: "h1"}
	if out := (*Handler).Process(h, immdef.Allowed); len(out) != 2 {
		panic("Handler.Process MethodExpr failed")
	}
	var hStringer fmt.Stringer = *h
	if hStringer.String() != "h1" {
		panic("Handler String() failed")
	}
	// Dynamic type assertion to interface containing //go:immutable-param method must NOT succeed.
	if _, ok := any(h).(interface{ Process([]string) []string }); ok {
		panic("Handler unexpectedly satisfied interface with //go:immutable param method at runtime")
	}

	// Local //go:immutable containers & container function calls.
	rawLHM := container.NewLinkedHashMap[string, int](true)
	rawLHM.Store("k", 1)
	rawSM := container.NewConcurrentSkipListMap[string, int](cmp.Compare[string])
	rawSM.Store("k", 2)
	//go:immutable
	immLHM := rawLHM
	//go:immutable
	immSM := rawSM
	if !immLHM.IsImmutable() || !immSM.IsImmutable() {
		panic("local containers not frozen")
	}
	if total := inspectContainers(immdef.GlobalList, passRingNext(immdef.GlobalRing), immLHM, immdef.GlobalOrdered, immdef.GlobalTree, immSM); total != 2+2+1+1+1+1 {
		panic(fmt.Sprintf("inspectContainers = %d", total))
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
	mustPanic("alias list", func() { mutateList(immdef.GlobalList) })
	mustPanic("alias ring", func() { mutateRing(immdef.GlobalRing) })
	mustPanic("alias orderedmap", func() { mutateOM(immdef.GlobalOrdered) })
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

	// Clipped 3-index subslice append allocates fresh slice without mutating localS.
	clippedExt := append(localS[:1:1], "z")
	if len(clippedExt) != 2 || clippedExt[1] != "z" || localS[1] != "b" {
		panic("clipped 3-index subslice append failed")
	}

	verifyGCReclaim()

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
			name: "convert_slice_to_array_ptr",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	s := []string{"a", "b"}
	p := (*[1]string)(s)
	_ = p
}
`,
			},
			wantErr: "cannot convert //go:immutable slice s to array pointer",
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
			wantErr: "//go:immutable only applies to slice, array, map, or container variables",
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
			name: "bare_func_without_supported_params",
			files: map[string]string{
				"main.go": `package main
//go:immutable
func foo() {}
func main() { foo() }
`,
			},
			wantErr: "//go:immutable function foo has no supported collection or container parameters",
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
		// Function //go:immutable syntax & validation errors.
		{
			name: "func_param_out_of_range",
			files: map[string]string{
				"main.go": `package main
//go:immutable args2
func foo(a []int, b []int) {}
func main() {}
`,
			},
			wantErr: "//go:immutable parameter args2 out of range for foo (have 2 parameters)",
		},
		{
			name: "func_param_unsupported_type",
			files: map[string]string{
				"main.go": `package main
//go:immutable args0
func foo(n int) {}
func main() {}
`,
			},
			wantErr: "//go:immutable parameter args0 of foo has unsupported type int",
		},
		{
			name: "func_result_shorthand_zero_results",
			files: map[string]string{
				"main.go": `package main
//go:immutable r
func foo(s []int) {}
func main() {}
`,
			},
			wantErr: "//go:immutable result r out of range for foo (have 0 results)",
		},
		{
			name: "func_result_shorthand_multiple_results",
			files: map[string]string{
				"main.go": `package main
//go:immutable r
func foo() ([]int, error) { return nil, nil }
func main() {}
`,
			},
			wantErr: "//go:immutable shorthand r requires 1 result (have 2 results)",
		},
		{
			name: "func_result_index_out_of_range",
			files: map[string]string{
				"main.go": `package main
//go:immutable r1
func foo() []int { return nil }
func main() {}
`,
			},
			wantErr: "//go:immutable result r1 out of range for foo (have 1 results)",
		},
		{
			name: "func_result_unsupported_type",
			files: map[string]string{
				"main.go": `package main
//go:immutable r1
func foo() ([]int, error) { return nil, nil }
func main() {}
`,
			},
			wantErr: "//go:immutable result r1 of foo has unsupported type error",
		},
		{
			name: "func_invalid_token",
			files: map[string]string{
				"main.go": `package main
//go:immutable arg0
func foo(s []int) {}
func main() {}
`,
			},
			wantErr: `invalid //go:immutable argument "arg0"`,
		},
		{
			name: "func_leading_zero_index",
			files: map[string]string{
				"main.go": `package main
//go:immutable args00
func foo(s []int) {}
func main() {}
`,
			},
			wantErr: `invalid //go:immutable argument "args00"`,
		},
		{
			name: "func_duplicate_param",
			files: map[string]string{
				"main.go": `package main
//go:immutable args0 args0
func foo(s []int) {}
func main() {}
`,
			},
			wantErr: "duplicate //go:immutable parameter args0",
		},
		{
			name: "func_duplicate_directive",
			files: map[string]string{
				"main.go": `package main
//go:immutable args0
//go:immutable r0
func foo(s []int) []int { return s }
func main() {}
`,
			},
			wantErr: "duplicate //go:immutable directive",
		},
		{
			name: "func_on_init",
			files: map[string]string{
				"main.go": `package main
//go:immutable
func init() {}
func main() {}
`,
			},
			wantErr: "cannot use //go:immutable on init function",
		},
		{
			name: "var_with_args",
			files: map[string]string{
				"main.go": `package main
//go:immutable args0
var s = []int{1}
func main() { _ = s }
`,
			},
			wantErr: "//go:immutable arguments are only allowed on function declarations",
		},
		{
			name: "short_var_with_args",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable args0
	s := []int{1}
	_ = s
}
`,
			},
			wantErr: "//go:immutable arguments are only allowed on function declarations",
		},
		{
			name: "anon_func_var_rejected",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	fn := func(s []int) {}
	_ = fn
}
`,
			},
			wantErr: "//go:immutable only applies to slice, array, map, or container variables",
		},
		{
			name: "bodyless_func_with_result_constraint",
			files: map[string]string{
				"main.go": `package main
import _ "unsafe"
//go:linkname ext ext
//go:immutable r0
func ext() []int
func main() {}
`,
			},
			wantErr: "cannot use //go:immutable result constraint on function ext without body",
		},
		// Call-site & callee-body enforcement errors.
		{
			name: "mutate_immutable_param_inside_callee",
			files: map[string]string{
				"main.go": `package main
//go:immutable args0
func foo(s []int) { s[0] = 9 }
func main() {}
`,
			},
			wantErr: "cannot assign to //go:immutable variable s",
		},
		{
			name: "pass_mutable_slice_to_immutable_param",
			files: map[string]string{
				"main.go": `package main
//go:immutable
func foo(s []int) {}
func main() {
	mut := []int{1, 2}
	foo(mut)
}
`,
			},
			wantErr: "cannot pass non-immutable argument as //go:immutable parameter args0 in call to foo",
		},
		{
			name: "pass_raw_composite_literal",
			files: map[string]string{
				"main.go": `package main
//go:immutable args0
func foo(s []int) {}
func main() { foo([]int{1, 2}) }
`,
			},
			wantErr: "cannot pass non-immutable argument as //go:immutable parameter args0 in call to foo",
		},
		{
			name: "pass_string_to_bytes_conversion",
			files: map[string]string{
				"main.go": `package main
//go:immutable args0
func foo(b []byte) {}
func main() { foo([]byte("hello")) }
`,
			},
			wantErr: "cannot pass non-immutable argument as //go:immutable parameter args0 in call to foo",
		},
		{
			name: "cross_pkg_pass_mutable_to_immutable_func",
			files: map[string]string{
				"sub/sub.go": `package sub
//go:immutable args0
func Check(s []string) {}
`,
				"main.go": `package main
import "example.com/bad/sub"
func main() {
	m := []string{"a"}
	sub.Check(m)
}
`,
			},
			wantErr: "cannot pass non-immutable argument as //go:immutable parameter args0 in call to Check",
		},
		{
			name: "cross_pkg_generic_method_mutable_arg",
			files: map[string]string{
				"sub/sub.go": `package sub
type Box[T any] struct{}
//go:immutable args0
func (b *Box[T]) Check(s []T) {}
`,
				"main.go": `package main
import "example.com/bad/sub"
func main() {
	b := &sub.Box[int]{}
	b.Check([]int{1})
}
`,
			},
			wantErr: "cannot pass non-immutable argument as //go:immutable parameter args0 in call to Check",
		},
		{
			name: "return_mutable_slice_from_immutable_result",
			files: map[string]string{
				"main.go": `package main
//go:immutable r0
func makeSlice() []int {
	s := []int{1, 2}
	return s
}
func main() {}
`,
			},
			wantErr: "cannot return non-immutable value for //go:immutable result r0 of makeSlice",
		},
		{
			name: "named_result_assign_mutable_and_bare_return",
			files: map[string]string{
				"main.go": `package main
//go:immutable r
func makeSlice() (out []int) {
	out = []int{1, 2}
	return
}
func main() {}
`,
			},
			wantErr: "cannot assign non-immutable value to //go:immutable result out",
		},
		{
			name: "named_result_defer_overwrite_bypass",
			files: map[string]string{
				"main.go": `package main
import "slices"
//go:immutable r0
func makeSlice() (out []int) {
	defer func() {
		out = []int{9, 9}
	}()
	return slices.ToImmutable([]int{1, 2})
}
func main() {}
`,
			},
			wantErr: "cannot assign non-immutable value to //go:immutable result out",
		},
		{
			name: "named_result_take_addr_bypass",
			files: map[string]string{
				"main.go": `package main
//go:immutable r
func makeArr() (out [2]int) {
	p := &out
	p[0] = 9
	return
}
func main() {}
`,
			},
			wantErr: "cannot take address of //go:immutable result out",
		},
		{
			name: "func_value_escape_local",
			files: map[string]string{
				"main.go": `package main
//go:immutable args0
func secret(s []int) {}
func main() {
	fn := secret
	fn([]int{1})
}
`,
			},
			wantErr: "cannot use //go:immutable function secret as function value",
		},
		{
			name: "method_value_escape",
			files: map[string]string{
				"main.go": `package main
type Svc struct{}
//go:immutable args0
func (Svc) Run(s []int) {}
func main() {
	fn := Svc{}.Run
	fn([]int{1})
}
`,
			},
			wantErr: "cannot use //go:immutable function Run as function value",
		},
		{
			name: "interface_satisfaction_escape",
			files: map[string]string{
				"main.go": `package main
type Runner interface { Run([]int) }
type Svc struct{}
//go:immutable args0
func (Svc) Run(s []int) {}
func main() {
	var r Runner = Svc{}
	r.Run([]int{1})
}
`,
			},
			wantErr: "does not implement Runner (Run method has //go:immutable parameter constraints)",
		},
		{
			name: "generic_constraint_satisfaction_escape",
			files: map[string]string{
				"main.go": `package main
type Runner interface { Run([]int) }
type Svc struct{}
//go:immutable args0
func (Svc) Run(s []int) {}
func run[T Runner](r T) { r.Run([]int{1}) }
func main() { run(Svc{}) }
`,
			},
			wantErr: "does not satisfy Runner (Run method has //go:immutable parameter constraints)",
		},
		{
			name: "pass_unfrozen_pkg_global_during_init",
			files: map[string]string{
				"main.go": `package main
//go:immutable
var Allowed = []string{"GET"}
//go:immutable args0
func Save(s []string) {}
func init() {
	Save(Allowed)
}
func main() {}
`,
			},
			wantErr: "cannot pass package-level //go:immutable variable Allowed before package initialization completes",
		},
		{
			name: "variadic_mutable_elements_rejected",
			files: map[string]string{
				"main.go": `package main
//go:immutable args0
func varFn(xs ...int) {}
func main() { varFn(1, 2, 3) }
`,
			},
			wantErr: "cannot pass mutable variadic arguments as //go:immutable parameter args0 in call to varFn",
		},
		// Container compile-time errors.
		{
			name: "mutate_immutable_list",
			files: map[string]string{
				"main.go": `package main
import "container/list"
func main() {
	//go:immutable
	l := list.New()
	l.PushBack(1)
}
`,
			},
			wantErr: "cannot call mutating method PushBack on //go:immutable variable l",
		},
		{
			name: "mutate_immutable_list_elem_value",
			files: map[string]string{
				"main.go": `package main
import "container/list"
func main() {
	//go:immutable
	l := list.New()
	l.Front().Value = 99
}
`,
			},
			wantErr: "cannot assign to //go:immutable variable l",
		},
		{
			name: "link_mutable_ring_with_immutable_ring",
			files: map[string]string{
				"main.go": `package main
import "container/ring"
func main() {
	//go:immutable
	imm := ring.New(2)
	mut := ring.New(2)
	mut.Link(imm)
}
`,
			},
			wantErr: "cannot link //go:immutable variable imm",
		},
		{
			name: "mutate_immutable_orderedmap",
			files: map[string]string{
				"main.go": `package main
import "sync/v2/container"
func main() {
	//go:immutable
	m := new(container.OrderedMap[string, int])
	m.Store("a", 1)
}
`,
			},
			wantErr: "cannot call mutating method Store on //go:immutable variable m",
		},
		{
			name: "append_unclipped_subslice",
			files: map[string]string{
				"main.go": `package main
func main() {
	//go:immutable
	s := []int64{10, 20, 30}
	_ = append(s[:1], 99)
}
`,
			},
			wantErr: "cannot append into unclipped subslice of //go:immutable slice s",
		},
		{
			name: "append_unclipped_immutable_param",
			files: map[string]string{
				"main.go": `package main
//go:immutable args0
func extend(p []int64) []int64 {
	return append(p, 99)
}
func main() {}
`,
			},
			wantErr: "cannot append directly to //go:immutable slice p without clipping capacity",
		},
		{
			name: "variadic_tuple_forwarding_bypass",
			files: map[string]string{
				"main.go": `package main
//go:immutable args0
func varSlices(xs ...[]int) {}
//go:immutable r0 r1
func pair() ([]int, []int) { return nil, nil }
func main() { varSlices(pair()) }
`,
			},
			wantErr: "cannot pass mutable variadic arguments as //go:immutable parameter args0 in call to varSlices",
		},
		{
			name: "spoofed_container_to_immutable_rejected",
			files: map[string]string{
				"main.go": `package main
import "sync/v2/container"
type FakeMap map[string]int
func (f FakeMap) ToImmutable() FakeMap { return f }
func (f FakeMap) IsImmutable() bool { return true }
//go:immutable args0
func trusted(m FakeMap) {}
func main() {
	raw := FakeMap{"k": 1}
	trusted(container.ToImmutable(raw))
}
`,
			},
			wantErr: "cannot pass non-immutable argument as //go:immutable parameter args0 in call to trusted",
		},
		{
			name: "typeparam_container_without_method_rejected",
			files: map[string]string{
				"main.go": `package main
import "container/list"
func bad[C ~*list.List](c C) {
	//go:immutable
	x := c
	_ = x
}
func main() {}
`,
			},
			wantErr: "//go:immutable only applies to slice, array, map, or container variables",
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
