// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xml

import (
	"bytes"
	"internal/byteorder"
	istrconv "internal/strconv"
	"io"
	"math/bits"
	"strconv"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

const (
	xmlSwarLo = 0x0101010101010101
	xmlSwarHi = 0x8080808080808080
	xmlSwarSp = 0x2020202020202020
	xmlSwarQt = 0x2222222222222222 // '"'
	xmlSwarAm = 0x2626262626262626 // '&'
	xmlSwarAp = 0x2727272727272727 // '\''
	xmlSwarLt = 0x3c3c3c3c3c3c3c3c // '<'
	xmlSwarGt = 0x3e3e3e3e3e3e3e3e // '>'
	xmlSwarRb = 0x5d5d5d5d5d5d5d5d // ']'
)

func xmlEscapeMask(w uint64) uint64 {
	xqt := w ^ xmlSwarQt
	xam := w ^ xmlSwarAm
	xap := w ^ xmlSwarAp
	xlt := w ^ xmlSwarLt
	xgt := w ^ xmlSwarGt
	return (w & xmlSwarHi) |
		((w - xmlSwarSp) & xmlSwarHi) |
		((xqt - xmlSwarLo) &^ xqt & xmlSwarHi) |
		((xam - xmlSwarLo) &^ xam & xmlSwarHi) |
		((xap - xmlSwarLo) &^ xap & xmlSwarHi) |
		((xlt - xmlSwarLo) &^ xlt & xmlSwarHi) |
		((xgt - xmlSwarLo) &^ xgt & xmlSwarHi)
}

func xmlCharDataMask(w uint64) uint64 {
	xam := w ^ xmlSwarAm
	xlt := w ^ xmlSwarLt
	xrb := w ^ xmlSwarRb
	return (w & xmlSwarHi) |
		((w - xmlSwarSp) & xmlSwarHi) |
		((xam - xmlSwarLo) &^ xam & xmlSwarHi) |
		((xlt - xmlSwarLo) &^ xlt & xmlSwarHi) |
		((xrb - xmlSwarLo) &^ xrb & xmlSwarHi)
}

func xmlAttrValMask(w, q8 uint64) uint64 {
	xq := w ^ q8
	xam := w ^ xmlSwarAm
	xlt := w ^ xmlSwarLt
	return (w & xmlSwarHi) |
		((w - xmlSwarSp) & xmlSwarHi) |
		((xq - xmlSwarLo) &^ xq & xmlSwarHi) |
		((xam - xmlSwarLo) &^ xam & xmlSwarHi) |
		((xlt - xmlSwarLo) &^ xlt & xmlSwarHi)
}

// xmlEscapeASCII reports whether a byte < 0x80 requires escaping (assuming escapeNewline=true).
var xmlEscapeASCII = [128]bool{
	0x00: true, 0x01: true, 0x02: true, 0x03: true, 0x04: true, 0x05: true, 0x06: true, 0x07: true,
	0x08: true, 0x09: true, 0x0A: true, 0x0B: true, 0x0C: true, 0x0D: true, 0x0E: true, 0x0F: true,
	0x10: true, 0x11: true, 0x12: true, 0x13: true, 0x14: true, 0x15: true, 0x16: true, 0x17: true,
	0x18: true, 0x19: true, 0x1A: true, 0x1B: true, 0x1C: true, 0x1D: true, 0x1E: true, 0x1F: true,
	'"':  true,
	'&':  true,
	'\'': true,
	'<':  true,
	'>':  true,
}

func appendEscapeXMLString(dst []byte, s string, escapeNewline bool) []byte {
	return appendEscapeXMLBytes(dst, unsafe.Slice(unsafe.StringData(s), len(s)), escapeNewline)
}

func appendEscapeXMLBytes(dst, s []byte, escapeNewline bool) []byte {
	i := 0
	for len(s)-i >= 8 {
		if m := xmlEscapeMask(byteorder.LEUint64(s[i:])); m != 0 {
			i += bits.TrailingZeros64(m) >> 3
			return appendEscapeXMLSlow(dst, s, i, escapeNewline)
		}
		i += 8
	}
	for i < len(s) {
		c := s[i]
		if c >= utf8.RuneSelf || xmlEscapeASCII[c] {
			return appendEscapeXMLSlow(dst, s, i, escapeNewline)
		}
		i++
	}
	return append(dst, s...)
}

func appendEscapeXMLSlow(dst, s []byte, i int, escapeNewline bool) []byte {
	last := 0
	first := true
	for i < len(s) {
		if !first {
			for len(s)-i >= 8 {
				if m := xmlEscapeMask(byteorder.LEUint64(s[i:])); m != 0 {
					i += bits.TrailingZeros64(m) >> 3
					break
				}
				i += 8
			}
			if i >= len(s) {
				break
			}
		}
		first = false
		c := s[i]
		if c < utf8.RuneSelf {
			if !xmlEscapeASCII[c] {
				i++
				continue
			}
			var esc []byte
			switch c {
			case '"':
				esc = escQuot
			case '\'':
				esc = escApos
			case '&':
				esc = escAmp
			case '<':
				esc = escLT
			case '>':
				esc = escGT
			case '\t':
				esc = escTab
			case '\n':
				if !escapeNewline {
					i++
					continue
				}
				esc = escNL
			case '\r':
				esc = escCR
			default:
				esc = escFFFD
			}
			dst = append(dst, s[last:i]...)
			dst = append(dst, esc...)
			i++
			last = i
			continue
		}
		// Fast non-ASCII UTF-8 validation.
		// All valid 2-byte and 4-byte UTF-8 runes are valid XML chars.
		// Valid 3-byte UTF-8 runes are valid XML chars except U+FFFE (EF BF BE) and U+FFFF (EF BF BF).
		if c >= 0xC2 && c <= 0xDF {
			if i+1 < len(s) && s[i+1]&0xC0 == 0x80 {
				i += 2
				continue
			}
		} else if c >= 0xE0 && c <= 0xEF {
			if i+2 < len(s) && s[i+2]&0xC0 == 0x80 {
				b1 := s[i+1]
				if c == 0xE0 {
					if b1 >= 0xA0 && b1 <= 0xBF {
						i += 3
						continue
					}
				} else if c == 0xED {
					if b1 >= 0x80 && b1 <= 0x9F {
						i += 3
						continue
					}
				} else if c == 0xEF && b1 == 0xBF && (s[i+2] == 0xBE || s[i+2] == 0xBF) {
					dst = append(dst, s[last:i]...)
					dst = append(dst, escFFFD...)
					i += 3
					last = i
					continue
				} else if b1&0xC0 == 0x80 {
					i += 3
					continue
				}
			}
		} else if c >= 0xF0 && c <= 0xF4 {
			if i+3 < len(s) && s[i+2]&0xC0 == 0x80 && s[i+3]&0xC0 == 0x80 {
				b1 := s[i+1]
				if (c == 0xF0 && b1 >= 0x90 && b1 <= 0xBF) ||
					(c >= 0xF1 && c <= 0xF3 && b1&0xC0 == 0x80) ||
					(c == 0xF4 && b1 >= 0x80 && b1 <= 0x8F) {
					i += 4
					continue
				}
			}
		}
		// Invalid UTF-8 byte.
		dst = append(dst, s[last:i]...)
		dst = append(dst, escFFFD...)
		i++
		last = i
	}
	return append(dst, s[last:]...)
}

// nameCharTable classifies ASCII bytes for XML names:
//   - bit 0 (1): valid first name char ('A'-'Z', 'a'-'z', '_', ':')
//   - bit 1 (2): valid subsequent name char (above plus '0'-'9', '.', '-')
var nameCharTable = [256]uint8{
	'-': 2,
	'.': 2,
	'0': 2, '1': 2, '2': 2, '3': 2, '4': 2, '5': 2, '6': 2, '7': 2, '8': 2, '9': 2,
	':': 3,
	'A': 3, 'B': 3, 'C': 3, 'D': 3, 'E': 3, 'F': 3, 'G': 3, 'H': 3, 'I': 3, 'J': 3, 'K': 3, 'L': 3, 'M': 3,
	'N': 3, 'O': 3, 'P': 3, 'Q': 3, 'R': 3, 'S': 3, 'T': 3, 'U': 3, 'V': 3, 'W': 3, 'X': 3, 'Y': 3, 'Z': 3,
	'_': 3,
	'a': 3, 'b': 3, 'c': 3, 'd': 3, 'e': 3, 'f': 3, 'g': 3, 'h': 3, 'i': 3, 'j': 3, 'k': 3, 'l': 3, 'm': 3,
	'n': 3, 'o': 3, 'p': 3, 'q': 3, 'r': 3, 's': 3, 't': 3, 'u': 3, 'v': 3, 'w': 3, 'x': 3, 'y': 3, 'z': 3,
}

func isNameFast(s []byte) bool {
	if len(s) == 0 {
		return false
	}
	if b0 := s[0]; b0 < utf8.RuneSelf {
		if nameCharTable[b0]&1 == 0 {
			return false
		}
		for i := 1; i < len(s); i++ {
			b := s[i]
			if b < utf8.RuneSelf {
				if nameCharTable[b]&2 == 0 {
					return false
				}
				continue
			}
			return isNameUnicodeTail(s[i:])
		}
		return true
	}
	return isNameUnicode(s)
}

func isNameUnicodeTail(s []byte) bool {
	for len(s) > 0 {
		c, n := utf8.DecodeRune(s)
		if c == utf8.RuneError && n == 1 {
			return false
		}
		if !unicode.Is(first, c) && !unicode.Is(second, c) {
			return false
		}
		s = s[n:]
	}
	return true
}

func isNameUnicode(s []byte) bool {
	c, n := utf8.DecodeRune(s)
	if c == utf8.RuneError && n == 1 {
		return false
	}
	if !unicode.Is(first, c) {
		return false
	}
	return isNameUnicodeTail(s[n:])
}

func isNameStringFast(s string) bool {
	return isNameFast(unsafe.Slice(unsafe.StringData(s), len(s)))
}

var asciiSpace = [256]bool{' ': true, '\t': true, '\n': true, '\r': true, '\v': true, '\f': true}

func trimSpaceBytes(s []byte) []byte {
	start := 0
	for ; start < len(s); start++ {
		c := s[start]
		if c >= utf8.RuneSelf {
			return bytes.TrimSpace(s[start:])
		}
		if !asciiSpace[c] {
			break
		}
	}
	stop := len(s)
	for ; stop > start; stop-- {
		c := s[stop-1]
		if c >= utf8.RuneSelf {
			return bytes.TrimSpace(s[start:stop])
		}
		if !asciiSpace[c] {
			break
		}
	}
	return s[start:stop]
}

func parseIntBytes(b []byte, bitSize int) (int64, error) {
	tb := trimSpaceBytes(b)
	s := unsafe.String(unsafe.SliceData(tb), len(tb))
	v, err := istrconv.ParseInt(s, 10, bitSize)
	if err != nil {
		return strconv.ParseInt(string(tb), 10, bitSize)
	}
	return v, nil
}

func parseUintBytes(b []byte, bitSize int) (uint64, error) {
	tb := trimSpaceBytes(b)
	s := unsafe.String(unsafe.SliceData(tb), len(tb))
	v, err := istrconv.ParseUint(s, 10, bitSize)
	if err != nil {
		return strconv.ParseUint(string(tb), 10, bitSize)
	}
	return v, nil
}

func parseFloatBytes(b []byte, bitSize int) (float64, error) {
	tb := trimSpaceBytes(b)
	s := unsafe.String(unsafe.SliceData(tb), len(tb))
	v, err := istrconv.ParseFloat(s, bitSize)
	if err != nil {
		return strconv.ParseFloat(string(tb), bitSize)
	}
	return v, nil
}

func parseBoolBytes(b []byte) (bool, error) {
	tb := trimSpaceBytes(b)
	switch len(tb) {
	case 1:
		switch tb[0] {
		case '1', 't', 'T':
			return true, nil
		case '0', 'f', 'F':
			return false, nil
		}
	case 4:
		if string(tb) == "true" || string(tb) == "TRUE" || string(tb) == "True" {
			return true, nil
		}
	case 5:
		if string(tb) == "false" || string(tb) == "FALSE" || string(tb) == "False" {
			return false, nil
		}
	}
	return strconv.ParseBool(string(tb))
}

func escapeTextFast(w io.Writer, s []byte, escapeNewline bool) error {
	if p, ok := w.(*printer); ok && p.w == nil {
		if p.closed && p.err == nil {
			p.err = errClosedEncoder
			return p.err
		}
		if p.err != nil {
			return p.err
		}
		p.out = appendEscapeXMLBytes(p.out, s, escapeNewline)
		return nil
	}
	if len(s) == 0 {
		_, err := w.Write(s)
		return err
	}
	i := 0
	for len(s)-i >= 8 {
		if m := xmlEscapeMask(byteorder.LEUint64(s[i:])); m != 0 {
			i += bits.TrailingZeros64(m) >> 3
			return escapeTextFastSlow(w, s, i, escapeNewline)
		}
		i += 8
	}
	for i < len(s) {
		c := s[i]
		if c >= utf8.RuneSelf || xmlEscapeASCII[c] {
			return escapeTextFastSlow(w, s, i, escapeNewline)
		}
		i++
	}
	_, err := w.Write(s)
	return err
}

func escapeTextFastSlow(w io.Writer, s []byte, i int, escapeNewline bool) error {
	var esc []byte
	last := 0
	first := true
	for i < len(s) {
		if !first {
			for len(s)-i >= 8 {
				if m := xmlEscapeMask(byteorder.LEUint64(s[i:])); m != 0 {
					i += bits.TrailingZeros64(m) >> 3
					break
				}
				i += 8
			}
			if i >= len(s) {
				break
			}
		}
		first = false
		c := s[i]
		if c < utf8.RuneSelf {
			if !xmlEscapeASCII[c] {
				i++
				continue
			}
			switch c {
			case '"':
				esc = escQuot
			case '\'':
				esc = escApos
			case '&':
				esc = escAmp
			case '<':
				esc = escLT
			case '>':
				esc = escGT
			case '\t':
				esc = escTab
			case '\n':
				if !escapeNewline {
					i++
					continue
				}
				esc = escNL
			case '\r':
				esc = escCR
			default:
				esc = escFFFD
			}
			if _, err := w.Write(s[last:i]); err != nil {
				return err
			}
			if _, err := w.Write(esc); err != nil {
				return err
			}
			i++
			last = i
			continue
		}
		r, width := utf8.DecodeRune(s[i:])
		i += width
		if !isInCharacterRange(r) || (r == 0xFFFD && width == 1) {
			if _, err := w.Write(s[last : i-width]); err != nil {
				return err
			}
			if _, err := w.Write(escFFFD); err != nil {
				return err
			}
			last = i
		}
	}
	_, err := w.Write(s[last:])
	return err
}
