// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bufio"
	"bytes"
	"io"
	"net/http/httptrace"
	"net/http/internal"
	"net/textproto"
	"slices"
	"strings"
	"sync/v2"
	"time"

	"golang.org/x/net/http/httpguts"
)

// A Header represents the key-value pairs in an HTTP header.
//
// The keys should be in canonical form, as returned by
// [CanonicalHeaderKey].
type Header map[string][]string

// Add adds the key, value pair to the header.
// It appends to any existing values associated with key.
// The key is case insensitive; it is canonicalized by
// [CanonicalHeaderKey].
func (h Header) Add(key, value string) {
	textproto.MIMEHeader(h).Add(key, value)
}

// Set sets the header entries associated with key to the
// single element value. It replaces any existing values
// associated with key. The key is case insensitive; it is
// canonicalized by [textproto.CanonicalMIMEHeaderKey].
// To use non-canonical keys, assign to the map directly.
func (h Header) Set(key, value string) {
	textproto.MIMEHeader(h).Set(key, value)
}

// Get gets the first value associated with the given key. If
// there are no values associated with the key, Get returns "".
// It is case insensitive; [textproto.CanonicalMIMEHeaderKey] is
// used to canonicalize the provided key. Get assumes that all
// keys are stored in canonical form. To use non-canonical keys,
// access the map directly.
func (h Header) Get(key string) string {
	return textproto.MIMEHeader(h).Get(key)
}

// Values returns all values associated with the given key.
// It is case insensitive; [textproto.CanonicalMIMEHeaderKey] is
// used to canonicalize the provided key. To use non-canonical
// keys, access the map directly.
// The returned slice is not a copy.
func (h Header) Values(key string) []string {
	return textproto.MIMEHeader(h).Values(key)
}

