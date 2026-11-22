// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.jsonv2

package json_test

import (
	"bytes"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"encoding/json/jsontext"
	json "encoding/json/v2"
)

type v2GenericNode struct {
	Name     string            `json:"name"`
	Count    int               `json:"count,omitempty"`
	Score    float64           `json:"score"`
	Active   bool              `json:"active"`
	Tags     []string          `json:"tags"`
	Nums     []uint64          `json:"nums"`
	Raw      []byte            `json:"raw"`
	Meta     map[string]string `json:"meta"`
	Extra    map[string]any    `json:"extra"`
	Fixed    [3]int            `json:"fixed"`
	Children []*v2GenericNode  `json:"children"`
	When     time.Time         `json:"when,omitzero"`
}

func assertV2Parity[T any](t *testing.T, name string, v T, opts ...json.Options) {
	t.Helper()
	want, wantErr := json.Marshal(v, opts...)
	gotOf, gotOfErr := json.MarshalOf(v, opts...)
	if (wantErr != nil) != (gotOfErr != nil) {
		t.Fatalf("%s: MarshalOf error mismatch: got %v, want %v", name, gotOfErr, wantErr)
	}
	if wantErr == nil && !bytes.Equal(gotOf, want) {
		t.Fatalf("%s: MarshalOf mismatch:\ngot:  %s\nwant: %s", name, gotOf, want)
	}

	prefix := []byte("HDR:")
	gotApp, gotAppErr := json.AppendMarshal(append([]byte(nil), prefix...), v, opts...)
	if (wantErr != nil) != (gotAppErr != nil) {
		t.Fatalf("%s: AppendMarshal error mismatch: got %v, want %v", name, gotAppErr, wantErr)
	}
	if wantErr != nil {
		if !bytes.Equal(gotApp, prefix) {
			t.Fatalf("%s: AppendMarshal did not truncate dst on error: got %q", name, gotApp)
		}
	} else if !bytes.Equal(gotApp, append(append([]byte(nil), prefix...), want...)) {
		t.Fatalf("%s: AppendMarshal mismatch", name)
	}

	gotAppOf, gotAppOfErr := json.AppendMarshalOf(append([]byte(nil), prefix...), v, opts...)
	if (wantErr != nil) != (gotAppOfErr != nil) {
		t.Fatalf("%s: AppendMarshalOf error mismatch: got %v, want %v", name, gotAppOfErr, wantErr)
	}
	if wantErr != nil {
		if !bytes.Equal(gotAppOf, prefix) {
			t.Fatalf("%s: AppendMarshalOf did not truncate dst on error: got %q", name, gotAppOf)
		}
	} else if !bytes.Equal(gotAppOf, append(append([]byte(nil), prefix...), want...)) {
		t.Fatalf("%s: AppendMarshalOf mismatch", name)
	}

	// MarshalWriteOf parity (bytes.Buffer fast path and custom io.Writer streaming path).
	var bufWant, bufGot bytes.Buffer
	errW1 := json.MarshalWrite(&bufWant, v, opts...)
	errW2 := json.MarshalWriteOf(&bufGot, v, opts...)
	if (errW1 != nil) != (errW2 != nil) {
		t.Fatalf("%s: MarshalWriteOf error mismatch: got %v, want %v", name, errW2, errW1)
	}
	if errW1 == nil && !bytes.Equal(bufGot.Bytes(), bufWant.Bytes()) {
		t.Fatalf("%s: MarshalWriteOf(bytes.Buffer) mismatch", name)
	}

	var customBuf bytes.Buffer
	errW3 := json.MarshalWriteOf(struct{ io.Writer }{&customBuf}, v, opts...)
	if (errW1 != nil) != (errW3 != nil) {
		t.Fatalf("%s: MarshalWriteOf(customWriter) error mismatch: got %v, want %v", name, errW3, errW1)
	}
	if errW1 == nil && !bytes.Equal(customBuf.Bytes(), bufWant.Bytes()) {
		t.Fatalf("%s: MarshalWriteOf(customWriter) mismatch", name)
	}

	// MarshalEncodeOf parity.
	var encWantBuf, encGotBuf bytes.Buffer
	errE1 := json.MarshalEncode(jsontext.NewEncoder(&encWantBuf), v, opts...)
	errE2 := json.MarshalEncodeOf(jsontext.NewEncoder(&encGotBuf), v, opts...)
	if (errE1 != nil) != (errE2 != nil) {
		t.Fatalf("%s: MarshalEncodeOf error mismatch: got %v, want %v", name, errE2, errE1)
	}
	if errE1 == nil && !bytes.Equal(encGotBuf.Bytes(), encWantBuf.Bytes()) {
		t.Fatalf("%s: MarshalEncodeOf mismatch", name)
	}

	if wantErr != nil {
		return
	}

	// UnmarshalOf / UnmarshalReadOf / UnmarshalDecodeOf parity.
	var uWant, uGot, rBufGot, rStrGot, dGot T
	if err := json.Unmarshal(want, &uWant, opts...); err != nil {
		t.Fatalf("%s: Unmarshal failed: %v", name, err)
	}
	if err := json.UnmarshalOf(want, &uGot, opts...); err != nil {
		t.Fatalf("%s: UnmarshalOf failed: %v", name, err)
	}
	if !reflect.DeepEqual(uGot, uWant) {
		t.Fatalf("%s: UnmarshalOf != Unmarshal:\ngot:  %#v\nwant: %#v", name, uGot, uWant)
	}

	bb := bytes.NewBuffer(append([]byte(nil), want...))
	if err := json.UnmarshalReadOf(bb, &rBufGot, opts...); err != nil {
		t.Fatalf("%s: UnmarshalReadOf(bytes.Buffer) failed: %v", name, err)
	}
	if bb.Len() != 0 {
		t.Fatalf("%s: UnmarshalReadOf did not consume bytes.Buffer: remaining %d", name, bb.Len())
	}
	if !reflect.DeepEqual(rBufGot, uWant) {
		t.Fatalf("%s: UnmarshalReadOf(bytes.Buffer) mismatch", name)
	}

	if err := json.UnmarshalReadOf(strings.NewReader(string(want)), &rStrGot, opts...); err != nil {
		t.Fatalf("%s: UnmarshalReadOf(strings.Reader) failed: %v", name, err)
	}
	if !reflect.DeepEqual(rStrGot, uWant) {
		t.Fatalf("%s: UnmarshalReadOf(strings.Reader) mismatch", name)
	}

	if err := json.UnmarshalDecodeOf(jsontext.NewDecoder(bytes.NewReader(want)), &dGot, opts...); err != nil {
		t.Fatalf("%s: UnmarshalDecodeOf failed: %v", name, err)
	}
	if !reflect.DeepEqual(dGot, uWant) {
		t.Fatalf("%s: UnmarshalDecodeOf mismatch", name)
	}
}

