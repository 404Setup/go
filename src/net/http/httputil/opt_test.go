// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httputil

import (
	"net/http"
	"reflect"
	"slices"
	"testing"
)

func TestCopyHeaderSlabIsolationAndDuplicateCanonicalKeys(t *testing.T) {
	t.Run("SliceIsolation", func(t *testing.T) {
		src := http.Header{
			"X-One":   {"1"},
			"X-Two":   {"2a", "2b"},
			"X-Empty": {},
		}
		dst := make(http.Header, len(src))
		copyHeader(dst, src)

		if _, ok := dst["X-Empty"]; ok {
			t.Errorf("X-Empty should not be copied into dst")
		}
		// Appending to X-One must not overwrite X-Two in the shared slab.
		dst["X-One"] = append(dst["X-One"], "1-mutated")
		if !reflect.DeepEqual(dst["X-Two"], []string{"2a", "2b"}) {
			t.Fatalf("appending to X-One corrupted X-Two: %#v", dst["X-Two"])
		}
		if !reflect.DeepEqual(src["X-One"], []string{"1"}) {
			t.Fatalf("appending to dst corrupted src: %#v", src["X-One"])
		}
	})

	t.Run("DuplicateCanonicalKeys", func(t *testing.T) {
		src := http.Header{
			"x-dup": {"a"},
			"X-Dup": {"b"},
			"X-Adj": {"keep"},
		}
		dst := make(http.Header)
		copyHeader(dst, src)

		got := append([]string(nil), dst["X-Dup"]...)
		slices.Sort(got)
		if !reflect.DeepEqual(got, []string{"a", "b"}) {
			t.Fatalf("X-Dup = %#v, want [a b]", dst["X-Dup"])
		}
		if !reflect.DeepEqual(dst["X-Adj"], []string{"keep"}) {
			t.Fatalf("X-Adj corrupted: %#v", dst["X-Adj"])
		}
	})

	t.Run("NonEmptyDst", func(t *testing.T) {
		dst := http.Header{"X-One": {"0"}}
		src := http.Header{"x-one": {"1"}, "X-Two": {"2"}}
		copyHeader(dst, src)
		if !reflect.DeepEqual(dst["X-One"], []string{"0", "1"}) {
			t.Fatalf("X-One = %#v, want [0 1]", dst["X-One"])
		}
		if !reflect.DeepEqual(dst["X-Two"], []string{"2"}) {
			t.Fatalf("X-Two = %#v, want [2]", dst["X-Two"])
		}
	})
}
