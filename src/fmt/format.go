// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fmt

import (
	"internal/byteorder"
	"internal/strconv"
	stdstrconv "strconv"
	"unicode/utf8"
	"unsafe"
)

const (
	ldigits = "0123456789abcdefx"
	udigits = "0123456789ABCDEFX"
)

const (
	signed   = true
	unsigned = false
)

const (
	spaces64 = "                                                                "
	zeros64  = "0000000000000000000000000000000000000000000000000000000000000000"
	smalls   = "00010203040506070809101112131415161718192021222324252627282930313233343536373839404142434445464748495051525354555657585960616263646566676869707172737475767778798081828384858687888990919293949596979899"
)

var (
	hexLowerLUT16 = func() (lut [256]uint16) {
		for i := range 256 {
			lut[i] = uint16(ldigits[i>>4]) | uint16(ldigits[i&0xf])<<8
		}
		return lut
	}()

	hexUpperLUT16 = func() (lut [256]uint16) {
		for i := range 256 {
			lut[i] = uint16(udigits[i>>4]) | uint16(udigits[i&0xf])<<8
		}
		return lut
	}()
)

// fmtFlags stores formatting flags as a compact bitmask.
type fmtFlags uint16

const (
	flagWidPresent fmtFlags = 1 << iota
	flagPrecPresent
	flagMinus
	flagPlus
	flagSharp
	flagSpace
	flagZero

	// For the formats %+v %#v, we set the plusV/sharpV flags
	// and clear the plus/sharp flags since %+v and %#v are in effect
	// different, flagless formats set at the top level.
	flagPlusV
	flagSharpV
)

// A fmt is the raw formatter used by Printf etc.
// It prints into a buffer that must be set up separately.
type fmt struct {
	buf *buffer

	flags fmtFlags

	wid  int // width
	prec int // precision

	// intbuf is large enough to store %b of an int64 with a sign and
	// avoids padding at the end of the struct on 32 bit architectures.
	intbuf [68]byte
}

func (f *fmt) clearflags() {
	f.flags = 0
	f.wid = 0
	f.prec = 0
}

func (f *fmt) init(buf *buffer) {
	f.buf = buf
	f.clearflags()
}

func appendPadding(dst []byte, n int, padByte byte) []byte {
	if n <= 0 {
		return dst
	}
	oldLen := len(dst)
	if n > cap(dst)-oldLen {
		newBuf := make([]byte, oldLen, cap(dst)*2+n)
		copy(newBuf, dst)
		dst = newBuf
	}
	newLen := oldLen + n
	dst = dst[:newLen]
	padding := dst[oldLen:newLen]
	if n <= 64 {
		if padByte == '0' {
			copy(padding, zeros64[:n])
		} else {
			copy(padding, spaces64[:n])
		}
		return dst
	}
	fill := spaces64
	if padByte == '0' {
		fill = zeros64
	}
	copy(padding, fill)
	for bp := 64; bp < n; bp *= 2 {
		copy(padding[bp:], padding[:bp])
	}
	return dst
}

// writePadding generates n bytes of padding.
func (f *fmt) writePadding(n int) {
	if n <= 0 { // No padding bytes needed.
		return
	}
	padByte := byte(' ')
	// Zero padding is allowed only to the left.
	if f.flags&(flagZero|flagMinus) == flagZero {
		padByte = '0'
	}
	*f.buf = appendPadding(*f.buf, n, padByte)
}

func runeCountStringFast(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return i + utf8.RuneCountInString(s[i:])
		}
	}
	return len(s)
}

func appendPad(dst, b []byte, flags fmtFlags, wid int) []byte {
	if flags&flagWidPresent == 0 || wid == 0 {
		return append(dst, b...)
	}
	var width int
	if len(b) >= 4*wid {
		width = 0
	} else {
		width = wid - utf8.RuneCount(b)
	}
	if width <= 0 {
		return append(dst, b...)
	}
	padByte := byte(' ')
	if flags&(flagZero|flagMinus) == flagZero {
		padByte = '0'
	}
	if flags&flagMinus == 0 {
		dst = appendPadding(dst, width, padByte)
		return append(dst, b...)
	}
	dst = append(dst, b...)
	return appendPadding(dst, width, padByte)
}