func TestV2GenericAPIParity(t *testing.T) {
	when := time.Date(2026, 11, 21, 15, 0, 0, 123456789, time.UTC)
	node := &v2GenericNode{
		Name:   "root-node",
		Count:  5,
		Score:  42.125,
		Active: true,
		Tags:   []string{"a", "b", "c"},
		Nums:   []uint64{0, 1, math.MaxUint64},
		Raw:    []byte{0xde, 0xad, 0xbe, 0xef},
		Meta:   map[string]string{"k1": "v1", "k2": "v2"},
		Extra:  map[string]any{"flag": true, "num": float64(7), "arr": []any{"x", nil}},
		Fixed:  [3]int{1, 2, 3},
		When:   when,
		Children: []*v2GenericNode{
			{Name: "leaf", Tags: []string{}, Children: []*v2GenericNode{}},
		},
	}

	assertV2Parity(t, "default/node-ptr", node, json.Deterministic(true))
	assertV2Parity(t, "default/node-val", *node, json.Deterministic(true))
	assertV2Parity(t, "omitzero/node", *node, json.Deterministic(true), json.OmitZeroStructFields(true))
	assertV2Parity(t, "multiline/node", *node, json.Deterministic(true), jsontext.WithIndent("  "))
	assertV2Parity(t, "nil-as-null", v2GenericNode{Name: "empty"}, json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true))
	assertV2Parity(t, "scalar/string", "hello, v2!")
	assertV2Parity(t, "scalar/int64", int64(-123456789))
	assertV2Parity(t, "scalar/uint64", uint64(123456789))
	assertV2Parity(t, "scalar/float64", float64(3.14159265))
	assertV2Parity(t, "scalar/bool", true)
	assertV2Parity(t, "bytes", []byte("hello bytes"))
	assertV2Parity(t, "array", [4]string{"a", "b", "c", "d"})
	assertV2Parity(t, "any-map", any(map[string]any{"a": "b", "n": float64(1), "list": []any{true, false, nil}}), json.Deterministic(true))

	// Error truncation checks.
	assertV2Parity(t, "err/nan", math.NaN())
	assertV2Parity(t, "err/chan", make(chan int))
}

