// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package textproto

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"internal/byteorder"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"unsafe" // for linkname and string slicing
)

// TODO: This should be a distinguishable error (ErrMessageTooLarge)
// to allow mime/multipart and net/http to detect it.
var errMessageTooLarge = errors.New("message too large")

// A Reader implements convenience methods for reading requests
// or responses from a text protocol network connection.
type Reader struct {
	R   *bufio.Reader
	dot *dotReader
	buf []byte // a re-usable buffer for readContinuedLineSlice
}

// NewReader returns a new [Reader] reading from r.
//
// To avoid denial of service attacks, the provided [bufio.Reader]
// should be reading from an [io.LimitReader] or similar Reader to bound
// the size of responses.
func NewReader(r *bufio.Reader) *Reader {
	return &Reader{R: r}
}

// ReadLine reads a single line from r,
// eliding the final \n or \r\n from the returned string.
func (r *Reader) ReadLine() (string, error) {
	line, err := r.readLineSlice(-1)
	return string(line), err
}

// ReadLineBytes is like [Reader.ReadLine] but returns a []byte instead of a string.
func (r *Reader) ReadLineBytes() ([]byte, error) {
	line, err := r.readLineSlice(-1)
	if line != nil {
		line = bytes.Clone(line)
	}
	return line, err
}

// readLineSlice reads a single line from r,
// up to lim bytes long (or unlimited if lim is less than 0),
// eliding the final \r or \r\n from the returned string.
func (r *Reader) readLineSlice(lim int64) ([]byte, error) {
	r.closeDot()
	var line []byte
	for {
		l, more, err := r.R.ReadLine()
		if err != nil {
			return nil, err
		}
		if lim >= 0 && int64(len(line))+int64(len(l)) > lim {
			return nil, errMessageTooLarge
		}
		// Avoid the copy if the first call produced a full line.
		if line == nil && !more {
			return l, nil
		}
		line = append(line, l...)
		if !more {
			break
		}
	}
	return line, nil
}

// ReadContinuedLine reads a possibly continued line from r,
// eliding the final trailing ASCII white space.
// Lines after the first are considered continuations if they
// begin with a space or tab character. In the returned data,
// continuation lines are separated from the previous line
// only by a single space: the newline and leading white space
// are removed.
//
// For example, consider this input:
//
//	Line 1
//	  continued...
//	Line 2
//
// The first call to ReadContinuedLine will return "Line 1 continued..."
// and the second will return "Line 2".
//
// Empty lines are never continued.
func (r *Reader) ReadContinuedLine() (string, error) {
	line, err := r.readContinuedLineSlice(-1, noValidation)
	return string(line), err
}

// trim returns s with leading and trailing spaces and tabs removed.
// It does not assume Unicode or UTF-8.
func trim(s []byte) []byte {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	s = s[i:]
	n := len(s) - 1
	for n >= 0 && (s[n] == ' ' || s[n] == '\t') {
		n--
	}
	return s[:n+1]
}

// ReadContinuedLineBytes is like [Reader.ReadContinuedLine] but
// returns a []byte instead of a string.
func (r *Reader) ReadContinuedLineBytes() ([]byte, error) {
	line, err := r.readContinuedLineSlice(-1, noValidation)
	if line != nil {
		line = bytes.Clone(line)
	}
	return line, err
}

// readContinuedLineSlice reads continued lines from the reader buffer,
// returning a byte slice with all lines. The validateFirstLine function
// is run on the first read line, and if it returns an error then this
// error is returned from readContinuedLineSlice.
// It reads up to lim bytes of data (or unlimited if lim is less than 0).
func (r *Reader) readContinuedLineSlice(lim int64, validateFirstLine func([]byte) error) ([]byte, error) {
	if validateFirstLine == nil {
		return nil, fmt.Errorf("missing validateFirstLine func")
	}

	// Read the first line.
	line, err := r.readLineSlice(lim)
	if err != nil {
		return nil, err
	}
	if len(line) == 0 { // blank line - no continuation
		return line, nil
	}

	if err := validateFirstLine(line); err != nil {
		return nil, err
	}

	// Optimistically assume that we have started to buffer the next line
	// and it starts with an ASCII letter (the next header key), or a blank
	// line, so we can avoid copying that buffered data around in memory
	// and skipping over non-existent whitespace.
	if r.R.Buffered() > 1 {
		peek, _ := r.R.Peek(2)
		if len(peek) > 0 && (isASCIILetter(peek[0]) || peek[0] == '\n') ||
			len(peek) == 2 && peek[0] == '\r' && peek[1] == '\n' {
			return trim(line), nil
		}
	}

	// ReadByte or the next readLineSlice will flush the read buffer;
	// copy the slice into buf.
	r.buf = append(r.buf[:0], trim(line)...)

	if lim < 0 {
		lim = math.MaxInt64
	}
	lim -= int64(len(r.buf))

	// Read continuation lines.
	for r.skipSpace() > 0 {
		r.buf = append(r.buf, ' ')
		if int64(len(r.buf)) >= lim {
			return nil, errMessageTooLarge
		}
		line, err := r.readLineSlice(lim - int64(len(r.buf)))
		if err != nil {
			return nil, err
		}
		r.buf = append(r.buf, trim(line)...)
	}
	return r.buf, nil
}

// skipSpace skips R over all spaces and returns the number of bytes skipped.
func (r *Reader) skipSpace() int {
	n := 0
	for {
		c, err := r.R.ReadByte()
		if err != nil {
			// Bufio will keep err until next read.
			break
		}
		if c != ' ' && c != '\t' {
			r.R.UnreadByte()
			break
		}
		n++
	}
	return n
}

func (r *Reader) readCodeLine(expectCode int) (code int, continued bool, message string, err error) {
	line, err := r.ReadLine()
	if err != nil {
		return
	}
	return parseCodeLine(line, expectCode)
}

