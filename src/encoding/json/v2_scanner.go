// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.jsonv2

package json

import (
	"errors"
	"internal/byteorder"
	"io"
	"math/bits"
	"strings"
	"unicode/utf8"

	"encoding/json/internal"
	"encoding/json/internal/jsonflags"
	"encoding/json/jsontext"
)

// export exposes internal functionality of the "jsontext" package.
var export = jsontext.Internal.Export(&internal.AllowInternalUse)

// Valid reports whether data is a valid JSON encoding.
func Valid(data []byte) bool {
	return v2CheckValidFast(data)
}

func v2CheckValidFast(data []byte) bool {
	n := len(data)
	pos := 0
	for pos < n && (data[pos] == ' ' || data[pos] == '\t' || data[pos] == '\r' || data[pos] == '\n') {
		pos++
	}
	if pos >= n {
		return false
	}

	var stackBuf [64]byte
	stack := stackBuf[:0]
	const maxDepth = 10000
	stateExpect := true

	for {
		if stateExpect {
			for pos < n && (data[pos] == ' ' || data[pos] == '\t' || data[pos] == '\r' || data[pos] == '\n') {
				pos++
			}
			if pos >= n {
				return false
			}
			switch data[pos] {
			case '{':
				if len(stack) >= maxDepth {
					return false
				}
				pos++
				for pos < n && (data[pos] == ' ' || data[pos] == '\t' || data[pos] == '\r' || data[pos] == '\n') {
					pos++
				}
				if pos >= n {
					return false
				}
				if data[pos] == '}' {
					pos++
					stateExpect = false
					continue
				}
				if data[pos] != '"' {
					return false
				}
				pos = v2ScanStringEnd(data, pos)
				if pos < 0 {
					return false
				}
				for pos < n && (data[pos] == ' ' || data[pos] == '\t' || data[pos] == '\r' || data[pos] == '\n') {
					pos++
				}
				if pos >= n || data[pos] != ':' {
					return false
				}
				pos++
				stack = append(stack, '}')
				continue

			case '[':
				if len(stack) >= maxDepth {
					return false
				}
				pos++
				for pos < n && (data[pos] == ' ' || data[pos] == '\t' || data[pos] == '\r' || data[pos] == '\n') {
					pos++
				}
				if pos >= n {
					return false
				}
				if data[pos] == ']' {
					pos++
					stateExpect = false
					continue
				}
				stack = append(stack, ']')
				continue

			case '"':
				pos = v2ScanStringEnd(data, pos)
				if pos < 0 {
					return false
				}
				stateExpect = false

			case 't':
				if pos+4 > n || data[pos+1] != 'r' || data[pos+2] != 'u' || data[pos+3] != 'e' {
					return false
				}
				pos += 4
				stateExpect = false

			case 'f':
				if pos+5 > n || data[pos+1] != 'a' || data[pos+2] != 'l' || data[pos+3] != 's' || data[pos+4] != 'e' {
					return false
				}
				pos += 5
				stateExpect = false

			case 'n':
				if pos+4 > n || data[pos+1] != 'u' || data[pos+2] != 'l' || data[pos+3] != 'l' {
					return false
				}
				pos += 4
				stateExpect = false

			case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
				pos = v2ScanNumberEnd(data, pos)
				if pos < 0 {
					return false
				}
				stateExpect = false

			default:
				return false
			}
		}

		for pos < n && (data[pos] == ' ' || data[pos] == '\t' || data[pos] == '\r' || data[pos] == '\n') {
			pos++
		}
		if len(stack) == 0 {
			return pos == n
		}
		if pos >= n {
			return false
		}
		top := stack[len(stack)-1]
		c := data[pos]
		if c == top {
			pos++
			stack = stack[:len(stack)-1]
			continue
		}
		if c != ',' {
			return false
		}
		pos++
		if top == '}' {
			for pos < n && (data[pos] == ' ' || data[pos] == '\t' || data[pos] == '\r' || data[pos] == '\n') {
				pos++
			}
			if pos >= n || data[pos] != '"' {
				return false
			}
			pos = v2ScanStringEnd(data, pos)
			if pos < 0 {
				return false
			}
			for pos < n && (data[pos] == ' ' || data[pos] == '\t' || data[pos] == '\r' || data[pos] == '\n') {
				pos++
			}
			if pos >= n || data[pos] != ':' {
				return false
			}
			pos++
		}
		stateExpect = true
	}
}

