// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package url

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// parseQueryReference is a straightforward implementation of ParseQuery that
// allocates every value slice individually. ParseQuery must agree with it.
func parseQueryReference(query string) (Values, error) {
	m := make(Values)
	var err error
	for query != "" {
		var key string
		key, query, _ = strings.Cut(query, "&")
		if strings.Contains(key, ";") {
			err = fmt.Errorf("invalid semicolon separator in query")
			continue
		}
		if key == "" {
			continue
		}
		key, value, _ := strings.Cut(key, "=")
		key, err1 := QueryUnescape(key)
		if err1 != nil {
			if err == nil {
				err = err1
			}
			continue
		}
		value, err1 = QueryUnescape(value)
		if err1 != nil {
			if err == nil {
				err = err1
			}
			continue
		}
		m[key] = append(m[key], value)
	}
	return m, err
}

var parseQueryCompareTests = []string{
	"",
	"&",
	"&&&&",
	"a",
	"a=",
	"a=1",
	"a=1&b=2",
	"a=1&a=2&a=3",
	"a=1&b=2&a=3&c=4&b=5",
	"a=1&&b=2&",
	"a+b=c+d&e%20f=g%26h",
	"a=%zz&b=2",
	"a=1&b=%zz&c=3",
	"%zz=1&b=2",
	"a=1;b=2&c=3",
	"a=1&b=2;c=3&d=4",
	"=1&=2",
	"a=b=c&d==e",
	"k=%E4%BD%A0%E5%A5%BD&%E5%90%8D=v",
	"a=1&a=%zz&a=3",
}

func TestParseQueryMatchesReference(t *testing.T) {
	queries := append([]string(nil), parseQueryCompareTests...)

	// Large inputs, including boundaries of smallQueryDistinctKeys,
	// maxQueryValBatch, and maxQueryMapHint.
	for _, n := range []int{7, 8, 9, 11, 12, 13, 63, 64, 65, 66, 129, 130, 255, 256, 257, 1000} {
		var distinct, repeated strings.Builder
		for i := 0; i < n; i++ {
			if i > 0 {
				distinct.WriteByte('&')
				repeated.WriteByte('&')
			}
			fmt.Fprintf(&distinct, "key%d=val%d", i, i)
			fmt.Fprintf(&repeated, "key%d=val%d", i%7, i)
		}
		queries = append(queries, distinct.String(), repeated.String())
	}
	queries = append(queries,
		strings.Repeat("&", 5000),
		strings.Repeat("&", 500)+"a=1&b=2",
		"a=1&b=2"+strings.Repeat("&", 500),
		"a=1&b=2&c=3&d=4&e=5&f=6&g=7"+strings.Repeat("&", 500),
		"a=1&b=2&c=3&d=4&e=5&f=6&g=7&h=8&i=9&j=10&k=11&l=12"+strings.Repeat("&", 500),
		"a=1&b=2"+strings.Repeat("&", 200)+"c=3&d=4",
		"a=1"+strings.Repeat("&a=2", 64),
		"a=1&b=2"+strings.Repeat("&c=3", 64)+"&d=4",
		"a=1&b=2&c=%zz&d=4;e=5&f=6&g=7&h=8&i=9&j=10",
		strings.Repeat("k=1&", 70)+"bad=%zz&tail=1",
	)

	for _, q := range queries {
		got, gotErr := ParseQuery(q)
		want, wantErr := parseQueryReference(q)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ParseQuery(%.60q) = %v, want %v", q, got, want)
		}
		if (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) {
			t.Errorf("ParseQuery(%.60q) error = %v, want %v", q, gotErr, wantErr)
		}
		if got == nil {
			t.Errorf("ParseQuery(%.60q) returned a nil map", q)
		}
	}
}

