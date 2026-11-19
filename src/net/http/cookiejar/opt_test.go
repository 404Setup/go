// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cookiejar

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestEntryKeySemicolonIsolation verifies that cookies whose Path/Name contain
// semicolons do not collide in the jar's internal map.
func TestEntryKeySemicolonIsolation(t *testing.T) {
	j, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://example.com/a;b/c")
	j.SetCookies(u, []*http.Cookie{
		{Name: "c", Value: "v1", Path: "/a;b"},
		{Name: "b;c", Value: "v2", Path: "/a"},
	})

	u1, _ := url.Parse("https://example.com/a;b/x")
	got1 := j.Cookies(u1)
	if len(got1) != 1 || got1[0].Name != "c" || got1[0].Value != "v1" {
		t.Fatalf("u1 cookies = %+v, want [c=v1]", got1)
	}
	u2, _ := url.Parse("https://example.com/a/x")
	got2 := j.Cookies(u2)
	if len(got2) != 1 || got2[0].Name != "b;c" || got2[0].Value != "v2" {
		t.Fatalf("u2 cookies = %+v, want [b;c=v2]", got2)
	}
}

func TestIsIPZeroAllocAndEquivalence(t *testing.T) {
	cases := []string{
		"",
		"example.com",
		"www.example.com",
		"127.0.0.1",
		"0.0.0.0",
		"255.255.255.255",
		"256.0.0.1",
		"01.2.3.4",
		"1.2.3",
		"1.2.3.4.",
		".1.2.3.4",
		"1.2.3.4a",
		"a1.2.3.4",
		"1234",
		"::1",
		"2001:db8::1",
		"::1%.www.example.com",
		"fe80::1%eth0",
	}
	for _, host := range cases {
		want := strings.ContainsAny(host, ":%") || net.ParseIP(host) != nil
		got := isIP(host)
		if got != want {
			t.Errorf("isIP(%q) = %v, want %v", host, got, want)
		}
	}

	for _, host := range []string{"www.example.com", "127.0.0.1", "::1"} {
		if allocs := testing.AllocsPerRun(100, func() {
			_ = isIP(host)
		}); allocs != 0 {
			t.Errorf("isIP(%q) allocs = %v, want 0", host, allocs)
		}
	}
}

func TestJarCookiesAllocsAndConcurrent(t *testing.T) {
	j, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://www.example.com/app/page")
	now := time.Now()
	var set []*http.Cookie
	for i := 0; i < 8; i++ {
		set = append(set, &http.Cookie{
			Name:  fmt.Sprintf("k%d", i),
			Value: fmt.Sprintf("v%d", i),
			Path:  "/app",
		})
	}
	j.setCookies(u, set, now)

	// Updating existing cookies should not allocate strings via fmt.Sprintf.
	if allocs := testing.AllocsPerRun(100, func() {
		j.setCookies(u, set, now)
	}); allocs != 0 {
		t.Errorf("setCookies update allocs = %v, want 0", allocs)
	}

	// Reading 8 cookies should allocate only the cookie slab and []*http.Cookie slice (2 allocs).
	if allocs := testing.AllocsPerRun(100, func() {
		got := j.cookies(u, now)
		if len(got) != 8 {
			t.Fatalf("got %d cookies, want 8", len(got))
		}
	}); allocs > 2 {
		t.Errorf("cookies(8) allocs = %v, want <= 2", allocs)
	}

	// Concurrent Cookies + SetCookies under -race.
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if id%2 == 0 {
					_ = j.Cookies(u)
				} else {
					j.SetCookies(u, set[:2])
				}
			}
		}(g)
	}
	wg.Wait()
}

func BenchmarkJarCookies(b *testing.B) {
	j, _ := New(nil)
	u, _ := url.Parse("https://www.example.com/app/page")
	now := time.Now()
	var set []*http.Cookie
	for i := 0; i < 8; i++ {
		set = append(set, &http.Cookie{
			Name:  fmt.Sprintf("cookie_%d", i),
			Value: fmt.Sprintf("value_%d", i),
			Path:  "/app",
		})
	}
	j.setCookies(u, set, now)
	b.ReportAllocs()
	for b.Loop() {
		_ = j.cookies(u, now)
	}
}

func BenchmarkJarSetCookies(b *testing.B) {
	j, _ := New(nil)
	u, _ := url.Parse("https://www.example.com/app/page")
	now := time.Now()
	set := []*http.Cookie{
		{Name: "session", Value: "abc123", Path: "/"},
		{Name: "prefs", Value: "dark", Path: "/app"},
	}
	j.setCookies(u, set, now)
	b.ReportAllocs()
	for b.Loop() {
		j.setCookies(u, set, now)
	}
}