func appendPadString(dst []byte, s string, flags fmtFlags, wid int) []byte {
	if flags&flagWidPresent == 0 || wid == 0 {
		return append(dst, s...)
	}
	var width int
	if len(s) >= 4*wid {
		width = 0
	} else {
		width = wid - runeCountStringFast(s)
	}
	if width <= 0 {
		return append(dst, s...)
	}
	padByte := byte(' ')
	if flags&(flagZero|flagMinus) == flagZero {
		padByte = '0'
	}
	if flags&flagMinus == 0 {
		dst = appendPadding(dst, width, padByte)
		return append(dst, s...)
	}
	dst = append(dst, s...)
	return appendPadding(dst, width, padByte)
}

func appendPadASCII(dst, b []byte, flags fmtFlags, wid int) []byte {
	if flags&flagWidPresent == 0 || wid <= len(b) {
		return append(dst, b...)
	}
	width := wid - len(b)
	padByte := byte(' ')
	if flags&(flagZero|flagMinus) == flagZero {
		padByte = '0'
	}
	if flags&flagMinus == 0 {
		dst = appendPadding(dst, width, padByte)
		return append(dst, b...)
	}
	dst = append(dst, b...)
	return appendPadding(dst, width, padByte)
}

func appendPadASCIIString(dst []byte, s string, flags fmtFlags, wid int) []byte {
	if flags&flagWidPresent == 0 || wid <= len(s) {
		return append(dst, s...)
	}
	width := wid - len(s)
	padByte := byte(' ')
	if flags&(flagZero|flagMinus) == flagZero {
		padByte = '0'
	}
	if flags&flagMinus == 0 {
		dst = appendPadding(dst, width, padByte)
		return append(dst, s...)
	}
	dst = append(dst, s...)
	return appendPadding(dst, width, padByte)
}

// pad appends b to f.buf, padded on left (!minus) or right (minus).
func (f *fmt) pad(b []byte) {
	*f.buf = appendPad(*f.buf, b, f.flags, f.wid)
}

// padString appends s to f.buf, padded on left (!minus) or right (minus).
func (f *fmt) padString(s string) {
	*f.buf = appendPadString(*f.buf, s, f.flags, f.wid)
}

func (f *fmt) padASCII(b []byte) {
	*f.buf = appendPadASCII(*f.buf, b, f.flags, f.wid)
}

func (f *fmt) padASCIIString(s string) {
	*f.buf = appendPadASCIIString(*f.buf, s, f.flags, f.wid)
}

func appendBoolean(dst []byte, v bool, flags fmtFlags, wid int) []byte {
	if v {
		return appendPadASCIIString(dst, "true", flags, wid)
	}
	return appendPadASCIIString(dst, "false", flags, wid)
}

// fmtBoolean formats a boolean.
func (f *fmt) fmtBoolean(v bool) {
	*f.buf = appendBoolean(*f.buf, v, f.flags, f.wid)
}

// fmtUnicode formats a uint64 as "U+0078" or with sharp set as "U+0078 'x'".
func (f *fmt) fmtUnicode(u uint64) {
	buf := f.intbuf[0:]

	// With default precision set the maximum needed buf length is 18
	// for formatting -1 with %#U ("U+FFFFFFFFFFFFFFFF") which fits
	// into the already allocated intbuf with a capacity of 68 bytes.
	prec := 4
	if f.flags&flagPrecPresent != 0 && f.prec > 4 {
		prec = f.prec
		// Compute space needed for "U+" , number, " '", character, "'".
		width := 2 + prec + 2 + utf8.UTFMax + 1
		if width > len(buf) {
			buf = make([]byte, width)
		}
	}

	// Format into buf, ending at buf[i]. Formatting numbers is easier right-to-left.
	i := len(buf)

	// For %#U we want to add a space and a quoted character at the end of the buffer.
	nonASCII := false
	if f.flags&flagSharp != 0 && u <= utf8.MaxRune && stdstrconv.IsPrint(rune(u)) {
		nonASCII = u >= utf8.RuneSelf
		i--
		buf[i] = '\''
		i -= utf8.RuneLen(rune(u))
		utf8.EncodeRune(buf[i:], rune(u))
		i--
		buf[i] = '\''
		i--
		buf[i] = ' '
	}
	// Format the Unicode code point u as a hexadecimal number.
	for u >= 16 {
		i--
		buf[i] = udigits[u&0xF]
		prec--
		u >>= 4
	}
	i--
	buf[i] = udigits[u]
	prec--
	// Add zeros in front of the number until requested precision is reached.
	for prec > 0 {
		i--
		buf[i] = '0'
		prec--
	}
	// Add a leading "U+".
	i--
	buf[i] = '+'
	i--
	buf[i] = 'U'

	flagsNoZero := f.flags &^ flagZero
	if nonASCII {
		*f.buf = appendPad(*f.buf, buf[i:], flagsNoZero, f.wid)
	} else {
		*f.buf = appendPadASCII(*f.buf, buf[i:], flagsNoZero, f.wid)
	}
}

