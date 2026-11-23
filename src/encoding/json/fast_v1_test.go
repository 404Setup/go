// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFastV1GenericMaps(t *testing.T) {
	type item struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	// >128 bytes forces IndirectElem in internal/runtime/maps.
	type bigItem struct {
		ID   int64   `json:"id"`
		Vals [24]int `json:"vals"`
	}
	type myStr string
	type payload struct {
		StrStruct map[string]item              `json:"str_struct"`
		Nested    map[string]map[string]string `json:"nested"`
		Named     map[myStr]int                `json:"named"`
		Int64Map  map[int64]item               `json:"int64_map"`
		Uint64Map map[uint64][]uint64          `json:"uint64_map"`
		BigMap    map[int64]bigItem            `json:"big_map"`
		EmptyOmit map[int64]int                `json:"empty_omit,omitempty"`
		ZeroOmit  map[string]item              `json:"zero_omit,omitzero"`
		NilMap    map[int64]string             `json:"nil_map"`
	}

	in := payload{
		StrStruct: map[string]item{"b": {Name: "two", Count: 2}, "a": {Name: "one", Count: 1}},
		Nested:    map[string]map[string]string{"outer": {"z": "1", "a": "2"}},
		Named:     map[myStr]int{"k": 42},
		Int64Map:  map[int64]item{2: {Name: "two"}, 10: {Name: "ten"}, -1: {Name: "m1"}, -10: {Name: "m10"}},
		Uint64Map: map[uint64][]uint64{2: {1, 2}, 10: {3}},
		BigMap:    map[int64]bigItem{7: {ID: 7, Vals: [24]int{1, 23: 99}}},
		EmptyOmit: map[int64]int{},
		ZeroOmit:  nil,
		NilMap:    nil,
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal(payload): %v", err)
	}
	s := string(b)
	// Verify lexicographical ordering of integer keys: "-1" < "-10" < "10" < "2".
	wantInt := `"int64_map":{"-1":{"name":"m1","count":0},"-10":{"name":"m10","count":0},"10":{"name":"ten","count":0},"2":{"name":"two","count":0}}`
	if !strings.Contains(s, wantInt) {
		t.Fatalf("int64_map ordering mismatch:\ngot:  %s\nwant: %s", s, wantInt)
	}
	if !strings.Contains(s, `"nil_map":null`) {
		t.Fatalf("expected nil_map:null in %s", s)
	}
	if strings.Contains(s, `"empty_omit"`) || strings.Contains(s, `"zero_omit"`) {
		t.Fatalf("omitempty/omitzero failed to omit map fields: %s", s)
	}

	var got payload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	got.EmptyOmit = map[int64]int{}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("roundtrip mismatch:\ngot:  %+v\nwant: %+v", got, in)
	}

	// Duplicate keys in v1 overwrite with zeroed entry (do not merge fields).
	var dupMap map[string]item
	if err := json.Unmarshal([]byte(`{"k":{"name":"first","count":1},"k":{"count":2}}`), &dupMap); err != nil {
		t.Fatalf("Unmarshal duplicate map key: %v", err)
	}
	if dupMap["k"] != (item{Name: "", Count: 2}) {
		t.Fatalf("v1 duplicate map overwrite = %+v, want {Name:\"\" Count:2}", dupMap["k"])
	}
}

