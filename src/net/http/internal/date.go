// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package internal

import (
	"internal/synctest"
	"sync/atomic"
	"time"
)

// HTTPTimeFormat is http.TimeFormat ("Mon, 02 Jan 2006 15:04:05 GMT").
const HTTPTimeFormat = "Mon, 02 Jan 2006 15:04:05 GMT"

var (
	weekdayPrefixes = [7]string{"Sun, ", "Mon, ", "Tue, ", "Wed, ", "Thu, ", "Fri, ", "Sat, "}
	monthPrefixes   = [13]string{
		1:  "Jan ",
		2:  "Feb ",
		3:  "Mar ",
		4:  "Apr ",
		5:  "May ",
		6:  "Jun ",
		7:  "Jul ",
		8:  "Aug ",
		9:  "Sep ",
		10: "Oct ",
		11: "Nov ",
		12: "Dec ",
	}
)

type httpDateCache struct {
	unixSec   int64
	formatted [len(HTTPTimeFormat)]byte
	str       string
}

var cachedHTTPDate atomic.Pointer[httpDateCache]

// AppendHTTPDate appends t formatted in HTTPTimeFormat ("Mon, 02 Jan 2006 15:04:05 GMT")
// in UTC to dst and returns the extended buffer.
func AppendHTTPDate(dst []byte, t time.Time) []byte {
	t = t.UTC()
	year, month, day := t.Date()
	if year < 0 || year > 9999 {
		return t.AppendFormat(dst, HTTPTimeFormat)
	}
	hour, min, sec := t.Clock()
	wd := t.Weekday()
	y0 := year / 100
	y1 := year % 100
	dst = append(dst, weekdayPrefixes[wd]...)
	dst = append(dst, byte('0'+day/10), byte('0'+day%10), ' ')
	dst = append(dst, monthPrefixes[month]...)
	dst = append(dst,
		byte('0'+y0/10), byte('0'+y0%10),
		byte('0'+y1/10), byte('0'+y1%10), ' ',
		byte('0'+hour/10), byte('0'+hour%10), ':',
		byte('0'+min/10), byte('0'+min%10), ':',
		byte('0'+sec/10), byte('0'+sec%10),
		' ', 'G', 'M', 'T',
	)
	return dst
}

// AppendHTTPDateNow appends the current UTC time formatted in HTTPTimeFormat to dst.
func AppendHTTPDateNow(dst []byte) []byte {
	now := time.Now()
	if synctest.IsInBubble() {
		return AppendHTTPDate(dst, now)
	}
	sec := now.Unix()
	if c := cachedHTTPDate.Load(); c != nil && c.unixSec == sec {
		return append(dst, c.formatted[:]...)
	}
	if sec < 0 || now.UTC().Year() > 9999 {
		return AppendHTTPDate(dst, now)
	}
	c := &httpDateCache{unixSec: sec}
	AppendHTTPDate(c.formatted[:0], now)
	c.str = string(c.formatted[:])
	cachedHTTPDate.Store(c)
	return append(dst, c.formatted[:]...)
}

// HTTPDateNowString returns the current UTC time formatted in HTTPTimeFormat.
func HTTPDateNowString() string {
	now := time.Now()
	if synctest.IsInBubble() {
		var buf [len(HTTPTimeFormat)]byte
		return string(AppendHTTPDate(buf[:0], now))
	}
	sec := now.Unix()
	if c := cachedHTTPDate.Load(); c != nil && c.unixSec == sec {
		return c.str
	}
	if sec < 0 || now.UTC().Year() > 9999 {
		var buf [len(HTTPTimeFormat)]byte
		return string(AppendHTTPDate(buf[:0], now))
	}
	c := &httpDateCache{unixSec: sec}
	AppendHTTPDate(c.formatted[:0], now)
	c.str = string(c.formatted[:])
	cachedHTTPDate.Store(c)
	return c.str
}

