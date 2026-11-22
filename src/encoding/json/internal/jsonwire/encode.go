// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.jsonv2

package jsonwire

import (
	"internal/byteorder"
	"math"
	"math/bits"
	"slices"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"encoding/json/internal/jsonflags"
)

const (
	swarLo8  = 0x0101010101010101
	swarHi8  = 0x8080808080808080
	swarSp8  = 0x2020202020202020
	swarQt8  = 0x2222222222222222 // '"'
	swarBs8  = 0x5c5c5c5c5c5c5c5c // '\\'
	swarLt8  = 0x3c3c3c3c3c3c3c3c // '<'
	swarGt8  = 0x3e3e3e3e3e3e3e3e // '>'
	swarAmp8 = 0x2626262626262626 // '&'
)

// escapeMaskNoHTML returns a bitmask with bit 7 set in each byte lane of w
// that is >= 0x80, < 0x20, '"', or '\\'.
func escapeMaskNoHTML(w uint64) uint64 {
	xq := w ^ swarQt8
	xb := w ^ swarBs8
	return (w & swarHi8) |
		((w - swarSp8) & swarHi8) |
		((xq - swarLo8) &^ xq & swarHi8) |
		((xb - swarLo8) &^ xb & swarHi8)
}

// escapeMaskHTML returns a bitmask with bit 7 set in each byte lane of w
// that is >= 0x80, < 0x20, '"', '\\', '<', '>', or '&'.
func escapeMaskHTML(w uint64) uint64 {
	xq := w ^ swarQt8
	xb := w ^ swarBs8
	xl := w ^ swarLt8
	xg := w ^ swarGt8
	xa := w ^ swarAmp8
	return (w & swarHi8) |
		((w - swarSp8) & swarHi8) |
		((xq - swarLo8) &^ xq & swarHi8) |
		((xb - swarLo8) &^ xb & swarHi8) |
		((xl - swarLo8) &^ xl & swarHi8) |
		((xg - swarLo8) &^ xg & swarHi8) |
		((xa - swarLo8) &^ xa & swarHi8)
}

// escapeASCII reports whether the ASCII character needs to be escaped.
// It conservatively assumes EscapeForHTML.
var escapeASCII = [...]uint8{
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, // escape control characters
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, // escape control characters
	0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, // escape '"' and '&'
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 1, 0, // escape '<' and '>'
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, // escape '\\'
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
}

// NeedEscape reports whether src needs escaping of any characters.
// It conservatively assumes EscapeForHTML and EscapeForJS.
// It reports true for inputs with invalid UTF-8.
func NeedEscape(src []byte) bool {
	var i int
	for len(src)-i >= 8 {
		if m := escapeMaskHTML(byteorder.LEUint64(src[i:])); m != 0 {
			i += bits.TrailingZeros64(m) >> 3
			break
		}
		i += 8
	}
	for uint(len(src)) > uint(i) {
		if c := src[i]; c < utf8.RuneSelf {
			if escapeASCII[c] > 0 {
				return true
			}
			i++
		} else {
			r, rn := utf8.DecodeRune(src[i:])
			if r == utf8.RuneError || r == '\u2028' || r == '\u2029' {
				return true
			}
			i += rn
		}
	}
	return false
}

// AppendQuote appends src to dst as a JSON string per RFC 7159, section 7.
//
// It takes in flags and respects the following:
//   - EscapeForHTML escapes '<', '>', and '&'.
//   - EscapeForJS escapes '\u2028' and '\u2029'.
//   - AllowInvalidUTF8 avoids reporting an error for invalid UTF-8.
//
// Regardless of whether AllowInvalidUTF8 is specified,
// invalid bytes are replaced with the Unicode replacement character ('\ufffd').
// If no escape flags are set, then the shortest representable form is used,
// which is also the canonical form for strings (RFC 8785, section 3.2.2.2).
func AppendQuote[Bytes ~[]byte | ~string](dst []byte, src Bytes, flags *jsonflags.Flags) ([]byte, error) {
	if s, ok := any(src).(string); ok {
		return appendQuoteBytes(dst, unsafe.Slice(unsafe.StringData(s), len(s)), flags)
	}
	return appendQuoteBytes(dst, []byte(src), flags)
}

// AppendQuoteString appends src to dst as a JSON string without converting src to []byte.
func AppendQuoteString(dst []byte, src string, flags *jsonflags.Flags) ([]byte, error) {
	return appendQuoteBytes(dst, unsafe.Slice(unsafe.StringData(src), len(src)), flags)
}