func TestFastV1QuotedNumericFields(t *testing.T) {
	type S struct {
		I   int     `json:"i,string"`
		I64 int64   `json:"i64,string"`
		U64 uint64  `json:"u64,string"`
		F32 float32 `json:"f32,string"`
		F64 float64 `json:"f64,string"`
	}
	in := S{I: -7, I64: -9223372036854775808, U64: 18446744073709551615, F32: 1.5, F64: -3.25}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"i":"-7","i64":"-9223372036854775808","u64":"18446744073709551615","f32":"1.5","f64":"-3.25"}`
	if string(b) != want {
		t.Fatalf("Marshal = %s, want %s", b, want)
	}
	var got S
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != in {
		t.Fatalf("Unmarshal = %+v, want %+v", got, in)
	}

	// v1 treats both unquoted null and quoted "null" as no-op on ,string fields.
	got = S{I: 11, I64: 22}
	if err := json.Unmarshal([]byte(`{"i":null,"i64":"null"}`), &got); err != nil {
		t.Fatalf("Unmarshal null/\"null\": %v", err)
	}
	if got.I != 11 || got.I64 != 22 {
		t.Fatalf("null/\"null\" mutated fields: %+v", got)
	}

	// Invalid syntax, overflow, and unquoted numbers must fail.
	for _, bad := range []string{`{"i":"abc"}`, `{"i64":"999999999999999999999999"}`, `{"i":123}`} {
		if err := json.Unmarshal([]byte(bad), &got); err == nil {
			t.Errorf("Unmarshal(%s) succeeded, want error", bad)
		}
	}
}

func TestFastV1TimeAndRawMessage(t *testing.T) {
	type doc struct {
		When time.Time       `json:"when"`
		Raw  json.RawMessage `json:"raw"`
		HTML json.RawMessage `json:"html"`
		Omit json.RawMessage `json:"omit,omitempty"`
	}
	ts := time.Date(2026, 11, 22, 3, 4, 5, 123456789, time.UTC)
	in := doc{
		When: ts,
		Raw:  json.RawMessage(` { "a" : 1 , "b" : [ 2 , 3 ] } `),
		HTML: json.RawMessage(`"<tag>&</tag>"`),
		Omit: nil,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal(doc): %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"raw":{"a":1,"b":[2,3]}`) {
		t.Fatalf("RawMessage not compacted: %s", s)
	}
	if !strings.Contains(s, `\u003ctag\u003e\u0026\u003c/tag\u003e`) {
		t.Fatalf("RawMessage HTML not escaped: %s", s)
	}
	if strings.Contains(s, `"omit"`) {
		t.Fatalf("nil RawMessage with omitempty not omitted: %s", s)
	}

	var got doc
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal(doc): %v", err)
	}
	if !got.When.Equal(ts) || string(got.Raw) != `{"a":1,"b":[2,3]}` {
		t.Fatalf("Unmarshal(doc) = %+v", got)
	}

	// Invalid RawMessage must fail Marshal.
	if _, err := json.Marshal(doc{Raw: json.RawMessage(`{invalid`)}); err == nil {
		t.Error("Marshal(invalid RawMessage) succeeded, want error")
	}
}

func TestFastV1FloatEdgeCases(t *testing.T) {
	vals := []float64{
		0, math.Copysign(0, -1), 1, -1,
		float64(1<<52 - 1), -float64(1<<52 - 1),
		float64(1 << 52), -float64(1 << 52),
		float64(1<<53 - 1), 9007199254740993,
		1e22, 1e23, 1e-22, 1e-23,
		math.SmallestNonzeroFloat64, math.MaxFloat64,
	}
	for _, v := range vals {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal(%g): %v", v, err)
		}
		var got float64
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if math.Float64bits(got) != math.Float64bits(v) {
			t.Errorf("float64 roundtrip(%g) = %s -> %g (0x%x != 0x%x)", v, b, got, math.Float64bits(got), math.Float64bits(v))
		}
	}
}