func parseCodeLine(line string, expectCode int) (code int, continued bool, message string, err error) {
	if len(line) < 4 || line[3] != ' ' && line[3] != '-' {
		err = ProtocolError(fmt.Sprintf("short response: %q", line))
		return
	}
	continued = line[3] == '-'
	code, err = strconv.Atoi(line[0:3])
	if err != nil || code < 100 {
		err = ProtocolError(fmt.Sprintf("invalid response code: %q", line))
		return
	}
	message = line[4:]
	if 1 <= expectCode && expectCode < 10 && code/100 != expectCode ||
		10 <= expectCode && expectCode < 100 && code/10 != expectCode ||
		100 <= expectCode && expectCode < 1000 && code != expectCode {
		err = &Error{code, message}
	}
	return
}

// ReadCodeLine reads a response code line of the form
//
//	code message
//
// where code is a three-digit status code and the message
// extends to the rest of the line. An example of such a line is:
//
//	220 plan9.bell-labs.com ESMTP
//
// If the prefix of the status does not match the digits in expectCode,
// ReadCodeLine returns with err set to &Error{code, message}.
// For example, if expectCode is 31, an error will be returned if
// the status is not in the range [310,319].
//
// If the response is multi-line, ReadCodeLine returns an error.
//
// An expectCode <= 0 disables the check of the status code.
func (r *Reader) ReadCodeLine(expectCode int) (code int, message string, err error) {
	code, continued, message, err := r.readCodeLine(expectCode)
	if err == nil && continued {
		err = ProtocolError(fmt.Sprintf("unexpected multi-line response: %q", message))
	}
	return
}

// ReadResponse reads a multi-line response of the form:
//
//	code-message line 1
//	code-message line 2
//	...
//	code message line n
//
// where code is a three-digit status code. The first line starts with the
// code and a hyphen. The response is terminated by a line that starts
// with the same code followed by a space. Each line in message is
// separated by a newline (\n).
//
// See page 36 of RFC 959 (https://www.ietf.org/rfc/rfc959.txt) for
// details of another form of response accepted:
//
//	code-message line 1
//	message line 2
//	...
//	code message line n
//
// If the prefix of the status does not match the digits in expectCode,
// ReadResponse returns with err set to &Error{code, message}.
// For example, if expectCode is 31, an error will be returned if
// the status is not in the range [310,319].
//
// An expectCode <= 0 disables the check of the status code.
func (r *Reader) ReadResponse(expectCode int) (code int, message string, err error) {
	code, continued, first, err := r.readCodeLine(expectCode)
	multi := continued
	var messageBuilder strings.Builder
	messageBuilder.WriteString(first)
	for continued {
		line, err := r.ReadLine()
		if err != nil {
			return 0, "", err
		}

		var code2 int
		var moreMessage string
		code2, continued, moreMessage, err = parseCodeLine(line, 0)
		if err != nil || code2 != code {
			messageBuilder.WriteByte('\n')
			messageBuilder.WriteString(strings.TrimRight(line, "\r\n"))
			continued = true
			continue
		}
		messageBuilder.WriteByte('\n')
		messageBuilder.WriteString(moreMessage)
	}
	message = messageBuilder.String()
	if err != nil && multi && message != "" {
		// replace one line error message with all lines (full message)
		err = &Error{code, message}
	}
	return
}

// DotReader returns a new [Reader] that satisfies Reads using the
// decoded text of a dot-encoded block read from r.
// The returned Reader is only valid until the next call
// to a method on r.
//
// Dot encoding is a common framing used for data blocks
// in text protocols such as SMTP.  The data consists of a sequence
// of lines, each of which ends in "\r\n".  The sequence itself
// ends at a line containing just a dot: ".\r\n".  Lines beginning
// with a dot are escaped with an additional dot to avoid
// looking like the end of the sequence.
//
// The decoded form returned by the Reader's Read method
// rewrites the "\r\n" line endings into the simpler "\n",
// removes leading dot escapes if present, and stops with error [io.EOF]
// after consuming (and discarding) the end-of-sequence line.
func (r *Reader) DotReader() io.Reader {
	r.closeDot()
	r.dot = &dotReader{r: r}
	return r.dot
}

type dotReader struct {
	r     *Reader
	state int
}

// Read satisfies reads by decoding dot-encoded data read from d.r.
func (d *dotReader) Read(b []byte) (n int, err error) {
	// Run data through a simple state machine to
	// elide leading dots, rewrite trailing \r\n into \n,
	// and detect ending .\r\n line.
	const (
		stateBeginLine = iota // beginning of line; initial state; must be zero
		stateDot              // read . at beginning of line
		stateDotCR            // read .\r at beginning of line
		stateCR               // read \r (possibly at end of line)
		stateData             // reading data in middle of line
		stateEOF              // reached .\r\n end marker line
	)
	br := d.r.R
	for n < len(b) && d.state != stateEOF {
		var c byte
		c, err = br.ReadByte()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			break
		}
		switch d.state {
		case stateBeginLine:
			if c == '.' {
				d.state = stateDot
				continue
			}
			if c == '\r' {
				d.state = stateCR
				continue
			}
			d.state = stateData

		case stateDot:
			if c == '\r' {
				d.state = stateDotCR
				continue
			}
			if c == '\n' {
				d.state = stateEOF
				continue
			}
			d.state = stateData

		case stateDotCR:
			if c == '\n' {
				d.state = stateEOF
				continue
			}
			// Not part of .\r\n.
			// Consume leading dot and emit saved \r.
			br.UnreadByte()
			c = '\r'
			d.state = stateData

		case stateCR:
			if c == '\n' {
				d.state = stateBeginLine
				break
			}
			// Not part of \r\n. Emit saved \r
			br.UnreadByte()
			c = '\r'
			d.state = stateData

		case stateData:
			if c == '\r' {
				d.state = stateCR
				continue
			}
			if c == '\n' {
				d.state = stateBeginLine
			}
		}
		b[n] = c
		n++
	}
	if err == nil && d.state == stateEOF {
		err = io.EOF
	}
	if err != nil && d.r.dot == d {
		d.r.dot = nil
	}
	return
}

// closeDot drains the current DotReader if any,
// making sure that it reads until the ending dot line.
func (r *Reader) closeDot() {
	if r.dot == nil {
		return
	}
	buf := make([]byte, 128)
	for r.dot != nil {
		// When Read reaches EOF or an error,
		// it will set r.dot == nil.
		r.dot.Read(buf)
	}
}