// AppendQuoteBytes appends src to dst as a JSON string without boxing or copying.
func AppendQuoteBytes(dst, src []byte, flags *jsonflags.Flags) ([]byte, error) {
	return appendQuoteBytes(dst, src, flags)
}

func appendQuoteBytes(dst, src []byte, flags *jsonflags.Flags) ([]byte, error) {
	need := len(src) + 2
	if cap(dst)-len(dst) < need {
		dst = slices.Grow(dst, need)
	}
	escapeHTML := flags.Get(jsonflags.EscapeForHTML)
	n := 0
	if !escapeHTML {
		for len(src)-n >= 8 {
			if m := escapeMaskNoHTML(byteorder.LEUint64(src[n:])); m != 0 {
				return appendQuoteBytesSlow(dst, src, n+(bits.TrailingZeros64(m)>>3), flags)
			}
			n += 8
		}
		if len(src)-n >= 4 {
			w := uint64(byteorder.LEUint32(src[n:])) | 0x20202020_00000000
			if m := escapeMaskNoHTML(w); m != 0 {
				return appendQuoteBytesSlow(dst, src, n+(bits.TrailingZeros64(m)>>3), flags)
			}
			n += 4
		}
		for n < len(src) {
			c := src[n]
			if c >= utf8.RuneSelf || c < ' ' || c == '"' || c == '\\' {
				return appendQuoteBytesSlow(dst, src, n, flags)
			}
			n++
		}
	} else {
		for len(src)-n >= 8 {
			if m := escapeMaskHTML(byteorder.LEUint64(src[n:])); m != 0 {
				return appendQuoteBytesSlow(dst, src, n+(bits.TrailingZeros64(m)>>3), flags)
			}
			n += 8
		}
		if len(src)-n >= 4 {
			w := uint64(byteorder.LEUint32(src[n:])) | 0x20202020_00000000
			if m := escapeMaskHTML(w); m != 0 {
				return appendQuoteBytesSlow(dst, src, n+(bits.TrailingZeros64(m)>>3), flags)
			}
			n += 4
		}
		for n < len(src) {
			c := src[n]
			if c >= utf8.RuneSelf || escapeASCII[c] > 0 {
				return appendQuoteBytesSlow(dst, src, n, flags)
			}
			n++
		}
	}
	off := len(dst)
	dst = dst[:off+need]
	dst[off] = '"'
	copy(dst[off+1:], src)
	dst[off+1+len(src)] = '"'
	return dst, nil
}

func appendQuoteBytesSlow(dst, src []byte, n int, flags *jsonflags.Flags) ([]byte, error) {
	var i int
	var hasInvalidUTF8 bool
	escapeHTML := flags.Get(jsonflags.EscapeForHTML)
	dst = append(dst, '"')
	first := true
	for uint(len(src)) > uint(n) {
		if !first {
			if len(src)-n >= 8 {
				peelEnd := n + 4
				if !escapeHTML {
					for n < peelEnd {
						c := src[n]
						if c >= utf8.RuneSelf || c < ' ' || c == '"' || c == '\\' {
							goto found
						}
						n++
					}
					for len(src)-n >= 8 {
						if m := escapeMaskNoHTML(byteorder.LEUint64(src[n:])); m != 0 {
							n += bits.TrailingZeros64(m) >> 3
							break
						}
						n += 8
					}
				} else {
					for n < peelEnd {
						c := src[n]
						if c >= utf8.RuneSelf || escapeASCII[c] > 0 {
							goto found
						}
						n++
					}
					for len(src)-n >= 8 {
						if m := escapeMaskHTML(byteorder.LEUint64(src[n:])); m != 0 {
							n += bits.TrailingZeros64(m) >> 3
							break
						}
						n += 8
					}
				}
			}
		found:
			if uint(len(src)) <= uint(n) {
				break
			}
		}
		first = false
		c := src[n]
		if c < utf8.RuneSelf {
			// Handle single-byte ASCII.
			n++
			if escapeASCII[c] == 0 {
				continue // no escaping possibly needed
			}
			// Handle escaping of single-byte ASCII.
			if !(c == '<' || c == '>' || c == '&') || escapeHTML {
				dst = append(dst, src[i:n-1]...)
				dst = appendEscapedASCII(dst, c)
				i = n
			}
		} else {
			// Fast-skip runs of valid 2-byte (0xC2..0xDF) and safe 3-byte (0xE1, 0xE3..0xEC, 0xEE..0xEF) UTF-8.
			for uint(len(src)) > uint(n) {
				b0 := src[n]
				if b0 < utf8.RuneSelf {
					break
				}
				if b0 >= 0xE3 && b0 <= 0xEC || b0 == 0xE1 || b0 == 0xEE || b0 == 0xEF {
					if n+2 < len(src) && src[n+1]&0xC0 == 0x80 && src[n+2]&0xC0 == 0x80 {
						n += 3
						continue
					}
				} else if b0 >= 0xC2 && b0 <= 0xDF {
					if n+1 < len(src) && src[n+1]&0xC0 == 0x80 {
						n += 2
						continue
					}
				}
				// Handle remaining / boundary multi-byte Unicode via utf8.DecodeRune.
				r, rn := utf8.DecodeRune(src[n:])
				n += rn
				if r != utf8.RuneError && r != '\u2028' && r != '\u2029' {
					continue
				}
				switch {
				case isInvalidUTF8(r, rn):
					hasInvalidUTF8 = true
					dst = append(dst, src[i:n-rn]...)
					dst = append(dst, "\ufffd"...)
					i = n
				case (r == '\u2028' || r == '\u2029') && flags.Get(jsonflags.EscapeForJS):
					dst = append(dst, src[i:n-rn]...)
					dst = appendEscapedUnicode(dst, r)
					i = n
				}
			}
		}
	}
	dst = append(dst, src[i:n]...)
	dst = append(dst, '"')
	if hasInvalidUTF8 && !flags.Get(jsonflags.AllowInvalidUTF8) {
		return dst, ErrInvalidUTF8
	}
	return dst, nil
}

