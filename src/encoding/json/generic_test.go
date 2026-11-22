// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type genericEmbedded struct {
	EmbStr string `json:"emb_str"`
	EmbInt int    `json:"emb_int,omitempty"`
}

type GenericPtrEmbedded struct {
	PtrEmb string `json:"ptr_emb"`
}

type genericValMarshaler struct {
	V string
}

func (m genericValMarshaler) MarshalJSON() ([]byte, error) {
	return json.Marshal("val:" + m.V)
}

func (m *genericValMarshaler) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	m.V = strings.TrimPrefix(s, "val:")
	return nil
}

type genericPtrMarshaler struct {
	V string
}

func (m *genericPtrMarshaler) MarshalJSON() ([]byte, error) {
	return json.Marshal("ptr:" + m.V)
}

func (m *genericPtrMarshaler) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	m.V = strings.TrimPrefix(s, "ptr:")
	return nil
}

type genericTextMarshaler struct {
	V string
}

func (m genericTextMarshaler) MarshalText() ([]byte, error) {
	return []byte("txt:" + m.V), nil
}

func (m *genericTextMarshaler) UnmarshalText(b []byte) error {
	m.V = strings.TrimPrefix(string(b), "txt:")
	return nil
}

type genericErrMarshaler struct{}

func (genericErrMarshaler) MarshalJSON() ([]byte, error) {
	return nil, errors.New("boom")
}

type genericTreeNode struct {
	Name     string             `json:"name"`
	Count    int                `json:"count,omitempty"`
	Weight   float64            `json:"weight"`
	Active   bool               `json:"active"`
	Tags     []string           `json:"tags"`
	Scores   []int64            `json:"scores"`
	Flags    []bool             `json:"flags"`
	Raw      []byte             `json:"raw"`
	Meta     map[string]string  `json:"meta"`
	Counts   map[string]int     `json:"counts"`
	Extra    map[string]any     `json:"extra"`
	Fixed    [3]int             `json:"fixed"`
	Children []*genericTreeNode `json:"children"`
	Next     *genericTreeNode   `json:"next,omitempty"`
	When     time.Time          `json:"when,omitzero"`
}

type genericRichStruct struct {
	genericEmbedded
	*GenericPtrEmbedded
	ID       uint64               `json:"id"`
	Quoted   int                  `json:"quoted,string"`
	ValM     genericValMarshaler  `json:"val_m"`
	PtrM     *genericPtrMarshaler `json:"ptr_m"`
	TxtM     genericTextMarshaler `json:"txt_m"`
	OmitZero int                  `json:"omit_zero,omitzero"`
	HTML     string               `json:"html"`
}

