// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package reflect_test

import (
	"math"
	. "reflect"
	"slices"
	"testing"
	"time"
	"unsafe"
)

type fieldCacheInner struct{ X int }

type fieldCacheInner2 struct{ X, Y int }

type fieldCacheOuter struct {
	A int
	fieldCacheInner
}

func TestFieldByNameCacheIndexNotShared(t *testing.T) {
	typ := TypeOf(fieldCacheOuter{})
	for range 3 {
		f, ok := typ.FieldByName("X")
		if !ok {
			t.Fatal("FieldByName(X) not found")
		}
		if !slices.Equal(f.Index, []int{1, 0}) {
			t.Fatalf("Index = %v, want [1 0]", f.Index)
		}
		// Modifying the returned slice must not affect later lookups.
		f.Index[0] = 99
		f.Index[1] = 99
	}
	v := ValueOf(&fieldCacheOuter{fieldCacheInner: fieldCacheInner{X: 7}}).Elem()
	for range 3 {
		if got := v.FieldByName("X"); !got.IsValid() || got.Int() != 7 {
			t.Fatalf("Value.FieldByName(X) = %v, want 7", got)
		}
	}
	if v.FieldByName("Nope").IsValid() || v.FieldByName("").IsValid() {
		t.Fatal("Value.FieldByName found a nonexistent field")
	}
	if _, ok := typ.FieldByName("Nope"); ok {
		t.Fatal("Type.FieldByName found a nonexistent field")
	}
}

func TestFieldByNameCacheNegative(t *testing.T) {
	typ := TypeOf(fieldCacheOuter{})
	long := string(make([]byte, 1000))
	for range 3 {
		for _, name := range []string{"Nope", "x", long} {
			if f, ok := typ.FieldByName(name); ok || !slices.Equal(f.Index, nil) {
				t.Fatalf("FieldByName(%q) = %v, %v; want not found", name, f, ok)
			}
		}
	}
	// An ambiguous name is reported as missing, repeatedly.
	type amb struct {
		fieldCacheInner
		fieldCacheInner2
		A int
	}
	for range 3 {
		if _, ok := TypeOf(amb{}).FieldByName("A"); !ok {
			t.Fatal("A should be found at depth 0")
		}
		if _, ok := TypeOf(amb{}).FieldByName("X"); ok {
			t.Fatal("X is ambiguous at depth 1 and should not be found")
		}
		if f, ok := TypeOf(amb{}).FieldByName("Y"); !ok || !slices.Equal(f.Index, []int{1, 1}) {
			t.Fatalf("FieldByName(Y) = %v, %v", f.Index, ok)
		}
	}
}

func BenchmarkFieldByNameEmbedded(b *testing.B) {
	t := TypeOf(fieldCacheOuter{})
	v := ValueOf(&fieldCacheOuter{}).Elem()
	b.Run("Type", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			t.FieldByName("X")
		}
	})
	b.Run("Value", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			v.FieldByName("X")
		}
	})
}

func TestMethodCacheStable(t *testing.T) {
	typ := TypeOf(time.Time{})
	for i := range typ.NumMethod() {
		m1, m2 := typ.Method(i), typ.Method(i)
		if m1.Name != m2.Name || m1.Index != i || m1.Type != m2.Type || m1.Func.Pointer() != m2.Func.Pointer() {
			t.Fatalf("Method(%d) not stable: %v vs %v", i, m1, m2)
		}
	}
	m, ok := typ.MethodByName("Unix")
	if !ok {
		t.Fatal("Unix not found")
	}
	out := m.Func.Call([]Value{ValueOf(time.Unix(1234, 0))})
	if out[0].Int() != 1234 {
		t.Fatalf("Unix via Method.Func = %v, want 1234", out[0])
	}
}

type isZeroInc struct {
	a int8
	b [2]func()
	c string
	_ int // blank fields are ignored
	d []int
	e any
	f float64
	g complex64
	h [2]struct {
		m map[int]int
		s string
	}
}

