// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.jsonv2

package json

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"encoding/json/internal/jsonflags"
	"encoding/json/internal/jsonopts"
	"encoding/json/internal/jsonwire"
	"encoding/json/jsontext"
)

func flagOpt(flags ...jsonflags.Bools) *jsonopts.Struct {
	var o jsonopts.Struct
	for _, f := range flags {
		o.Flags.Set(f | 1)
	}
	return &o
}

func TestFastBase64StrictRFC4648(t *testing.T) {
	badInputs := []string{
		`"QQ==\r\n\r\n"`,
		`"\r\n\r\n"`,
		`"QQ\r\n=="`,
		`"QQ\n=="`,
		`"QQ"`,
		`"QQ="`,
	}
	for _, in := range badInputs {
		var dstSlice []byte
		if err := Unmarshal([]byte(in), &dstSlice); err == nil {
			t.Errorf("Unmarshal(%s, &[]byte) succeeded (%x), want RFC 4648 error", in, dstSlice)
		}
		var dstArr [1]byte
		if err := Unmarshal([]byte(in), &dstArr); err == nil {
			t.Errorf("Unmarshal(%s, &[1]byte) succeeded (%x), want RFC 4648 error", in, dstArr)
		}
	}

	// Verify loose RFC 4648 option still accepts CRLF in base64.
	looseOpt := flagOpt(jsonflags.ParseBytesWithLooseRFC4648)
	var dstSlice []byte
	if err := Unmarshal([]byte(`"QQ==\r\n\r\n"`), &dstSlice, looseOpt); err != nil {
		t.Fatalf("loose Unmarshal([]byte) failed: %v", err)
	}
	if string(dstSlice) != "A" {
		t.Fatalf("loose Unmarshal([]byte) = %q, want %q", dstSlice, "A")
	}
	var dstArr [1]byte
	if err := Unmarshal([]byte(`"QQ==\r\n\r\n"`), &dstArr, looseOpt); err != nil {
		t.Fatalf("loose Unmarshal([1]byte) failed: %v", err)
	}
	if dstArr[0] != 'A' {
		t.Fatalf("loose Unmarshal([1]byte) = %q, want 'A'", dstArr[0])
	}
}

func TestFastDeepNestingIterativeSkip(t *testing.T) {
	// Test both below and above the 10000 fast-path depth limit.
	for _, depth := range []int{100, 2000, 10050} {
		nested := strings.Repeat("[", depth) + "1" + strings.Repeat("]", depth)
		in := []byte(`{"unknown":` + nested + `,"keep":42}`)
		type S struct {
			Keep int `json:"keep"`
		}
		var got S
		err := Unmarshal(in, &got)
		if depth <= 10000 {
			if err != nil {
				t.Errorf("depth=%d: Unmarshal unexpected error: %v", depth, err)
			}
			if got.Keep != 42 {
				t.Errorf("depth=%d: got.Keep = %d, want 42", depth, got.Keep)
			}
		} else if err == nil {
			t.Errorf("depth=%d: expected max depth error, got nil", depth)
		}
	}
}

func TestFastMarshalEncodeMultiValue(t *testing.T) {
	type item struct {
		X int `json:"x"`
	}
	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf)
	if err := MarshalEncode(enc, item{X: 1}); err != nil {
		t.Fatalf("MarshalEncode #1: %v", err)
	}
	if err := MarshalEncode(enc, item{X: 2}); err != nil {
		t.Fatalf("MarshalEncode #2: %v", err)
	}
	if got, want := buf.String(), "{\"x\":1}\n{\"x\":2}\n"; got != want {
		t.Fatalf("MarshalEncode multi-value = %q, want %q", got, want)
	}

	buf.Reset()
	enc = jsontext.NewEncoder(&buf)
	if err := MarshalEncodeOf(enc, item{X: 10}); err != nil {
		t.Fatalf("MarshalEncodeOf #1: %v", err)
	}
	if err := MarshalEncodeOf(enc, item{X: 20}); err != nil {
		t.Fatalf("MarshalEncodeOf #2: %v", err)
	}
	if got, want := buf.String(), "{\"x\":10}\n{\"x\":20}\n"; got != want {
		t.Fatalf("MarshalEncodeOf multi-value = %q, want %q", got, want)
	}
}

func TestFastNonZeroTargetPreservedOnSyntaxError(t *testing.T) {
	type inner struct {
		A int `json:"a"`
	}
	type target struct {
		M map[int64]inner `json:"m"`
		T time.Time       `json:"t"`
		B [4]byte         `json:"b"`
		N int64           `json:"n,string"`
	}
	origTime := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	dst := target{
		M: map[int64]inner{7: {A: 99}},
		T: origTime,
		B: [4]byte{1, 2, 3, 4},
		N: 12345,
	}
	legacyErrOpt := flagOpt(jsonflags.ReportErrorsWithLegacySemantics)
	err := Unmarshal([]byte(`{"m":{"7":{"a":1}},"t":"2026-01-01T00:00:00Z","b":"AAAA","n":"9",`), &dst, legacyErrOpt)
	if err == nil {
		t.Fatal("expected syntax error, got nil")
	}
	if dst.M[7].A != 99 || !dst.T.Equal(origTime) || dst.B != [4]byte{1, 2, 3, 4} || dst.N != 12345 {
		t.Fatalf("target mutated on syntax error with ReportErrorsWithLegacySemantics: %+v", dst)
	}
}

