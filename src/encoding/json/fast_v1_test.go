// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json_test

import (
	"encoding/json"
	"math"
	"reflect"
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
