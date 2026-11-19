// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httpcommon

// The HTTP protocols are defined in terms of ASCII, not Unicode. This file
// contains helper functions which may use Unicode-aware functions which would
// otherwise be unsafe and could introduce vulnerabilities if used improperly.

// validWireHeaderNameTable marks RFC 9110 tchar bytes excluding ASCII uppercase
// letters 'A'..'Z', matching HTTP/2 (RFC 9113 §8.2.1) and HTTP/3 (RFC 9114 §4.2)
// wire header field name rules.
var validWireHeaderNameTable = [256]bool{
	'!': true, '#': true, '$': true, '%': true, '&': true, '\'': true,
	'*': true, '+': true, '-': true, '.': true,
	'0': true, '1': true, '2': true, '3': true, '4': true,
	'5': true, '6': true, '7': true, '8': true, '9': true,
	'^': true, '_': true, '`': true,
	'a': true, 'b': true, 'c': true, 'd': true, 'e': true, 'f': true,
	'g': true, 'h': true, 'i': true, 'j': true, 'k': true, 'l': true,
	'm': true, 'n': true, 'o': true, 'p': true, 'q': true, 'r': true,
	's': true, 't': true, 'u': true, 'v': true, 'w': true, 'x': true,
	'y': true, 'z': true,
	'|': true, '~': true,
}

// ValidWireHeaderFieldName reports whether v is a valid lowercase HTTP/2 or
// HTTP/3 wire header field name (non-empty RFC 9110 token without 'A'-'Z').
// This function does not validate whether a pseudo-header field name is valid.
func ValidWireHeaderFieldName(v string) bool {
	if len(v) == 0 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if !validWireHeaderNameTable[v[i]] {
			return false
		}
	}
	return true
}

// asciiEqualFold is strings.EqualFold, ASCII only. It reports whether s and t
// are equal, ASCII-case-insensitively.
func asciiEqualFold(s, t string) bool {
	if len(s) != len(t) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if lower(s[i]) != lower(t[i]) {
			return false
		}
	}
	return true
}

// lower returns the ASCII lowercase version of b.
func lower(b byte) byte {
	if 'A' <= b && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// isASCIIPrint returns whether s is ASCII and printable according to
// https://tools.ietf.org/html/rfc20#section-4.2.
func isASCIIPrint(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// asciiToLower returns the lowercase version of s if s is ASCII and printable,
// and whether or not it was.
func asciiToLower(s string) (lower string, ok bool) {
	firstUpper := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < ' ' || c > '~' {
			return "", false
		}
		if 'A' <= c && c <= 'Z' && firstUpper < 0 {
			firstUpper = i
		}
	}
	if firstUpper < 0 {
		return s, true
	}
	var stack [64]byte
	var buf []byte
	if len(s) <= len(stack) {
		buf = stack[:len(s)]
	} else {
		buf = make([]byte, len(s))
	}
	copy(buf, s[:firstUpper])
	for i := firstUpper; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		buf[i] = c
	}
	return string(buf), true
}