func TestFastReadFloatExact(t *testing.T) {
	cases := []string{
		"0", "-0", "0.0", "-0.0", "0e0", "-0e-5",
		"1", "-1", "42", "-42", "0.5", "-0.5", "0.1", "0.2", "0.3",
		"3.141592653589793", "-123.456789012345",
		// float64 exact boundaries (2^53 - 1, 2^53, 2^53 + 1, pow10 ±22/±23)
		"9007199254740991", "9007199254740992", "9007199254740993",
		"9007199254740999", "-9007199254740991", "-9007199254740993",
		"1e22", "1e23", "1e-22", "1e-23", "900719925474099e22", "900719925474099e-22",
		"123456789012345e22", "123456789012345e23", "123456789012345e-22", "123456789012345e-23",
		// float32 exact boundaries (2^24 - 1, 2^24, pow10 ±10/±11)
		"16777215", "16777216", "16777217", "1e10", "1e11", "1e-10", "1e-11",
		"16777215e10", "16777215e-10", "16777215e11", "16777215e-11",
		// Subnormals and extremes
		"5e-324", "2.2250738585072014e-308", "1.7976931348623157e+308",
		"1.401298464324817e-45", "3.4028234663852886e+38",
		"0.00000000000000000000001", "99999999999999999999999999",
	}
	for _, s := range cases {
		for _, bits := range []int{64, 32} {
			want, wantErr := strconv.ParseFloat(s, bits)
			d := fastDecoder{data: []byte(s)}
			got, err := d.readFloat(bits)
			if wantErr != nil {
				if err == nil {
					t.Errorf("readFloat(%q, %d) = %v, want error", s, bits, got)
				}
				continue
			}
			if err != nil {
				t.Errorf("readFloat(%q, %d) unexpected error: %v", s, bits, err)
				continue
			}
			if d.pos != len(s) {
				t.Errorf("readFloat(%q, %d) pos = %d, want %d", s, bits, d.pos, len(s))
			}
			if bits == 64 {
				if math.Float64bits(got) != math.Float64bits(want) {
					t.Errorf("readFloat(%q, 64) = %g (0x%x), want %g (0x%x)",
						s, got, math.Float64bits(got), want, math.Float64bits(want))
				}
			} else {
				if math.Float32bits(float32(got)) != math.Float32bits(float32(want)) {
					t.Errorf("readFloat(%q, 32) = %g (0x%x), want %g (0x%x)",
						s, float32(got), math.Float32bits(float32(got)), float32(want), math.Float32bits(float32(want)))
				}
			}
		}
	}

	bad := []string{"", "-", "+", "+1", "01", "-01", ".5", "1.", "1e", "1e+", "1e-", "1e309", "NaN", "Inf"}
	for _, s := range bad {
		d := fastDecoder{data: []byte(s)}
		if v, err := d.readFloat(64); err == nil && d.pos == len(s) {
			t.Errorf("readFloat(%q, 64) = %v, want error", s, v)
		}
	}
}

func TestFastAppendFloatExact(t *testing.T) {
	f64s := []float64{
		0, math.Copysign(0, -1), 1, -1, 42, -42,
		float64(1<<52 - 1), -float64(1<<52 - 1),
		float64(1 << 52), -float64(1 << 52),
		float64(1<<53 - 1), float64(1 << 53),
		1e20, -1e20, 1e21, -1e21, 0.5, -0.125, math.MaxFloat64, math.SmallestNonzeroFloat64,
	}
	for _, v := range f64s {
		got := string(jsonwire.AppendFloat(nil, v, 64))
		// Compare against strconv using jsonwire's exact non-integer formatting or roundtrip.
		var parsed float64
		if err := Unmarshal([]byte(got), &parsed); err != nil {
			t.Fatalf("AppendFloat(%g, 64) produced invalid JSON %q: %v", v, got, err)
		}
		if math.Float64bits(parsed) != math.Float64bits(v) {
			t.Errorf("AppendFloat(%g, 64) = %q -> %g (0x%x != 0x%x)", v, got, parsed, math.Float64bits(parsed), math.Float64bits(v))
		}
		// Ensure -0.0 produces "-0".
		if v == 0 && math.Signbit(v) && got != "-0" {
			t.Errorf("AppendFloat(-0.0, 64) = %q, want %q", got, "-0")
		}
	}
	f32s := []float32{
		0, float32(math.Copysign(0, -1)), 1, -1,
		float32(1<<23 - 1), -float32(1<<23 - 1),
		float32(1 << 23), -float32(1 << 23),
		1e7, -1e7, 0.25, math.MaxFloat32, math.SmallestNonzeroFloat32,
	}
	for _, v := range f32s {
		got := string(jsonwire.AppendFloat(nil, float64(v), 32))
		var parsed float32
		if err := Unmarshal([]byte(got), &parsed); err != nil {
			t.Fatalf("AppendFloat(%g, 32) produced invalid JSON %q: %v", v, got, err)
		}
		if math.Float32bits(parsed) != math.Float32bits(v) {
			t.Errorf("AppendFloat(%g, 32) = %q -> %g", v, got, parsed)
		}
	}
}

func FuzzFastReadFloat(f *testing.F) {
	for _, seed := range []string{"0", "-0", "1.23456789", "9007199254740991", "9007199254740993", "1e22", "1e-22", "1e23", "16777215e10"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := jsonwire.ConsumeNumber([]byte(s))
		if err != nil || n != len(s) {
			return
		}
		for _, bits := range []int{64, 32} {
			want, wantErr := strconv.ParseFloat(s, bits)
			d := fastDecoder{data: []byte(s)}
			got, gotErr := d.readFloat(bits)
			if wantErr != nil {
				if gotErr == nil {
					t.Fatalf("readFloat(%q, %d) succeeded (%v), strconv failed (%v)", s, bits, got, wantErr)
				}
				continue
			}
			if gotErr != nil || d.pos != len(s) {
				t.Fatalf("readFloat(%q, %d) failed (%v, pos=%d), strconv=%v", s, bits, gotErr, d.pos, want)
			}
			if bits == 64 && math.Float64bits(got) != math.Float64bits(want) {
				t.Fatalf("readFloat(%q, 64) = 0x%x, want 0x%x", s, math.Float64bits(got), math.Float64bits(want))
			}
			if bits == 32 && math.Float32bits(float32(got)) != math.Float32bits(float32(want)) {
				t.Fatalf("readFloat(%q, 32) = 0x%x, want 0x%x", s, math.Float32bits(float32(got)), math.Float32bits(float32(want)))
			}
		}
	})
}