// ParseHTTPDate parses HTTP/1.1 time headers in TimeFormat, time.RFC850, or time.ANSIC.
// If it returns false, callers should fall back to time.Parse.
func ParseHTTPDate(text string) (time.Time, bool) {
	n := len(text)
	// Fast path 1: TimeFormat "Mon, 02 Jan 2006 15:04:05 GMT" (29 bytes)
	if n == 29 && text[3] == ',' && text[4] == ' ' && text[7] == ' ' &&
		text[11] == ' ' && text[16] == ' ' && text[19] == ':' && text[22] == ':' &&
		text[25] == ' ' && text[26] == 'G' && text[27] == 'M' && text[28] == 'T' {
		if _, ok := parseShortWeekday(text[0], text[1], text[2]); !ok {
			return time.Time{}, false
		}
		day, ok1 := parse2Digits(text[5], text[6])
		month, ok2 := parseMonth(text[8], text[9], text[10])
		year, ok3 := parse4Digits(text[12], text[13], text[14], text[15])
		hour, ok4 := parse2Digits(text[17], text[18])
		min, ok5 := parse2Digits(text[20], text[21])
		sec, ok6 := parse2Digits(text[23], text[24])
		if ok1 && ok2 && ok3 && ok4 && ok5 && ok6 {
			return makeValidUTCDate(year, month, day, hour, min, sec)
		}
		return time.Time{}, false
	}

	// Fast path 2: ANSIC "Mon Jan _2 15:04:05 2006" (24 bytes)
	if n == 24 && text[3] == ' ' && text[7] == ' ' && text[10] == ' ' &&
		text[13] == ':' && text[16] == ':' && text[19] == ' ' {
		if _, ok := parseShortWeekday(text[0], text[1], text[2]); !ok {
			return time.Time{}, false
		}
		month, ok1 := parseMonth(text[4], text[5], text[6])
		day, ok2 := parse2DigitsOrSpace(text[8], text[9])
		hour, ok3 := parse2Digits(text[11], text[12])
		min, ok4 := parse2Digits(text[14], text[15])
		sec, ok5 := parse2Digits(text[17], text[18])
		year, ok6 := parse4Digits(text[20], text[21], text[22], text[23])
		if ok1 && ok2 && ok3 && ok4 && ok5 && ok6 {
			return makeValidUTCDate(year, month, day, hour, min, sec)
		}
		return time.Time{}, false
	}

	// Fast path 3: RFC850 "Monday, 02-Jan-06 15:04:05 GMT"
	if n >= 30 && n <= 33 && text[n-4] == ' ' && text[n-3] == 'G' && text[n-2] == 'M' && text[n-1] == 'T' {
		comma := n - 24
		if text[comma] == ',' && text[comma+1] == ' ' &&
			text[comma+4] == '-' && text[comma+8] == '-' && text[comma+11] == ' ' &&
			text[comma+14] == ':' && text[comma+17] == ':' {
			if validLongWeekday(text[:comma]) {
				day, ok1 := parse2Digits(text[comma+2], text[comma+3])
				month, ok2 := parseMonth(text[comma+5], text[comma+6], text[comma+7])
				yy, ok3 := parse2Digits(text[comma+9], text[comma+10])
				hour, ok4 := parse2Digits(text[comma+12], text[comma+13])
				min, ok5 := parse2Digits(text[comma+15], text[comma+16])
				sec, ok6 := parse2Digits(text[comma+18], text[comma+19])
				if ok1 && ok2 && ok3 && ok4 && ok5 && ok6 {
					year := 2000 + yy
					if yy >= 69 {
						year = 1900 + yy
					}
					return makeValidUTCDate(year, month, day, hour, min, sec)
				}
			}
		}
	}

	return time.Time{}, false
}

// ParseCookieDate parses Set-Cookie Expires timestamps in either
// "Mon, 02 Jan 2006 15:04:05 GMT|UTC" or "Mon, 02-Jan-2006 15:04:05 GMT|UTC".
// If it returns false, callers should fall back to time.Parse.
func ParseCookieDate(text string) (time.Time, bool) {
	if len(text) != 29 {
		return time.Time{}, false
	}
	sep := text[7]
	if (sep != ' ' && sep != '-') || text[11] != sep ||
		text[3] != ',' || text[4] != ' ' || text[16] != ' ' ||
		text[19] != ':' || text[22] != ':' || text[25] != ' ' {
		return time.Time{}, false
	}
	z0, z1, z2 := text[26], text[27], text[28]
	if !((z0 == 'G' && z1 == 'M' && z2 == 'T') || (z0 == 'U' && z1 == 'T' && z2 == 'C')) {
		return time.Time{}, false
	}
	if _, ok := parseShortWeekday(text[0], text[1], text[2]); !ok {
		return time.Time{}, false
	}
	day, ok1 := parse2Digits(text[5], text[6])
	month, ok2 := parseMonth(text[8], text[9], text[10])
	year, ok3 := parse4Digits(text[12], text[13], text[14], text[15])
	hour, ok4 := parse2Digits(text[17], text[18])
	min, ok5 := parse2Digits(text[20], text[21])
	sec, ok6 := parse2Digits(text[23], text[24])
	if ok1 && ok2 && ok3 && ok4 && ok5 && ok6 {
		return makeValidUTCDate(year, month, day, hour, min, sec)
	}
	return time.Time{}, false
}