func appendEscapedASCII(dst []byte, c byte) []byte {
	switch c {
	case '"', '\\':
		dst = append(dst, '\\', c)
	case '\b':
		dst = append(dst, "\\b"...)
	case '\f':
		dst = append(dst, "\\f"...)
	case '\n':
		dst = append(dst, "\\n"...)
	case '\r':
		dst = append(dst, "\\r"...)
	case '\t':
		dst = append(dst, "\\t"...)
	default:
		dst = appendEscapedUTF16(dst, uint16(c))
	}
	return dst
}

func appendEscapedUnicode(dst []byte, r rune) []byte {
	if r1, r2 := utf16.EncodeRune(r); r1 != '\ufffd' && r2 != '\ufffd' {
		dst = appendEscapedUTF16(dst, uint16(r1))
		dst = appendEscapedUTF16(dst, uint16(r2))
	} else {
		dst = appendEscapedUTF16(dst, uint16(r))
	}
	return dst
}

func appendEscapedUTF16(dst []byte, x uint16) []byte {
	const hex = "0123456789abcdef"
	return append(dst, '\\', 'u', hex[(x>>12)&0xf], hex[(x>>8)&0xf], hex[(x>>4)&0xf], hex[(x>>0)&0xf])
}

// ReformatString consumes a JSON string from src and appends it to dst,
// reformatting it if necessary according to the specified flags.
// It returns the appended output and the number of consumed input bytes.
func ReformatString(dst, src []byte, flags *jsonflags.Flags) ([]byte, int, error) {
	// TODO: Should this update ValueFlags as input?
	var valFlags ValueFlags
	n, err := ConsumeString(&valFlags, src, !flags.Get(jsonflags.AllowInvalidUTF8))
	if err != nil {
		return dst, n, err
	}

	// If the output requires no special escapes, and the input
	// is already in canonical form or should be preserved verbatim,
	// then directly copy the input to the output.
	if !flags.Get(jsonflags.AnyEscape) &&
		(valFlags.IsCanonical() || flags.Get(jsonflags.PreserveRawStrings)) {
		dst = append(dst, src[:n]...) // copy the string verbatim
		return dst, n, nil
	}

	// Under [jsonflags.PreserveRawStrings], any pre-escaped sequences
	// remain escaped, however we still need to respect the
	// [jsonflags.EscapeForHTML] and [jsonflags.EscapeForJS] options.
	if flags.Get(jsonflags.PreserveRawStrings) {
		var i, lastAppendIndex int
		for i < n {
			if c := src[i]; c < utf8.RuneSelf {
				if (c == '<' || c == '>' || c == '&') && flags.Get(jsonflags.EscapeForHTML) {
					dst = append(dst, src[lastAppendIndex:i]...)
					dst = appendEscapedASCII(dst, c)
					lastAppendIndex = i + 1
				}
				i++
			} else {
				r, rn := utf8.DecodeRune(src[i:])
				if (r == '\u2028' || r == '\u2029') && flags.Get(jsonflags.EscapeForJS) {
					dst = append(dst, src[lastAppendIndex:i]...)
					dst = appendEscapedUnicode(dst, r)
					lastAppendIndex = i + rn
				}
				i += rn
			}
		}
		return append(dst, src[lastAppendIndex:n]...), n, nil
	}

	// The input contains characters that might need escaping,
	// unnecessary escape sequences, or invalid UTF-8.
	// Perform a round-trip unquote and quote to properly reformat
	// these sequences according to the current flags.
	b, _ := AppendUnquote(nil, src[:n])
	dst, _ = AppendQuote(dst, b, flags)
	return dst, n, nil
}