func assertMarshalParity[T any](t *testing.T, name string, v T) {
	t.Helper()
	want, wantErr := json.Marshal(v)
	gotOf, gotOfErr := json.MarshalOf(v)
	if (wantErr != nil) != (gotOfErr != nil) {
		t.Fatalf("%s: MarshalOf error mismatch: got %v, want %v", name, gotOfErr, wantErr)
	}
	if wantErr == nil && !bytes.Equal(gotOf, want) {
		t.Fatalf("%s: MarshalOf mismatch:\ngot:  %s\nwant: %s", name, gotOf, want)
	}

	prefix := []byte("PREFIX:")
	gotApp, gotAppErr := json.AppendMarshal(append([]byte(nil), prefix...), v)
	if (wantErr != nil) != (gotAppErr != nil) {
		t.Fatalf("%s: AppendMarshal error mismatch: got %v, want %v", name, gotAppErr, wantErr)
	}
	if wantErr != nil {
		if !bytes.Equal(gotApp, prefix) {
			t.Fatalf("%s: AppendMarshal on error did not truncate dst: got %q, want %q", name, gotApp, prefix)
		}
	} else if !bytes.Equal(gotApp, append(append([]byte(nil), prefix...), want...)) {
		t.Fatalf("%s: AppendMarshal mismatch:\ngot:  %s\nwant: %s%s", name, gotApp, prefix, want)
	}

	gotAppOf, gotAppOfErr := json.AppendMarshalOf(append([]byte(nil), prefix...), v)
	if (wantErr != nil) != (gotAppOfErr != nil) {
		t.Fatalf("%s: AppendMarshalOf error mismatch: got %v, want %v", name, gotAppOfErr, wantErr)
	}
	if wantErr != nil {
		if !bytes.Equal(gotAppOf, prefix) {
			t.Fatalf("%s: AppendMarshalOf on error did not truncate dst: got %q, want %q", name, gotAppOf, prefix)
		}
	} else if !bytes.Equal(gotAppOf, append(append([]byte(nil), prefix...), want...)) {
		t.Fatalf("%s: AppendMarshalOf mismatch:\ngot:  %s\nwant: %s%s", name, gotAppOf, prefix, want)
	}

	// Indent parity.
	wantInd, wantIndErr := json.MarshalIndent(v, ">", "  ")
	gotIndOf, gotIndOfErr := json.MarshalIndentOf(v, ">", "  ")
	if (wantIndErr != nil) != (gotIndOfErr != nil) {
		t.Fatalf("%s: MarshalIndentOf error mismatch: got %v, want %v", name, gotIndOfErr, wantIndErr)
	}
	if wantIndErr == nil && !bytes.Equal(gotIndOf, wantInd) {
		t.Fatalf("%s: MarshalIndentOf mismatch:\ngot:  %s\nwant: %s", name, gotIndOf, wantInd)
	}

	gotAppInd, gotAppIndErr := json.AppendMarshalIndent(append([]byte(nil), prefix...), v, ">", "  ")
	if (wantIndErr != nil) != (gotAppIndErr != nil) {
		t.Fatalf("%s: AppendMarshalIndent error mismatch: got %v, want %v", name, gotAppIndErr, wantIndErr)
	}
	if wantIndErr != nil {
		if !bytes.Equal(gotAppInd, prefix) {
			t.Fatalf("%s: AppendMarshalIndent did not truncate on error: got %q", name, gotAppInd)
		}
	} else if !bytes.Equal(gotAppInd, append(append([]byte(nil), prefix...), wantInd...)) {
		t.Fatalf("%s: AppendMarshalIndent mismatch", name)
	}

	gotAppIndOf, gotAppIndOfErr := json.AppendMarshalIndentOf(append([]byte(nil), prefix...), v, ">", "  ")
	if (wantIndErr != nil) != (gotAppIndOfErr != nil) {
		t.Fatalf("%s: AppendMarshalIndentOf error mismatch: got %v, want %v", name, gotAppIndOfErr, wantIndErr)
	}
	if wantIndErr != nil {
		if !bytes.Equal(gotAppIndOf, prefix) {
			t.Fatalf("%s: AppendMarshalIndentOf did not truncate on error: got %q", name, gotAppIndOf)
		}
	} else if !bytes.Equal(gotAppIndOf, append(append([]byte(nil), prefix...), wantInd...)) {
		t.Fatalf("%s: AppendMarshalIndentOf mismatch", name)
	}
}

func assertRoundTripParity[T any](t *testing.T, name string, v T) {
	t.Helper()
	assertMarshalParity(t, name, v)

	data, err := json.MarshalOf(v)
	if err != nil {
		t.Fatalf("%s: MarshalOf failed: %v", name, err)
	}
	if !json.Valid(data) {
		t.Fatalf("%s: Valid returned false on marshaled output: %s", name, data)
	}

	var wantDst, gotDst T
	if err := json.Unmarshal(data, &wantDst); err != nil {
		t.Fatalf("%s: Unmarshal failed: %v", name, err)
	}
	if err := json.UnmarshalOf(data, &gotDst); err != nil {
		t.Fatalf("%s: UnmarshalOf failed: %v", name, err)
	}
	if !reflect.DeepEqual(gotDst, wantDst) {
		t.Fatalf("%s: UnmarshalOf != Unmarshal:\ngot:  %#v\nwant: %#v", name, gotDst, wantDst)
	}

	// Encoder.Encode vs EncodeOf, Decoder.Decode vs DecodeOf.
	var wantBuf, gotBuf bytes.Buffer
	if err := json.NewEncoder(&wantBuf).Encode(v); err != nil {
		t.Fatalf("%s: Encode failed: %v", name, err)
	}
	if err := json.EncodeOf(json.NewEncoder(&gotBuf), v); err != nil {
		t.Fatalf("%s: EncodeOf failed: %v", name, err)
	}
	if !bytes.Equal(gotBuf.Bytes(), wantBuf.Bytes()) {
		t.Fatalf("%s: EncodeOf mismatch:\ngot:  %s\nwant: %s", name, gotBuf.Bytes(), wantBuf.Bytes())
	}

	var decWant, decGot T
	if err := json.NewDecoder(bytes.NewReader(wantBuf.Bytes())).Decode(&decWant); err != nil {
		t.Fatalf("%s: Decode failed: %v", name, err)
	}
	if err := json.DecodeOf(json.NewDecoder(bytes.NewReader(gotBuf.Bytes())), &decGot); err != nil {
		t.Fatalf("%s: DecodeOf failed: %v", name, err)
	}
	if !reflect.DeepEqual(decGot, decWant) {
		t.Fatalf("%s: DecodeOf != Decode:\ngot:  %#v\nwant: %#v", name, decGot, decWant)
	}
}