func TestFastV1DeepNestingAndErrorPreservation(t *testing.T) {
	// Deeply nested unknown value must not overflow stack.
	nested := strings.Repeat("[", 5000) + "1" + strings.Repeat("]", 5000)
	type S struct {
		Keep int `json:"keep"`
	}
	var s S
	if err := json.Unmarshal([]byte(`{"skip":`+nested+`,"keep":7}`), &s); err != nil || s.Keep != 7 {
		t.Fatalf("deep skip Unmarshal = %+v, %v", s, err)
	}
	tooDeep := strings.Repeat("[", 10050) + "1" + strings.Repeat("]", 10050)
	if json.Valid([]byte(tooDeep)) {
		t.Error("json.Valid accepted depth 10050")
	}

	// Non-zero target must be untouched on syntax error.
	dst := struct {
		A int           `json:"a"`
		M map[int64]int `json:"m"`
		T time.Time     `json:"t"`
	}{
		A: 99,
		M: map[int64]int{1: 2},
		T: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := json.Unmarshal([]byte(`{"a":1,"m":{"1":9},"t":"2026-01-01T00:00:00Z",`), &dst); err == nil {
		t.Fatal("expected syntax error")
	}
	if dst.A != 99 || dst.M[1] != 2 || dst.T.Year() != 2025 {
		t.Fatalf("target mutated on syntax error: %+v", dst)
	}
}

func BenchmarkFastV1GenericMap(b *testing.B) {
	type entry struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Flag bool   `json:"flag"`
	}
	m := make(map[int64]entry, 64)
	for i := range int64(64) {
		m[i*13+1] = entry{ID: i, Name: "item_" + strconv.FormatInt(i, 10), Flag: i%2 == 0}
	}
	data, err := json.Marshal(m)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("Marshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			if _, err := json.Marshal(m); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Unmarshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			var dst map[int64]entry
			if err := json.Unmarshal(data, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
}

type fastV1CountMarshaler struct {
	calls *int
	out   []byte
	err   error
}

func (m fastV1CountMarshaler) MarshalJSON() ([]byte, error) {
	*m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.out, nil
}

type fastV1DirectMarshaler struct {
	calls *int
}

func (m fastV1DirectMarshaler) MarshalJSON() ([]byte, error) {
	*m.calls++
	return []byte(`{"ok":"<tag>\u0020\u2028&end"}`), nil
}

type fastV1CountUnmarshaler struct {
	calls *int
	err   error
	val   string
}

func (u *fastV1CountUnmarshaler) UnmarshalJSON(data []byte) error {
	if u.calls != nil {
		*u.calls++
	}
	if u.err != nil {
		return u.err
	}
	u.val = string(data)
	return nil
}

func fastV1GrowStack(depth int, fn func()) {
	var buf [256]byte
	buf[depth&255] = byte(depth)
	if depth > 0 {
		fastV1GrowStack(depth-1, fn)
		runtime.KeepAlive(buf)
		return
	}
	fn()
}

func TestFastV1CustomSingleInvocationAndErrors(t *testing.T) {
	sentinel := errors.New("boom")

	t.Run("MarshalErrorAndInvalidJSONSingleCall", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			out  []byte
			err  error
		}{
			{"methodErr", nil, sentinel},
			{"invalidJSON", []byte(`{bad`), nil},
			{"trailingToken", []byte(`1 2`), nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				calls := 0
				v := struct {
					A fastV1CountMarshaler `json:"a"`
				}{A: fastV1CountMarshaler{calls: &calls, out: tc.out, err: tc.err}}

				if _, err := json.Marshal(v); err == nil {
					t.Fatal("Marshal succeeded, want error")
				}
				if calls != 1 {
					t.Fatalf("Marshal calls = %d, want 1", calls)
				}

				calls = 0
				if _, err := json.MarshalOf(v); err == nil {
					t.Fatal("MarshalOf succeeded, want error")
				}
				if calls != 1 {
					t.Fatalf("MarshalOf calls = %d, want 1", calls)
				}

				calls = 0
				prefix := []byte("PRE:")
				got, err := json.AppendMarshalOf(prefix, v)
				if err == nil {
					t.Fatal("AppendMarshalOf succeeded, want error")
				}
				if string(got) != "PRE:" {
					t.Fatalf("AppendMarshalOf truncated buf = %q, want %q", got, "PRE:")
				}
				if calls != 1 {
					t.Fatalf("AppendMarshalOf calls = %d, want 1", calls)
				}

				calls = 0
				var buf bytes.Buffer
				if err := json.EncodeOf(json.NewEncoder(&buf), v); err == nil {
					t.Fatal("EncodeOf succeeded, want error")
				}
				if calls != 1 {
					t.Fatalf("EncodeOf calls = %d, want 1", calls)
				}
			})
		}
	})

	t.Run("MarshalHTMLAndManyKeysAndSiblingFailSingleCall", func(t *testing.T) {
		calls := 0
		rawMany := `{"k1":"<a&b>\u0020","k2":"x\u2028y","k3":1e2,"k4":4,"k5":5,"k6":6,"k7":7,"k8":8,"k9":9,"k10":10}`
		v1 := struct {
			A fastV1CountMarshaler `json:"a"`
			B float64              `json:"b"`
		}{
			A: fastV1CountMarshaler{calls: &calls, out: []byte(rawMany)},
			B: 1.5,
		}
		out, err := json.Marshal(v1)
		if err != nil {
			t.Fatalf("Marshal(v1): %v", err)
		}
		if calls != 1 {
			t.Fatalf("Marshal(v1) calls = %d, want 1", calls)
		}
		if !strings.Contains(string(out), `\u003ca\u0026b\u003e`) || !strings.Contains(string(out), `\u2028`) {
			t.Fatalf("Marshal(v1) missing expected HTML/JS escapes: %s", out)
		}

		// DirectIface custom marshaler with failing NaN sibling must run once.
		calls = 0
		vBad := struct {
			A fastV1DirectMarshaler `json:"a"`
			B float64               `json:"b"`
		}{A: fastV1DirectMarshaler{calls: &calls}, B: math.NaN()}
		if _, err := json.MarshalOf(vBad); err == nil {
			t.Fatal("MarshalOf(vBad) succeeded, want error")
		}
		if calls != 1 {
			t.Fatalf("MarshalOf(vBad) calls = %d, want 1", calls)
		}

		// Sibling inside []any with NaN must run once.
		calls = 0
		sBad := []any{fastV1DirectMarshaler{calls: &calls}, math.Inf(1)}
		if _, err := json.Marshal(sBad); err == nil {
			t.Fatal("Marshal(sBad) succeeded, want error")
		}
		if calls != 1 {
			t.Fatalf("Marshal(sBad) calls = %d, want 1", calls)
		}

		// Bad time.Time year after custom marshaler.
		calls = 0
		tBad := struct {
			A fastV1DirectMarshaler `json:"a"`
			T time.Time             `json:"t"`
		}{A: fastV1DirectMarshaler{calls: &calls}, T: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}
		if _, err := json.MarshalOf(tBad); err == nil {
			t.Fatal("MarshalOf(tBad) succeeded, want error")
		}
		if calls != 1 {
			t.Fatalf("MarshalOf(tBad) calls = %d, want 1", calls)
		}
	})

	t.Run("UnmarshalInvalidNeverCallsCustom", func(t *testing.T) {
		tooDeep := strings.Repeat("[", 10005) + "1" + strings.Repeat("]", 10005)
		for _, bad := range []string{`{invalid`, `{"a":1} trailing`, `{"a":1`, tooDeep} {
			calls := 0
			dst := struct {
				A fastV1CountUnmarshaler `json:"a"`
			}{A: fastV1CountUnmarshaler{calls: &calls}}
			if err := json.Unmarshal([]byte(bad), &dst); err == nil {
				t.Fatalf("Unmarshal(%q) succeeded, want error", bad)
			}
			if calls != 0 {
				t.Fatalf("Unmarshal(%q) invoked UnmarshalJSON %d times, want 0", bad, calls)
			}
			if err := json.UnmarshalOf([]byte(bad), &dst); err == nil {
				t.Fatalf("UnmarshalOf(%q) succeeded, want error", bad)
			}
			if calls != 0 {
				t.Fatalf("UnmarshalOf(%q) invoked UnmarshalJSON %d times, want 0", bad, calls)
			}
		}
	})

	t.Run("UnmarshalSiblingAndTypeErrorSingleCall", func(t *testing.T) {
		type S struct {
			A fastV1CountUnmarshaler `json:"a"`
			B int                    `json:"b"`
		}
		calls := 0
		dst := S{A: fastV1CountUnmarshaler{calls: &calls}}
		if err := json.UnmarshalOf([]byte(`{"a":{"x":1},"b":"not-int"}`), &dst); err == nil {
			t.Fatal("expected UnmarshalTypeError")
		}
		if calls != 1 || dst.A.val != `{"x":1}` {
			t.Fatalf("calls=%d val=%q, want 1 / {\"x\":1}", calls, dst.A.val)
		}

		// Custom returning *UnmarshalTypeError inside struct.
		calls = 0
		dst2 := S{A: fastV1CountUnmarshaler{
			calls: &calls,
			err:   &json.UnmarshalTypeError{Value: "string", Type: reflect.TypeFor[int]()},
		}}
		err := json.Unmarshal([]byte(`{"a":"hi","b":2}`), &dst2)
		var ute *json.UnmarshalTypeError
		if !errors.As(err, &ute) {
			t.Fatalf("err = %v, want *UnmarshalTypeError", err)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1", calls)
		}

		// Null behavior on value vs pointer custom unmarshaler.
		calls = 0
		type P struct {
			Val fastV1CountUnmarshaler  `json:"val"`
			Ptr *fastV1CountUnmarshaler `json:"ptr"`
		}
		pVal := fastV1CountUnmarshaler{val: "keep"}
		p := P{Val: fastV1CountUnmarshaler{calls: &calls}, Ptr: &pVal}
		if err := json.Unmarshal([]byte(`{"val":null,"ptr":null}`), &p); err != nil {
			t.Fatalf("Unmarshal null: %v", err)
		}
		if calls != 1 || p.Val.val != "null" || p.Ptr != nil {
			t.Fatalf("unexpected null custom result: calls=%d val=%q ptr=%v", calls, p.Val.val, p.Ptr)
		}
	})

	t.Run("CycleWithCustomMarshaler", func(t *testing.T) {
		type node struct {
			calls *int
			Next  *node
		}
		// Ensure cyclic struct with custom marshaler does not hang or panic.
		type customNode struct {
			C    fastV1DirectMarshaler `json:"c"`
			Next *customNode           `json:"next"`
		}
		calls := 0
		n := &customNode{C: fastV1DirectMarshaler{calls: &calls}}
		n.Next = n
		if _, err := json.Marshal(n); err == nil {
			t.Fatal("expected cycle error")
		}
	})
}