func TestIsZeroNested(t *testing.T) {
	negZero := math.Copysign(0, -1)
	emptyNonNil := "abc"[1:1]
	mod := []struct {
		name string
		f    func(*isZeroInc)
		zero bool
	}{
		{"none", func(*isZeroInc) {}, true},
		{"int8", func(x *isZeroInc) { x.a = 1 }, false},
		{"func", func(x *isZeroInc) { x.b[1] = func() {} }, false},
		{"string", func(x *isZeroInc) { x.c = "x" }, false},
		{"emptyString", func(x *isZeroInc) { x.c = emptyNonNil }, true},
		{"blank", func(x *isZeroInc) {
			*(*int)(unsafe.Pointer(ValueOf(x).Elem().Field(3).UnsafeAddr())) = 5
		}, true},
		{"emptySlice", func(x *isZeroInc) { x.d = []int{} }, false},
		{"iface", func(x *isZeroInc) { x.e = 0 }, false},
		{"negZero", func(x *isZeroInc) { x.f = negZero }, true},
		{"nan", func(x *isZeroInc) { x.f = math.NaN() }, false},
		{"complex", func(x *isZeroInc) { x.g = complex(0, 1) }, false},
		{"nestedMap", func(x *isZeroInc) { x.h[1].m = map[int]int{} }, false},
		{"nestedString", func(x *isZeroInc) { x.h[0].s = "y" }, false},
	}
	for _, m := range mod {
		var x isZeroInc
		m.f(&x)
		if got := ValueOf(x).IsZero(); got != m.zero {
			t.Errorf("%s: IsZero(struct) = %v, want %v", m.name, got, m.zero)
		}
		arr := [3]isZeroInc{}
		m.f(&arr[2])
		if got := ValueOf(arr).IsZero(); got != m.zero {
			t.Errorf("%s: IsZero(array) = %v, want %v", m.name, got, m.zero)
		}
		if got := ValueOf(&x).Elem().IsZero(); got != m.zero {
			t.Errorf("%s: IsZero(addressable) = %v, want %v", m.name, got, m.zero)
		}
	}
}

type callRecv struct{ n int }

func (r *callRecv) Add(a, b int) int { return r.n + a + b }

func BenchmarkCallSerial(b *testing.B) {
	b.Run("NoRet", func(b *testing.B) {
		fv := ValueOf(func(a, b string) {})
		args := []Value{ValueOf("a"), ValueOf("b")}
		b.ReportAllocs()
		for b.Loop() {
			fv.Call(args)
		}
	})
	b.Run("IntRet", func(b *testing.B) {
		fv := ValueOf(func(a, b int) int { return a + b })
		args := []Value{ValueOf(1), ValueOf(2)}
		b.ReportAllocs()
		for b.Loop() {
			fv.Call(args)
		}
	})
	b.Run("StringRet", func(b *testing.B) {
		fv := ValueOf(func(a string) string { return a })
		args := []Value{ValueOf("a")}
		b.ReportAllocs()
		for b.Loop() {
			fv.Call(args)
		}
	})
	b.Run("Method", func(b *testing.B) {
		fv := ValueOf(&callRecv{n: 1}).MethodByName("Add")
		args := []Value{ValueOf(1), ValueOf(2)}
		b.ReportAllocs()
		for b.Loop() {
			fv.Call(args)
		}
	})
	b.Run("Variadic", func(b *testing.B) {
		fv := ValueOf(func(a ...int) int { return len(a) })
		args := []Value{ValueOf(1), ValueOf(2), ValueOf(3)}
		b.ReportAllocs()
		for b.Loop() {
			fv.Call(args)
		}
	})
	b.Run("Iface", func(b *testing.B) {
		fv := ValueOf(func(a any) {})
		args := []Value{ValueOf(1)}
		b.ReportAllocs()
		for b.Loop() {
			fv.Call(args)
		}
	})
}

type callIface interface{ Add(a, b int) int }