func TestFastQuoteUnquoteUTF8(t *testing.T) {
	stringsToTest := []string{
		"",
		"plain ascii string without escapes 0123456789",
		"with \"quotes\" and \\backslashes\\ and \n\t\r",
		"<html>&amp;</html>",
		"2-byte UTF-8: café résumé naïve Привет мир Ελληνικά العربية",
		"3-byte safe UTF-8: 你好世界 日本語 한국어 हिन्दी ไทย",
		"3-byte boundary: \u0800 \u0fff \u2027 \u2028 \u2029 \u202a \ud7ff \ue000 \uffff",
		"4-byte emoji: 😀🚀🎉𝄞𠀀",
	}
	for _, s := range stringsToTest {
		for _, opt := range []Options{DefaultOptionsV2(), jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true)} {
			out, err := Marshal(s, opt)
			if err != nil {
				t.Fatalf("Marshal(%q) error: %v", s, err)
			}
			var got string
			if err := Unmarshal(out, &got); err != nil {
				t.Fatalf("Unmarshal(%s) error: %v", out, err)
			}
			if got != s {
				t.Errorf("roundtrip(%q) = %q (encoded=%s)", s, got, out)
			}
		}
	}

	// Invalid UTF-8 must be rejected by default and mangled with AllowInvalidUTF8.
	badUTF8 := "abc\xff\xfe\xc0\xaf\xed\xa0\x80def"
	if _, err := Marshal(badUTF8); err == nil {
		t.Error("Marshal(invalid UTF-8) succeeded, want error")
	}
	out, err := Marshal(badUTF8, jsontext.AllowInvalidUTF8(true))
	if err != nil {
		t.Fatalf("Marshal(invalid UTF-8, AllowInvalidUTF8) error: %v", err)
	}
	if !strings.Contains(string(out), "\ufffd") {
		t.Errorf("AllowInvalidUTF8 output missing replacement char: %s", out)
	}
}

func TestFastGenericMaps(t *testing.T) {
	type smallVal struct {
		A int    `json:"a"`
		B string `json:"b"`
	}
	// >128 bytes forces IndirectElem in internal/runtime/maps.
	type bigVal struct {
		ID   int64   `json:"id"`
		Data [20]int `json:"data"`
	}
	type myKey string
	type container struct {
		StrStruct map[string]smallVal          `json:"str_struct"`
		Nested    map[string]map[string]string `json:"nested"`
		NamedKey  map[myKey]int                `json:"named_key"`
		Int64Map  map[int64]smallVal           `json:"int64_map"`
		Uint64Map map[uint64][]uint64          `json:"uint64_map"`
		Int8Map   map[int8]string              `json:"int8_map"`
		Uint16Map map[uint16]bool              `json:"uint16_map"`
		BigMap    map[int64]bigVal             `json:"big_map"`
		EmptyOmit map[int64]int                `json:"empty_omit,omitempty"`
		ZeroOmit  map[string]smallVal          `json:"zero_omit,omitzero"`
		NilMap    map[uint64]string            `json:"nil_map"`
	}

	in := container{
		StrStruct: map[string]smallVal{"b": {A: 2, B: "two"}, "a": {A: 1, B: "one"}},
		Nested:    map[string]map[string]string{"x": {"k2": "v2", "k1": "v1"}},
		NamedKey:  map[myKey]int{"k": 7},
		Int64Map:  map[int64]smallVal{2: {A: 2}, 10: {A: 10}, -1: {A: -1}, -10: {A: -10}},
		Uint64Map: map[uint64][]uint64{2: {1, 2}, 10: {3}},
		Int8Map:   map[int8]string{-128: "min", 127: "max"},
		Uint16Map: map[uint16]bool{65535: true, 2: false},
		BigMap:    map[int64]bigVal{100: {ID: 100, Data: [20]int{1, 2, 3, 19: 99}}},
		EmptyOmit: map[int64]int{},
		ZeroOmit:  nil,
		NilMap:    nil,
	}

	out, err := Marshal(in, Deterministic(true))
	if err != nil {
		t.Fatalf("Marshal(container) error: %v", err)
	}
	// Verify lexicographical ordering of integer keys: "-1" < "-10" < "10" < "2".
	s := string(out)
	wantOrder := `"int64_map":{"-1":{"a":-1,"b":""},"-10":{"a":-10,"b":""},"10":{"a":10,"b":""},"2":{"a":2,"b":""}}`
	if !strings.Contains(s, wantOrder) {
		t.Fatalf("int64_map deterministic ordering mismatch:\ngot:  %s\nwant substring: %s", s, wantOrder)
	}
	wantUintOrder := `"uint64_map":{"10":[3],"2":[1,2]}`
	if !strings.Contains(s, wantUintOrder) {
		t.Fatalf("uint64_map deterministic ordering mismatch:\ngot:  %s\nwant substring: %s", s, wantUintOrder)
	}
	if strings.Contains(s, `"empty_omit"`) || strings.Contains(s, `"zero_omit"`) {
		t.Fatalf("omitempty/omitzero failed to omit empty/nil maps: %s", s)
	}

	var got container
	if err := Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal(container) error: %v", err)
	}
	got.EmptyOmit = map[int64]int{}
	got.NilMap = nil
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("roundtrip mismatch:\ngot:  %+v\nwant: %+v", got, in)
	}

	// Duplicate keys rejected by default (including "0" and "-0").
	var mInt map[int64]int
	if err := Unmarshal([]byte(`{"1":1,"1":2}`), &mInt); err == nil {
		t.Error("expected duplicate key error for {\"1\":1,\"1\":2}")
	}
	mInt = nil
	if err := Unmarshal([]byte(`{"0":1,"-0":2}`), &mInt); err == nil {
		t.Error("expected duplicate key error for {\"0\":1,\"-0\":2}")
	}

	// Duplicate keys merge in v2 vs overwrite in legacyMerge.
	var mMerge map[string]smallVal
	if err := Unmarshal([]byte(`{"k":{"a":1,"b":"hi"},"k":{"a":2}}`), &mMerge, jsontext.AllowDuplicateNames(true)); err != nil {
		t.Fatalf("AllowDuplicateNames v2 merge error: %v", err)
	}
	if mMerge["k"] != (smallVal{A: 2, B: "hi"}) {
		t.Fatalf("v2 duplicate merge = %+v, want {A:2 B:hi}", mMerge["k"])
	}
	var mLegacy map[string]smallVal
	legacyMergeOpt := flagOpt(jsonflags.AllowDuplicateNames, jsonflags.MergeWithLegacySemantics)
	if err := Unmarshal([]byte(`{"k":{"a":1,"b":"hi"},"k":{"a":2}}`), &mLegacy, legacyMergeOpt); err != nil {
		t.Fatalf("AllowDuplicateNames legacy merge error: %v", err)
	}
	if mLegacy["k"] != (smallVal{A: 2, B: ""}) {
		t.Fatalf("legacy duplicate overwrite = %+v, want {A:2 B:\"\"}", mLegacy["k"])
	}

	// map[time.Duration]int must NOT take the integer-key fast path.
	durMap := map[time.Duration]int{time.Second: 1}
	if _, err := Marshal(durMap); err == nil {
		t.Error("Marshal(map[time.Duration]int) unexpectedly succeeded without FormatDurationAsNano")
	}
}

