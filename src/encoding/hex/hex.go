// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package hex implements hexadecimal encoding and decoding.
package hex

import (
	"errors"
	"fmt"
	"internal/bytealg"
	"internal/byteorder"
	"io"
	"slices"
	"unsafe"
)

const (
	hextable        = "0123456789abcdef"
	reverseHexTable = "" +
		"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\xff\xff\xff\xff\xff\xff" +
		"\xff\x0a\x0b\x0c\x0d\x0e\x0f\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\xff\x0a\x0b\x0c\x0d\x0e\x0f\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff"
)

var (
	hexEncodeLUT16 = func() (lut [256]uint16) {
		for i := range 256 {
			lut[i] = uint16(hextable[i>>4]) | uint16(hextable[i&0x0f])<<8
		}
		return lut
	}()

	dumpCharTable = func() (tab [256]byte) {
		for i := range 256 {
			if i >= 32 && i <= 126 {
				tab[i] = byte(i)
			} else {
				tab[i] = '.'
			}
		}
		return tab
	}()

	hexDecLUT4 = func() (lut [4][256]uint32) {
		for j := range 4 {
			for i := range 256 {
				lut[j][i] = 0x80000000
			}
		}
		for i := range 256 {
			v := reverseHexTable[i]
			if v <= 0x0f {
				u := uint32(v)
				lut[0][i] = u << 4
				lut[1][i] = u
				lut[2][i] = u << 12
				lut[3][i] = u << 8
			}
		}
		return lut
	}()
)

// EncodedLen returns the length of an encoding of n source bytes.
// Specifically, it returns n * 2.
func EncodedLen(n int) int { return n * 2 }

// Encode encodes src into [EncodedLen](len(src))
// bytes of dst. As a convenience, it returns the number
// of bytes written to dst, but this value is always [EncodedLen](len(src)).
// Encode implements hexadecimal encoding.
func Encode(dst, src []byte) int {
	srcLen := len(src)
	if srcLen < 4 {
		if srcLen == 0 {
			return 0
		}
		dstLen := srcLen * 2
		_ = dst[dstLen-1]
		if srcLen >= 2 {
			v := uint32(hexEncodeLUT16[src[0]]) | uint32(hexEncodeLUT16[src[1]])<<16
			byteorder.LEPutUint32(dst, v)
			if srcLen == 3 {
				byteorder.LEPutUint16(dst[4:], hexEncodeLUT16[src[2]])
			}
			return dstLen
		}
		byteorder.LEPutUint16(dst, hexEncodeLUT16[src[0]])
		return 2
	}
	dstLen := srcLen * 2
	_ = dst[dstLen-1] // Early bounds check; guarantees safety of writes below.

	src, dst = src[:srcLen:srcLen], dst[:dstLen:dstLen]
	if srcLen >= 16 && haveSIMD && srcLen >= minSIMDEncode {
		si, di := encodeSIMD(dst, src)
		src = src[si:]
		dst = dst[di:]
	}

	for len(src) >= 8 {
		_ = dst[15]
		v0 := uint64(hexEncodeLUT16[src[0]]) |
			uint64(hexEncodeLUT16[src[1]])<<16 |
			uint64(hexEncodeLUT16[src[2]])<<32 |
			uint64(hexEncodeLUT16[src[3]])<<48
		v1 := uint64(hexEncodeLUT16[src[4]]) |
			uint64(hexEncodeLUT16[src[5]])<<16 |
			uint64(hexEncodeLUT16[src[6]])<<32 |
			uint64(hexEncodeLUT16[src[7]])<<48
		byteorder.LEPutUint64(dst, v0)
		byteorder.LEPutUint64(dst[8:], v1)
		src = src[8:]
		dst = dst[16:]
	}
	if len(src) >= 4 {
		_ = dst[7]
		v := uint64(hexEncodeLUT16[src[0]]) |
			uint64(hexEncodeLUT16[src[1]])<<16 |
			uint64(hexEncodeLUT16[src[2]])<<32 |
			uint64(hexEncodeLUT16[src[3]])<<48
		byteorder.LEPutUint64(dst, v)
		src = src[4:]
		dst = dst[8:]
	}
	if len(src) >= 2 {
		_ = dst[3]
		v := uint32(hexEncodeLUT16[src[0]]) | uint32(hexEncodeLUT16[src[1]])<<16
		byteorder.LEPutUint32(dst, v)
		src = src[2:]
		dst = dst[4:]
	}
	if len(src) == 1 {
		byteorder.LEPutUint16(dst, hexEncodeLUT16[src[0]])
	}

	return dstLen
}

