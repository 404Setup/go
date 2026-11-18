// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package textproto

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

var lookupKeys = []string{
	"",
	"a",
	"A",
	"Accept",
	"accept",
	"ACCEPT",
	"content-type",
	"Content-Type",
	"CONTENT-TYPE",
	"cOnTeNt-TyPe",
	"x-forwarded-for",
	"x-custom-thing-id",
	"X-Custom-Thing-Id",
	"x_custom",
	"X_Custom",
	"-",
	"--a--b--",
	"a-",
	"-a",
	"foo bar",
	"Foo Bar",
	"a-b c",
	"A-b c",
	"Accept ",
	" Accept",
	"foo\x00bar",
	"a-b\x00",
	"foo:bar",
	"f\u00f6\u00f6",
	"1abc",
	"abc1def-2ghi",
	strings.Repeat("a", maxLookupKeyLen),
	strings.Repeat("a", maxLookupKeyLen+1),
	"a-b c" + strings.Repeat("d", maxLookupKeyLen),
	strings.Repeat("a-", 100),
	strings.Repeat("A-B", 50),
}

func TestMIMEHeaderLookupMatchesCanonical(t *testing.T) {
	h := MIMEHeader{}
	for _, k := range lookupKeys {
		// Store under the canonical key, and also under the raw key
		// so that a lookup mistakenly bypassing canonicalization is detected.
		h[CanonicalMIMEHeaderKey(k)] = []string{"canon", k}
		if _, ok := h[k]; !ok {
			h[k] = []string{"raw", k}
		}
	}
	h["Empty-Slice"] = nil
	for _, k := range append(append([]string(nil), lookupKeys...), "empty-slice", "Empty-Slice") {
		want := h[CanonicalMIMEHeaderKey(k)]
		if got := h.Values(k); !reflect.DeepEqual(got, want) {
			t.Errorf("Values(%q) = %q, want %q", k, got, want)
		}
		wantGet := ""
		if len(want) > 0 {
			wantGet = want[0]
		}
		if got := h.Get(k); got != wantGet {
			t.Errorf("Get(%q) = %q, want %q", k, got, wantGet)
		}
	}
}

func TestMIMEHeaderLookupRandom(t *testing.T) {
	const alphabet = "abcXYZ-_ 019:\x00\u00e9"
	rng := rand.New(rand.NewSource(1))
	h := MIMEHeader{}
	var keys []string
	for i := 0; i < 2000; i++ {
		n := rng.Intn(24)
		b := make([]byte, n)
		for j := range b {
			b[j] = alphabet[rng.Intn(len(alphabet))]
		}
		k := string(b)
		keys = append(keys, k)
		if i%2 == 0 {
			h.Set(k, k)
		}
	}
	for _, k := range keys {
		want := h[CanonicalMIMEHeaderKey(k)]
		if got := h.Values(k); !reflect.DeepEqual(got, want) {
			t.Fatalf("Values(%q) = %q, want %q", k, got, want)
		}
	}
}

func TestMIMEHeaderLookupNilAndDel(t *testing.T) {
	var h MIMEHeader
	if got := h.Get("x-foo"); got != "" {
		t.Errorf("Get on nil header = %q, want empty", got)
	}
	if got := h.Values("x-foo"); got != nil {
		t.Errorf("Values on nil header = %q, want nil", got)
	}
	h.Del("x-foo")

	empty := MIMEHeader{}
	empty.Del("x-foo")
	if got := empty.Get("x-foo"); got != "" {
		t.Errorf("Get on empty header = %q, want empty", got)
	}
	if got := empty.Values("x-foo"); got != nil {
		t.Errorf("Values on empty header = %q, want nil", got)
	}

	h = MIMEHeader{
		"Content-Type":      {"text/plain"},
		"X-Custom-Thing-Id": {"1"},
		"Empty-Slice":       nil,
		"a-b c":             {"space"},
	}
	h.Del("content-type")
	h.Del("X-CUSTOM-THING-ID")
	h.Del("empty-slice")
	h.Del("A-b c") // must not canonicalize keys with spaces
	if _, ok := h["Content-Type"]; ok {
		t.Error("Content-Type not deleted")
	}
	if _, ok := h["X-Custom-Thing-Id"]; ok {
		t.Error("X-Custom-Thing-Id not deleted")
	}
	if _, ok := h["Empty-Slice"]; ok {
		t.Error("Empty-Slice not deleted")
	}
	if got := h["a-b c"]; !reflect.DeepEqual(got, []string{"space"}) {
		t.Errorf("h[\"a-b c\"] = %q after Del(\"A-b c\"), want [space]", got)
	}
	h.Del("a-b c")
	if _, ok := h["a-b c"]; ok {
		t.Error("a-b c not deleted")
	}
}

func TestMIMEHeaderGetAllocs(t *testing.T) {
	h := MIMEHeader{"X-Custom-Thing-Id": {"1"}, "Content-Type": {"text/html"}}
	for _, key := range []string{"x-custom-thing-id", "X-CUSTOM-THING-ID", "content-type", "Content-Type", "x-missing", "a-b c"} {
		allocs := testing.AllocsPerRun(100, func() {
			_ = h.Get(key)
			_ = h.Values(key)
		})
		if allocs != 0 {
			t.Errorf("Get/Values(%q) allocs = %v, want 0", key, allocs)
		}
	}
	empty := MIMEHeader{}
	if allocs := testing.AllocsPerRun(100, func() {
		if empty.Get("x-custom-thing-id") != "" || empty.Values("x-custom-thing-id") != nil {
			t.Fatal("non-empty result on empty header")
		}
		empty.Del("x-custom-thing-id")
	}); allocs != 0 {
		t.Errorf("empty Get/Values/Del allocs = %v, want 0", allocs)
	}
}

func BenchmarkMIMEHeaderGet(b *testing.B) {
	h := MIMEHeader{
		"Content-Type":      {"text/html"},
		"X-Custom-Thing-Id": {"1"},
		"X-Forwarded-For":   {"10.0.0.1"},
		"Accept-Encoding":   {"gzip"},
	}
	empty := MIMEHeader{}
	for _, bm := range []struct {
		name string
		hdr  MIMEHeader
		key  string
	}{
		{"Canonical", h, "Content-Type"},
		{"CanonicalCustom", h, "X-Custom-Thing-Id"},
		{"LowerCommon", h, "accept-encoding"},
		{"LowerCustom", h, "x-custom-thing-id"},
		{"Missing", h, "x-not-here"},
		{"Empty", empty, "x-custom-thing-id"},
	} {
		b.Run(bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				bm.hdr.Get(bm.key)
			}
		})
	}
}

func BenchmarkCanonicalMIMEHeaderKeyCases(b *testing.B) {
	for _, bm := range []struct{ name, key string }{
		{"CanonicalCommon", "Content-Type"},
		{"CanonicalCustom", "X-Custom-Thing-Id"},
		{"LowerCommon", "accept-encoding"},
		{"LowerCustom", "x-custom-thing-id"},
	} {
		b.Run(bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = CanonicalMIMEHeaderKey(bm.key)
			}
		})
	}
}