func TestCallMethodForms(t *testing.T) {
	r := &callRecv{n: 10}
	// Concrete receiver, repeatedly (the code pointer cell is per call).
	m := ValueOf(r).MethodByName("Add")
	for i := range 3 {
		if got := m.Call([]Value{ValueOf(i), ValueOf(1)})[0].Int(); got != int64(11+i) {
			t.Fatalf("concrete Add = %d", got)
		}
	}
	// Interface receiver.
	var ci callIface = r
	mi := ValueOf(&ci).Elem().MethodByName("Add")
	if got := mi.Call([]Value{ValueOf(1), ValueOf(2)})[0].Int(); got != 13 {
		t.Fatalf("interface Add = %d", got)
	}
	// As a real func through Interface (goes through callMethod).
	f := m.Interface().(func(int, int) int)
	if got := f(2, 3); got != 15 {
		t.Fatalf("method value = %d", got)
	}
	f = mi.Interface().(func(int, int) int)
	if got := f(2, 3); got != 15 {
		t.Fatalf("interface method value = %d", got)
	}
}

func TestCallVariadicForms(t *testing.T) {
	sum := func(pre string, xs ...int) string {
		s := pre
		for _, x := range xs {
			s += string(rune('0' + x))
		}
		return s
	}
	fv := ValueOf(sum)
	for n := range 12 { // also more arguments than fit in any small stack buffer
		args := []Value{ValueOf("p")}
		want := "p"
		for i := range n {
			args = append(args, ValueOf(i%10))
			want += string(rune('0' + i%10))
		}
		if got := fv.Call(args)[0].String(); got != want {
			t.Fatalf("Call with %d variadic args = %q, want %q", n, got, want)
		}
		// The caller's slice must not be modified.
		if len(args) != n+1 {
			t.Fatalf("args length changed")
		}
	}
	// CallSlice with an explicit slice.
	if got := fv.CallSlice([]Value{ValueOf("q"), ValueOf([]int{1, 2})})[0].String(); got != "q12" {
		t.Fatalf("CallSlice = %q", got)
	}
	// Interface-typed variadic elements, with values of several types.
	anyv := ValueOf(func(xs ...any) int { return len(xs) })
	if got := anyv.Call([]Value{ValueOf(1), ValueOf("a"), ValueOf(2.5)})[0].Int(); got != 3 {
		t.Fatalf("[]any variadic = %d", got)
	}
	if got := anyv.Call(nil)[0].Int(); got != 0 {
		t.Fatalf("empty []any variadic = %d", got)
	}
	// A value that is not assignable to the element type still panics.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("no panic for unassignable variadic argument")
			}
		}()
		fv.Call([]Value{ValueOf("p"), ValueOf("not an int")})
	}()
}

func TestSelectSharedRecvBuffer(t *testing.T) {
	const n = 6
	chans := make([]chan string, n)
	cases := make([]SelectCase, n)
	for i := range chans {
		chans[i] = make(chan string, 1)
		cases[i] = SelectCase{Dir: SelectRecv, Chan: ValueOf(chans[i])}
	}
	for i := n - 1; i >= 0; i-- {
		chans[i] <- "v" + string(rune('0'+i))
		chosen, v, ok := Select(cases)
		if chosen != i || !ok || v.String() != "v"+string(rune('0'+i)) {
			t.Fatalf("Select = %d, %v, %v; want %d", chosen, v, ok, i)
		}
	}
	// Mixed element types do not share a buffer.
	ci, cs := make(chan int, 1), make(chan string, 1)
	mixed := []SelectCase{
		{Dir: SelectRecv, Chan: ValueOf(ci)},
		{Dir: SelectRecv, Chan: ValueOf(cs)},
		{Dir: SelectRecv, Chan: ValueOf(ci)},
	}
	cs <- "x"
	if chosen, v, _ := Select(mixed); chosen != 1 || v.String() != "x" {
		t.Fatalf("mixed Select = %d, %v", chosen, v)
	}
	ci <- 7
	if chosen, v, _ := Select(mixed); (chosen != 0 && chosen != 2) || v.Int() != 7 {
		t.Fatalf("mixed Select = %d, %v", chosen, v)
	}
}

type deepKey struct {
	a int
	b string
}