// AppendEncode appends the hexadecimally encoded src to dst
// and returns the extended buffer.
func AppendEncode(dst, src []byte) []byte {
	n := EncodedLen(len(src))
	dst = slices.Grow(dst, n)
	Encode(dst[len(dst):][:n], src)
	return dst[:len(dst)+n]
}

// ErrLength reports an attempt to decode an odd-length input
// using [Decode] or [DecodeString].
// The stream-based Decoder returns [io.ErrUnexpectedEOF] instead of ErrLength.
var ErrLength = errors.New("encoding/hex: odd length hex string")

// InvalidByteError values describe errors resulting from an invalid byte in a hex string.
type InvalidByteError byte

func (e InvalidByteError) Error() string {
	return fmt.Sprintf("encoding/hex: invalid byte: %#U", rune(e))
}

// DecodedLen returns the length of a decoding of x source bytes.
// Specifically, it returns x / 2.
func DecodedLen(x int) int { return x / 2 }

// Decode decodes src into [DecodedLen](len(src)) bytes,
// returning the actual number of bytes written to dst.
//
// Decode expects that src contains only hexadecimal
// characters and that src has even length.
// If the input is malformed, Decode returns the number
// of bytes decoded before the error.
func Decode(dst, src []byte) (int, error) {
	origDstLen := len(dst)
	if len(src) >= 16 && haveSIMD && len(src) >= minSIMDDecode && len(dst) >= minSIMDDecodeOut {
		si, di := decodeSIMD(dst, src)
		if si == len(src) {
			return di, nil
		}
		src = src[si:]
		dst = dst[di:]
	}

	t0, t1, t2, t3 := &hexDecLUT4[0], &hexDecLUT4[1], &hexDecLUT4[2], &hexDecLUT4[3]
	// The len(dst) >= 4 condition ensures that a too-short dst falls through
	// to the scalar loop below, which validates src before touching dst, so
	// malformed input is reported as an error rather than a bounds panic.
	for len(src) >= 8 && len(dst) >= 4 {
		_ = dst[3] // Eliminate bounds checks.
		v0 := t0[src[0]] | t1[src[1]] | t2[src[2]] | t3[src[3]]
		v1 := t0[src[4]] | t1[src[5]] | t2[src[6]] | t3[src[7]]
		if (v0|v1)&0x80000000 != 0 {
			break
		}
		byteorder.LEPutUint32(dst, v0|(v1<<16))
		src = src[8:]
		dst = dst[4:]
	}

	// Process the rest.
	var p, q, a, b byte
	i, j := 0, 0
	for ; j < len(src)-1; j += 2 {
		p = src[j]
		q = src[j+1]
		a = reverseHexTable[p]
		b = reverseHexTable[q]
		if a > 0x0f {
			return origDstLen - len(dst) + i, InvalidByteError(p)
		}
		if b > 0x0f {
			return origDstLen - len(dst) + i, InvalidByteError(q)
		}
		dst[i] = (a << 4) | b
		i++
	}
	if j == len(src)-1 { // true if and only if len(src)%2 == 1
		// Check for invalid char before reporting bad length,
		// since the invalid char (if present) is an earlier problem.
		p = src[j]
		if reverseHexTable[p] > 0x0f {
			return origDstLen - len(dst) + i, InvalidByteError(p)
		}
		return origDstLen - len(dst) + i, ErrLength
	}

	return origDstLen - len(dst) + i, nil
}

// AppendDecode appends the hexadecimally decoded src to dst
// and returns the extended buffer.
// If the input is malformed, it returns the partially decoded src and an error.
func AppendDecode(dst, src []byte) ([]byte, error) {
	n := DecodedLen(len(src))
	dst = slices.Grow(dst, n)
	n, err := Decode(dst[len(dst):][:n], src)
	return dst[:len(dst)+n], err
}

// EncodeToString returns the hexadecimal encoding of src.
func EncodeToString(src []byte) string {
	if len(src) == 0 {
		return ""
	}
	n := EncodedLen(len(src))
	b := bytealg.MakeNoZero(n)
	Encode(b, src)
	return unsafe.String(unsafe.SliceData(b), n)
}