func TestFastQuotedNumericFields(t *testing.T) {
	type S struct {
		I   int     `json:"i,string"`
		I8  int8    `json:"i8,string"`
		I16 int16   `json:"i16,string"`
		I32 int32   `json:"i32,string"`
		I64 int64   `json:"i64,string"`
		U   uint    `json:"u,string"`
		U8  uint8   `json:"u8,string"`
		U16 uint16  `json:"u16,string"`
		U32 uint32  `json:"u32,string"`
		U64 uint64  `json:"u64,string"`
		F32 float32 `json:"f32,string"`
		F64 float64 `json:"f64,string"`
	}
	in := S{
		I: -1, I8: -128, I16: -32768, I32: -2147483648, I64: -9223372036854775808,
		U: 1, U8: 255, U16: 65535, U32: 4294967295, U64: 18446744073709551615,
		F32: 1.5, F64: -2.25,
	}
	out, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal quoted struct: %v", err)
	}
	var got S
	if err := Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal quoted struct: %v", err)
	}
	if got != in {
		t.Fatalf("quoted roundtrip = %+v, want %+v", got, in)
	}

	// Unquoted null resets field in v2.
	got.I64 = 99
	if err := Unmarshal([]byte(`{"i64":null}`), &got); err != nil {
		t.Fatalf("Unmarshal unquoted null into ,string: %v", err)
	}
	if got.I64 != 0 {
		t.Fatalf("got.I64 = %d, want 0", got.I64)
	}

	// Quoted "null" is rejected in v2, accepted in legacy stringify.
	if err := Unmarshal([]byte(`{"i64":"null"}`), &got); err == nil {
		t.Error("expected v2 error for quoted \"null\" on ,string field")
	}
	legacyStrOpt := flagOpt(jsonflags.StringifyWithLegacySemantics)
	got.I64 = 77
	if err := Unmarshal([]byte(`{"i64":"null"}`), &got, legacyStrOpt); err != nil {
		t.Fatalf("legacy quoted \"null\" failed: %v", err)
	}
	if got.I64 != 0 {
		t.Fatalf("legacy quoted \"null\": got.I64 = %d, want 0", got.I64)
	}

	// Overflow and unquoted number must fail.
	if err := Unmarshal([]byte(`{"i8":"128"}`), &got); err == nil {
		t.Error("expected overflow error for i8=\"128\"")
	}
	if err := Unmarshal([]byte(`{"i64":123}`), &got); err == nil {
		t.Error("expected error for unquoted number on ,string field")
	}
}

func TestFastTimeUnmarshal(t *testing.T) {
	type S struct {
		T time.Time `json:"t"`
	}
	good := []string{
		"2026-11-22T03:04:05Z",
		"2026-11-22T03:04:05.123456789Z",
		"2026-11-22T03:04:05+05:30",
		"2026-11-22T03:04:05.5-08:00",
	}
	for _, ts := range good {
		var got S
		if err := Unmarshal([]byte(`{"t":"`+ts+`"}`), &got); err != nil {
			t.Errorf("Unmarshal(%q) failed: %v", ts, err)
			continue
		}
		want, _ := time.Parse(time.RFC3339Nano, ts)
		if !got.T.Equal(want) {
			t.Errorf("Unmarshal(%q) = %v, want %v", ts, got.T, want)
		}
	}

	// Strict RFC 3339 rejects comma subsecond, single-digit hour, and out-of-range offsets.
	strictBad := []string{
		"2026-11-22T03:04:05,123Z",
		"2026-11-22T3:04:05Z",
		"2026-11-22T03:04:05+24:00",
		"2026-11-22T03:04:05+00:60",
		"not-a-time",
	}
	for _, ts := range strictBad {
		var got S
		if err := Unmarshal([]byte(`{"t":"`+ts+`"}`), &got); err == nil {
			t.Errorf("strict Unmarshal(%q) succeeded (%v), want error", ts, got.T)
		}
	}

	// null clears time.Time in v2.
	got := S{T: time.Now()}
	if err := Unmarshal([]byte(`{"t":null}`), &got); err != nil {
		t.Fatalf("Unmarshal null time: %v", err)
	}
	if !got.T.IsZero() {
		t.Fatalf("got.T = %v, want zero", got.T)
	}
}

func TestFastByteArray(t *testing.T) {
	// Test 0-length, 32-byte (stack buffer), and 100-byte (heap buffer).
	var a0 [0]byte
	b0, err := Marshal(a0)
	if err != nil || string(b0) != `""` {
		t.Fatalf("Marshal([0]byte) = %q, %v", b0, err)
	}
	if err := Unmarshal(b0, &a0); err != nil {
		t.Fatalf("Unmarshal([0]byte): %v", err)
	}

	var a32 [32]byte
	for i := range a32 {
		a32[i] = byte(i * 7)
	}
	b32, err := Marshal(a32)
	if err != nil {
		t.Fatalf("Marshal([32]byte): %v", err)
	}
	var got32 [32]byte
	if err := Unmarshal(b32, &got32); err != nil {
		t.Fatalf("Unmarshal([32]byte): %v", err)
	}
	if got32 != a32 {
		t.Fatalf("[32]byte mismatch")
	}

	var a100 [100]byte
	for i := range a100 {
		a100[i] = byte(i)
	}
	b100, err := Marshal(a100)
	if err != nil {
		t.Fatalf("Marshal([100]byte): %v", err)
	}
	var got100 [100]byte
	if err := Unmarshal(b100, &got100); err != nil {
		t.Fatalf("Unmarshal([100]byte): %v", err)
	}
	if got100 != a100 {
		t.Fatalf("[100]byte mismatch")
	}

	// Length mismatch must fail by default.
	if err := Unmarshal([]byte(`"QQ=="`), &got32); err == nil {
		t.Error("expected error for decoded length 1 into [32]byte")
	}

	// FormatByteArrayAsArray produces JSON array of ints.
	arrOpt := flagOpt(jsonflags.FormatByteArrayAsArray)
	bArr, err := Marshal([3]byte{1, 2, 3}, arrOpt)
	if err != nil || string(bArr) != `[1,2,3]` {
		t.Fatalf("FormatByteArrayAsArray Marshal = %q, %v", bArr, err)
	}
	var got3 [3]byte
	if err := Unmarshal(bArr, &got3, arrOpt); err != nil || got3 != [3]byte{1, 2, 3} {
		t.Fatalf("FormatByteArrayAsArray Unmarshal = %v, %v", got3, err)
	}
}

