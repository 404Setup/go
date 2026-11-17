// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package internal

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestAppendHTTPDate(t *testing.T) {
	times := []time.Time{
		time.Date(0, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1601, time.February, 28, 23, 59, 59, 0, time.UTC),
		time.Date(1970, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1994, time.November, 6, 8, 49, 37, 0, time.UTC),
		time.Date(2000, time.February, 29, 12, 0, 0, 0, time.UTC),
		time.Date(2024, time.July, 4, 15, 30, 45, 0, time.FixedZone("EST", -5*3600)),
		time.Date(2100, time.March, 1, 1, 2, 3, 0, time.UTC),
		time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC),
		time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(-1, time.June, 15, 10, 20, 30, 0, time.UTC),
	}
	// Cover every weekday and every month.
	for d := range 7 {
		times = append(times, time.Date(2026, time.March, 1+d, 9, 8, 7, 0, time.UTC))
	}
	for m := time.January; m <= time.December; m++ {
		times = append(times, time.Date(2026, m, 15, 16, 17, 18, 0, time.UTC))
	}

	for _, tm := range times {
		want := tm.UTC().Format(HTTPTimeFormat)
		got := string(AppendHTTPDate(nil, tm))
		if got != want {
			t.Errorf("AppendHTTPDate(nil, %v) = %q, want %q", tm, got, want)
		}
		prefix := "Date: "
		gotPrefixed := string(AppendHTTPDate([]byte(prefix), tm))
		if gotPrefixed != prefix+want {
			t.Errorf("AppendHTTPDate(%q, %v) = %q, want %q", prefix, tm, gotPrefixed, prefix+want)
		}
	}
}

func TestHTTPDateNow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got1 := HTTPDateNowString()
		buf1 := string(AppendHTTPDateNow(nil))
		want1 := time.Now().UTC().Format(HTTPTimeFormat)
		if got1 != want1 || buf1 != want1 {
			t.Fatalf("t0: got (%q, %q), want %q", got1, buf1, want1)
		}

		time.Sleep(1 * time.Second)

		got2 := HTTPDateNowString()
		buf2 := string(AppendHTTPDateNow(nil))
		want2 := time.Now().UTC().Format(HTTPTimeFormat)
		if got2 != want2 || buf2 != want2 {
			t.Fatalf("t0+1s: got (%q, %q), want %q (cached stale date inside bubble?)", got2, buf2, want2)
		}
		if got2 == got1 {
			t.Fatalf("date did not advance after 1s sleep: %q", got2)
		}
	})

	// Outside synctest bubble, cached string and bytes formatting match.
	s := HTTPDateNowString()
	b := string(AppendHTTPDateNow([]byte("Date: ")))
	if len(s) != len(HTTPTimeFormat) {
		t.Fatalf("HTTPDateNowString() = %q, len = %d, want %d", s, len(s), len(HTTPTimeFormat))
	}
	if _, ok := ParseHTTPDate(s); !ok {
		t.Fatalf("ParseHTTPDate(%q) failed", s)
	}
	if _, ok := ParseHTTPDate(b[len("Date: "):]); !ok {
		t.Fatalf("ParseHTTPDate(%q) failed", b[len("Date: "):])
	}
}

func TestParseHTTPDateDifferential(t *testing.T) {
	layouts := []string{HTTPTimeFormat, time.RFC850, time.ANSIC}
	parseWithTime := func(s string) (time.Time, bool) {
		for _, layout := range layouts {
			if tm, err := time.Parse(layout, s); err == nil {
				return tm.UTC(), true
			}
		}
		return time.Time{}, false
	}

	inputs := []string{
		// RFC 1123 / HTTPTimeFormat
		"Sun, 06 Nov 1994 08:49:37 GMT",
		"Mon, 01 Jan 0000 00:00:00 GMT",
		"Fri, 31 Dec 9999 23:59:59 GMT",
		"Thu, 29 Feb 2024 12:00:00 GMT", // leap year
		"Sat, 29 Feb 2000 12:00:00 GMT", // leap century
		"Wed, 29 Feb 2023 12:00:00 GMT", // non-leap year
		"Mon, 29 Feb 1900 12:00:00 GMT", // non-leap century
		"Tue, 31 Apr 2024 12:00:00 GMT", // invalid day for April
		"Tue, 00 Jan 2024 12:00:00 GMT",
		"Tue, 32 Jan 2024 12:00:00 GMT",
		"Tue, 01 Jan 2024 24:00:00 GMT",
		"Tue, 01 Jan 2024 00:60:00 GMT",
		"Tue, 01 Jan 2024 00:00:60 GMT",
		"Sun, 06 Nov 1994 08:49:37 UTC", // wrong tz for ParseHTTPDate fast path
		"Foo, 06 Nov 1994 08:49:37 GMT", // invalid weekday
		"Sun, 06 FOO 1994 08:49:37 GMT", // invalid month

		// ANSIC
		"Sun Nov  6 08:49:37 1994",
		"Sun Nov 16 08:49:37 1994",
		"Mon Jan  1 00:00:00 2024",
		"Tue Feb 29 23:59:59 2024",
		"Wed Feb 29 23:59:59 2023",
		"Sun Nov  6 24:49:37 1994",

		// RFC 850 (all weekdays + 1969/2000 pivot)
		"Sunday, 06-Nov-94 08:49:37 GMT",
		"Monday, 01-Jan-00 00:00:00 GMT",
		"Tuesday, 31-Dec-68 23:59:59 GMT",
		"Wednesday, 01-Jan-69 00:00:00 GMT",
		"Thursday, 31-Dec-99 23:59:59 GMT",
		"Friday, 29-Feb-24 12:00:00 GMT",
		"Saturday, 29-Feb-23 12:00:00 GMT",
		"Funday, 06-Nov-94 08:49:37 GMT",

		// Malformed lengths & punctuation
		"",
		"invalid",
		"Sun, 06-Nov-1994 08:49:37 GMT", // hyphen format is not in HTTPTimeFormat/RFC850
	}

	for _, in := range inputs {
		gotTime, gotOK := ParseHTTPDate(in)
		wantTime, wantOK := parseWithTime(in)
		if gotOK != wantOK {
			t.Errorf("ParseHTTPDate(%q) ok = %v, want %v (time.Parse=%v)", in, gotOK, wantOK, wantTime)
			continue
		}
		if gotOK && !gotTime.Equal(wantTime) {
			t.Errorf("ParseHTTPDate(%q) = %v, want %v", in, gotTime, wantTime)
		}
	}
}