func TestFastV1EdgeCasesAndGCStress(t *testing.T) {
	t.Run("QuotedStringTagAndNegZeroOmitZero", func(t *testing.T) {
		type S struct {
			Bar    string  `json:"bar,string"`
			Empty  string  `json:"empty,string,omitempty"`
			Long   string  `json:"long,string"`
			NegF64 float64 `json:"negf64,omitzero"`
			NegF32 float32 `json:"negf32,omitzero"`
			PosF64 float64 `json:"posf64,omitzero"`
		}
		longStr := strings.Repeat(`a<b>"`, 40)
		in := S{
			Bar:    `foo"bar`,
			Empty:  "",
			Long:   longStr,
			NegF64: math.Copysign(0, -1),
			NegF32: float32(math.Copysign(0, -1)),
			PosF64: 1.25,
		}
		b, err := json.MarshalOf(in)
		if err != nil {
			t.Fatalf("MarshalOf: %v", err)
		}
		if strings.Contains(string(b), "negf64") || strings.Contains(string(b), "negf32") || strings.Contains(string(b), "empty") {
			t.Fatalf("unexpected non-omitted zero/empty field in %s", b)
		}
		var out S
		if err := json.UnmarshalOf(b, &out); err != nil {
			t.Fatalf("UnmarshalOf: %v", err)
		}
		if out.Bar != in.Bar || out.Long != in.Long || out.PosF64 != 1.25 {
			t.Fatalf("roundtrip mismatch: got %+v", out)
		}
	})

	t.Run("Float2ArrayAndStreamingDecode", func(t *testing.T) {
		type geo struct {
			Coords [][2]float64 `json:"coords"`
		}
		in := geo{Coords: [][2]float64{{-75.6972, 45.4215}, {0, -0.0}, {1.5e-4, -2.5e3}}}
		b, err := json.MarshalOf(in)
		if err != nil {
			t.Fatalf("MarshalOf(geo): %v", err)
		}
		var out geo
		if err := json.UnmarshalOf(b, &out); err != nil {
			t.Fatalf("UnmarshalOf(geo): %v", err)
		}
		if len(out.Coords) != len(in.Coords) || out.Coords[0] != in.Coords[0] || out.Coords[2] != in.Coords[2] {
			t.Fatalf("geo mismatch: %+v vs %+v", out, in)
		}

		// Multi-value stream + reuse + InputOffset + trailing truncated value.
		stream := ` {"coords":[[-1.5,2.5]]}  {"coords":[[3.25,-4.75]]} {"coords":[`
		dec := json.NewDecoder(bytes.NewReader([]byte(stream)))
		var d1, d2, d3 geo
		if err := json.DecodeOf(dec, &d1); err != nil {
			t.Fatalf("DecodeOf 1: %v", err)
		}
		if !dec.More() {
			t.Fatal("expected dec.More() = true before 2nd object")
		}
		if err := dec.Decode(&d2); err != nil {
			t.Fatalf("Decode 2: %v", err)
		}
		if d1.Coords[0] != [2]float64{-1.5, 2.5} || d2.Coords[0] != [2]float64{3.25, -4.75} {
			t.Fatalf("stream values mismatch: d1=%+v d2=%+v", d1, d2)
		}
		if err := json.DecodeOf(dec, &d3); err != io.ErrUnexpectedEOF {
			t.Fatalf("DecodeOf 3 err = %v, want io.ErrUnexpectedEOF", err)
		}
	})

	t.Run("NoescapeStackGrowthGCStress", func(t *testing.T) {
		calls := 0
		type stackCustom struct {
			m fastV1CountMarshaler
			u fastV1CountUnmarshaler
		}
		var sc stackCustom
		sc.m = fastV1CountMarshaler{calls: &calls, out: []byte(`42`)}
		sc.u = fastV1CountUnmarshaler{calls: &calls}
		_ = sc
		for i := range 200 {
			type box struct {
				C fastV1DirectMarshaler `json:"c"`
				N int                   `json:"n"`
			}
			v := box{C: fastV1DirectMarshaler{calls: &calls}, N: i}
			fastV1GrowStack(64, func() {
				runtime.GC()
				b, err := json.MarshalOf(v)
				if err != nil {
					t.Fatalf("MarshalOf during GC: %v", err)
				}
				var out struct {
					C map[string]string `json:"c"`
					N int               `json:"n"`
				}
				if err := json.UnmarshalOf(b, &out); err != nil || out.N != i {
					t.Fatalf("UnmarshalOf during GC = %+v, %v", out, err)
				}
			})
		}
		if calls != 200 {
			t.Fatalf("calls = %d, want 200", calls)
		}
	})
}

