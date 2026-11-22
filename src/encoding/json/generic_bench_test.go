// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json_test

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

type benchSmallStruct struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	Email   string   `json:"email"`
	Active  bool     `json:"active"`
	Score   float64  `json:"score"`
	Tags    []string `json:"tags"`
	Balance uint64   `json:"balance"`
}

var benchSmallVal = benchSmallStruct{
	ID:      9876543210,
	Name:    "Alice Example",
	Email:   "alice@example.com",
	Active:  true,
	Score:   98.75,
	Tags:    []string{"admin", "verified", "beta"},
	Balance: 42000,
}

var benchSmallJSON = func() []byte {
	b, err := json.Marshal(benchSmallVal)
	if err != nil {
		panic(err)
	}
	return b
}()

func BenchmarkGenericSmallStructMarshal(b *testing.B) {
	b.Run("Marshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(benchSmallJSON)))
		for b.Loop() {
			if _, err := json.Marshal(benchSmallVal); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("MarshalOf", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(benchSmallJSON)))
		for b.Loop() {
			if _, err := json.MarshalOf(benchSmallVal); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("AppendMarshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(benchSmallJSON)))
		buf := make([]byte, 0, 256)
		for b.Loop() {
			var err error
			buf, err = json.AppendMarshal(buf[:0], benchSmallVal)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("AppendMarshalOf", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(benchSmallJSON)))
		buf := make([]byte, 0, 256)
		for b.Loop() {
			var err error
			buf, err = json.AppendMarshalOf(buf[:0], benchSmallVal)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkGenericSmallStructUnmarshal(b *testing.B) {
	b.Run("Unmarshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(benchSmallJSON)))
		for b.Loop() {
			var dst benchSmallStruct
			if err := json.Unmarshal(benchSmallJSON, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("UnmarshalOf", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(benchSmallJSON)))
		for b.Loop() {
			var dst benchSmallStruct
			if err := json.UnmarshalOf(benchSmallJSON, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("UnmarshalReuse", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(benchSmallJSON)))
		var dst benchSmallStruct
		for b.Loop() {
			if err := json.Unmarshal(benchSmallJSON, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("UnmarshalOfReuse", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(benchSmallJSON)))
		var dst benchSmallStruct
		for b.Loop() {
			if err := json.UnmarshalOf(benchSmallJSON, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkGenericStreamEncodeDecode(b *testing.B) {
	b.Run("Encoder.Encode", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(benchSmallJSON)))
		enc := json.NewEncoder(io.Discard)
		for b.Loop() {
			if err := enc.Encode(benchSmallVal); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("EncodeOf", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(benchSmallJSON)))
		enc := json.NewEncoder(io.Discard)
		for b.Loop() {
			if err := json.EncodeOf(enc, benchSmallVal); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Decoder.Decode", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(benchSmallJSON)))
		var dst benchSmallStruct
		for b.Loop() {
			dec := json.NewDecoder(bytes.NewReader(benchSmallJSON))
			if err := dec.Decode(&dst); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("DecodeOf", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(benchSmallJSON)))
		var dst benchSmallStruct
		for b.Loop() {
			dec := json.NewDecoder(bytes.NewReader(benchSmallJSON))
			if err := json.DecodeOf(dec, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkGenericValid(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(benchSmallJSON)))
	for b.Loop() {
		if !json.Valid(benchSmallJSON) {
			b.Fatal("expected valid JSON")
		}
	}
}