func TestParseCookieDateDifferential(t *testing.T) {
	layouts := []string{"Mon, 02 Jan 2006 15:04:05 MST", "Mon, 02-Jan-2006 15:04:05 MST"}
	parseWithTime := func(s string) (time.Time, bool) {
		for _, layout := range layouts {
			if tm, err := time.Parse(layout, s); err == nil {
				return tm.UTC(), true
			}
		}
		return time.Time{}, false
	}

	inputs := []string{
		"Tue, 10 Nov 2009 23:00:00 GMT",
		"Tue, 10-Nov-2009 23:00:00 GMT",
		"Tue, 10 Nov 2009 23:00:00 UTC",
		"Tue, 10-Nov-2009 23:00:00 UTC",
		"Thu, 29-Feb-2024 00:00:00 GMT",
		"Wed, 29-Feb-2023 00:00:00 GMT",
		"Tue, 10-Nov 2009 23:00:00 GMT", // mismatched separator
		"Tue, 10 Nov-2009 23:00:00 GMT", // mismatched separator
		"Tue, 10-Nov-2009 24:00:00 GMT", // invalid hour
		"Foo, 10-Nov-2009 23:00:00 GMT", // invalid weekday
		"Tue, 10-Foo-2009 23:00:00 GMT", // invalid month
		"short",
	}

	for _, in := range inputs {
		gotTime, gotOK := ParseCookieDate(in)
		wantTime, wantOK := parseWithTime(in)
		if gotOK != wantOK {
			t.Errorf("ParseCookieDate(%q) ok = %v, want %v", in, gotOK, wantOK)
			continue
		}
		if gotOK && !gotTime.Equal(wantTime) {
			t.Errorf("ParseCookieDate(%q) = %v, want %v", in, gotTime, wantTime)
		}
	}
}

func TestDateAllocs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	var buf [64]byte
	// Warm the atomic date cache before measuring allocations.
	_ = AppendHTTPDateNow(buf[:0])
	_ = HTTPDateNowString()

	if n := testing.AllocsPerRun(100, func() {
		_ = AppendHTTPDateNow(buf[:0])
	}); n > 0 {
		t.Errorf("AppendHTTPDateNow allocs = %v, want 0", n)
	}
	if n := testing.AllocsPerRun(100, func() {
		_ = HTTPDateNowString()
	}); n > 0 {
		t.Errorf("HTTPDateNowString allocs = %v, want 0", n)
	}
	if n := testing.AllocsPerRun(100, func() {
		_, _ = ParseHTTPDate("Sun, 06 Nov 1994 08:49:37 GMT")
	}); n > 0 {
		t.Errorf("ParseHTTPDate allocs = %v, want 0", n)
	}
	if n := testing.AllocsPerRun(100, func() {
		_, _ = ParseCookieDate("Tue, 10-Nov-2009 23:00:00 GMT")
	}); n > 0 {
		t.Errorf("ParseCookieDate allocs = %v, want 0", n)
	}
}

func BenchmarkAppendHTTPDate(b *testing.B) {
	tm := time.Date(2026, time.November, 17, 12, 34, 56, 0, time.UTC)
	var buf [len(HTTPTimeFormat)]byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = AppendHTTPDate(buf[:0], tm)
	}
}

func BenchmarkAppendHTTPDateNow(b *testing.B) {
	var buf [len(HTTPTimeFormat)]byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = AppendHTTPDateNow(buf[:0])
	}
}

func BenchmarkHTTPDateNowString(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = HTTPDateNowString()
	}
}

func BenchmarkParseHTTPDate(b *testing.B) {
	for _, tc := range []struct {
		name string
		s    string
	}{
		{"TimeFormat", "Sun, 06 Nov 1994 08:49:37 GMT"},
		{"RFC850", "Sunday, 06-Nov-94 08:49:37 GMT"},
		{"ANSIC", "Sun Nov  6 08:49:37 1994"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, ok := ParseHTTPDate(tc.s); !ok {
					b.Fatal("unexpected parse failure")
				}
			}
		})
	}
}

func BenchmarkParseCookieDate(b *testing.B) {
	const s = "Tue, 10-Nov-2009 23:00:00 GMT"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := ParseCookieDate(s); !ok {
			b.Fatal("unexpected parse failure")
		}
	}
}