// ReadDotBytes reads a dot-encoding and returns the decoded data.
//
// See the documentation for the [Reader.DotReader] method for details about dot-encoding.
func (r *Reader) ReadDotBytes() ([]byte, error) {
	return io.ReadAll(r.DotReader())
}

// ReadDotLines reads a dot-encoding and returns a slice
// containing the decoded lines, with the final \r\n or \n elided from each.
//
// See the documentation for the [Reader.DotReader] method for details about dot-encoding.
func (r *Reader) ReadDotLines() ([]string, error) {
	// We could use ReadDotBytes and then Split it,
	// but reading a line at a time avoids needing a
	// large contiguous block of memory and is simpler.
	var v []string
	var err error
	for {
		var line string
		line, err = r.ReadLine()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			break
		}

		// Dot by itself marks end; otherwise cut one dot.
		if len(line) > 0 && line[0] == '.' {
			if len(line) == 1 {
				break
			}
			line = line[1:]
		}
		v = append(v, line)
	}
	return v, err
}

var colon = []byte(":")

// ReadMIMEHeader reads a MIME-style header from r.
// The header is a sequence of possibly continued Key: Value lines
// ending in a blank line.
// The returned map m maps [CanonicalMIMEHeaderKey](key) to a
// sequence of values in the same order encountered in the input.
//
// For example, consider this input:
//
//	My-Key: Value 1
//	Long-Key: Even
//	       Longer Value
//	My-Key: Value 2
//
// Given that input, ReadMIMEHeader returns the map:
//
//	map[string][]string{
//		"My-Key": {"Value 1", "Value 2"},
//		"Long-Key": {"Even Longer Value"},
//	}
func (r *Reader) ReadMIMEHeader() (MIMEHeader, error) {
	return readMIMEHeader(r, math.MaxInt64, math.MaxInt64)
}

// readMIMEHeader should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/qtgolang/SunnyNet
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname readMIMEHeader

// readMIMEHeader is a version of ReadMIMEHeader which takes a limit on the header size.
// It is called by the mime/multipart and net/http package.
func readMIMEHeader(r *Reader, maxMemory, maxHeaders int64) (MIMEHeader, error) {
	if maxMemory < 0 {
		return nil, errMessageTooLarge
	}
	r.closeDot()

	if m, _, _, _, handled, err := readMIMEHeaderBuffered(r.R, maxMemory, maxHeaders, false); handled {
		return m, err
	}

	// Avoid lots of small slice allocations later by allocating one
	// large one ahead of time which we'll cut up into smaller
	// slices. If this isn't big enough later, we allocate small ones.
	var strs []string
	hint, valBytes := r.upcomingHeaderInfo()
	if hint > 0 {
		if hint > 1000 {
			hint = 1000 // set a cap to avoid overallocation
		}
		strs = make([]string, hint)
	}

	m := make(MIMEHeader, hint)

	// The first line cannot start with a leading space.
	if buf, err := r.R.Peek(1); err == nil && (buf[0] == ' ' || buf[0] == '\t') {
		const errorLimit = 80 // arbitrary limit on how much of the line we'll quote
		line, err := r.readLineSlice(errorLimit)
		if err != nil {
			return m, err
		}
		return m, ProtocolError(fmt.Sprintf("malformed MIME header initial line: %q", line))
	}

	var valBuf []byte
	remKeys := hint

	for {
		kv, err := r.readContinuedLineSlice(maxMemory, mustHaveFieldNameColon)
		if len(kv) == 0 {
			return m, err
		}
		remKeys--

		// Key ends at first colon.
		k, v, ok := bytes.Cut(kv, colon)
		if !ok {
			return m, ProtocolError(fmt.Sprintf("malformed MIME header line: %q", kv))
		}
		if !validHeaderValueBytes(v) {
			return m, ProtocolError(fmt.Sprintf("malformed MIME header line: %q", kv))
		}
		key, ok := canonicalMIMEHeaderKey(k)
		if !ok {
			return m, ProtocolError(fmt.Sprintf("malformed MIME header line: %q", kv))
		}

		maxHeaders--
		if maxHeaders < 0 {
			return nil, errMessageTooLarge
		}

		// Skip initial spaces in value.
		v = bytes.TrimLeft(v, " \t")
		valBytes -= len(v)
		value, interned := internHeaderValue(v)
		if !interned {
			if len(valBuf) < len(v) {
				if remKeys > 0 && valBytes > 0 && int64(len(v)+valBytes) <= maxMemory {
					allocSize := len(v) + valBytes
					if allocSize > 4096 {
						allocSize = max(len(v), 4096)
					}
					valBuf = make([]byte, allocSize)
				}
			}
			if len(valBuf) >= len(v) {
				copy(valBuf, v)
				value = unsafe.String(&valBuf[0], len(v))
				valBuf = valBuf[len(v):]
			} else {
				value = string(v)
			}
		}

		vv := m[key]
		if vv == nil {
			// Account for per-entry overhead.
			// 200 bytes is based on benchmarks circa Go 1.20.
			const mapEntryOverhead = 200

			maxMemory -= int64(len(key))
			maxMemory -= mapEntryOverhead
		}
		maxMemory -= int64(len(value))
		if maxMemory < 0 {
			return m, errMessageTooLarge
		}
		if vv == nil && len(strs) > 0 {
			// More than likely this will be a single-element key.
			// Most headers aren't multi-valued.
			// Set the capacity on strs[0] to 1, so any future append
			// won't extend the slice into the other strings.
			vv, strs = strs[:1:1], strs[1:]
			vv[0] = value
			m[key] = vv
		} else {
			m[key] = append(vv, value)
		}

		if err != nil {
			return m, err
		}
	}
}

//go:linkname readMIMEHeaderFast
func readMIMEHeaderFast(b *bufio.Reader, maxMemory, maxHeaders int64, skipHost bool) (m MIMEHeader, haveHost bool, hostHeader string, hostCount int, handled bool, err error) {
	return readMIMEHeaderBuffered(b, maxMemory, maxHeaders, skipHost)
}