func TestV2GenericOptionsAndSecurity(t *testing.T) {
	// Duplicate object names rejected by default in UnmarshalOf (struct, map, and any).
	var node v2GenericNode
	if err := json.UnmarshalOf([]byte(`{"name":"a","name":"b"}`), &node); err == nil {
		t.Fatal("UnmarshalOf(struct) allowed duplicate object names by default")
	}
	var m map[string]any
	if err := json.UnmarshalOf([]byte(`{"k":1,"k":2}`), &m); err == nil {
		t.Fatal("UnmarshalOf(map[string]any) allowed duplicate object names by default")
	}
	var a any
	if err := json.UnmarshalOf([]byte(`{"k":1,"k":2}`), &a); err == nil {
		t.Fatal("UnmarshalOf(any) allowed duplicate object names by default")
	}

	// RejectUnknownMembers.
	if err := json.UnmarshalOf([]byte(`{"name":"ok","extra_unknown":1}`), &node, json.RejectUnknownMembers(true)); err == nil {
		t.Fatal("UnmarshalOf did not reject unknown member")
	}

	// MatchCaseInsensitiveNames.
	var ciNode v2GenericNode
	if err := json.UnmarshalOf([]byte(`{"NAME":"folded","COUNT":9}`), &ciNode, json.MatchCaseInsensitiveNames(true)); err != nil {
		t.Fatalf("MatchCaseInsensitiveNames failed: %v", err)
	}
	if ciNode.Name != "folded" || ciNode.Count != 9 {
		t.Fatalf("unexpected folded result: %+v", ciNode)
	}

	// Invalid UTF-8 rejected by default.
	var s string
	if err := json.UnmarshalOf([]byte("\"\xff\""), &s); err == nil {
		t.Fatal("UnmarshalOf allowed invalid UTF-8 by default")
	}
	if _, err := json.MarshalOf("bad\xffutf8"); err == nil {
		t.Fatal("MarshalOf allowed invalid UTF-8 by default")
	}

	// Nil target returns SemanticError; nil writer/reader/encoder/decoder panics.
	var nilNode *v2GenericNode
	if err := json.UnmarshalOf([]byte(`{}`), nilNode); err == nil {
		t.Fatal("UnmarshalOf(nil) succeeded")
	}
	if err := json.UnmarshalReadOf(strings.NewReader(`{}`), nilNode); err == nil {
		t.Fatal("UnmarshalReadOf(nil) succeeded")
	}

	assertPanics := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("%s: expected panic", name)
			}
		}()
		fn()
	}
	assertPanics("MarshalWriteOf(nil)", func() { _ = json.MarshalWriteOf(nil, node) })
	assertPanics("MarshalEncodeOf(nil)", func() { _ = json.MarshalEncodeOf(nil, node) })
	assertPanics("UnmarshalReadOf(nil)", func() { _ = json.UnmarshalReadOf(nil, &node) })
	assertPanics("UnmarshalDecodeOf(nil)", func() { _ = json.UnmarshalDecodeOf(nil, &node) })

	// Verify earliest-error ordering in unmarshalObjectAny (regression test for arshal_any.go fix).
	errFirst := errors.New("first")
	errSecond := errors.New("second")
	calls := 0
	unmarshalers := json.WithUnmarshalers(json.UnmarshalFromFunc(func(dec *jsontext.Decoder, dst *any) error {
		if dec.PeekKind() != '"' {
			return errors.ErrUnsupported
		}
		if _, err := dec.ReadToken(); err != nil {
			return err
		}
		calls++
		if calls == 1 {
			return errFirst
		}
		return errSecond
	}))
	var obj any
	err := json.Unmarshal([]byte(`{"a":"1","b":"2"}`), &obj, unmarshalers)
	if !errors.Is(err, errFirst) {
		t.Fatalf("unmarshalObjectAny error ordering: got %v, want earliest error (%v)", err, errFirst)
	}
}