func appendInteger(dst []byte, intbuf *[68]byte, u uint64, base int, isSigned bool, verb rune, digits string, flags fmtFlags, wid, prec int) []byte {
	negative := isSigned && int64(u) < 0
	if negative {
		u = -u
	}

	if flags == 0 {
		switch base {
		case 10:
			if !negative {
				if u < 10 {
					return append(dst, byte('0'+u))
				}
				if u < 100 {
					r := u * 2
					return append(dst, smalls[r], smalls[r+1])
				}
				return strconv.AppendUint10(dst, u)
			}
			dst = append(dst, '-')
			return strconv.AppendUint10(dst, u)
		case 16:
			if u < 16 {
				return append(dst, digits[u])
			}
		}
	}

	return appendIntegerSlow(dst, intbuf, u, negative, base, verb, digits, flags, wid, prec)
}

func appendIntegerSlow(dst []byte, intbuf *[68]byte, u uint64, negative bool, base int, verb rune, digits string, flags fmtFlags, wid, prec int) []byte {
	// Two ways to ask for extra leading zero digits: %.3d or %03d.
	// If both are specified the zero flag is ignored and
	// padding with spaces is used instead.
	p := 0
	if flags&flagPrecPresent != 0 {
		p = prec
		// Precision of 0 and value of 0 means "print nothing" but padding.
		if p == 0 && u == 0 {
			if flags&flagWidPresent != 0 && wid > 0 {
				return appendPadding(dst, wid, ' ')
			}
			return dst
		}
	} else if flags&(flagZero|flagMinus|flagWidPresent) == (flagZero | flagWidPresent) {
		p = wid
		if negative || flags&(flagPlus|flagSpace) != 0 {
			p-- // leave room for sign
		}
	}

	// Format digits right-to-left into the 68-byte buffer (sufficient for
	// 64 binary digits + 2-byte prefix + 1-byte sign).
	buf := intbuf[:]
	i := len(buf)
	switch base {
	case 10:
		for u >= 100 {
			q := u / 100
			r := (u - q*100) * 2
			i -= 2
			buf[i+1] = smalls[r+1]
			buf[i] = smalls[r]
			u = q
		}
		if u >= 10 {
			r := u * 2
			i -= 2
			buf[i+1] = smalls[r+1]
			buf[i] = smalls[r]
		} else {
			i--
			buf[i] = byte('0' + u)
		}
	case 16:
		for u >= 16 {
			i--
			buf[i] = digits[u&0xF]
			u >>= 4
		}
		i--
		buf[i] = digits[u]
	case 8:
		for u >= 8 {
			i--
			buf[i] = byte('0' + u&7)
			u >>= 3
		}
		i--
		buf[i] = byte('0' + u)
	case 2:
		for u >= 2 {
			i--
			buf[i] = byte('0' + u&1)
			u >>= 1
		}
		i--
		buf[i] = byte('0' + u)
	default:
		panic("fmt: unknown base; can't happen")
	}

	ndigits := len(buf) - i
	extraZeros := p - ndigits
	if extraZeros <= 0 {
		extraZeros = 0
	}

	// Fast path: fits inside the remaining 68-byte stack buffer (always true for extraZeros <= i-3).
	if extraZeros <= i-3 {
		for extraZeros > 0 {
			i--
			buf[i] = '0'
			extraZeros--
		}

		// Various prefixes: 0x, -, etc.
		if flags&flagSharp != 0 {
			switch base {
			case 2:
				i -= 2
				buf[i] = '0'
				buf[i+1] = 'b'
			case 8:
				if buf[i] != '0' {
					i--
					buf[i] = '0'
				}
			case 16:
				i -= 2
				buf[i] = '0'
				buf[i+1] = digits[16]
			}
		}
		if verb == 'O' {
			i -= 2
			buf[i] = '0'
			buf[i+1] = 'o'
		}

		if negative {
			i--
			buf[i] = '-'
		} else if flags&flagPlus != 0 {
			i--
			buf[i] = '+'
		} else if flags&flagSpace != 0 {
			i--
			buf[i] = ' '
		}

		return appendPadASCII(dst, buf[i:], flags&^flagZero, wid)
	}

	// Large precision path: write sign, prefix, zero padding, and digits
	// directly to dst without allocating a temporary buffer.
	var signByte byte
	signLen := 0
	if negative {
		signByte = '-'
		signLen = 1
	} else if flags&flagPlus != 0 {
		signByte = '+'
		signLen = 1
	} else if flags&flagSpace != 0 {
		signByte = ' '
		signLen = 1
	}

	prefixLen := 0
	var p0, p1 byte
	if flags&flagSharp != 0 {
		switch base {
		case 2:
			p0, p1, prefixLen = '0', 'b', 2
		case 8:
			// extraZeros > 0 here so the leading digit is already '0'.
		case 16:
			p0, p1, prefixLen = '0', digits[16], 2
		}
	}
	if verb == 'O' {
		p0, p1, prefixLen = '0', 'o', 2
	}

	totalLen := signLen + prefixLen + extraZeros + ndigits
	padLen := 0
	if flags&flagWidPresent != 0 && wid > totalLen {
		padLen = wid - totalLen
	}

	if padLen > 0 && flags&flagMinus == 0 {
		dst = appendPadding(dst, padLen, ' ')
	}
	if signLen > 0 {
		dst = append(dst, signByte)
	}
	switch prefixLen {
	case 1:
		dst = append(dst, p0)
	case 2:
		dst = append(dst, p0, p1)
	}
	dst = appendPadding(dst, extraZeros, '0')
	dst = append(dst, buf[i:]...)
	if padLen > 0 && flags&flagMinus != 0 {
		dst = appendPadding(dst, padLen, ' ')
	}
	return dst
}