type hdrSpan struct {
	kStart uint32
	kEnd   uint32
	vStart uint32
	vEnd   uint32
}

// readMIMEHeaderBuffered parses an entire buffered MIME header block in two
// passes over r.Peek without invoking ReadLine or allocating a textproto.Reader.
// When the block is incomplete, contains obs-fold continuations, or is missing a
// colon on any line, it leaves b unconsumed and unmodified and returns handled=false.
func readMIMEHeaderBuffered(b *bufio.Reader, maxMemory, maxHeaders int64, skipHost bool) (m MIMEHeader, haveHost bool, hostHeader string, hostCount int, handled bool, err error) {
	if maxMemory < 0 {
		return nil, false, "", 0, true, errMessageTooLarge
	}
	if b.Buffered() == 0 {
		b.Peek(1)
	}
	s := b.Buffered()
	if s == 0 {
		return nil, false, "", 0, false, nil
	}
	peek, _ := b.Peek(s)
	if len(peek) == 0 || peek[0] == ' ' || peek[0] == '\t' {
		return nil, false, "", 0, false, nil
	}

	var stackSpans [32]hdrSpan
	var extraSpans []hdrSpan
	n := 0
	valBytes := 0
	pos := 0
	hasEnd := false
	for pos < len(peek) {
		nl := bytes.IndexByte(peek[pos:], '\n')
		if nl < 0 {
			return nil, false, "", 0, false, nil
		}
		lineStart := pos
		lineEnd := pos + nl
		if lineEnd > lineStart && peek[lineEnd-1] == '\r' {
			lineEnd--
		}
		pos += nl + 1
		if lineEnd == lineStart {
			hasEnd = true
			break
		}
		if peek[lineStart] == ' ' || peek[lineStart] == '\t' {
			return nil, false, "", 0, false, nil
		}
		line := peek[lineStart:lineEnd]
		colonIdx := bytes.IndexByte(line, ':')
		if colonIdx < 0 {
			return nil, false, "", 0, false, nil
		}
		if maxMemory != math.MaxInt64 && int64(pos)+int64(n+1)*200 > maxMemory {
			return nil, false, "", 0, false, nil
		}
		if int64(len(line)) > maxMemory {
			return nil, false, "", 0, false, nil
		}
		if int64(pos) > math.MaxUint32 {
			return nil, false, "", 0, false, nil
		}
		kEnd := lineStart + colonIdx
		start := kEnd + 1
		for start < lineEnd && (peek[start] == ' ' || peek[start] == '\t') {
			start++
		}
		end := lineEnd
		if start == lineEnd {
			start = kEnd + 1
			end = kEnd + 1
		} else {
			for end > start && (peek[end-1] == ' ' || peek[end-1] == '\t') {
				end--
			}
		}
		valBytes += end - start
		sp := hdrSpan{
			kStart: uint32(lineStart),
			kEnd:   uint32(kEnd),
			vStart: uint32(start),
			vEnd:   uint32(end),
		}
		if n < len(stackSpans) {
			stackSpans[n] = sp
		} else {
			extraSpans = append(extraSpans, sp)
		}
		n++
	}
	if !hasEnd {
		return nil, false, "", 0, false, nil
	}
	if n == 0 {
		b.Discard(pos)
		return make(MIMEHeader), false, "", 0, true, nil
	}

	hint := n
	if hint > 1000 {
		hint = 1000
	}
	strs := make([]string, hint)
	mapHint := hint
	if skipHost && mapHint > 1 {
		mapHint--
	}
	m = make(MIMEHeader, mapHint)

	var valBuf []byte
	remKeys := n
	remVal := valBytes
	for i := 0; i < n; i++ {
		var sp hdrSpan
		if i < len(stackSpans) {
			sp = stackSpans[i]
		} else {
			sp = extraSpans[i-len(stackSpans)]
		}
		k := peek[sp.kStart:sp.kEnd]
		v := peek[sp.vStart:sp.vEnd]

		if !validHeaderValueBytes(v) {
			cur := int(sp.vEnd) + bytes.IndexByte(peek[sp.vEnd:pos], '\n') + 1
			err := ProtocolError(fmt.Sprintf("malformed MIME header line: %q", peek[sp.kStart:sp.vEnd]))
			b.Discard(cur)
			return m, haveHost, hostHeader, hostCount, true, err
		}
		key, ok := canonicalMIMEHeaderKey(k)
		if !ok {
			cur := int(sp.vEnd) + bytes.IndexByte(peek[sp.vEnd:pos], '\n') + 1
			err := ProtocolError(fmt.Sprintf("malformed MIME header line: %q", peek[sp.kStart:sp.vEnd]))
			b.Discard(cur)
			return m, haveHost, hostHeader, hostCount, true, err
		}

		maxHeaders--
		if maxHeaders < 0 {
			cur := int(sp.vEnd) + bytes.IndexByte(peek[sp.vEnd:pos], '\n') + 1
			b.Discard(cur)
			return nil, haveHost, hostHeader, hostCount, true, errMessageTooLarge
		}

		remKeys--
		remVal -= len(v)
		value, interned := internHeaderValue(v)
		if !interned {
			if len(valBuf) < len(v) {
				if remKeys > 0 && remVal > 0 && int64(len(v)+remVal) <= maxMemory {
					allocSize := len(v) + remVal
					if allocSize > 4096 {
						allocSize = max(len(v), 4096)
					}
					valBuf = make([]byte, allocSize)
				}
			}
			if len(valBuf) >= len(v) {
				copy(valBuf, v)
				value = unsafe.String(&valBuf[0], len(v))
				valBuf = valBuf[len(v):]
			} else {
				value = string(v)
			}
		}

		if skipHost && key == "Host" {
			const mapEntryOverhead = 200
			if hostCount == 0 {
				maxMemory -= int64(len(key)) + mapEntryOverhead
				haveHost = true
				hostHeader = value
			}
			hostCount++
			maxMemory -= int64(len(value))
			if maxMemory < 0 {
				cur := int(sp.vEnd) + bytes.IndexByte(peek[sp.vEnd:pos], '\n') + 1
				b.Discard(cur)
				return m, haveHost, hostHeader, hostCount, true, errMessageTooLarge
			}
			continue
		}

		vv := m[key]
		if vv == nil {
			const mapEntryOverhead = 200
			maxMemory -= int64(len(key))
			maxMemory -= mapEntryOverhead
		}
		maxMemory -= int64(len(value))
		if maxMemory < 0 {
			cur := int(sp.vEnd) + bytes.IndexByte(peek[sp.vEnd:pos], '\n') + 1
			b.Discard(cur)
			return m, haveHost, hostHeader, hostCount, true, errMessageTooLarge
		}
		if vv == nil && len(strs) > 0 {
			vv, strs = strs[:1:1], strs[1:]
			vv[0] = value
			m[key] = vv
		} else {
			m[key] = append(vv, value)
		}
	}

	b.Discard(pos)
	return m, haveHost, hostHeader, hostCount, true, nil
}