func TestV2GenericConcurrentRace(t *testing.T) {
	src := v2GenericNode{
		Name:   "v2-concurrent",
		Count:  3,
		Score:  12.5,
		Active: true,
		Tags:   []string{"a", "b"},
		Meta:   map[string]string{"x": "y"},
		Children: []*v2GenericNode{
			{Name: "c1", Tags: []string{}},
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
				var dst v2GenericNode
				if err := json.UnmarshalOf(buf, &dst); err != nil {
					t.Errorf("goroutine %d UnmarshalOf: %v", id, err)
					return
				}
				if dst.Name != src.Name || dst.Count != src.Count {
					t.Errorf("goroutine %d mismatch: %+v", id, dst)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

type v2BenchSmall struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	Email   string   `json:"email"`
	Active  bool     `json:"active"`
	Score   float64  `json:"score"`
	Tags    []string `json:"tags"`
	Balance uint64   `json:"balance"`
}

var v2BenchSmallVal = v2BenchSmall{
	ID:      9876543210,
	Name:    "Alice Example",
	Email:   "alice@example.com",
	Active:  true,
	Score:   98.75,
	Tags:    []string{"admin", "verified", "beta"},
	Balance: 42000,
}

var v2BenchSmallJSON = func() []byte {
	b, err := json.Marshal(v2BenchSmallVal)
	if err != nil {
		panic(err)
	}
	return b
}()

func BenchmarkV2GenericSmallStruct(b *testing.B) {
	b.Run("Marshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(v2BenchSmallJSON)))
		for b.Loop() {
			if _, err := json.Marshal(v2BenchSmallVal); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("MarshalOf", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(v2BenchSmallJSON)))
		for b.Loop() {
			if _, err := json.MarshalOf(v2BenchSmallVal); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("AppendMarshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(v2BenchSmallJSON)))
		buf := make([]byte, 0, 256)
		for b.Loop() {
			var err error
			buf, err = json.AppendMarshal(buf[:0], v2BenchSmallVal)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("AppendMarshalOf", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(v2BenchSmallJSON)))
		buf := make([]byte, 0, 256)
		for b.Loop() {
			var err error
			buf, err = json.AppendMarshalOf(buf[:0], v2BenchSmallVal)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Unmarshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(v2BenchSmallJSON)))
		for b.Loop() {
			var dst v2BenchSmall
			if err := json.Unmarshal(v2BenchSmallJSON, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("UnmarshalOf", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(v2BenchSmallJSON)))
		for b.Loop() {
			var dst v2BenchSmall
			if err := json.UnmarshalOf(v2BenchSmallJSON, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
}