func parse2Digits(a, b byte) (int, bool) {
	d0 := a - '0'
	d1 := b - '0'
	if d0 > 9 || d1 > 9 {
		return 0, false
	}
	return int(d0)*10 + int(d1), true
}

func parse2DigitsOrSpace(a, b byte) (int, bool) {
	d1 := b - '0'
	if d1 > 9 {
		return 0, false
	}
	if a == ' ' {
		return int(d1), true
	}
	d0 := a - '0'
	if d0 > 9 {
		return 0, false
	}
	return int(d0)*10 + int(d1), true
}

func parse4Digits(a, b, c, d byte) (int, bool) {
	d0 := a - '0'
	d1 := b - '0'
	d2 := c - '0'
	d3 := d - '0'
	if d0 > 9 || d1 > 9 || d2 > 9 || d3 > 9 {
		return 0, false
	}
	return int(d0)*1000 + int(d1)*100 + int(d2)*10 + int(d3), true
}

func parseShortWeekday(a, b, c byte) (time.Weekday, bool) {
	switch uint32(a)<<16 | uint32(b)<<8 | uint32(c) {
	case 'S'<<16 | 'u'<<8 | 'n':
		return time.Sunday, true
	case 'M'<<16 | 'o'<<8 | 'n':
		return time.Monday, true
	case 'T'<<16 | 'u'<<8 | 'e':
		return time.Tuesday, true
	case 'W'<<16 | 'e'<<8 | 'd':
		return time.Wednesday, true
	case 'T'<<16 | 'h'<<8 | 'u':
		return time.Thursday, true
	case 'F'<<16 | 'r'<<8 | 'i':
		return time.Friday, true
	case 'S'<<16 | 'a'<<8 | 't':
		return time.Saturday, true
	}
	return 0, false
}

func validLongWeekday(s string) bool {
	switch s {
	case "Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday":
		return true
	}
	return false
}

func parseMonth(a, b, c byte) (time.Month, bool) {
	switch uint32(a)<<16 | uint32(b)<<8 | uint32(c) {
	case 'J'<<16 | 'a'<<8 | 'n':
		return time.January, true
	case 'F'<<16 | 'e'<<8 | 'b':
		return time.February, true
	case 'M'<<16 | 'a'<<8 | 'r':
		return time.March, true
	case 'A'<<16 | 'p'<<8 | 'r':
		return time.April, true
	case 'M'<<16 | 'a'<<8 | 'y':
		return time.May, true
	case 'J'<<16 | 'u'<<8 | 'n':
		return time.June, true
	case 'J'<<16 | 'u'<<8 | 'l':
		return time.July, true
	case 'A'<<16 | 'u'<<8 | 'g':
		return time.August, true
	case 'S'<<16 | 'e'<<8 | 'p':
		return time.September, true
	case 'O'<<16 | 'c'<<8 | 't':
		return time.October, true
	case 'N'<<16 | 'o'<<8 | 'v':
		return time.November, true
	case 'D'<<16 | 'e'<<8 | 'c':
		return time.December, true
	}
	return 0, false
}

func makeValidUTCDate(year int, month time.Month, day, hour, min, sec int) (time.Time, bool) {
	if hour > 23 || min > 59 || sec > 59 || day < 1 {
		return time.Time{}, false
	}
	var maxDay int
	switch month {
	case time.February:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			maxDay = 29
		} else {
			maxDay = 28
		}
	case time.April, time.June, time.September, time.November:
		maxDay = 30
	default:
		maxDay = 31
	}
	if day > maxDay {
		return time.Time{}, false
	}
	return time.Date(year, month, day, hour, min, sec, 0, time.UTC), true
}