// fmtInteger formats signed and unsigned integers.
func (f *fmt) fmtInteger(u uint64, base int, isSigned bool, verb rune, digits string) {
	*f.buf = appendInteger(*f.buf, &f.intbuf, u, base, isSigned, verb, digits, f.flags, f.wid, f.prec)
}

func truncateString(s string, flags fmtFlags, prec int) string {
	if flags&flagPrecPresent == 0 || len(s) <= prec {
		return s
	}
	if prec <= 0 {
		return ""
	}
	for i := 0; i < prec; i++ {
		if s[i] >= utf8.RuneSelf {
			n := prec - i
			for j := range s[i:] {
				n--
				if n < 0 {
					return s[:i+j]
				}
			}
			return s
		}
	}
	return s[:prec]
}

func truncateBytes(b []byte, flags fmtFlags, prec int) []byte {
	if flags&flagPrecPresent == 0 || len(b) <= prec {
		return b
	}
	if prec <= 0 {
		return b[:0]
	}
	for i := 0; i < prec; i++ {
		if b[i] >= utf8.RuneSelf {
			n := prec - i
			for j := i; j < len(b); {
				n--
				if n < 0 {
					return b[:j]
				}
				_, wid := utf8.DecodeRune(b[j:])
				j += wid
			}
			return b
		}
	}
	return b[:prec]
}

// truncateString truncates the string s to the specified precision, if present.
func (f *fmt) truncateString(s string) string {
	return truncateString(s, f.flags, f.prec)
}

// truncate truncates the byte slice b as a string of the specified precision, if present.
func (f *fmt) truncate(b []byte) []byte {
	return truncateBytes(b, f.flags, f.prec)
}

// fmtS formats a string.
func (f *fmt) fmtS(s string) {
	s = truncateString(s, f.flags, f.prec)
	*f.buf = appendPadString(*f.buf, s, f.flags, f.wid)
}