type badRawMarshaler []byte

func (b badRawMarshaler) MarshalJSON() ([]byte, error) {
	if string(b) == "ERR" {
		return nil, errors.New("boom")
	}
	return b, nil
}

func TestFastCustomMarshalerUnmarshaler(t *testing.T) {
	type doc struct {
		Raw jsontext.Value `json:"raw"`
		Opt jsontext.Value `json:"opt,omitempty"`
		N   int            `json:"n"`
	}
	in := doc{
		Raw: jsontext.Value(` { "a" : 1 , "b" : [ true , false , null , "hi" ] } `),
		Opt: jsontext.Value(`null`),
		N:   5,
	}
	out, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal(doc): %v", err)
	}
	want := `{"raw":{"a":1,"b":[true,false,null,"hi"]},"n":5}`
	if string(out) != want {
		t.Fatalf("Marshal(doc) = %s, want %s", out, want)
	}

	var got doc
	if err := Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal(doc): %v", err)
	}
	if string(got.Raw) != `{"a":1,"b":[true,false,null,"hi"]}` || got.N != 5 {
		t.Fatalf("Unmarshal(doc) = %+v", got)
	}

	// Duplicate keys inside Raw value must be rejected on both Marshal and Unmarshal!
	type badDoc struct {
		B badRawMarshaler `json:"b"`
	}
	for _, bad := range []string{`{invalid`, `123 456`, `{"a":1,"a":2}`, "ERR", `"\xff"`} {
		if _, err := Marshal(badDoc{B: badRawMarshaler(bad)}); err == nil {
			t.Errorf("Marshal(badRawMarshaler(%q)) succeeded, want error", bad)
		}
	}
	if err := Unmarshal([]byte(`{"raw":{"a":1,"a":2},"n":1}`), &got); err == nil {
		t.Error("Unmarshal with duplicate key inside jsontext.Value succeeded, want error")
	}

	// UnmarshalRead fast path on *bytes.Buffer.
	buf := bytes.NewBuffer(out)
	var gotBuf doc
	if err := UnmarshalRead(buf, &gotBuf); err != nil {
		t.Fatalf("UnmarshalRead(*bytes.Buffer): %v", err)
	}
	if buf.Len() != 0 || gotBuf.N != 5 {
		t.Fatalf("UnmarshalRead(*bytes.Buffer): remaining=%d got=%+v", buf.Len(), gotBuf)
	}
}