// noValidation is a no-op validation func for readContinuedLineSlice
// that permits any lines.
func noValidation(_ []byte) error { return nil }

// mustHaveFieldNameColon ensures that, per RFC 7230, the
// field-name is on a single line, so the first line must
// contain a colon.
func mustHaveFieldNameColon(line []byte) error {
	if bytes.IndexByte(line, ':') < 0 {
		return ProtocolError(fmt.Sprintf("malformed MIME header: missing colon: %q", line))
	}
	return nil
}

var nl = []byte("\n")

// upcomingHeaderKeys returns an approximation of the number of keys
// that will be in this header. If it gets confused, it returns 0.
func (r *Reader) upcomingHeaderKeys() (n int) {
	n, _ = r.upcomingHeaderInfo()
	return n
}

// upcomingHeaderInfo returns an approximation of the number of keys
// that will be in this header and the total byte length of the trimmed header values.
func (r *Reader) upcomingHeaderInfo() (n, valBytes int) {
	// Try to determine the 'hint' size.
	r.R.Peek(1) // force a buffer load if empty
	s := r.R.Buffered()
	if s == 0 {
		return
	}
	peek, _ := r.R.Peek(s)
	for len(peek) > 0 && n < 1000 {
		var line []byte
		line, peek, _ = bytes.Cut(peek, nl)
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(line) == 0 {
			// Blank line separating headers from the body.
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			// Folded continuation of the previous line.
			valBytes += len(trim(line)) + 1
			continue
		}
		if colonIdx := bytes.IndexByte(line, ':'); colonIdx >= 0 {
			valBytes += len(trim(line[colonIdx+1:]))
		} else {
			valBytes += len(line)
		}
		n++
	}
	return n, valBytes
}

// CanonicalMIMEHeaderKey returns the canonical format of the
// MIME header key s. The canonicalization converts the first
// letter and any letter following a hyphen to upper case;
// the rest are converted to lowercase. For example, the
// canonical key for "accept-encoding" is "Accept-Encoding".
// MIME header keys are assumed to be ASCII only.
// If s contains a space or invalid header field bytes as
// defined by RFC 9112, it is returned without modifications.
func CanonicalMIMEHeaderKey(s string) string {
	switch s {
	case "Accept",
		"Accept-Charset",
		"Accept-Encoding",
		"Accept-Language",
		"Accept-Ranges",
		"Authorization",
		"Cache-Control",
		"Connection",
		"Content-Disposition",
		"Content-Encoding",
		"Content-Language",
		"Content-Length",
		"Content-Location",
		"Content-Range",
		"Content-Security-Policy",
		"Content-Type",
		"Cookie",
		"Date",
		"Etag",
		"Expect",
		"Expires",
		"Forwarded",
		"From",
		"Host",
		"If-Match",
		"If-Modified-Since",
		"If-None-Match",
		"If-Range",
		"If-Unmodified-Since",
		"Last-Modified",
		"Link",
		"Location",
		"Max-Forwards",
		"Origin",
		"Pragma",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Range",
		"Referer",
		"Retry-After",
		"Server",
		"Set-Cookie",
		"Strict-Transport-Security",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
		"User-Agent",
		"Vary",
		"Via",
		"Warning",
		"Www-Authenticate",
		"X-Content-Type-Options",
		"X-Forwarded-For",
		"X-Forwarded-Host",
		"X-Forwarded-Proto",
		"X-Frame-Options",
		"X-Powered-By",
		"X-Real-Ip",
		"X-Request-Id",
		"X-Requested-With":
		return s
	}
	return canonicalMIMEHeaderKeySlow(s)
}

func canonicalMIMEHeaderKeySlow(s string) string {
	if len(s) == 0 {
		return ""
	}
	if 'a' <= s[0] && s[0] <= 'z' {
		if v := lookupLowerCommonHeader(s); v != "" {
			return v
		}
	}
	// Quick check for canonical encoding.
	upper := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !validHeaderFieldByte(c) {
			return s
		}
		if upper && 'a' <= c && c <= 'z' {
			return canonicalMIMEHeaderKeyString(s, i)
		}
		if !upper && 'A' <= c && c <= 'Z' {
			return canonicalMIMEHeaderKeyString(s, i)
		}
		upper = c == '-'
	}
	return s
}

func canonicalMIMEHeaderKeyString(s string, firstBad int) string {
	var stack [64]byte
	var a []byte
	if len(s) <= len(stack) {
		a = stack[:len(s)]
	} else {
		a = make([]byte, len(s))
	}
	copy(a, s[:firstBad])
	upper := firstBad == 0 || s[firstBad-1] == '-'
	for i := firstBad; i < len(s); i++ {
		c := s[i]
		if !validHeaderFieldByte(c) {
			return s
		}
		if upper && 'a' <= c && c <= 'z' {
			c -= toLower
		} else if !upper && 'A' <= c && c <= 'Z' {
			c += toLower
		}
		a[i] = c
		upper = c == '-'
	}
	if v := lookupCommonHeader(a); v != "" {
		return v
	}
	commonHeaderOnce.Do(initCommonHeader)
	if v := commonHeader[string(a)]; v != "" {
		return v
	}
	return string(a)
}