func TestGenericMarshalUnmarshalParity(t *testing.T) {
	assertRoundTripParity(t, "bool-true", true)
	assertRoundTripParity(t, "bool-false", false)
	assertRoundTripParity(t, "string-ascii", "hello, world! 0123456789")
	assertRoundTripParity(t, "string-escape", "line1\nline2\t\"quoted\"\\<script>&\u2028\u2029\x00\x1f")
	assertRoundTripParity(t, "string-unicode", "你好世界🚀 café")
	assertRoundTripParity(t, "int", -42)
	assertRoundTripParity(t, "int8", int8(-128))
	assertRoundTripParity(t, "int16", int16(32767))
	assertRoundTripParity(t, "int32", int32(-2147483648))
	assertRoundTripParity(t, "int64", int64(9223372036854775807))
	assertRoundTripParity(t, "uint", uint(42))
	assertRoundTripParity(t, "uint8", uint8(255))
	assertRoundTripParity(t, "uint16", uint16(65535))
	assertRoundTripParity(t, "uint32", uint32(4294967295))
	assertRoundTripParity(t, "uint64", uint64(18446744073709551615))
	assertRoundTripParity(t, "float32", float32(3.14159))
	assertRoundTripParity(t, "float64", float64(1e-7))
	assertRoundTripParity(t, "float64-big", float64(1e22))
	assertRoundTripParity(t, "bytes-nil", ([]byte)(nil))
	assertRoundTripParity(t, "bytes-empty", []byte{})
	assertRoundTripParity(t, "bytes-data", []byte("binary\x00\xffdata"))
	assertRoundTripParity(t, "slice-string", []string{"a", "b<c>", ""})
	assertRoundTripParity(t, "slice-int", []int{1, -2, 3, 0})
	assertRoundTripParity(t, "slice-int64", []int64{math.MinInt64, 0, math.MaxInt64})
	assertRoundTripParity(t, "slice-uint64", []uint64{0, 1, math.MaxUint64})
	assertRoundTripParity(t, "slice-float64", []float64{-1.5, 0, 2.25})
	assertRoundTripParity(t, "slice-float32", []float32{-1.5, 0, 2.25})
	assertRoundTripParity(t, "slice-bool", []bool{true, false, true})
	assertRoundTripParity(t, "slice-empty", []int{})
	assertRoundTripParity(t, "slice-nil", ([]int)(nil))
	assertRoundTripParity(t, "array-int", [4]int{10, 20, 30, 40})
	assertRoundTripParity(t, "map-str-str", map[string]string{"z": "last", "a": "first", "m": "<mid>"})
	assertRoundTripParity(t, "map-str-int", map[string]int{"b": 2, "a": 1})
	assertRoundTripParity(t, "map-str-any", map[string]any{"s": "str", "n": float64(42), "b": true, "nil": nil, "arr": []any{"x", float64(1)}})
	assertRoundTripParity(t, "map-nil", (map[string]string)(nil))
	assertRoundTripParity(t, "map-empty", map[string]string{})
	assertRoundTripParity(t, "any-nil", any(nil))
	assertRoundTripParity(t, "any-map", any(map[string]any{"k": "v", "list": []any{true, nil, float64(3.5)}}))

	var nilIntPtr *int
	assertRoundTripParity(t, "ptr-nil", nilIntPtr)
	n := 99
	assertRoundTripParity(t, "ptr-val", &n)

	when := time.Date(2026, 11, 21, 12, 34, 56, 789000000, time.UTC)
	tree := &genericTreeNode{
		Name:   "root",
		Count:  2,
		Weight: 1.75,
		Active: true,
		Tags:   []string{"alpha", "beta<1>"},
		Scores: []int64{100, -200},
		Flags:  []bool{true, false},
		Raw:    []byte{1, 2, 3, 4},
		Meta:   map[string]string{"env": "prod", "region": "us"},
		Counts: map[string]int{"ok": 10, "err": 0},
		Extra:  map[string]any{"nested": map[string]any{"flag": true}, "nums": []any{float64(1), float64(2)}},
		Fixed:  [3]int{7, 8, 9},
		When:   when,
		Children: []*genericTreeNode{
			{Name: "leaf1", Tags: []string{}, Children: []*genericTreeNode{}},
			{Name: "leaf2", Count: 5, Next: &genericTreeNode{Name: "linked"}},
		},
	}
	assertRoundTripParity(t, "tree-ptr", tree)
	assertRoundTripParity(t, "tree-val", *tree)

	rich := genericRichStruct{
		genericEmbedded:    genericEmbedded{EmbStr: "emb", EmbInt: 7},
		GenericPtrEmbedded: &GenericPtrEmbedded{PtrEmb: "ptr-emb"},
		ID:                 1234567890123,
		Quoted:             77,
		ValM:               genericValMarshaler{V: "hello"},
		PtrM:               &genericPtrMarshaler{V: "world"},
		TxtM:               genericTextMarshaler{V: "text"},
		HTML:               "<b>&</b>",
	}
	assertRoundTripParity(t, "rich-struct-ptr", &rich)
	assertRoundTripParity(t, "rich-struct-val", rich)

	// Invalid UTF-8 coercion parity.
	assertMarshalParity(t, "invalid-utf8", "bad\xff\xfeutf8")
}