// DecodeString returns the bytes represented by the hexadecimal string s.
//
// DecodeString expects that src contains only hexadecimal
// characters and that src has even length.
// If the input is malformed, DecodeString returns
// the bytes decoded before the error.
func DecodeString(s string) ([]byte, error) {
	dst := make([]byte, DecodedLen(len(s)))
	n, err := Decode(dst, unsafe.Slice(unsafe.StringData(s), len(s)))
	return dst[:n], err
}

const spaces50 = "                                                  "

func formatDumpLine(line *[79]byte, offset uint32, chunk []byte) int {
	o := uint64(hexEncodeLUT16[byte(offset>>24)]) |
		uint64(hexEncodeLUT16[byte(offset>>16)])<<16 |
		uint64(hexEncodeLUT16[byte(offset>>8)])<<32 |
		uint64(hexEncodeLUT16[byte(offset)])<<48
	byteorder.LEPutUint64(line[0:8], o)
	byteorder.LEPutUint16(line[8:10], 0x2020)

	if len(chunk) == 16 {
		_ = chunk[15]
		byteorder.LEPutUint64(line[10:18],
			uint64(hexEncodeLUT16[chunk[0]])|
				uint64(' ')<<16|
				uint64(hexEncodeLUT16[chunk[1]])<<24|
				uint64(' ')<<40|
				uint64(hexEncodeLUT16[chunk[2]])<<48)
		byteorder.LEPutUint32(line[18:22],
			uint32(' ')|uint32(hexEncodeLUT16[chunk[3]])<<8|uint32(' ')<<24)
		byteorder.LEPutUint64(line[22:30],
			uint64(hexEncodeLUT16[chunk[4]])|
				uint64(' ')<<16|
				uint64(hexEncodeLUT16[chunk[5]])<<24|
				uint64(' ')<<40|
				uint64(hexEncodeLUT16[chunk[6]])<<48)
		byteorder.LEPutUint32(line[30:34],
			uint32(' ')|uint32(hexEncodeLUT16[chunk[7]])<<8|uint32(' ')<<24)
		line[34] = ' '

		byteorder.LEPutUint64(line[35:43],
			uint64(hexEncodeLUT16[chunk[8]])|
				uint64(' ')<<16|
				uint64(hexEncodeLUT16[chunk[9]])<<24|
				uint64(' ')<<40|
				uint64(hexEncodeLUT16[chunk[10]])<<48)
		byteorder.LEPutUint32(line[43:47],
			uint32(' ')|uint32(hexEncodeLUT16[chunk[11]])<<8|uint32(' ')<<24)
		byteorder.LEPutUint64(line[47:55],
			uint64(hexEncodeLUT16[chunk[12]])|
				uint64(' ')<<16|
				uint64(hexEncodeLUT16[chunk[13]])<<24|
				uint64(' ')<<40|
				uint64(hexEncodeLUT16[chunk[14]])<<48)
		byteorder.LEPutUint32(line[55:59],
			uint32(' ')|uint32(hexEncodeLUT16[chunk[15]])<<8|uint32(' ')<<24)

		byteorder.LEPutUint16(line[59:61], 0x7c20)
		for i := range 16 {
			line[61+i] = dumpCharTable[chunk[i]]
		}
		byteorder.LEPutUint16(line[77:79], 0x0a7c)
		return 79
	}

	pos := 10
	for i := range len(chunk) {
		byteorder.LEPutUint16(line[pos:pos+2], hexEncodeLUT16[chunk[i]])
		line[pos+2] = ' '
		pos += 3
		if i == 7 {
			line[pos] = ' '
			pos++
		}
	}
	const hexAreaEnd = 60 // 10 + 8*3 + 1 + 8*3 + 1
	if pos < hexAreaEnd {
		copy(line[pos:hexAreaEnd], spaces50)
	}
	line[hexAreaEnd] = '|'
	pos = hexAreaEnd + 1
	for _, b := range chunk {
		line[pos] = dumpCharTable[b]
		pos++
	}
	line[pos] = '|'
	line[pos+1] = '\n'
	return pos + 2
}

func fillDump(out, data []byte) {
	pos := 0
	var offset uint32
	if len(data) >= 32 && haveSIMD {
		if lines := dumpLinesSIMD(out, data, 0); lines > 0 {
			pos = lines * 79
			offset = uint32(lines * 16)
			data = data[lines*16:]
		}
	}
	for len(data) >= 16 {
		formatDumpLine((*[79]byte)(out[pos:pos+79]), offset, data[:16])
		pos += 79
		offset += 16
		data = data[16:]
	}
	if len(data) > 0 {
		var tail [79]byte
		n := formatDumpLine(&tail, offset, data)
		copy(out[pos:], tail[:n])
	}
}