// get is like Get, but key must already be in CanonicalHeaderKey form.
func (h Header) get(key string) string {
	if v := h[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// has reports whether h has the provided key defined, even if it's
// set to 0-length slice.
func (h Header) has(key string) bool {
	_, ok := h[key]
	return ok
}

// Del deletes the values associated with key.
// The key is case insensitive; it is canonicalized by
// [CanonicalHeaderKey].
func (h Header) Del(key string) {
	textproto.MIMEHeader(h).Del(key)
}

// Write writes a header in wire format.
func (h Header) Write(w io.Writer) error {
	return h.write(w, nil)
}

func (h Header) write(w io.Writer, trace *httptrace.ClientTrace) error {
	return h.writeSubset(w, nil, trace)
}

// Clone returns a copy of h or nil if h is nil.
func (h Header) Clone() Header {
	if h == nil {
		return nil
	}
	if len(h) == 0 {
		return make(Header)
	}

	// Find total number of values.
	nv := 0
	for _, vv := range h {
		nv += len(vv)
	}
	var sv []string
	if nv > 0 {
		sv = make([]string, nv) // shared backing array for headers' values
	}
	h2 := make(Header, len(h))
	for k, vv := range h {
		if vv == nil {
			// Preserve nil values. ReverseProxy distinguishes
			// between nil and zero-length header values.
			h2[k] = nil
			continue
		}
		n := copy(sv, vv)
		h2[k] = sv[:n:n]
		sv = sv[n:]
	}
	return h2
}

var timeFormats = []string{
	TimeFormat,
	time.RFC850,
	time.ANSIC,
}

// ParseTime parses a time header (such as the Date: header),
// trying each of the three formats allowed by HTTP/1.1:
// [TimeFormat], [time.RFC850], and [time.ANSIC].
func ParseTime(text string) (t time.Time, err error) {
	if t, ok := internal.ParseHTTPDate(text); ok {
		return t, nil
	}
	for _, layout := range timeFormats {
		t, err = time.Parse(layout, text)
		if err == nil {
			return
		}
	}
	return
}

var headerNewlineToSpace = strings.NewReplacer("\n", " ", "\r", " ")

// stringWriter implements WriteString on a Writer.
type stringWriter struct {
	w io.Writer
}

func (w stringWriter) WriteString(s string) (n int, err error) {
	return w.w.Write([]byte(s))
}

type keyValues struct {
	key    string
	values []string
}

// headerSorter contains a slice of keyValues sorted by keyValues.key.
type headerSorter struct {
	kvs []keyValues
}

var headerSorterPool = sync.Pool[*headerSorter]{
	New: func() *headerSorter { return new(headerSorter) },
}

// sortedKeyValues returns h's keys sorted in the returned kvs
// slice. The headerSorter used to sort is also returned, for possible
// return to headerSorterCache.
func (h Header) sortedKeyValues(exclude map[string]bool) (kvs []keyValues, hs *headerSorter) {
	hs = headerSorterPool.Get()
	if cap(hs.kvs) < len(h) {
		hs.kvs = make([]keyValues, 0, len(h))
	}
	kvs = hs.kvs[:0]
	hasExclude := len(exclude) > 0
	for k, vv := range h {
		if !hasExclude || !exclude[k] {
			kvs = append(kvs, keyValues{k, vv})
		}
	}
	hs.kvs = kvs
	if len(kvs) > 1 {
		slices.SortFunc(hs.kvs, func(a, b keyValues) int { return strings.Compare(a.key, b.key) })
	}
	return kvs, hs
}

// WriteSubset writes a header in wire format.
// If exclude is not nil, keys where exclude[key] == true are not written.
// Keys are not canonicalized before checking the exclude map.
func (h Header) WriteSubset(w io.Writer, exclude map[string]bool) error {
	return h.writeSubset(w, exclude, nil)
}

func formatHeaderValue(v string) string {
	if strings.IndexByte(v, '\r') >= 0 || strings.IndexByte(v, '\n') >= 0 {
		v = headerNewlineToSpace.Replace(v)
	}
	return textproto.TrimString(v)
}

func (h Header) writeSubset(w io.Writer, exclude map[string]bool, trace *httptrace.ClientTrace) error {
	if len(h) == 0 {
		return nil
	}
	var stackKVs [16]keyValues
	var kvs []keyValues
	var sorter *headerSorter
	if len(h) <= len(stackKVs) {
		kvs = stackKVs[:0]
		hasExclude := len(exclude) > 0
		for k, vv := range h {
			if !hasExclude || !exclude[k] {
				kvs = append(kvs, keyValues{k, vv})
			}
		}
		for i := 1; i < len(kvs); i++ {
			cur := kvs[i]
			j := i - 1
			for j >= 0 && kvs[j].key > cur.key {
				kvs[j+1] = kvs[j]
				j--
			}
			kvs[j+1] = cur
		}
	} else {
		kvs, sorter = h.sortedKeyValues(exclude)
	}

	if trace == nil || trace.WroteHeaderField == nil {
		switch cw := w.(type) {
		case *bufio.Writer:
			buf := cw.AvailableBuffer()
			avail := cap(buf)
			for _, kv := range kvs {
				if !httpguts.ValidHeaderFieldName(kv.key) {
					continue
				}
				for _, v := range kv.values {
					v = formatHeaderValue(v)
					need := len(kv.key) + len(v) + 4
					if len(buf)+need <= avail {
						buf = append(buf, kv.key...)
						buf = append(buf, ':', ' ')
						buf = append(buf, v...)
						buf = append(buf, '\r', '\n')
						continue
					}
					if len(buf) > 0 {
						if _, err := cw.Write(buf); err != nil {
							if sorter != nil {
								headerSorterPool.Put(sorter)
							}
							return err
						}
						buf = cw.AvailableBuffer()
						avail = cap(buf)
						if need <= avail {
							buf = append(buf, kv.key...)
							buf = append(buf, ':', ' ')
							buf = append(buf, v...)
							buf = append(buf, '\r', '\n')
							continue
						}
					}
					if err := writeHeaderLine(cw, kv.key, v); err != nil {
						if sorter != nil {
							headerSorterPool.Put(sorter)
						}
						return err
					}
					buf = cw.AvailableBuffer()
					avail = cap(buf)
				}
			}
			if len(buf) > 0 {
				if _, err := cw.Write(buf); err != nil {
					if sorter != nil {
						headerSorterPool.Put(sorter)
					}
					return err
				}
			}
			if sorter != nil {
				headerSorterPool.Put(sorter)
			}
			return nil
		case *bytes.Buffer:
			var scratch [512]byte
			off := 0
			for _, kv := range kvs {
				if !httpguts.ValidHeaderFieldName(kv.key) {
					continue
				}
				for _, v := range kv.values {
					v = formatHeaderValue(v)
					need := len(kv.key) + len(v) + 4
					if off+need <= len(scratch) {
						off += copy(scratch[off:], kv.key)
						scratch[off] = ':'
						scratch[off+1] = ' '
						off += 2
						off += copy(scratch[off:], v)
						scratch[off] = '\r'
						scratch[off+1] = '\n'
						off += 2
						continue
					}
					if off > 0 {
						cw.Write(scratch[:off])
						off = 0
						if need <= len(scratch) {
							off += copy(scratch[off:], kv.key)
							scratch[off] = ':'
							scratch[off+1] = ' '
							off += 2
							off += copy(scratch[off:], v)
							scratch[off] = '\r'
							scratch[off+1] = '\n'
							off += 2
							continue
						}
					}
					writeHeaderLine(cw, kv.key, v)
				}
			}
			if off > 0 {
				cw.Write(scratch[:off])
			}
			if sorter != nil {
				headerSorterPool.Put(sorter)
			}
			return nil
		case *strings.Builder:
			var scratch [512]byte
			off := 0
			for _, kv := range kvs {
				if !httpguts.ValidHeaderFieldName(kv.key) {
					continue
				}
				for _, v := range kv.values {
					v = formatHeaderValue(v)
					need := len(kv.key) + len(v) + 4
					if off+need <= len(scratch) {
						off += copy(scratch[off:], kv.key)
						scratch[off] = ':'
						scratch[off+1] = ' '
						off += 2
						off += copy(scratch[off:], v)
						scratch[off] = '\r'
						scratch[off+1] = '\n'
						off += 2
						continue
					}
					if off > 0 {
						cw.Write(scratch[:off])
						off = 0
						if need <= len(scratch) {
							off += copy(scratch[off:], kv.key)
							scratch[off] = ':'
							scratch[off+1] = ' '
							off += 2
							off += copy(scratch[off:], v)
							scratch[off] = '\r'
							scratch[off+1] = '\n'
							off += 2
							continue
						}
					}
					writeHeaderLine(cw, kv.key, v)
				}
			}
			if off > 0 {
				cw.Write(scratch[:off])
			}
			if sorter != nil {
				headerSorterPool.Put(sorter)
			}
			return nil
		}
	}

	ws, ok := w.(io.StringWriter)
	if !ok {
		ws = stringWriter{w}
	}
	var formattedVals []string
	for _, kv := range kvs {
		if !httpguts.ValidHeaderFieldName(kv.key) {
			// This could be an error. In the common case of
			// writing response headers, however, we have no good
			// way to provide the error back to the server
			// handler, so just drop invalid headers instead.
			continue
		}
		for _, v := range kv.values {
			v = formatHeaderValue(v)
			if err := writeHeaderLine(ws, kv.key, v); err != nil {
				if sorter != nil {
					headerSorterPool.Put(sorter)
				}
				return err
			}
			if trace != nil && trace.WroteHeaderField != nil {
				formattedVals = append(formattedVals, v)
			}
		}
		if trace != nil && trace.WroteHeaderField != nil {
			trace.WroteHeaderField(kv.key, formattedVals)
			formattedVals = nil
		}
	}
	if sorter != nil {
		headerSorterPool.Put(sorter)
	}
	return nil
}

// writeHeaderLine writes "key: value\r\n" to ws.
func writeHeaderLine(ws io.StringWriter, key, value string) error {
	if _, err := ws.WriteString(key); err != nil {
		return err
	}
	if _, err := ws.WriteString(": "); err != nil {
		return err
	}
	if _, err := ws.WriteString(value); err != nil {
		return err
	}
	_, err := ws.WriteString("\r\n")
	return err
}

// CanonicalHeaderKey returns the canonical format of the
// header key s. The canonicalization converts the first
// letter and any letter following a hyphen to upper case;
// the rest are converted to lowercase. For example, the
// canonical key for "accept-encoding" is "Accept-Encoding".
// If s contains a space or invalid header field bytes, it is
// returned without modifications.
func CanonicalHeaderKey(s string) string { return textproto.CanonicalMIMEHeaderKey(s) }