type deepCyc struct {
	next *deepCyc
	s    []string
}

func TestDeepEqualFastPaths(t *testing.T) {
	type pad struct { // not regular memory: has padding
		a int8
		b int64
	}
	type zero struct{}
	big := func(n int) [64]int { var a [64]int; a[0] = n; return a }

	tests := []struct {
		name string
		x, y any
		want bool
	}{
		{"ints equal", []int{1, 2, 3}, []int{1, 2, 3}, true},
		{"ints differ", []int{1, 2, 3}, []int{1, 2, 4}, false},
		{"ints len", []int{1, 2, 3}, []int{1, 2}, false},
		{"empty vs nil", []int{}, []int(nil), false},
		{"int32 empty", []int32{}, []int32{}, true},
		{"bool", []bool{true, false}, []bool{true, false}, true},
		{"bool differ", []bool{true, false}, []bool{true, true}, false},
		{"arrays of structs", [][2]deepKey{{{1, "a"}, {2, "b"}}}, [][2]deepKey{{{1, "a"}, {2, "b"}}}, true},
		{"zero size", []zero{{}, {}}, []zero{{}, {}}, true},
		{"padded struct slice", []pad{{1, 2}, {3, 4}}, []pad{{1, 2}, {3, 4}}, true},
		{"padded struct slice differ", []pad{{1, 2}, {3, 4}}, []pad{{1, 2}, {3, 5}}, false},
		{"float slice NaN", []float64{math.NaN()}, []float64{math.NaN()}, false},
		{"float slice zero", []float64{0}, []float64{math.Copysign(0, -1)}, true},
		{"strings", []string{"a", "b"}, []string{"a", "b"}, true},
		{"strings differ", []string{"a", "b"}, []string{"a", "c"}, false},
		{"big array slice", [][64]int{big(1), big(2)}, [][64]int{big(1), big(2)}, true},
		{"big array slice differ", [][64]int{big(1), big(2)}, [][64]int{big(1), big(3)}, false},

		{"map scalar", map[int]int{1: 1, 2: 2}, map[int]int{2: 2, 1: 1}, true},
		{"map scalar value differs", map[int]int{1: 1, 2: 2}, map[int]int{1: 1, 2: 3}, false},
		{"map scalar key differs", map[int]int{1: 1, 2: 2}, map[int]int{1: 1, 3: 2}, false},
		{"map string", map[string]string{"a": "b"}, map[string]string{"a": "b"}, true},
		{"map string differs", map[string]string{"a": "b"}, map[string]string{"a": "c"}, false},
		{"map struct key", map[deepKey]int{{1, "a"}: 1}, map[deepKey]int{{1, "a"}: 1}, true},
		{"map struct key differs", map[deepKey]int{{1, "a"}: 1}, map[deepKey]int{{1, "b"}: 1}, false},
		{"map big value", map[int][64]int{1: big(1)}, map[int][64]int{1: big(1)}, true},
		{"map big value differs", map[int][64]int{1: big(1)}, map[int][64]int{1: big(2)}, false},
		{"map big key", map[[64]int]int{big(1): 1}, map[[64]int]int{big(1): 1}, true},
		{"map slice value", map[string][]int{"a": {1}}, map[string][]int{"a": {1}}, true},
		{"map slice value differs", map[string][]int{"a": {1}}, map[string][]int{"a": {2}}, false},
		{"map pointer value", map[int]*int{1: new(int)}, map[int]*int{1: new(int)}, true},
		{"map nested", map[string]map[string]int{"a": {"b": 1}}, map[string]map[string]int{"a": {"b": 1}}, true},
		{"map nested differs", map[string]map[string]int{"a": {"b": 1}}, map[string]map[string]int{"a": {"b": 2}}, false},
		{"map any", map[string]any{"a": 1, "b": []int{1}}, map[string]any{"a": 1, "b": []int{1}}, true},
		{"map any differs", map[string]any{"a": 1, "b": []int{1}}, map[string]any{"a": 1, "b": []int{2}}, false},
		{"map nil vs empty", map[int]int{}, map[int]int(nil), false},
		{"map NaN keys", map[float64]int{math.NaN(): 1}, map[float64]int{math.NaN(): 1}, false},
		{"map len", map[int]int{1: 1}, map[int]int{1: 1, 2: 2}, false},

		{"pointer to int", new(int), new(int), true},
		{"pointer to struct", &deepKey{1, "a"}, &deepKey{1, "a"}, true},
		{"pointer to struct differs", &deepKey{1, "a"}, &deepKey{1, "b"}, false},
	}
	for _, tt := range tests {
		for range 2 { // map iteration order varies
			if got := DeepEqual(tt.x, tt.y); got != tt.want {
				t.Errorf("%s: DeepEqual = %v, want %v", tt.name, got, tt.want)
			}
		}
	}

	// Cycles must still terminate, including through slices and maps.
	a, b := &deepCyc{s: []string{"x"}}, &deepCyc{s: []string{"x"}}
	a.next, b.next = a, b
	if !DeepEqual(a, b) {
		t.Error("cyclic structs not DeepEqual")
	}
	type cyclicMap map[string]cyclicMap
	m1, m2 := cyclicMap{}, cyclicMap{}
	m1["self"], m2["self"] = m1, m2
	if !DeepEqual(m1, m2) {
		t.Error("cyclic maps not DeepEqual")
	}
	type cyclicSlice []any
	s1, s2 := cyclicSlice{nil}, cyclicSlice{nil}
	s1[0], s2[0] = s1, s2
	if !DeepEqual(s1, s2) {
		t.Error("cyclic slices not DeepEqual")
	}
}