// Dump returns a string that contains a hex dump of the given data. The format
// of the hex dump matches the output of `hexdump -C` on the command line.
func Dump(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	const maxDumpLines = (int(^uint(0)>>1) - 79) / 79
	fullLines := len(data) / 16
	if fullLines > maxDumpLines {
		panic("encoding/hex: dump output length overflow")
	}
	rem := len(data) % 16
	totalLen := fullLines * 79
	if rem > 0 {
		totalLen += 63 + rem
	}

	out := bytealg.MakeNoZero(totalLen)
	fillDump(out, data)
	return unsafe.String(unsafe.SliceData(out), totalLen)
}

// bufferSize is the number of hexadecimal characters to buffer in encoder and decoder.
const bufferSize = 1024

type encoder struct {
	w   io.Writer
	err error
	out [bufferSize]byte // output buffer
}

// NewEncoder returns an [io.Writer] that writes lowercase hexadecimal characters to w.
func NewEncoder(w io.Writer) io.Writer {
	return &encoder{w: w}
}

func (e *encoder) Write(p []byte) (n int, err error) {
	for len(p) > 0 && e.err == nil {
		chunkSize := bufferSize / 2
		if len(p) < chunkSize {
			chunkSize = len(p)
		}

		var written int
		encoded := Encode(e.out[:], p[:chunkSize])
		written, e.err = e.w.Write(e.out[:encoded])
		n += written / 2
		p = p[chunkSize:]
	}
	return n, e.err
}

type decoder struct {
	r   io.Reader
	err error
	in  []byte           // input buffer (encoded form)
	arr [bufferSize]byte // backing array for in
}

// NewDecoder returns an [io.Reader] that decodes hexadecimal characters from r.
// NewDecoder expects that r contain only an even number of hexadecimal characters.
func NewDecoder(r io.Reader) io.Reader {
	return &decoder{r: r}
}

func (d *decoder) Read(p []byte) (n int, err error) {
	// Fast path for large reads when internal buffer holds at most 1 leftover byte:
	// read encoded input directly into p and decode in place.
	if len(p) >= bufferSize && len(d.in) < 2 && d.err == nil {
		numCopy := copy(p, d.in)
		d.in = nil
		var numRead int
		numRead, d.err = d.r.Read(p[numCopy:])
		total := numCopy + numRead
		if total >= 2 {
			even := total &^ 1
			hasOdd := total&1 != 0
			var oddByte byte
			if hasOdd {
				oddByte = p[total-1]
				if d.err != io.EOF {
					d.arr[0] = oddByte
					d.in = d.arr[:1]
				}
			}
			numDec, decErr := Decode(p, p[:even])
			if decErr != nil {
				d.in, d.err = nil, decErr
				return numDec, d.err
			}
			if hasOdd && d.err == io.EOF {
				if reverseHexTable[oddByte] > 0x0f {
					d.err = InvalidByteError(oddByte)
				} else {
					d.err = io.ErrUnexpectedEOF
				}
			}
			return numDec, d.err
		}
		if total == 1 {
			d.arr[0] = p[0]
			d.in = d.arr[:1]
			if d.err == io.EOF {
				if reverseHexTable[d.arr[0]] > 0x0f {
					d.err = InvalidByteError(d.arr[0])
				} else {
					d.err = io.ErrUnexpectedEOF
				}
			}
		}
		return 0, d.err
	}

	// Fill internal buffer with sufficient bytes to decode
	if len(d.in) < 2 && d.err == nil {
		var numCopy, numRead int
		numCopy = copy(d.arr[:], d.in) // Copies either 0 or 1 bytes
		numRead, d.err = d.r.Read(d.arr[numCopy:])
		d.in = d.arr[:numCopy+numRead]
		if d.err == io.EOF && len(d.in)%2 != 0 {
			if a := reverseHexTable[d.in[len(d.in)-1]]; a > 0x0f {
				d.err = InvalidByteError(d.in[len(d.in)-1])
			} else {
				d.err = io.ErrUnexpectedEOF
			}
		}
	}

	// Decode internal buffer into output buffer
	if numAvail := len(d.in) / 2; len(p) > numAvail {
		p = p[:numAvail]
	}
	numDec, err := Decode(p, d.in[:len(p)*2])
	d.in = d.in[2*numDec:]
	if err != nil {
		d.in, d.err = nil, err // Decode error; discard input remainder
	}

	if len(d.in) < 2 {
		return numDec, d.err // Only expose errors when buffer fully consumed
	}
	return numDec, nil
}