func lookupLowerCommonHeader(s string) string {
	switch len(s) {
	case 2:
		switch s {
		case "cc":
			return "Cc"
		case "te":
			return "Te"
		case "to":
			return "To"
		}
	case 3:
		if s == "via" {
			return "Via"
		}
	case 4:
		switch s {
		case "date":
			return "Date"
		case "etag":
			return "Etag"
		case "from":
			return "From"
		case "host":
			return "Host"
		case "link":
			return "Link"
		case "vary":
			return "Vary"
		}
	case 5:
		if s == "range" {
			return "Range"
		}
	case 6:
		switch s {
		case "accept":
			return "Accept"
		case "cookie":
			return "Cookie"
		case "expect":
			return "Expect"
		case "origin":
			return "Origin"
		case "pragma":
			return "Pragma"
		case "server":
			return "Server"
		}
	case 7:
		switch s {
		case "expires":
			return "Expires"
		case "referer":
			return "Referer"
		case "subject":
			return "Subject"
		case "trailer":
			return "Trailer"
		case "upgrade":
			return "Upgrade"
		case "warning":
			return "Warning"
		}
	case 8:
		switch s {
		case "if-match":
			return "If-Match"
		case "if-range":
			return "If-Range"
		case "location":
			return "Location"
		case "received":
			return "Received"
		}
	case 9:
		switch s {
		case "forwarded":
			return "Forwarded"
		case "x-real-ip":
			return "X-Real-Ip"
		}
	case 10:
		switch s {
		case "connection":
			return "Connection"
		case "content-id":
			return "Content-Id"
		case "message-id":
			return "Message-Id"
		case "set-cookie":
			return "Set-Cookie"
		case "user-agent":
			return "User-Agent"
		}
	case 11:
		switch s {
		case "in-reply-to":
			return "In-Reply-To"
		case "retry-after":
			return "Retry-After"
		case "return-path":
			return "Return-Path"
		}
	case 12:
		switch s {
		case "content-type":
			return "Content-Type"
		case "max-forwards":
			return "Max-Forwards"
		case "mime-version":
			return "Mime-Version"
		case "x-imforwards":
			return "X-Imforwards"
		case "x-powered-by":
			return "X-Powered-By"
		case "x-request-id":
			return "X-Request-Id"
		}
	case 13:
		switch s {
		case "accept-ranges":
			return "Accept-Ranges"
		case "authorization":
			return "Authorization"
		case "cache-control":
			return "Cache-Control"
		case "content-range":
			return "Content-Range"
		case "if-none-match":
			return "If-None-Match"
		case "last-modified":
			return "Last-Modified"
		}
	case 14:
		switch s {
		case "accept-charset":
			return "Accept-Charset"
		case "content-length":
			return "Content-Length"
		case "dkim-signature":
			return "Dkim-Signature"
		}
	case 15:
		switch s {
		case "accept-encoding":
			return "Accept-Encoding"
		case "accept-language":
			return "Accept-Language"
		case "x-forwarded-for":
			return "X-Forwarded-For"
		case "x-frame-options":
			return "X-Frame-Options"
		}
	case 16:
		switch s {
		case "content-encoding":
			return "Content-Encoding"
		case "content-language":
			return "Content-Language"
		case "content-location":
			return "Content-Location"
		case "www-authenticate":
			return "Www-Authenticate"
		case "x-forwarded-host":
			return "X-Forwarded-Host"
		case "x-requested-with":
			return "X-Requested-With"
		}
	case 17:
		switch s {
		case "if-modified-since":
			return "If-Modified-Since"
		case "transfer-encoding":
			return "Transfer-Encoding"
		case "x-forwarded-proto":
			return "X-Forwarded-Proto"
		}
	case 18:
		if s == "proxy-authenticate" {
			return "Proxy-Authenticate"
		}
	case 19:
		switch s {
		case "content-disposition":
			return "Content-Disposition"
		case "if-unmodified-since":
			return "If-Unmodified-Since"
		case "proxy-authorization":
			return "Proxy-Authorization"
		}
	case 22:
		if s == "x-content-type-options" {
			return "X-Content-Type-Options"
		}
	case 23:
		if s == "content-security-policy" {
			return "Content-Security-Policy"
		}
	case 25:
		switch s {
		case "content-transfer-encoding":
			return "Content-Transfer-Encoding"
		case "strict-transport-security":
			return "Strict-Transport-Security"
		}
	}
	return ""
}

const toLower = 'a' - 'A'

// validHeaderFieldByte reports whether c is a valid byte in a header
// field name. RFC 7230 says:
//
//	header-field   = field-name ":" OWS field-value OWS
//	field-name     = token
//	tchar = "!" / "#" / "$" / "%" / "&" / "'" / "*" / "+" / "-" / "." /
//	        "^" / "_" / "`" / "|" / "~" / DIGIT / ALPHA
//	token = 1*tchar
func validHeaderFieldByte(c byte) bool {
	// mask is a 128-bit bitmap with 1s for allowed bytes,
	// so that the byte c can be tested with a shift and an and.
	// If c >= 128, then 1<<c and 1<<(c-64) will both be zero,
	// and this function will return false.
	const mask = 0 |
		(1<<(10)-1)<<'0' |
		(1<<(26)-1)<<'a' |
		(1<<(26)-1)<<'A' |
		1<<'!' |
		1<<'#' |
		1<<'$' |
		1<<'%' |
		1<<'&' |
		1<<'\'' |
		1<<'*' |
		1<<'+' |
		1<<'-' |
		1<<'.' |
		1<<'^' |
		1<<'_' |
		1<<'`' |
		1<<'|' |
		1<<'~'
	return ((uint64(1)<<c)&(mask&(1<<64-1)) |
		(uint64(1)<<(c-64))&(mask>>64)) != 0
}

// validHeaderValueByte reports whether c is a valid byte in a header
// field value. RFC 7230 says:
//
//	field-content  = field-vchar [ 1*( SP / HTAB ) field-vchar ]
//	field-vchar    = VCHAR / obs-text
//	obs-text       = %x80-FF
//
// RFC 5234 says:
//
//	HTAB           =  %x09
//	SP             =  %x20
//	VCHAR          =  %x21-7E
func validHeaderValueByte(c byte) bool {
	return (c >= ' ' && c != 0x7f) || c == '\t'
}