// fmtBs formats the byte slice b as if it was formatted as string with fmtS.
func (f *fmt) fmtBs(b []byte) {
	b = truncateBytes(b, f.flags, f.prec)
	*f.buf = appendPad(*f.buf, b, f.flags, f.wid)
}

func appendSbx(dst []byte, s string, b []byte, digits string, flags fmtFlags, wid, prec int) []byte {
	length := len(b)
	if b == nil {
		// No byte slice present. Assume string s should be encoded.
		length = len(s)
	} else {
		s = unsafe.String(unsafe.SliceData(b), len(b))
	}
	// Set length to not process more bytes than the precision demands.
	if flags&flagPrecPresent != 0 && prec < length {
		length = prec
	}
	sharp := flags&flagSharp != 0
	space := flags&flagSpace != 0
	// Compute width of the encoding taking into account the sharp and space flag.
	width := 2 * length
	if width > 0 {
		if space {
			// Each element encoded by two hexadecimals will get a leading 0x or 0X.
			if sharp {
				width *= 2
			}
			// Elements will be separated by a space.
			width += length - 1
		} else if sharp {
			// Only a leading 0x or 0X will be added for the whole string.
			width += 2
		}
	} else { // The byte slice or string that should be encoded is empty.
		if flags&flagWidPresent != 0 && wid > 0 {
			padByte := byte(' ')
			if flags&(flagZero|flagMinus) == flagZero {
				padByte = '0'
			}
			return appendPadding(dst, wid, padByte)
		}
		return dst
	}
	padByte := byte(' ')
	if flags&(flagZero|flagMinus) == flagZero {
		padByte = '0'
	}
	// Handle padding to the left.
	if flags&flagWidPresent != 0 && wid > width && flags&flagMinus == 0 {
		dst = appendPadding(dst, wid-width, padByte)
	}
	// Pre-grow dst once for the entire hex encoding.
	oldLen := len(dst)
	if width > cap(dst)-oldLen {
		newBuf := make([]byte, oldLen, cap(dst)*2+width)
		copy(newBuf, dst)
		dst = newBuf
	}
	dst = dst[:oldLen+width]
	out := dst[oldLen:]
	lut := &hexLowerLUT16
	if digits[10] == 'A' {
		lut = &hexUpperLUT16
	}
	pos := 0
	if !space {
		if sharp {
			out[0] = '0'
			out[1] = digits[16]
			pos = 2
		}
		s = s[:length]
		outHex := out[pos:]
		i := 0
		for i+4 <= len(s) {
			v := uint64(lut[s[i]]) |
				uint64(lut[s[i+1]])<<16 |
				uint64(lut[s[i+2]])<<32 |
				uint64(lut[s[i+3]])<<48
			byteorder.LEPutUint64(outHex, v)
			outHex = outHex[8:]
			i += 4
		}
		for i < len(s) {
			byteorder.LEPutUint16(outHex, lut[s[i]])
			outHex = outHex[2:]
			i++
		}
	} else {
		prefix := digits[16]
		s = s[:length]
		for i := 0; i < len(s); i++ {
			if i > 0 {
				out[pos] = ' '
				pos++
			}
			if sharp {
				out[pos] = '0'
				out[pos+1] = prefix
				pos += 2
			}
			byteorder.LEPutUint16(out[pos:pos+2], lut[s[i]])
			pos += 2
		}
	}
	// Handle padding to the right.
	if flags&flagWidPresent != 0 && wid > width && flags&flagMinus != 0 {
		dst = appendPadding(dst, wid-width, ' ')
	}
	return dst
}

// fmtSbx formats a string or byte slice as a hexadecimal encoding of its bytes.
func (f *fmt) fmtSbx(s string, b []byte, digits string) {
	*f.buf = appendSbx(*f.buf, s, b, digits, f.flags, f.wid, f.prec)
}

// fmtSx formats a string as a hexadecimal encoding of its bytes.
func (f *fmt) fmtSx(s, digits string) {
	f.fmtSbx(s, nil, digits)
}

// fmtBx formats a byte slice as a hexadecimal encoding of its bytes.
func (f *fmt) fmtBx(b []byte, digits string) {
	f.fmtSbx("", b, digits)
}