func TestVisibleFieldsFlat(t *testing.T) {
	type one struct {
		A int
		_ int
		b string
	}
	type two struct {
		A int
		_ int
		_ string
		b string
	}
	type three struct {
		_ int
		_ int
		_ int
	}
	for _, tt := range []struct {
		v    any
		want []string
		idx  [][]int
	}{
		{one{}, []string{"A", "_", "b"}, [][]int{{0}, {1}, {2}}},
		{two{}, []string{"A", "b"}, [][]int{{0}, {3}}},
		{three{}, nil, nil},
		{struct{}{}, nil, nil},
	} {
		got := VisibleFields(TypeOf(tt.v))
		var names []string
		for i, f := range got {
			names = append(names, f.Name)
			if !slices.Equal(f.Index, tt.idx[i]) {
				t.Errorf("%T: field %s Index = %v, want %v", tt.v, f.Name, f.Index, tt.idx[i])
			}
		}
		if !slices.Equal(names, tt.want) {
			t.Errorf("%T: VisibleFields = %v, want %v", tt.v, names, tt.want)
		}
		// The results must be freely modifiable, and independent of each other.
		for _, f := range got {
			f.Index[0] = 99
			_ = append(f.Index, 100)
		}
		for i, f := range VisibleFields(TypeOf(tt.v)) {
			if !slices.Equal(f.Index, tt.idx[i]) {
				t.Errorf("%T: modified Index leaked into a later call: %v", tt.v, f.Index)
			}
		}
		for i := 1; i < len(got); i++ {
			if got[i].Index[0] != 99 {
				t.Errorf("%T: Index slices overlap", tt.v)
			}
		}
	}
}

func TestComparableNested(t *testing.T) {
	type inner struct {
		a any
		b [2]any
	}
	type outer struct {
		i inner
		s string
	}
	tests := []struct {
		v    any
		want bool
	}{
		{outer{}, true},
		{outer{i: inner{a: 1}}, true},
		{outer{i: inner{a: []int{1}}}, false},
		{outer{i: inner{b: [2]any{1, func() {}}}}, false},
		{[0]func(){}, false},
		{[2]struct{ f func() }{}, false},
		{[2]any{1, "a"}, true},
		{[2]any{1, map[int]int{}}, false},
		{struct{ p *int }{}, true},
		{struct{ f func() }{}, false},
	}
	for _, tt := range tests {
		if got := ValueOf(tt.v).Comparable(); got != tt.want {
			t.Errorf("Comparable(%T %v) = %v, want %v", tt.v, tt.v, got, tt.want)
		}
	}
}