func TestGenericMarshalErrorsAndTruncation(t *testing.T) {
	assertMarshalParity(t, "nan", math.NaN())
	assertMarshalParity(t, "inf", math.Inf(1))
	assertMarshalParity(t, "neg-inf", math.Inf(-1))
	assertMarshalParity(t, "chan", make(chan int))
	assertMarshalParity(t, "func", func() {})
	assertMarshalParity(t, "err-marshaler", genericErrMarshaler{})

	// Cyclic pointer must fail without stack overflow.
	type cycle struct {
		Next *cycle
	}
	c := &cycle{}
	c.Next = c
	assertMarshalParity(t, "cycle-ptr", c)

	// Cyclic map must fail without stack overflow.
	cm := map[string]any{}
	cm["self"] = cm
	assertMarshalParity(t, "cycle-map", cm)
}

func TestGenericUnmarshalNilAndReuseErrors(t *testing.T) {
	// Nil pointer destination must return InvalidUnmarshalError on valid JSON.
	var nilPtr *genericTreeNode
	err := json.UnmarshalOf([]byte(`{"name":"a"}`), nilPtr)
	var iue *json.InvalidUnmarshalError
	if !errors.As(err, &iue) {
		t.Fatalf("UnmarshalOf(valid, nil) = %v, want *InvalidUnmarshalError", err)
	}
	// Invalid JSON with nil pointer must match Unmarshal's exact error type and message.
	wantNilErr := json.Unmarshal([]byte(`{invalid`), nilPtr)
	gotNilErr := json.UnmarshalOf([]byte(`{invalid`), nilPtr)
	if gotNilErr == nil || wantNilErr == nil || reflect.TypeOf(gotNilErr) != reflect.TypeOf(wantNilErr) || gotNilErr.Error() != wantNilErr.Error() {
		t.Fatalf("UnmarshalOf(invalid, nil) = %T(%v), want %T(%v)", gotNilErr, gotNilErr, wantNilErr, wantNilErr)
	}

	// Pre-populated target must NOT be mutated when syntax error occurs mid-way.
	pre := genericTreeNode{
		Name:   "keep-me",
		Count:  42,
		Tags:   []string{"orig"},
		Scores: []int64{99},
	}
	if err := json.UnmarshalOf([]byte(`{"name":"changed","count":1,invalid}`), &pre); err == nil {
		t.Fatal("expected syntax error")
	}
	if pre.Name != "keep-me" || pre.Count != 42 || len(pre.Tags) != 1 || pre.Tags[0] != "orig" {
		t.Fatalf("pre-populated struct mutated on syntax error: %+v", pre)
	}

	// Pre-populated target with mid-way semantic error must match Unmarshal behavior.
	wantPre := genericTreeNode{Name: "orig", Count: 10, Tags: []string{"a", "b"}}
	gotPre := genericTreeNode{Name: "orig", Count: 10, Tags: []string{"a", "b"}}
	badSemantic := []byte(`{"name":"updated","count":"not-a-number","tags":["x"]}`)
	wantErr := json.Unmarshal(badSemantic, &wantPre)
	gotErr := json.UnmarshalOf(badSemantic, &gotPre)
	if (wantErr != nil) != (gotErr != nil) {
		t.Fatalf("semantic error mismatch: got %v, want %v", gotErr, wantErr)
	}
	if !reflect.DeepEqual(gotPre, wantPre) {
		t.Fatalf("pre-populated semantic error state mismatch:\ngot:  %#v\nwant: %#v", gotPre, wantPre)
	}

	// Integer overflow must fail cleanly and leave scalar untouched.
	var small int8 = 7
	if err := json.UnmarshalOf([]byte(`999`), &small); err == nil {
		t.Fatal("expected overflow error for int8")
	}
	if small != 7 {
		t.Fatalf("scalar mutated on overflow: got %d, want 7", small)
	}
	var usmall uint8 = 9
	if err := json.UnmarshalOf([]byte(`-1`), &usmall); err == nil {
		t.Fatal("expected negative error for uint8")
	}
	if usmall != 9 {
		t.Fatalf("uint8 mutated on error: got %d, want 9", usmall)
	}
}