// Dumper returns a [io.WriteCloser] that writes a hex dump of all written data to
// w. The format of the dump matches the output of `hexdump -C` on the command
// line.
func Dumper(w io.Writer) io.WriteCloser {
	return &dumper{w: w}
}

const dumperBatchLines = 16

type dumper struct {
	w          io.Writer
	rightChars [18]byte
	batch      [dumperBatchLines * 79]byte
	used       int  // number of bytes in the current line
	n          uint // number of bytes, total
	closed     bool
}

func toChar(b byte) byte {
	return dumpCharTable[b]
}

func (h *dumper) Write(data []byte) (n int, err error) {
	if h.closed {
		return 0, errors.New("encoding/hex: dumper closed")
	}

	// Output lines look like:
	// 00000010  2e 2f 30 31 32 33 34 35  36 37 38 39 3a 3b 3c 3d  |./0123456789:;<=|
	// ^ offset                          ^ extra space              ^ ASCII of line.
	i := 0
	for i < len(data) {
		if h.used == 0 && len(data)-i >= 16 {
			lines := min((len(data)-i)/16, dumperBatchLines)
			done := 0
			if lines >= 2 && haveSIMD {
				done = dumpLinesSIMD(h.batch[:lines*79], data[i:i+lines*16], uint32(h.n))
			}
			for j := done; j < lines; j++ {
				off := j * 79
				srcOff := i + j*16
				formatDumpLine((*[79]byte)(h.batch[off:off+79]), uint32(h.n)+uint32(j*16), data[srcOff:srcOff+16])
			}
			_, err = h.w.Write(h.batch[:lines*79])
			if err != nil {
				return n, err
			}
			consumed := lines * 16
			n += consumed
			h.n += uint(consumed)
			i += consumed
			continue
		}

		take := min(16-h.used, len(data)-i)
		pos := 0
		if h.used == 0 {
			byteorder.LEPutUint64(h.batch[0:8],
				uint64(hexEncodeLUT16[byte(h.n>>24)])|
					uint64(hexEncodeLUT16[byte(h.n>>16)])<<16|
					uint64(hexEncodeLUT16[byte(h.n>>8)])<<32|
					uint64(hexEncodeLUT16[byte(h.n)])<<48)
			byteorder.LEPutUint16(h.batch[8:10], 0x2020)
			pos = 10
		}
		for k := range take {
			b := data[i+k]
			u := h.used + k
			byteorder.LEPutUint16(h.batch[pos:pos+2], hexEncodeLUT16[b])
			h.batch[pos+2] = ' '
			pos += 3
			if u == 7 {
				h.batch[pos] = ' '
				pos++
			} else if u == 15 {
				h.batch[pos] = ' '
				h.batch[pos+1] = '|'
				pos += 2
			}
			h.rightChars[u] = dumpCharTable[b]
		}
		if h.used+take == 16 {
			h.rightChars[16] = '|'
			h.rightChars[17] = '\n'
			copy(h.batch[pos:pos+18], h.rightChars[:])
			pos += 18
		}
		_, err = h.w.Write(h.batch[:pos])
		if err != nil {
			return n, err
		}
		n += take
		h.n += uint(take)
		h.used += take
		i += take
		if h.used == 16 {
			h.used = 0
		}
	}
	return
}

func (h *dumper) Close() (err error) {
	// See the comments in Write() for the details of this format.
	if h.closed {
		return
	}
	h.closed = true
	if h.used == 0 {
		return
	}
	nBytes := h.used
	h.used = 16
	spaces := (16-nBytes)*3 + 1
	if nBytes <= 7 {
		spaces++
	}
	copy(h.batch[:spaces], spaces50)
	h.batch[spaces] = '|'
	copy(h.batch[spaces+1:], h.rightChars[:nBytes])
	h.batch[spaces+1+nBytes] = '|'
	h.batch[spaces+2+nBytes] = '\n'
	_, err = h.w.Write(h.batch[:spaces+3+nBytes])
	return
}