func v2ScanStringEnd(data []byte, pos int) int {
	const (
		lo8 = 0x0101010101010101
		hi8 = 0x8080808080808080
		sp8 = 0x2020202020202020
		qt8 = 0x2222222222222222
		bs8 = 0x5c5c5c5c5c5c5c5c
	)
	n := len(data)
	i := pos + 1
	for {
		for n-i >= 8 {
			w := byteorder.LEUint64(data[i:])
			xq := w ^ qt8
			xb := w ^ bs8
			if m := (w & hi8) | ((w - sp8) & hi8) | ((xq - lo8) &^ xq & hi8) | ((xb - lo8) &^ xb & hi8); m != 0 {
				i += bits.TrailingZeros64(m) >> 3
				goto special
			}
			i += 8
		}
		for i < n {
			c := data[i]
			if c == '"' {
				return i + 1
			}
			if c == '\\' || c < 0x20 {
				goto special
			}
			i++
		}
		return -1

	special:
		c := data[i]
		if c == '"' {
			return i + 1
		}
		if c >= utf8.RuneSelf {
			i++
			continue
		}
		if c < 0x20 {
			return -1
		}
		i++
		if i >= n {
			return -1
		}
		switch data[i] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			i++
		case 'u':
			if i+4 >= n {
				return -1
			}
			for j := 1; j <= 4; j++ {
				h := data[i+j]
				if !((h >= '0' && h <= '9') || (h >= 'a' && h <= 'f') || (h >= 'A' && h <= 'F')) {
					return -1
				}
			}
			i += 5
		default:
			return -1
		}
	}
}

func v2ScanNumberEnd(data []byte, pos int) int {
	n := len(data)
	i := pos
	if i < n && data[i] == '-' {
		i++
		if i >= n {
			return -1
		}
	}
	if data[i] == '0' {
		i++
	} else if '1' <= data[i] && data[i] <= '9' {
		i++
		for i < n && '0' <= data[i] && data[i] <= '9' {
			i++
		}
	} else {
		return -1
	}
	if i < n && data[i] == '.' {
		i++
		if i >= n || data[i] < '0' || data[i] > '9' {
			return -1
		}
		i++
		for i < n && '0' <= data[i] && data[i] <= '9' {
			i++
		}
	}
	if i < n && (data[i] == 'e' || data[i] == 'E') {
		i++
		if i < n && (data[i] == '+' || data[i] == '-') {
			i++
		}
		if i >= n || data[i] < '0' || data[i] > '9' {
			return -1
		}
		i++
		for i < n && '0' <= data[i] && data[i] <= '9' {
			i++
		}
	}
	return i
}

func checkValid(data []byte) error {
	d := export.GetBufferedDecoder(data)
	defer export.PutBufferedDecoder(d)
	xd := export.Decoder(d)
	xd.Struct.Flags.Set(jsonflags.ReportErrorsWithLegacySemantics | jsonflags.AllowDuplicateNames | jsonflags.AllowInvalidUTF8 | 1)
	if _, err := d.ReadValue(); err != nil {
		if err == io.EOF {
			offset := d.InputOffset() + int64(len(d.UnreadBuffer()))
			err = &jsontext.SyntacticError{ByteOffset: offset, Err: io.ErrUnexpectedEOF}
		}
		return transformSyntacticError(err)
	}
	if err := xd.CheckEOF(); err != nil {
		return transformSyntacticError(err)
	}
	return nil
}

// A SyntaxError is a description of a JSON syntax error.
// [Unmarshal] will return a SyntaxError if the JSON can't be parsed.
type SyntaxError struct {
	msg    string // description of error
	Offset int64  // error occurred after reading Offset bytes
}

func (e *SyntaxError) Error() string { return e.msg }

var errUnexpectedEnd = errors.New("unexpected end of JSON input")

func transformSyntacticError(err error) error {
	switch serr, ok := err.(*jsontext.SyntacticError); {
	case serr != nil:
		// If the SyntacticError wraps an IO error, unwrap it
		// to match v1 behavior which returned IO errors directly.
		if export.IsIOError(serr.Err) {
			return errors.Unwrap(serr.Err)
		}
		if serr.Err == io.ErrUnexpectedEOF {
			serr.Err = errUnexpectedEnd
		}
		msg := serr.Err.Error()
		if i := strings.Index(msg, " (expecting"); i >= 0 && !strings.Contains(msg, " in literal") {
			msg = msg[:i]
		}
		return &SyntaxError{Offset: serr.ByteOffset, msg: syntaxErrorReplacer.Replace(msg)}
	case ok:
		return (*SyntaxError)(nil)
	case export.IsIOError(err):
		return errors.Unwrap(err) // v1 historically did not wrap IO errors
	default:
		return err
	}
}

// syntaxErrorReplacer replaces certain string literals in the v2 error
// to better match the historical string rendering of syntax errors.
// In particular, v2 uses the terminology "object name" to match RFC 8259,
// while v1 uses "object key", which is not a term found in JSON literature.
var syntaxErrorReplacer = strings.NewReplacer(
	"object name", "object key",
	"at start of value", "looking for beginning of value",
	"at start of string", "looking for beginning of object key string",
	"after object value", "after object key:value pair",
	"in number", "in numeric literal",
)