func TestFastV1RegressionAndSecurity(t *testing.T) {
	t.Run("DAGAndCyclicCustom", func(t *testing.T) {
		type leaf struct {
			Name string `json:"name"`
		}
		type node struct {
			L1   leaf                  `json:"l1"`
			L2   *leaf                 `json:"l2"`
			C    fastV1DirectMarshaler `json:"c"`
			Any  any                   `json:"any,omitempty"`
			Next *node                 `json:"next,omitempty"`
		}
		calls := 0
		in := &node{
			L1:  leaf{Name: "a"},
			L2:  &leaf{Name: "b"},
			C:   fastV1DirectMarshaler{calls: &calls},
			Any: []any{"x", float64(2)},
			Next: &node{
				L1: leaf{Name: "c"},
				C:  fastV1DirectMarshaler{calls: &calls},
			},
		}
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if calls != 2 {
			t.Fatalf("Marshal custom calls = %d, want 2", calls)
		}
		var out map[string]any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
	})

	t.Run("CustomPlusTimeErrorReplay", func(t *testing.T) {
		type payload struct {
			T1   time.Time              `json:"t1"`
			U    fastV1CountUnmarshaler `json:"u"`
			T2   time.Time              `json:"t2"`
			Bad  int                    `json:"bad"`
			Tail int                    `json:"tail"`
		}
		var uCalls int
		p := payload{U: fastV1CountUnmarshaler{calls: &uCalls}}
		err := json.Unmarshal([]byte(`{"t1":"2026-01-02T03:04:05Z","u":1,"t2":"bad-time","tail":5}`), &p)
		if err == nil {
			t.Fatal("expected error for invalid t2")
		}
		if uCalls != 1 {
			t.Fatalf("uCalls = %d, want 1", uCalls)
		}

		// When Bad fails with type mismatch after T1 and U, replay continues to populate Tail.
		uCalls = 0
		p2 := payload{U: fastV1CountUnmarshaler{calls: &uCalls}}
		err = json.Unmarshal([]byte(`{"t1":"2026-01-02T03:04:05Z","u":1,"bad":"oops","tail":5}`), &p2)
		if err == nil {
			t.Fatal("expected UnmarshalTypeError for bad int")
		}
		if uCalls != 1 {
			t.Fatalf("replay uCalls = %d, want 1", uCalls)
		}
		if p2.Tail != 5 {
			t.Fatalf("expected Tail=5 after replay continuation, got %d", p2.Tail)
		}
	})

	t.Run("ValidParityEdgeCases", func(t *testing.T) {
		cases := []struct {
			in   string
			want bool
		}{
			{"", false},
			{"   \t\r\n ", false},
			{"null", true},
			{"true", true},
			{"false", true},
			{"123", true},
			{"-0.5e+2", true},
			{"01", false},
			{"-01", false},
			{"1.", false},
			{"1e+", false},
			{`{"a":[1,2,3],"b":"x\u0020y"}`, true},
			{"\"bad\nstr\"", false},
			{"\"bad\x00\"", false},
			{"\"raw\xffutf8\"", true},
			{`"\u123"`, false},
			{`"\uZZZZ"`, false},
			{`{"a":1,} `, false},
			{`[1,2,]`, false},
			{`{"a":1}{"b":2}`, false},
			{strings.Repeat("[", 10000) + "0" + strings.Repeat("]", 10000), true},
			{strings.Repeat("[", 10001) + "0" + strings.Repeat("]", 10001), false},
		}
		for _, tc := range cases {
			got := json.Valid([]byte(tc.in))
			if got != tc.want {
				t.Errorf("Valid(%q) = %v, want %v", tc.in, got, tc.want)
			}
			var v any
			err := json.Unmarshal([]byte(tc.in), &v)
			if (err == nil) != tc.want {
				t.Errorf("Unmarshal(%q) err=%v vs wantValid=%v", tc.in, err, tc.want)
			}
		}
	})

	t.Run("SlicePoint2F64MergeAndNaN", func(t *testing.T) {
		type geo struct {
			Coords [][2]float64 `json:"coords"`
		}
		g := geo{Coords: [][2]float64{{10, 20}, {30, 40}}}
		if err := json.Unmarshal([]byte(`{"coords":[[1,2],null]}`), &g); err != nil {
			t.Fatalf("Unmarshal non-empty merge: %v", err)
		}
		if g.Coords[0] != [2]float64{1, 2} || g.Coords[1] != [2]float64{30, 40} {
			t.Fatalf("Coords = %+v, want [[1 2] [30 40]]", g.Coords)
		}
		if _, err := json.Marshal(geo{Coords: [][2]float64{{1, math.Inf(1)}}}); err == nil {
			t.Fatal("expected error for +Inf in [][2]float64")
		}
	})

	t.Run("GenericOfZeroAlloc", func(t *testing.T) {
		type flat struct {
			A int64   `json:"a"`
			B string  `json:"b"`
			C float64 `json:"c"`
			D bool    `json:"d"`
		}
		v := flat{A: 99, B: "ok", C: 1.25, D: false}
		buf := make([]byte, 0, 256)
		if n := testing.AllocsPerRun(50, func() {
			var err error
			buf, err = json.AppendMarshalOf(buf[:0], v)
			if err != nil {
				t.Fatal(err)
			}
		}); n != 0 {
			t.Fatalf("AppendMarshalOf allocs = %v, want 0", n)
		}
		if n := testing.AllocsPerRun(50, func() {
			out, err := json.MarshalOf(v)
			if err != nil || len(out) == 0 {
				t.Fatalf("MarshalOf: %v", err)
			}
		}); n != 1 {
			t.Fatalf("MarshalOf allocs = %v, want 1", n)
		}
		enc := json.NewEncoder(io.Discard)
		if n := testing.AllocsPerRun(50, func() {
			if err := json.EncodeOf(enc, v); err != nil {
				t.Fatal(err)
			}
		}); n != 0 {
			t.Fatalf("EncodeOf allocs = %v, want 0", n)
		}
	})
}