// TestParseQueryValueIsolation verifies that values of different keys do not
// share capacity, so appending to one key cannot overwrite another.
func TestParseQueryValueIsolation(t *testing.T) {
	for _, n := range []int{4, 16, 130} {
		q := distinctQuery(n)
		v, err := ParseQuery(q)
		if err != nil {
			t.Fatal(err)
		}
		for k, vs := range v {
			if len(vs) != 1 || cap(vs) != 1 {
				t.Errorf("n=%d v[%q]: len = %d, cap = %d, want 1, 1", n, k, len(vs), cap(vs))
			}
		}
		v["key0"] = append(v["key0"], "x", "y")
		v.Add("key1", "z")
		v.Set("key2", "w")
		if got := v["key3"]; !reflect.DeepEqual(got, []string{"value3"}) {
			t.Errorf("n=%d v[key3] = %v after modifying neighbors, want [value3]", n, got)
		}
	}

	// Repeated keys are appended to the first occurrence.
	v, err := ParseQuery("a=1&b=2&a=3&c=4&d=5&e=6&f=7&g=8&h=9&i=10&a=11")
	if err != nil {
		t.Fatal(err)
	}
	if got := v["a"]; !reflect.DeepEqual(got, []string{"1", "3", "11"}) {
		t.Errorf("v[a] = %v, want [1 3 11]", got)
	}
	v["b"] = append(v["b"], "99")
	if got := v["c"]; !reflect.DeepEqual(got, []string{"4"}) {
		t.Errorf("v[c] = %v after appending to v[b], want [4]", got)
	}
}

func TestParseQueryAllocs(t *testing.T) {
	query32 := distinctQuery(32)
	if allocs := testing.AllocsPerRun(100, func() {
		if _, err := ParseQuery(query32); err != nil {
			t.Fatal(err)
		}
	}); allocs > 16 {
		t.Errorf("ParseQuery(32 distinct) allocs = %v, want <= 16", allocs)
	}

	empty := strings.Repeat("&", 64)
	if allocs := testing.AllocsPerRun(100, func() {
		if _, err := ParseQuery(empty); err != nil {
			t.Fatal(err)
		}
	}); allocs > 1 {
		t.Errorf("ParseQuery(64 empty &) allocs = %v, want <= 1", allocs)
	}
}

func TestParseQueryLimit(t *testing.T) {
	q := strings.Repeat("a=1&", defaultMaxParams) + "a=1"
	if _, err := ParseQuery(q); err == nil {
		t.Errorf("ParseQuery with %d settings: expected error", defaultMaxParams+1)
	}
}

func repeatedQuery(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte('&')
		}
		fmt.Fprintf(&sb, "id=%d", i)
	}
	return sb.String()
}

var parseQueryBenchmarks = []struct {
	name  string
	query string
}{
	{"Short", "a=1&b=2"},
	{"Typical", "q=golang+http&page=2&sort=desc&lang=en&utm_source=newsletter&utm_medium=email"},
	{"Escaped", "name=John+Doe&email=john%40example.com&msg=hello%20world%21&x=%E4%BD%A0%E5%A5%BD"},
	{"Repeated", repeatedQuery(8)},
	{"Repeated16", repeatedQuery(16)},
	{"EmptyAmpersands64", strings.Repeat("&", 63)},
	{"SparseTrailing", "a=1&b=2" + strings.Repeat("&", 64)},
	{"Distinct9", distinctQuery(9)},
	{"Distinct16", distinctQuery(16)},
	{"Distinct32", distinctQuery(32)},
	{"Many", distinctQuery(64)},
}

// distinctQuery returns a query of n settings with distinct keys.
func distinctQuery(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte('&')
		}
		fmt.Fprintf(&sb, "key%d=value%d", i, i)
	}
	return sb.String()
}

var parseQuerySink Values

func BenchmarkParseQuery(b *testing.B) {
	for _, bm := range parseQueryBenchmarks {
		b.Run(bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				parseQuerySink, _ = ParseQuery(bm.query)
			}
		})
	}
}