func TestGenericEncoderDecoderFeatures(t *testing.T) {
	// EncodeOf with SetEscapeHTML(false) and SetIndent.
	type msg struct {
		HTML string `json:"html"`
		N    int    `json:"n"`
	}
	in := msg{HTML: "<a>&</a>", N: 3}

	var wantBuf, gotBuf bytes.Buffer
	encWant := json.NewEncoder(&wantBuf)
	encWant.SetEscapeHTML(false)
	encWant.SetIndent(">", "  ")
	if err := encWant.Encode(in); err != nil {
		t.Fatal(err)
	}
	encGot := json.NewEncoder(&gotBuf)
	encGot.SetEscapeHTML(false)
	encGot.SetIndent(">", "  ")
	if err := json.EncodeOf(encGot, in); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBuf.Bytes(), wantBuf.Bytes()) {
		t.Fatalf("EncodeOf(escapeHTML=false, indent) mismatch:\ngot:  %s\nwant: %s", gotBuf.Bytes(), wantBuf.Bytes())
	}

	// DecodeOf with DisallowUnknownFields.
	dec := json.NewDecoder(strings.NewReader(`{"html":"ok","n":1,"unknown":true}`))
	dec.DisallowUnknownFields()
	var out msg
	if err := json.DecodeOf(dec, &out); err == nil {
		t.Fatal("DecodeOf expected error for unknown field")
	}

	// DecodeOf with UseNumber into any.
	decNum := json.NewDecoder(strings.NewReader(`{"big":12345678901234567890}`))
	decNum.UseNumber()
	var anyOut any
	if err := json.DecodeOf(decNum, &anyOut); err != nil {
		t.Fatalf("DecodeOf(UseNumber) failed: %v", err)
	}
	m, ok := anyOut.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", anyOut)
	}
	num, ok := m["big"].(json.Number)
	if !ok || num != "12345678901234567890" {
		t.Fatalf("expected json.Number(12345678901234567890), got %T(%v)", m["big"], m["big"])
	}

	// Sequential streaming DecodeOf.
	stream := strings.NewReader(`{"html":"one","n":1} {"html":"two","n":2}`)
	decSeq := json.NewDecoder(stream)
	var m1, m2 msg
	if err := json.DecodeOf(decSeq, &m1); err != nil {
		t.Fatalf("DecodeOf #1: %v", err)
	}
	if err := json.DecodeOf(decSeq, &m2); err != nil {
		t.Fatalf("DecodeOf #2: %v", err)
	}
	if m1 != (msg{HTML: "one", N: 1}) || m2 != (msg{HTML: "two", N: 2}) {
		t.Fatalf("unexpected sequential decode results: %+v, %+v", m1, m2)
	}

	// Nil encoder / decoder must panic cleanly.
	assertPanics := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("%s: expected panic", name)
			}
		}()
		fn()
	}
	assertPanics("EncodeOf(nil)", func() { _ = json.EncodeOf(nil, in) })
	assertPanics("DecodeOf(nil)", func() { _ = json.DecodeOf(nil, &out) })
}