// AppendFloat appends src to dst as a JSON number per RFC 7159, section 6.
// It formats numbers similar to the ES6 number-to-string conversion.
// See https://go.dev/issue/14135.
//
// The output is identical to ECMA-262, 6th edition, section 7.1.12.1 and with
// RFC 8785, section 3.2.2.3 for 64-bit floating-point numbers except for -0,
// which is formatted as -0 instead of just 0.
//
// For 32-bit floating-point numbers,
// the output is a 32-bit equivalent of the algorithm.
// Note that ECMA-262 specifies no algorithm for 32-bit numbers.
func AppendFloat(dst []byte, src float64, bits int) []byte {
	if bits == 32 {
		src = float64(float32(src))
	}

	if src == 0 {
		if math.Signbit(src) {
			return append(dst, '-', '0')
		}
		return append(dst, '0')
	}

	abs := math.Abs(src)
	if bits == 64 {
		if abs < 1<<52 {
			if i := int64(src); float64(i) == src {
				return strconv.AppendInt(dst, i, 10)
			}
		}
	} else {
		if abs < 1<<23 {
			if i := int64(src); float64(i) == src {
				return strconv.AppendInt(dst, i, 10)
			}
		}
	}

	fmt := byte('f')
	if bits == 64 && (float64(abs) < 1e-6 || float64(abs) >= 1e21) ||
		bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21) {
		fmt = 'e'
	}
	dst = strconv.AppendFloat(dst, src, fmt, -1, bits)
	if fmt == 'e' {
		// Clean up e-09 to e-9.
		n := len(dst)
		if n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst
}

// ReformatNumber consumes a JSON number from src and appends it to dst,
// canonicalizing it if specified.
// It returns the appended output and the number of consumed input bytes.
func ReformatNumber(dst, src []byte, flags *jsonflags.Flags) ([]byte, int, error) {
	n, err := ConsumeNumber(src)
	if err != nil {
		return dst, n, err
	}
	if !flags.Get(jsonflags.CanonicalizeNumbers) {
		dst = append(dst, src[:n]...) // copy the number verbatim
		return dst, n, nil
	}

	// Identify the kind of number.
	var isFloat bool
	for _, c := range src[:n] {
		if c == '.' || c == 'e' || c == 'E' {
			isFloat = true // has fraction or exponent
			break
		}
	}

	// Check if need to canonicalize this kind of number.
	switch {
	case string(src[:n]) == "-0":
		break // canonicalize -0 as 0 regardless of kind
	case isFloat:
		if !flags.Get(jsonflags.CanonicalizeRawFloats) {
			dst = append(dst, src[:n]...) // copy the number verbatim
			return dst, n, nil
		}
	default:
		// As an optimization, we can copy integer numbers below 2⁵³ verbatim
		// since the canonical form is always identical.
		const maxExactIntegerDigits = 16 // len(strconv.AppendUint(nil, 1<<53, 10))
		if !flags.Get(jsonflags.CanonicalizeRawInts) || n < maxExactIntegerDigits {
			dst = append(dst, src[:n]...) // copy the number verbatim
			return dst, n, nil
		}
	}

	// Parse and reformat the number (which uses a canonical format).
	fv, _ := strconv.ParseFloat(string(src[:n]), 64)
	switch {
	case fv == 0:
		fv = 0 // normalize negative zero as just zero
	case math.IsInf(fv, +1):
		fv = +math.MaxFloat64
	case math.IsInf(fv, -1):
		fv = -math.MaxFloat64
	}
	return AppendFloat(dst, fv, 64), n, nil
}