func TestSwapperOptimizations(t *testing.T) {
	// Interface slice
	ifaces := []any{1, "two", 3.0}
	sw := Swapper(ifaces)
	sw(0, 2)
	if ifaces[0] != 3.0 || ifaces[2] != 1 {
		t.Fatalf("Swapper interface slice failed: %v", ifaces)
	}

	// 16-byte struct slice
	type pair struct{ a, b int64 }
	pairs := []pair{{1, 2}, {3, 4}, {5, 6}}
	swP := Swapper(pairs)
	swP(0, 2)
	if pairs[0] != (pair{5, 6}) || pairs[2] != (pair{1, 2}) {
		t.Fatalf("Swapper 16-byte slice failed: %v", pairs)
	}
}

func TestAppendSliceOptimized(t *testing.T) {
	s1 := ValueOf([]int{1, 2})
	s2 := ValueOf([]int{3, 4, 5})
	res := AppendSlice(s1, s2)
	if got := res.Interface().([]int); !slices.Equal(got, []int{1, 2, 3, 4, 5}) {
		t.Fatalf("AppendSlice = %v, want [1 2 3 4 5]", got)
	}

	// Empty append
	empty := ValueOf([]int{})
	res2 := AppendSlice(s1, empty)
	if got := res2.Interface().([]int); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("AppendSlice empty = %v, want [1 2]", got)
	}

	// Append multiple values directly
	res3 := Append(s1, ValueOf(10), ValueOf(20))
	if got := res3.Interface().([]int); !slices.Equal(got, []int{1, 2, 10, 20}) {
		t.Fatalf("Append = %v, want [1 2 10 20]", got)
	}
}

func TestConvertAndCanConvert(t *testing.T) {
	v := ValueOf(65)
	if !v.CanConvert(TypeFor[string]()) {
		t.Fatal("int should be convertible to string")
	}
	s := v.Convert(TypeFor[string]())
	if s.String() != "A" {
		t.Fatalf("Convert(int -> string) = %q, want 'A'", s.String())
	}

	b := ValueOf([]byte("hello"))
	str := b.Convert(TypeFor[string]())
	if str.String() != "hello" {
		t.Fatalf("Convert([]byte -> string) = %q, want 'hello'", str.String())
	}

	strVal := ValueOf("world")
	bVal := strVal.Convert(TypeFor[[]byte]())
	if got := bVal.Bytes(); !slices.Equal(got, []byte("world")) {
		t.Fatalf("Convert(string -> []byte) = %s, want 'world'", got)
	}

	rVal := strVal.Convert(TypeFor[[]rune]())
	if got := rVal.Interface().([]rune); !slices.Equal(got, []rune("world")) {
		t.Fatalf("Convert(string -> []rune) = %v, want 'world'", got)
	}
}

func TestIteratorsAndMapKeys(t *testing.T) {
	// Seq and Seq2
	slice := []int{10, 20, 30}
	v := ValueOf(slice)
	var indices []int
	for idx := range v.Seq() {
		indices = append(indices, int(idx.Int()))
	}
	if !slices.Equal(indices, []int{0, 1, 2}) {
		t.Fatalf("Seq indices = %v, want [0 1 2]", indices)
	}

	var idx2, val2 []int
	for i, val := range v.Seq2() {
		idx2 = append(idx2, int(i.Int()))
		val2 = append(val2, int(val.Int()))
	}
	if !slices.Equal(idx2, []int{0, 1, 2}) || !slices.Equal(val2, []int{10, 20, 30}) {
		t.Fatalf("Seq2 = %v, %v", idx2, val2)
	}

	// MapKeys empty map
	emptyMap := ValueOf(map[string]int{})
	if keys := emptyMap.MapKeys(); len(keys) != 0 || keys == nil {
		t.Fatalf("MapKeys on empty map = %v, want non-nil empty slice", keys)
	}
	nilMap := ValueOf((map[string]int)(nil))
	if keys := nilMap.MapKeys(); len(keys) != 0 || keys == nil {
		t.Fatalf("MapKeys on nil map = %v, want non-nil empty slice", keys)
	}

	// Fields and Methods iterators
	type testStruct struct {
		X int
		Y string
	}
	ts := testStruct{X: 1, Y: "abc"}
	vts := ValueOf(ts)
	var fNames []string
	for sf, val := range vts.Fields() {
		fNames = append(fNames, sf.Name)
		if sf.Name == "X" && val.Int() != 1 {
			t.Fatalf("Field X value = %v", val)
		}
	}
	if !slices.Equal(fNames, []string{"X", "Y"}) {
		t.Fatalf("Fields = %v, want [X Y]", fNames)
	}
}