func TestSecurityMaxNestingDepth(t *testing.T) {
	const overMax = 10001

	// 1. Decoder.Token must reject nesting deeper than maxNestingDepth (10000)
	// for both arrays and objects (prevents unbounded tokenStack memory growth).
	for _, tc := range []struct {
		name string
		open string
		shut string
	}{
		{"array", "[", "]"},
		{"object", `{"a":`, "}"},
	} {
		t.Run("DecoderToken/"+tc.name, func(t *testing.T) {
			in := strings.Repeat(tc.open, overMax) + "0" + strings.Repeat(tc.shut, overMax)
			dec := json.NewDecoder(strings.NewReader(in))
			var gotErr error
			for {
				_, err := dec.Token()
				if err != nil {
					gotErr = err
					break
				}
			}
			if gotErr == nil {
				t.Fatalf("Decoder.Token allowed %d nested %s delimiters", overMax, tc.name)
			}
		})
	}

	// 2. Valid and UnmarshalOf must reject > 10000 nesting depth.
	deepArr := []byte(strings.Repeat("[", overMax) + strings.Repeat("]", overMax))
	if json.Valid(deepArr) {
		t.Fatalf("Valid accepted %d nested arrays", overMax)
	}
	var dst any
	if err := json.UnmarshalOf(deepArr, &dst); err == nil {
		t.Fatalf("UnmarshalOf accepted %d nested arrays", overMax)
	}

	// Exactly 10000 nested arrays (valid boundary: 10000 containers).
	atMax := []byte(strings.Repeat("[", 1000) + strings.Repeat("]", 1000))
	if !json.Valid(atMax) {
		t.Fatal("Valid rejected 1000 nested arrays")
	}
}

func TestGenericConcurrentRace(t *testing.T) {
	src := genericTreeNode{
		Name:   "concurrent",
		Count:  42,
		Weight: 9.5,
		Active: true,
		Tags:   []string{"x", "y", "z"},
		Scores: []int64{1, 2, 3},
		Meta:   map[string]string{"k1": "v1", "k2": "v2"},
		Extra:  map[string]any{"a": float64(1), "b": "two"},
		Children: []*genericTreeNode{
			{Name: "child1", Tags: []string{}},
			{Name: "child2", Count: 7},
		},
	}

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			var buf []byte
			for range 100 {
				var err error
				buf, err = json.AppendMarshalOf(buf[:0], src)
				if err != nil {
					t.Errorf("goroutine %d AppendMarshalOf: %v", id, err)
					return
				}
				var dst genericTreeNode
				if err := json.UnmarshalOf(buf, &dst); err != nil {
					t.Errorf("goroutine %d UnmarshalOf: %v", id, err)
					return
				}
				if dst.Name != src.Name || dst.Count != src.Count || len(dst.Children) != 2 {
					t.Errorf("goroutine %d mismatch: %+v", id, dst)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