func appendQuoted(dst []byte, intbuf *[68]byte, s string, flags fmtFlags, wid, prec int) []byte {
	s = truncateString(s, flags, prec)
	if flags&flagSharp != 0 && stdstrconv.CanBackquote(s) {
		if flags&flagWidPresent == 0 || wid == 0 {
			dst = append(dst, '`')
			dst = append(dst, s...)
			return append(dst, '`')
		}
		var padLen int
		if len(s)+2 >= 4*wid {
			padLen = 0
		} else {
			padLen = wid - (runeCountStringFast(s) + 2)
		}
		padByte := byte(' ')
		if flags&(flagZero|flagMinus) == flagZero {
			padByte = '0'
		}
		if padLen > 0 && flags&flagMinus == 0 {
			dst = appendPadding(dst, padLen, padByte)
		}
		dst = append(dst, '`')
		dst = append(dst, s...)
		dst = append(dst, '`')
		if padLen > 0 && flags&flagMinus != 0 {
			dst = appendPadding(dst, padLen, padByte)
		}
		return dst
	}
	if flags&flagWidPresent == 0 {
		if flags&flagPlus != 0 {
			return stdstrconv.AppendQuoteToASCII(dst, s)
		}
		if len(s) == 0 {
			return append(dst, `""`...)
		}
		simple := true
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c < ' ' || c >= 0x7f || c == '"' || c == '\\' {
				simple = false
				break
			}
		}
		if simple {
			dst = append(dst, '"')
			dst = append(dst, s...)
			return append(dst, '"')
		}
		return stdstrconv.AppendQuote(dst, s)
	}
	if flags&flagPlus != 0 {
		return appendPadASCII(dst, stdstrconv.AppendQuoteToASCII(intbuf[:0], s), flags, wid)
	}
	return appendPad(dst, stdstrconv.AppendQuote(intbuf[:0], s), flags, wid)
}

// fmtQ formats a string as a double-quoted, escaped Go string constant.
// If sharp is set a raw (backquoted) string may be returned instead
// if the string does not contain any control characters other than tab.
func (f *fmt) fmtQ(s string) {
	*f.buf = appendQuoted(*f.buf, &f.intbuf, s, f.flags, f.wid, f.prec)
}

func appendC(dst []byte, intbuf *[68]byte, c uint64, flags fmtFlags, wid int) []byte {
	// Explicitly check whether c exceeds utf8.MaxRune since the conversion
	// of a uint64 to a rune may lose precision that indicates an overflow.
	r := rune(c)
	if c > utf8.MaxRune {
		r = utf8.RuneError
	}
	if flags&flagWidPresent == 0 || wid == 0 {
		return utf8.AppendRune(dst, r)
	}
	n := utf8.EncodeRune(intbuf[:], r)
	padByte := byte(' ')
	if flags&(flagZero|flagMinus) == flagZero {
		padByte = '0'
	}
	width := wid - 1
	if width > 0 && flags&flagMinus == 0 {
		dst = appendPadding(dst, width, padByte)
	}
	dst = append(dst, intbuf[:n]...)
	if width > 0 && flags&flagMinus != 0 {
		dst = appendPadding(dst, width, padByte)
	}
	return dst
}

// fmtC formats an integer as a Unicode character.
// If the character is not valid Unicode, it will print '\ufffd'.
func (f *fmt) fmtC(c uint64) {
	*f.buf = appendC(*f.buf, &f.intbuf, c, f.flags, f.wid)
}

func appendQc(dst []byte, intbuf *[68]byte, c uint64, flags fmtFlags, wid int) []byte {
	r := rune(c)
	if c > utf8.MaxRune {
		r = utf8.RuneError
	}
	if flags&flagWidPresent == 0 || wid == 0 {
		if flags&flagPlus != 0 {
			return stdstrconv.AppendQuoteRuneToASCII(dst, r)
		}
		return stdstrconv.AppendQuoteRune(dst, r)
	}
	if flags&flagPlus != 0 {
		return appendPadASCII(dst, stdstrconv.AppendQuoteRuneToASCII(intbuf[:0], r), flags, wid)
	}
	return appendPad(dst, stdstrconv.AppendQuoteRune(intbuf[:0], r), flags, wid)
}