func BenchmarkVisibleFields(b *testing.B) {
	type flat struct {
		A, B, C, D int
		S          string
	}
	b.Run("Flat", func(b *testing.B) {
		t := TypeOf(flat{})
		b.ReportAllocs()
		for b.Loop() {
			VisibleFields(t)
		}
	})
	b.Run("Embedded", func(b *testing.B) {
		t := TypeOf(fieldCacheOuter{})
		b.ReportAllocs()
		for b.Loop() {
			VisibleFields(t)
		}
	})
}

func BenchmarkAppend(b *testing.B) {
	s := ValueOf(make([]int, 0, 10))
	val := ValueOf(42)
	b.ReportAllocs()
	for b.Loop() {
		Append(s, val)
	}
}

func BenchmarkAppendSlice(b *testing.B) {
	s := ValueOf(make([]int, 0, 10))
	t := ValueOf([]int{1, 2, 3})
	b.ReportAllocs()
	for b.Loop() {
		AppendSlice(s, t)
	}
}

func BenchmarkConvert(b *testing.B) {
	v := ValueOf(65)
	t := TypeFor[string]()
	b.Run("IntToString", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			v.Convert(t)
		}
	})
	b.Run("CanConvert", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			v.CanConvert(t)
		}
	})
}

func BenchmarkSwapper(b *testing.B) {
	b.Run("Interface", func(b *testing.B) {
		slice := []any{1, "two", 3.0}
		b.ReportAllocs()
		for b.Loop() {
			Swapper(slice)
		}
	})
	b.Run("Int64Pair", func(b *testing.B) {
		type pair struct{ a, b int64 }
		slice := []pair{{1, 2}, {3, 4}}
		b.ReportAllocs()
		for b.Loop() {
			Swapper(slice)
		}
	})
}

func BenchmarkMapKeys(b *testing.B) {
	b.Run("Empty", func(b *testing.B) {
		m := ValueOf(map[string]int{})
		b.ReportAllocs()
		for b.Loop() {
			m.MapKeys()
		}
	})
	b.Run("NonEmpty", func(b *testing.B) {
		m := ValueOf(map[string]int{"a": 1, "b": 2})
		b.ReportAllocs()
		for b.Loop() {
			m.MapKeys()
		}
	})
}

func BenchmarkSeq(b *testing.B) {
	v := ValueOf([]int{1, 2, 3, 4, 5})
	b.Run("Seq", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for range v.Seq() {
			}
		}
	})
	b.Run("Seq2", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for range v.Seq2() {
			}
		}
	})
}

func BenchmarkComparable(b *testing.B) {
	type str struct {
		a int
		b string
		c [4]float64
	}
	v := ValueOf(str{a: 1, b: "hello"})
	b.ReportAllocs()
	for b.Loop() {
		v.Comparable()
	}
}

func BenchmarkEqual(b *testing.B) {
	type reg struct{ a, b, c, d int64 }
	v1 := ValueOf(reg{1, 2, 3, 4})
	v2 := ValueOf(reg{1, 2, 3, 4})
	b.Run("RegularMemory", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			v1.Equal(v2)
		}
	})
	s1 := ValueOf("hello world")
	s2 := ValueOf("hello world")
	b.Run("String", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			s1.Equal(s2)
		}
	})
}