func validHeaderValueBytes(v []byte) bool {
	i := 0
	for ; i+8 <= len(v); i += 8 {
		w := byteorder.LEUint64(v[i:])
		x := w ^ 0x7f7f7f7f7f7f7f7f
		if ((w|(w-0x2020202020202020))&0x8080808080808080) != 0 ||
			(((x-0x0101010101010101)&^x)&0x8080808080808080) != 0 {
			for j := i; j < i+8; j++ {
				if !validHeaderValueByte(v[j]) {
					return false
				}
			}
		}
	}
	for ; i < len(v); i++ {
		if !validHeaderValueByte(v[i]) {
			return false
		}
	}
	return true
}

// canonicalMIMEHeaderKey is like CanonicalMIMEHeaderKey but is
// allowed to mutate the provided byte slice before returning the
// string.
//
// For invalid inputs (if a contains spaces or non-token bytes), a
// is unchanged and a string copy is returned.
//
// ok is true if the header key contains only valid characters and spaces.
// ReadMIMEHeader accepts header keys containing spaces, but does not
// canonicalize them.
func canonicalMIMEHeaderKey(a []byte) (_ string, ok bool) {
	if len(a) == 0 {
		return "", false
	}
	if 'A' <= a[0] && a[0] <= 'Z' {
		if v := lookupCommonHeader(a); v != "" {
			return v, true
		}
	}

	// See if a looks like a header key. If not, return it unchanged.
	noCanon := false
	needCanon := false
	upper := true
	for _, c := range a {
		if validHeaderFieldByte(c) {
			if upper {
				if 'a' <= c && c <= 'z' {
					needCanon = true
				}
			} else if 'A' <= c && c <= 'Z' {
				needCanon = true
			}
			upper = c == '-'
			continue
		}
		// Don't canonicalize.
		if c == ' ' {
			// We accept invalid headers with a space before the
			// colon, but must not canonicalize them.
			// See https://go.dev/issue/34540.
			noCanon = true
			continue
		}
		return string(a), false
	}
	if noCanon {
		return string(a), true
	}

	if needCanon {
		upper = true
		for i, c := range a {
			// Canonicalize: first letter upper case
			// and upper case after each dash.
			// (Host, User-Agent, If-Modified-Since).
			// MIME headers are ASCII only, so no Unicode issues.
			if upper && 'a' <= c && c <= 'z' {
				c -= toLower
			} else if !upper && 'A' <= c && c <= 'Z' {
				c += toLower
			}
			a[i] = c
			upper = c == '-' // for next time
		}
	}
	if v := lookupCommonHeader(a); v != "" {
		return v, true
	}
	commonHeaderOnce.Do(initCommonHeader)
	// The compiler recognizes m[string(byteSlice)] as a special
	// case, so a copy of a's bytes into a new string does not
	// happen in this map lookup:
	if v := commonHeader[string(a)]; v != "" {
		return v, true
	}
	return string(a), true
}

func lookupCommonHeader(a []byte) string {
	switch len(a) {
	case 2:
		switch string(a) {
		case "Cc":
			return "Cc"
		case "Te":
			return "Te"
		case "To":
			return "To"
		}
	case 3:
		if string(a) == "Via" {
			return "Via"
		}
	case 4:
		switch string(a) {
		case "Host":
			return "Host"
		case "Date":
			return "Date"
		case "Etag":
			return "Etag"
		case "From":
			return "From"
		case "Link":
			return "Link"
		case "Vary":
			return "Vary"
		}
	case 5:
		if string(a) == "Range" {
			return "Range"
		}
	case 6:
		switch string(a) {
		case "Accept":
			return "Accept"
		case "Cookie":
			return "Cookie"
		case "Expect":
			return "Expect"
		case "Origin":
			return "Origin"
		case "Pragma":
			return "Pragma"
		case "Server":
			return "Server"
		}
	case 7:
		switch string(a) {
		case "Expires":
			return "Expires"
		case "Referer":
			return "Referer"
		case "Subject":
			return "Subject"
		case "Trailer":
			return "Trailer"
		case "Upgrade":
			return "Upgrade"
		case "Warning":
			return "Warning"
		}
	case 8:
		switch string(a) {
		case "Location":
			return "Location"
		case "Received":
			return "Received"
		case "If-Match":
			return "If-Match"
		case "If-Range":
			return "If-Range"
		}
	case 9:
		switch string(a) {
		case "Forwarded":
			return "Forwarded"
		case "X-Real-Ip":
			return "X-Real-Ip"
		}
	case 10:
		switch string(a) {
		case "Connection":
			return "Connection"
		case "User-Agent":
			return "User-Agent"
		case "Set-Cookie":
			return "Set-Cookie"
		case "Content-Id":
			return "Content-Id"
		case "Message-Id":
			return "Message-Id"
		}
	case 11:
		switch string(a) {
		case "In-Reply-To":
			return "In-Reply-To"
		case "Retry-After":
			return "Retry-After"
		case "Return-Path":
			return "Return-Path"
		}
	case 12:
		switch string(a) {
		case "Content-Type":
			return "Content-Type"
		case "Max-Forwards":
			return "Max-Forwards"
		case "Mime-Version":
			return "Mime-Version"
		case "X-Imforwards":
			return "X-Imforwards"
		case "X-Powered-By":
			return "X-Powered-By"
		case "X-Request-Id":
			return "X-Request-Id"
		}
	case 13:
		switch string(a) {
		case "Accept-Ranges":
			return "Accept-Ranges"
		case "Authorization":
			return "Authorization"
		case "Cache-Control":
			return "Cache-Control"
		case "Content-Range":
			return "Content-Range"
		case "If-None-Match":
			return "If-None-Match"
		case "Last-Modified":
			return "Last-Modified"
		}
	case 14:
		switch string(a) {
		case "Content-Length":
			return "Content-Length"
		case "Accept-Charset":
			return "Accept-Charset"
		case "Dkim-Signature":
			return "Dkim-Signature"
		}
	case 15:
		switch string(a) {
		case "Accept-Encoding":
			return "Accept-Encoding"
		case "Accept-Language":
			return "Accept-Language"
		case "X-Forwarded-For":
			return "X-Forwarded-For"
		case "X-Frame-Options":
			return "X-Frame-Options"
		}
	case 16:
		switch string(a) {
		case "Content-Encoding":
			return "Content-Encoding"
		case "Content-Language":
			return "Content-Language"
		case "Content-Location":
			return "Content-Location"
		case "Www-Authenticate":
			return "Www-Authenticate"
		case "X-Forwarded-Host":
			return "X-Forwarded-Host"
		case "X-Requested-With":
			return "X-Requested-With"
		}
	case 17:
		switch string(a) {
		case "If-Modified-Since":
			return "If-Modified-Since"
		case "Transfer-Encoding":
			return "Transfer-Encoding"
		case "X-Forwarded-Proto":
			return "X-Forwarded-Proto"
		}
	case 18:
		if string(a) == "Proxy-Authenticate" {
			return "Proxy-Authenticate"
		}
	case 19:
		switch string(a) {
		case "Content-Disposition":
			return "Content-Disposition"
		case "If-Unmodified-Since":
			return "If-Unmodified-Since"
		case "Proxy-Authorization":
			return "Proxy-Authorization"
		}
	case 22:
		if string(a) == "X-Content-Type-Options" {
			return "X-Content-Type-Options"
		}
	case 23:
		if string(a) == "Content-Security-Policy" {
			return "Content-Security-Policy"
		}
	case 25:
		switch string(a) {
		case "Content-Transfer-Encoding":
			return "Content-Transfer-Encoding"
		case "Strict-Transport-Security":
			return "Strict-Transport-Security"
		}
	}
	return ""
}

