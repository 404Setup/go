// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http3

import (
	"net/http/internal/httpcommon"
	"sync"
)

var qpackBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 256)
		return &b
	},
}

type qpackEncoder struct {
	// The encoder has no state for now,
	// but that'll change once we add dynamic table support.
	//
	// TODO: dynamic table support.
}

func (qe *qpackEncoder) init() {
	staticTableOnce.Do(initStaticTableMaps)
}

// lookupStaticFast checks the most common HTTP/3 request and response header
// names/values without map lookups. Returns (index, matchedValue, matchedName).
func lookupStaticFast(itype indexType, name, value string) (idx int, matchVal, matchName bool) {
	switch name {
	case ":status":
		if itype == mayIndex {
			switch value {
			case "200":
				return 25, true, true
			case "204":
				return 64, true, true
			case "304":
				return 26, true, true
			case "404":
				return 27, true, true
			case "500":
				return 71, true, true
			case "100":
				return 63, true, true
			case "103":
				return 24, true, true
			case "206":
				return 65, true, true
			case "302":
				return 66, true, true
			case "400":
				return 67, true, true
			case "403":
				return 68, true, true
			case "421":
				return 69, true, true
			case "425":
				return 70, true, true
			case "503":
				return 28, true, true
			}
		}
		return 24, false, true
	case ":method":
		if itype == mayIndex {
			switch value {
			case "GET":
				return 17, true, true
			case "POST":
				return 20, true, true
			case "HEAD":
				return 18, true, true
			case "PUT":
				return 21, true, true
			case "DELETE":
				return 16, true, true
			case "OPTIONS":
				return 19, true, true
			case "CONNECT":
				return 15, true, true
			}
		}
		return 15, false, true
	case ":scheme":
		if itype == mayIndex {
			switch value {
			case "https":
				return 23, true, true
			case "http":
				return 22, true, true
			}
		}
		return 22, false, true
	case ":path":
		if itype == mayIndex && value == "/" {
			return 1, true, true
		}
		return 1, false, true
	case ":authority":
		if itype == mayIndex && value == "" {
			return 0, true, true
		}
		return 0, false, true
	case "date":
		if itype == mayIndex && value == "" {
			return 6, true, true
		}
		return 6, false, true
	case "content-length":
		if itype == mayIndex && value == "0" {
			return 4, true, true
		}
		return 4, false, true
	case "content-type":
		if itype == mayIndex {
			switch value {
			case "text/plain; charset=utf-8":
				return 44, false, true
			case "application/dns-message":
				return 44, true, true
			case "application/javascript":
				return 45, true, true
			case "application/json":
				return 46, true, true
			case "application/x-www-form-urlencoded":
				return 47, true, true
			case "image/gif":
				return 48, true, true
			case "image/jpeg":
				return 49, true, true
			case "image/png":
				return 50, true, true
			case "text/css":
				return 51, true, true
			case "text/html; charset=utf-8":
				return 52, true, true
			case "text/plain":
				return 53, true, true
			case "text/plain;charset=utf-8":
				return 54, true, true
			}
		}
		return 44, false, true
	case "user-agent":
		if itype == mayIndex && value == "" {
			return 95, true, true
		}
		return 95, false, true
	case "accept-encoding":
		if itype == mayIndex && value == "gzip, deflate, br" {
			return 31, true, true
		}
		return 31, false, true
	}
	return 0, false, false
}

func (qe *qpackEncoder) appendEncoded(b []byte, headers func(func(itype indexType, name, value string))) []byte {
	// Encoded Field Section prefix.
	//
	// We don't yet use the dynamic table, so both values here are zero.
	b = append(b, 0, 0) // Required Insert Count = 0, Delta Base = 0

	headers(func(itype indexType, name, value string) {
		// Technically, it is the responsibility of the protocol using HTTP/3
		// to ensure that all field names are already in lowercase. However,
		// this QPACK implementation is solely used by and live in the http3
		// package. So, we might as well do the lowercasing here to make sure
		// we do not miss any callsites or need to create yet another struct
		// wrapping the qpackEncoder.
		name, ascii := httpcommon.LowerHeader(name)
		// Skip writing invalid headers. Per RFC 9114 section 4.2: "Field
		// names are strings containing a subset of ASCII characters."
		if !ascii {
			return
		}
		if idx, matchVal, matchName := lookupStaticFast(itype, name, value); matchName {
			if matchVal {
				b = appendIndexedFieldLine(b, staticTable, idx)
			} else {
				b = appendLiteralFieldLineWithNameReference(b, staticTable, itype, idx, value)
			}
			return
		}
		if itype == mayIndex {
			if i, ok := staticTableByNameValue[tableEntry{name, value}]; ok {
				b = appendIndexedFieldLine(b, staticTable, i)
				return
			}
		}
		if i, ok := staticTableByName[name]; ok {
			b = appendLiteralFieldLineWithNameReference(b, staticTable, itype, i, value)
		} else {
			b = appendLiteralFieldLineWithLiteralName(b, itype, name, value)
		}
	})

	return b
}

// encode encodes a list of headers into a QPACK encoded field section.
//
// The headers func must produce the same headers on repeated calls,
// although the order may vary.
func (qe *qpackEncoder) encode(headers func(func(itype indexType, name, value string))) []byte {
	return qe.appendEncoded(make([]byte, 0, 128), headers)
}

// writeHeadersFrame encodes headers using a pooled buffer and writes the
// complete HTTP/3 HEADERS frame (frame type, length, and QPACK payload)
// in a single stream Write call.
func (qe *qpackEncoder) writeHeadersFrame(st *stream, headers func(func(itype indexType, name, value string))) {
	_ = qe.writeHeadersFrameErr(st, func(yield func(itype indexType, name, value string)) error {
		headers(yield)
		return nil
	})
}

// writeHeadersFrameErr is like writeHeadersFrame but aborts without writing to
// st if headers returns a non-nil error.
func (qe *qpackEncoder) writeHeadersFrameErr(st *stream, headers func(func(itype indexType, name, value string)) error) error {
	const headerReserve = 9 // 1 byte frameTypeHeaders + up to 8 bytes varint length
	bp := qpackBufPool.Get().(*[]byte)
	buf := *bp
	if cap(buf) < headerReserve+128 {
		buf = make([]byte, headerReserve, 256)
	} else {
		buf = buf[:headerReserve]
	}
	var err error
	buf = qe.appendEncoded(buf, func(yield func(itype indexType, name, value string)) {
		err = headers(yield)
	})
	if err == nil {
		payloadLen := len(buf) - headerReserve
		hdrLen := 1 + sizeVarint(uint64(payloadLen))
		start := headerReserve - hdrLen
		hdr := appendVarint(buf[start:start], uint64(frameTypeHeaders))
		_ = appendVarint(hdr, uint64(payloadLen))
		st.Write(buf[start:])
	}
	if cap(buf) <= 16*1024 {
		*bp = buf[:0]
		qpackBufPool.Put(bp)
	}
	return err
}