func BenchmarkFastGenericMap(b *testing.B) {
	type entry struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Flag bool   `json:"flag"`
	}
	m := make(map[int64]entry, 64)
	for i := range int64(64) {
		m[i*13+1] = entry{ID: i, Name: "item_" + strconv.FormatInt(i, 10), Flag: i%2 == 0}
	}
	data, err := Marshal(m, Deterministic(true))
	if err != nil {
		b.Fatal(err)
	}
	b.Run("Marshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			if _, err := Marshal(m, Deterministic(true)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Unmarshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			var dst map[int64]entry
			if err := Unmarshal(data, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
}

type testV2CountCustom struct {
	calls *int
	out   []byte
	err   error
	val   string
}

func (c testV2CountCustom) MarshalJSON() ([]byte, error) {
	*c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return c.out, nil
}

func (c *testV2CountCustom) UnmarshalJSON(b []byte) error {
	*c.calls++
	if c.err != nil {
		return c.err
	}
	c.val = string(b)
	return nil
}

type testV2DirectCustom struct {
	calls *int
}

func (c testV2DirectCustom) MarshalJSON() ([]byte, error) {
	*c.calls++
	return []byte(`{"x":"<a>\u0020\u2028"}`), nil
}

func testV2GrowStack(depth int, fn func()) {
	var pad [256]byte
	pad[depth&255] = byte(depth)
	if depth > 0 {
		testV2GrowStack(depth-1, fn)
		runtime.KeepAlive(pad)
		return
	}
	fn()
}

func TestFastCustomSingleInvocationAndPartialOutput(t *testing.T) {
	t.Run("PartialOutputOnSiblingFailure", func(t *testing.T) {
		type doc struct {
			First  testV2DirectCustom `json:"first"`
			Second float64            `json:"second"`
		}
		calls := 0
		in := doc{First: testV2DirectCustom{calls: &calls}, Second: math.NaN()}

		// v2 non-legacy preserves partial output emitted before failing member value.
		out, err := Marshal(in)
		if err == nil {
			t.Fatal("Marshal succeeded, want error")
		}
		wantPrefix := "{\"first\":{\"x\":\"<a> \u2028\"},\"second\""
		if string(out) != wantPrefix {
			t.Fatalf("Marshal partial = %q, want %q", out, wantPrefix)
		}
		if calls != 1 {
			t.Fatalf("Marshal calls = %d, want 1", calls)
		}

		// With EscapeForHTML and EscapeForJS enabled, custom output is re-escaped.
		calls = 0
		outEsc, err := Marshal(in, jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
		if err == nil {
			t.Fatal("Marshal(EscapeHTML/JS) succeeded, want error")
		}
		wantEscPrefix := `{"first":{"x":"\u003ca\u003e \u2028"},"second"`
		if string(outEsc) != wantEscPrefix {
			t.Fatalf("Marshal(EscapeHTML/JS) partial = %q, want %q", outEsc, wantEscPrefix)
		}

		// MarshalOf preserves partial output; MarshalWriteOf does not flush small partial buffers on error (matching MarshalWrite).
		calls = 0
		outOf, err := MarshalOf(in)
		if err == nil {
			t.Fatal("MarshalOf succeeded, want error")
		}
		if string(outOf) != wantPrefix || calls != 1 {
			t.Fatalf("MarshalOf partial=%q calls=%d, want %q / 1", outOf, calls, wantPrefix)
		}

		calls = 0
		var wb bytes.Buffer
		if err := MarshalWriteOf(&wb, in); err == nil {
			t.Fatal("MarshalWriteOf succeeded, want error")
		}
		if wb.Len() != 0 {
			t.Fatalf("MarshalWriteOf partial = %q, want empty unflushed buffer", wb.String())
		}
		if calls != 1 {
			t.Fatalf("MarshalWriteOf calls = %d, want 1", calls)
		}

		// Legacy mode omits partial output.
		calls = 0
		outLeg, err := Marshal(in, flagOpt(jsonflags.ReportErrorsWithLegacySemantics))
		if err == nil {
			t.Fatal("legacy Marshal succeeded, want error")
		}
		if outLeg != nil || calls != 1 {
			t.Fatalf("legacy Marshal out=%v calls=%d, want nil / 1", outLeg, calls)
		}
	})

	t.Run("CustomJSONDepth10000AndOver8Keys", func(t *testing.T) {
		manyKeys := `{"k1":1,"k2":2,"k3":3,"k4":4,"k5":5,"k6":6,"k7":7,"k8":8,"k9":9,"k10":10}`
		calls := 0
		type doc struct {
			A testV2CountCustom `json:"a"`
		}
		out, err := MarshalOf(doc{A: testV2CountCustom{calls: &calls, out: []byte(manyKeys)}})
		if err != nil {
			t.Fatalf("MarshalOf >8 keys: %v", err)
		}
		if calls != 1 || string(out) != `{"a":`+manyKeys+`}` {
			t.Fatalf("calls=%d out=%s", calls, out)
		}

		// Depth 10000 succeeds; depth 10001 fails, both calling MarshalJSON once.
		d10000 := strings.Repeat("[", 9999) + "1" + strings.Repeat("]", 9999)
		calls = 0
		if _, err := Marshal(doc{A: testV2CountCustom{calls: &calls, out: []byte(d10000)}}); err != nil {
			t.Fatalf("depth 10000 inside custom: %v", err)
		}
		if calls != 1 {
			t.Fatalf("depth 10000 calls = %d, want 1", calls)
		}
		d10001 := strings.Repeat("[", 10000) + "1" + strings.Repeat("]", 10000)
		calls = 0
		if _, err := Marshal(doc{A: testV2CountCustom{calls: &calls, out: []byte(d10001)}}); err == nil {
			t.Fatal("depth 10001 inside custom succeeded, want error")
		}
		if calls != 1 {
			t.Fatalf("depth 10001 calls = %d, want 1", calls)
		}
	})

	t.Run("UnmarshalInvalidAndSiblingSingleCall", func(t *testing.T) {
		calls := 0
		type doc struct {
			A testV2CountCustom `json:"a"`
			B int               `json:"b"`
		}
		dst := doc{A: testV2CountCustom{calls: &calls}}
		if err := Unmarshal([]byte(`{"a":1,"b":`), &dst, flagOpt(jsonflags.ReportErrorsWithLegacySemantics)); err == nil {
			t.Fatal("expected syntax error")
		}
		if calls != 0 {
			t.Fatalf("legacy invalid JSON calls = %d, want 0", calls)
		}

		calls = 0
		if err := Unmarshal([]byte(`{"a":{"k":1},"b":"bad"}`), &dst); err == nil {
			t.Fatal("expected semantic error")
		}
		if calls != 1 || dst.A.val != `{"k":1}` {
			t.Fatalf("sibling error calls=%d val=%q", calls, dst.A.val)
		}
	})

	t.Run("AnyCycleDepth1001", func(t *testing.T) {
		calls := 0
		root := make([]any, 2)
		root[0] = testV2DirectCustom{calls: &calls}
		root[1] = root
		if _, err := Marshal(root); err == nil {
			t.Fatal("expected cycle error on []any")
		}

		mroot := map[string]any{"a": testV2DirectCustom{calls: &calls}}
		mroot["b"] = mroot
		if _, err := Marshal(mroot); err == nil {
			t.Fatal("expected cycle error on map[string]any")
		}
	})
}

func TestFastOptionsAndEdgeSemantics(t *testing.T) {
	t.Run("StringifyNumbersAndQuotedString", func(t *testing.T) {
		type S struct {
			N int64   `json:"n"`
			F float64 `json:"f"`
			S string  `json:"s,string"`
		}
		in := S{N: 42, F: 3.5, S: "hi"}
		// Without StringifyWithLegacySemantics and without ReportErrorsWithLegacySemantics, string tag on string fails.
		if _, err := Marshal(in); err == nil {
			t.Fatal("expected error for string tag on string in strict v2")
		}
		// With StringifyWithLegacySemantics + StringifyNumbers.
		opts := JoinOptions(flagOpt(jsonflags.StringifyWithLegacySemantics), StringifyNumbers(true))
		out, err := Marshal(in, opts)
		if err != nil {
			t.Fatalf("Marshal StringifyNumbers: %v", err)
		}
		want := `{"n":"42","f":"3.5","s":"\"hi\""}`
		if string(out) != want {
			t.Fatalf("got %s, want %s", out, want)
		}
	})

	t.Run("AllowDuplicateNamesAndMapAnyMerge", func(t *testing.T) {
		type inner struct {
			A int `json:"a"`
			B int `json:"b"`
		}
		m := map[string]any{
			"obj": &inner{A: 1},
		}
		data := []byte(`{"obj":{"a":10},"obj":{"b":20}}`)
		if err := Unmarshal(data, &m, jsontext.AllowDuplicateNames(true)); err != nil {
			t.Fatalf("Unmarshal AllowDuplicateNames: %v", err)
		}
		got := m["obj"].(*inner)
		if got.A != 10 || got.B != 20 {
			t.Fatalf("map[string]any duplicate key merge = %+v, want {10 20}", got)
		}

		// Duplicate invalid UTF-8 keys in map with AllowInvalidUTF8 + !AllowDuplicateNames must fail.
		badKeyMap := map[string]int{"\x80": 1, "\x81": 2}
		if _, err := Marshal(badKeyMap, flagOpt(jsonflags.AllowInvalidUTF8)); err == nil {
			t.Fatal("expected duplicate invalid UTF-8 key error (both coerce to U+FFFD)")
		}
	})

	t.Run("UnmarshalArrayFromAnyLengthAndMerge", func(t *testing.T) {
		dst := [4]int{1, 2, 3, 4}
		opts := flagOpt(jsonflags.UnmarshalArrayFromAnyLength, jsonflags.MergeWithLegacySemantics)
		if err := Unmarshal([]byte(`[9,8]`), &dst, opts); err != nil {
			t.Fatalf("UnmarshalArrayFromAnyLength: %v", err)
		}
		if dst != [4]int{9, 8, 0, 0} {
			t.Fatalf("underflow array = %v, want [9 8 0 0]", dst)
		}
	})

	t.Run("NegZeroOmitZeroAndStrictRFC3339", func(t *testing.T) {
		type S struct {
			Z float64   `json:"z,omitzero"`
			T time.Time `json:"t"`
		}
		b, err := Marshal(S{Z: math.Copysign(0, -1), T: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if strings.Contains(string(b), `"z"`) {
			t.Fatalf("-0.0 should be omitted with omitzero: %s", b)
		}
		var dst S
		if err := Unmarshal([]byte(`{"t":"2026-01-02T03:04:05,5Z"}`), &dst); err == nil {
			t.Fatal("comma fractional separator in RFC3339 should fail strict v2 Unmarshal")
		}
		if err := Unmarshal([]byte(`{"t":"2026-01-02T03:04:05,5Z"}`), &dst, flagOpt(jsonflags.ParseTimeWithLooseRFC3339)); err != nil {
			t.Fatalf("loose RFC3339 failed: %v", err)
		}
	})

	t.Run("PreserveRawStringsAndDecodeOfContiguous", func(t *testing.T) {
		type S struct {
			A string `json:"a"`
		}
		out, err := Marshal(S{A: "<script>"}, flagOpt(jsonflags.PreserveRawStrings))
		if err != nil {
			t.Fatalf("PreserveRawStrings Marshal: %v", err)
		}
		_ = out

		// UnmarshalDecodeOf multi-value stream.
		stream := `{"a":"one"} {"a":"two"}`
		dec := jsontext.NewDecoder(strings.NewReader(stream))
		var s1, s2 S
		if err := UnmarshalDecodeOf(dec, &s1); err != nil || s1.A != "one" {
			t.Fatalf("UnmarshalDecodeOf 1 = %+v, %v", s1, err)
		}
		if err := UnmarshalDecodeOf(dec, &s2); err != nil || s2.A != "two" {
			t.Fatalf("UnmarshalDecodeOf 2 = %+v, %v", s2, err)
		}
	})

	t.Run("NoescapeStackGrowthGCStress", func(t *testing.T) {
		calls := 0
		for range 200 {
			type S struct {
				C testV2DirectCustom `json:"c"`
			}
			in := S{C: testV2DirectCustom{calls: &calls}}
			testV2GrowStack(64, func() {
				runtime.GC()
				if _, err := MarshalOf(in); err != nil {
					t.Fatalf("MarshalOf under GC stress: %v", err)
				}
			})
		}
		if calls != 200 {
			t.Fatalf("calls = %d, want 200", calls)
		}
	})
}

type testV2RecCustom struct {
	calls *int
	val   string
}

func (c testV2RecCustom) MarshalJSON() ([]byte, error) {
	if c.calls != nil {
		*c.calls++
	}
	return Marshal(c.val)
}

func (c *testV2RecCustom) UnmarshalJSON(b []byte) error {
	if c.calls != nil {
		*c.calls++
	}
	return Unmarshal(b, &c.val)
}

func TestFastRegressionDAGCycleAndReplay(t *testing.T) {
	t.Run("DAGSharedTypeNotMarkedCyclic", func(t *testing.T) {
		type sharedLeaf struct {
			K string `json:"k"`
			V int    `json:"v"`
		}
		type dagRoot struct {
			A sharedLeaf   `json:"a"`
			B *sharedLeaf  `json:"b"`
			C []sharedLeaf `json:"c"`
			D any          `json:"d"`
		}
		mp := lookupFastMarshalPlan(abiTypeOfReflect(reflect.TypeFor[dagRoot]()))
		up := lookupFastUnmarshalPlan(abiTypeOfReflect(reflect.TypeFor[dagRoot]()))
		if mp.hasCycle || up.hasCycle {
			t.Fatalf("DAG marked cyclic: mp.hasCycle=%v up.hasCycle=%v", mp.hasCycle, up.hasCycle)
		}
		if !mp.canFast || !up.canFast {
			t.Fatalf("DAG fast path disabled: mp.canFast=%v up.canFast=%v", mp.canFast, up.canFast)
		}
		in := dagRoot{
			A: sharedLeaf{K: "one", V: 1},
			B: &sharedLeaf{K: "two", V: 2},
			C: []sharedLeaf{{K: "three", V: 3}},
			D: map[string]any{"x": float64(4)},
		}
		got, err := Marshal(in)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var out dagRoot
		if err := Unmarshal(got, &out); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if !reflect.DeepEqual(out, in) {
			t.Fatalf("roundtrip mismatch: got %+v, want %+v", out, in)
		}
	})

	t.Run("TrueCycleWithCustomAndAnyStaysFast", func(t *testing.T) {
		type node struct {
			Name string          `json:"name"`
			Raw  any             `json:"raw,omitempty"`
			C    testV2RecCustom `json:"c"`
			Next *node           `json:"next,omitempty"`
		}
		mp := lookupFastMarshalPlan(abiTypeOfReflect(reflect.TypeFor[node]()))
		up := lookupFastUnmarshalPlan(abiTypeOfReflect(reflect.TypeFor[node]()))
		if !mp.hasCycle || !up.hasCycle {
			t.Fatalf("expected true cycle detection: mp=%v up=%v", mp.hasCycle, up.hasCycle)
		}
		if !mp.canFast || !up.canFast {
			t.Fatalf("expected hasCustom&&hasCycle fast path enabled: mp=%v up=%v", mp.canFast, up.canFast)
		}
		calls := 0
		in := &node{
			Name: "root",
			Raw:  "val",
			C:    testV2RecCustom{calls: &calls, val: "custom1"},
			Next: &node{
				Name: "child",
				C:    testV2RecCustom{calls: &calls, val: "custom2"},
			},
		}
		b, err := Marshal(in)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if calls != 2 {
			t.Fatalf("marshal custom calls = %d, want 2", calls)
		}
		var out node
		if err := Unmarshal(b, &out); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if out.Name != "root" || out.C.val != "custom1" || out.Next == nil || out.Next.Name != "child" || out.Next.C.val != "custom2" {
			t.Fatalf("unexpected decoded tree: %+v", out)
		}
	})

	t.Run("MapWithCyclicCustomUnmarshalerDisabledSafely", func(t *testing.T) {
		type cyclicCustom struct {
			C    testV2RecCustom `json:"c"`
			Next *cyclicCustom   `json:"next,omitempty"`
		}
		mp := lookupFastUnmarshalPlan(abiTypeOfReflect(reflect.TypeFor[map[string]cyclicCustom]()))
		if mp.canFast {
			t.Fatalf("map with cyclic custom unmarshaler element must not use fast unmarshal")
		}
		var m map[string]cyclicCustom
		if err := Unmarshal([]byte(`{"k":{"c":"v","next":{"c":"v2"}}}`), &m); err != nil {
			t.Fatalf("Unmarshal map[string]cyclicCustom: %v", err)
		}
		if m["k"].C.val != "v" || m["k"].Next == nil || m["k"].Next.C.val != "v2" {
			t.Fatalf("decoded map mismatch: %+v", m)
		}
	})

	t.Run("CustomUnmarshalerAndTimeErrorReplay", func(t *testing.T) {
		type S struct {
			T1   time.Time       `json:"t1"`
			C1   testV2RecCustom `json:"c1"`
			T2   time.Time       `json:"t2"`
			C2   testV2RecCustom `json:"c2"`
			Tail int             `json:"tail"`
		}

		// Case 1: T1 precedes C1; T2 has an invalid timestamp.
		// C1 must run once and T2 must not be skipped by custom-unmarshaler replay.
		var c1Calls, c2Calls int
		s := S{C1: testV2RecCustom{calls: &c1Calls}, C2: testV2RecCustom{calls: &c2Calls}}
		inBadTimeAfter := []byte(`{"t1":"2026-01-02T03:04:05Z","c1":"ok1","t2":"not-a-time","c2":"ok2","tail":7}`)
		err := Unmarshal(inBadTimeAfter, &s)
		if err == nil {
			t.Fatal("expected error for invalid t2")
		}
		if c1Calls != 1 {
			t.Fatalf("c1 calls = %d, want 1", c1Calls)
		}

		// Case 2: Legacy semantics with custom unmarshaler before invalid T2 and trailing valid field.
		c1Calls, c2Calls = 0, 0
		s2 := S{C1: testV2RecCustom{calls: &c1Calls}, C2: testV2RecCustom{calls: &c2Calls}}
		legacyOpts := flagOpt(jsonflags.ReportErrorsWithLegacySemantics, jsonflags.MergeWithLegacySemantics)
		err = Unmarshal(inBadTimeAfter, &s2, legacyOpts)
		if err == nil {
			t.Fatal("expected legacy error for invalid t2")
		}
		if c1Calls != 1 {
			t.Fatalf("legacy c1 calls = %d, want 1", c1Calls)
		}

		// Case 3: Legacy semantics where C1 runs before Tail type error ("tail":"bad") causing replay.
		c1Calls, c2Calls = 0, 0
		s3 := S{C1: testV2RecCustom{calls: &c1Calls}, C2: testV2RecCustom{calls: &c2Calls}}
		err = Unmarshal([]byte(`{"c1":"ok1","t1":"2026-01-02T03:04:05Z","tail":"bad"}`), &s3, legacyOpts)
		if err == nil {
			t.Fatal("expected error for non-int tail")
		}
		if c1Calls != 1 {
			t.Fatalf("legacy replay c1 calls = %d, want 1", c1Calls)
		}
		if s3.C1.val != "ok1" {
			t.Fatalf("legacy partial c1 val = %q, want ok1", s3.C1.val)
		}
	})

	t.Run("SlicePoint2F64Parity", func(t *testing.T) {
		type ring struct {
			Pts [][2]float64 `json:"pts"`
		}
		in := ring{Pts: [][2]float64{{-122.4194, 37.7749}, {0, -0.0}, {139.6917, 35.6895}}}
		b, err := Marshal(in)
		if err != nil {
			t.Fatalf("Marshal ring: %v", err)
		}
		var gotV2 ring
		if err := Unmarshal(b, &gotV2); err != nil {
			t.Fatalf("Unmarshal v2 ring: %v", err)
		}
		if !reflect.DeepEqual(gotV2, in) {
			t.Fatalf("v2 ring mismatch: got %+v, want %+v", gotV2, in)
		}

		// Legacy merge into non-empty slice with null element must fall back cleanly and preserve existing element.
		pre := ring{Pts: [][2]float64{{10, 20}, {30, 40}}}
		if err := Unmarshal([]byte(`{"pts":[[1,2],null]}`), &pre, flagOpt(jsonflags.MergeWithLegacySemantics)); err != nil {
			t.Fatalf("Unmarshal legacy merge ring: %v", err)
		}
		if pre.Pts[0] != [2]float64{1, 2} || pre.Pts[1] != [2]float64{30, 40} {
			t.Fatalf("legacy merge ring = %+v, want [[1 2] [30 40]]", pre.Pts)
		}

		// NaN error path parity.
		badRing := ring{Pts: [][2]float64{{1, 2}, {3, math.NaN()}}}
		if _, err := Marshal(badRing); err == nil {
			t.Fatal("expected error marshaling NaN in [][2]float64")
		}
	})
}

func TestFastGenericOfZeroAlloc(t *testing.T) {
	type flat struct {
		A int64   `json:"a"`
		B string  `json:"b"`
		C float64 `json:"c"`
		D bool    `json:"d"`
	}
	v := flat{A: 42, B: "hello", C: 3.5, D: true}
	buf := make([]byte, 0, 256)
	if n := testing.AllocsPerRun(50, func() {
		var err error
		buf, err = AppendMarshalOf(buf[:0], v)
		if err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("AppendMarshalOf allocs = %v, want 0", n)
	}
	if n := testing.AllocsPerRun(50, func() {
		out, err := MarshalOf(v)
		if err != nil || len(out) == 0 {
			t.Fatalf("MarshalOf: %v", err)
		}
	}); n != 1 {
		t.Fatalf("MarshalOf allocs = %v, want 1", n)
	}
}