func internHeaderValue(v []byte) (string, bool) {
	switch len(v) {
	case 0:
		return "", true
	case 1:
		switch v[0] {
		case '0':
			return "0", true
		case '1':
			return "1", true
		}
	case 3:
		if string(v) == "*/*" {
			return "*/*", true
		}
	case 4:
		if string(v) == "gzip" {
			return "gzip", true
		}
	case 5:
		switch string(v) {
		case "close":
			return "close", true
		case "Close":
			return "Close", true
		case "bytes":
			return "bytes", true
		}
	case 7:
		switch string(v) {
		case "chunked":
			return "chunked", true
		case "private":
			return "private", true
		case "nosniff":
			return "nosniff", true
		}
	case 8:
		switch string(v) {
		case "no-cache":
			return "no-cache", true
		case "no-store":
			return "no-store", true
		}
	case 9:
		switch string(v) {
		case "max-age=0":
			return "max-age=0", true
		case "localhost":
			return "localhost", true
		case "text/html":
			return "text/html", true
		case "127.0.0.1":
			return "127.0.0.1", true
		}
	case 10:
		switch string(v) {
		case "keep-alive":
			return "keep-alive", true
		case "Keep-Alive":
			return "Keep-Alive", true
		case "text/plain":
			return "text/plain", true
		case "golang.org":
			return "golang.org", true
		}
	case 12:
		if string(v) == "gzip,deflate" {
			return "gzip,deflate", true
		}
	case 13:
		if string(v) == "gzip, deflate" {
			return "gzip, deflate", true
		}
	case 14:
		switch string(v) {
		case "localhost:8080":
			return "localhost:8080", true
		case "en-US,en;q=0.8":
			return "en-US,en;q=0.8", true
		}
	case 16:
		if string(v) == "application/json" {
			return "application/json", true
		}
	case 17:
		switch string(v) {
		case "gzip,deflate,sdch":
			return "gzip,deflate,sdch", true
		case "gzip, deflate, br":
			return "gzip, deflate, br", true
		}
	case 18:
		switch string(v) {
		case "Go-http-client/1.1":
			return "Go-http-client/1.1", true
		case "Go-http-client/2.0":
			return "Go-http-client/2.0", true
		}
	case 24:
		if string(v) == "text/html; charset=utf-8" {
			return "text/html; charset=utf-8", true
		}
	case 25:
		if string(v) == "text/plain; charset=utf-8" {
			return "text/plain; charset=utf-8", true
		}
	case 31:
		if string(v) == "application/json; charset=utf-8" {
			return "application/json; charset=utf-8", true
		}
	}
	return "", false
}

// commonHeader interns common header strings.
var commonHeader map[string]string

var commonHeaderOnce sync.Once

func initCommonHeader() {
	commonHeader = make(map[string]string)
	for _, v := range []string{
		"Accept",
		"Accept-Charset",
		"Accept-Encoding",
		"Accept-Language",
		"Accept-Ranges",
		"Authorization",
		"Cache-Control",
		"Cc",
		"Connection",
		"Content-Disposition",
		"Content-Encoding",
		"Content-Id",
		"Content-Language",
		"Content-Length",
		"Content-Location",
		"Content-Range",
		"Content-Security-Policy",
		"Content-Transfer-Encoding",
		"Content-Type",
		"Cookie",
		"Date",
		"Dkim-Signature",
		"Etag",
		"Expect",
		"Expires",
		"Forwarded",
		"From",
		"Host",
		"If-Match",
		"If-Modified-Since",
		"If-None-Match",
		"If-Range",
		"If-Unmodified-Since",
		"In-Reply-To",
		"Last-Modified",
		"Link",
		"Location",
		"Max-Forwards",
		"Message-Id",
		"Mime-Version",
		"Origin",
		"Pragma",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Range",
		"Received",
		"Referer",
		"Retry-After",
		"Return-Path",
		"Server",
		"Set-Cookie",
		"Strict-Transport-Security",
		"Subject",
		"Te",
		"To",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
		"User-Agent",
		"Vary",
		"Via",
		"Warning",
		"Www-Authenticate",
		"X-Content-Type-Options",
		"X-Forwarded-For",
		"X-Forwarded-Host",
		"X-Forwarded-Proto",
		"X-Frame-Options",
		"X-Imforwards",
		"X-Powered-By",
		"X-Real-Ip",
		"X-Request-Id",
		"X-Requested-With",
	} {
		commonHeader[v] = v
	}
}