// fmtQc formats an integer as a single-quoted, escaped Go character constant.
// If the character is not valid Unicode, it will print '\ufffd'.
func (f *fmt) fmtQc(c uint64) {
	*f.buf = appendQc(*f.buf, &f.intbuf, c, f.flags, f.wid)
}

func appendFloat(dst []byte, intbuf *[68]byte, v float64, size int, verb rune, prec int, flags fmtFlags, wid int) []byte {
	if flags&^flagPrecPresent == 0 {
		return strconv.AppendFloat(dst, v, byte(verb), prec, size)
	}
	// Format number, reserving space for leading + sign if needed.
	num := strconv.AppendFloat(intbuf[:1], v, byte(verb), prec, size)
	if num[1] == '-' || num[1] == '+' {
		num = num[1:]
	} else {
		num[0] = '+'
	}
	// space means to add a leading space instead of a "+" sign unless
	// the sign is explicitly asked for by plus.
	if flags&(flagSpace|flagPlus) == flagSpace && num[0] == '+' {
		num[0] = ' '
	}
	// Special handling for infinities and NaN,
	// which don't look like a number so shouldn't be padded with zeros.
	if num[1] == 'I' || num[1] == 'N' {
		// Remove sign before NaN if not asked for.
		if num[1] == 'N' && flags&(flagSpace|flagPlus) == 0 {
			num = num[1:]
		}
		return appendPadASCII(dst, num, flags&^flagZero, wid)
	}
	// The sharp flag forces printing a decimal point for non-binary formats
	// and retains trailing zeros, which we may need to restore.
	if flags&flagSharp != 0 && verb != 'b' {
		digits := 0
		switch verb {
		case 'v', 'g', 'G', 'x':
			digits = prec
			// If no precision is set explicitly use a precision of 6.
			if digits == -1 {
				digits = 6
			}
		}

		// Buffer pre-allocated with enough room for
		// exponent notations of the form "e+123" or "p-1023".
		var tailBuf [6]byte
		tail := tailBuf[:0]

		hasDecimalPoint := false
		sawNonzeroDigit := false
		// Starting from i = 1 to skip sign at num[0].
		for i := 1; i < len(num); i++ {
			switch num[i] {
			case '.':
				hasDecimalPoint = true
			case 'p', 'P':
				tail = append(tail, num[i:]...)
				num = num[:i]
			case 'e', 'E':
				if verb != 'x' && verb != 'X' {
					tail = append(tail, num[i:]...)
					num = num[:i]
					break
				}
				fallthrough
			default:
				if num[i] != '0' {
					sawNonzeroDigit = true
				}
				// Count significant digits after the first non-zero digit.
				if sawNonzeroDigit {
					digits--
				}
			}
		}
		if !hasDecimalPoint {
			// Leading digit 0 should contribute once to digits.
			if len(num) == 2 && num[1] == '0' {
				digits--
			}
			num = append(num, '.')
		}
		for digits > 0 {
			num = append(num, '0')
			digits--
		}
		num = append(num, tail...)
	}
	// We want a sign if asked for and if the sign is not positive.
	if flags&flagPlus != 0 || num[0] != '+' {
		// If we're zero padding to the left we want the sign before the leading zeros.
		// Achieve this by writing the sign out and then padding the unsigned number.
		// Zero padding is allowed only to the left.
		if flags&(flagZero|flagMinus|flagWidPresent) == (flagZero|flagWidPresent) && wid > len(num) {
			dst = append(dst, num[0])
			dst = appendPadding(dst, wid-len(num), '0')
			return append(dst, num[1:]...)
		}
		return appendPadASCII(dst, num, flags, wid)
	}
	// No sign to show and the number is positive; just print the unsigned number.
	return appendPadASCII(dst, num[1:], flags, wid)
}

// fmtFloat formats a float64. It assumes that verb is a valid format specifier
// for strconv.AppendFloat and therefore fits into a byte.
func (f *fmt) fmtFloat(v float64, size int, verb rune, prec int) {
	p := prec
	if f.flags&flagPrecPresent != 0 {
		p = f.prec
	}
	*f.buf = appendFloat(*f.buf, &f.intbuf, v, size, verb, p, f.flags, f.wid)
}
