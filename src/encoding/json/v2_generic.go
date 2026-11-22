// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.jsonv2

package json

import (
	"io"
	"sync/v2"

	jsonv2 "encoding/json/v2"
)

var v2IndentBufPool = sync.Pool[*[]byte]{
	New: func() *[]byte {
		b := make([]byte, 0, 256)
		return &b
	},
}

func getIndentScratch() *[]byte {
	bp := v2IndentBufPool.Get()
	*bp = (*bp)[:0]
	return bp
}

func putIndentScratch(bp *[]byte) {
	if cap(*bp) > 64<<10 {
		*bp = nil
	}
	v2IndentBufPool.Put(bp)
}

// MarshalOf is a generic version of [Marshal] that returns the JSON encoding of v
// without boxing v into an interface value.
func MarshalOf[T any](v T) ([]byte, error) {
	return jsonv2.MarshalOf(v, DefaultOptionsV1())
}

// AppendMarshal appends the JSON encoding of v to dst and returns the extended buffer.
// On error, it returns dst truncated to its original length alongside the error.
func AppendMarshal(dst []byte, v any) ([]byte, error) {
	return jsonv2.AppendMarshal(dst, v, DefaultOptionsV1())
}

// AppendMarshalOf is a generic version of [AppendMarshal] that appends the JSON
// encoding of v to dst without boxing v into an interface value.
// On error, it returns dst truncated to its original length alongside the error.
func AppendMarshalOf[T any](dst []byte, v T) ([]byte, error) {
	return jsonv2.AppendMarshalOf(dst, v, DefaultOptionsV1())
}

// MarshalIndentOf is a generic version of [MarshalIndent] without boxing v
// into an interface value.
func MarshalIndentOf[T any](v T, prefix, indent string) ([]byte, error) {
	bp := getIndentScratch()
	b, err := AppendMarshalOf(*bp, v)
	if err != nil {
		*bp = b[:0]
		putIndentScratch(bp)
		return nil, err
	}
	*bp = b
	out, err := appendIndent(nil, b, prefix, indent)
	putIndentScratch(bp)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AppendMarshalIndent is like [MarshalIndent] but appends the formatted JSON
// encoding of v to dst. On error, it returns dst truncated to its original
// length alongside the error.
func AppendMarshalIndent(dst []byte, v any, prefix, indent string) ([]byte, error) {
	origLen := len(dst)
	bp := getIndentScratch()
	b, err := AppendMarshal(*bp, v)
	if err != nil {
		*bp = b[:0]
		putIndentScratch(bp)
		return dst[:origLen], err
	}
	*bp = b
	out, err := appendIndent(dst, b, prefix, indent)
	putIndentScratch(bp)
	if err != nil {
		return dst[:origLen], err
	}
	return out, nil
}

// AppendMarshalIndentOf is a generic version of [AppendMarshalIndent] without
// boxing v into an interface value. On error, it returns dst truncated to its
// original length alongside the error.
func AppendMarshalIndentOf[T any](dst []byte, v T, prefix, indent string) ([]byte, error) {
	origLen := len(dst)
	bp := getIndentScratch()
	b, err := AppendMarshalOf(*bp, v)
	if err != nil {
		*bp = b[:0]
		putIndentScratch(bp)
		return dst[:origLen], err
	}
	*bp = b
	out, err := appendIndent(dst, b, prefix, indent)
	putIndentScratch(bp)
	if err != nil {
		return dst[:origLen], err
	}
	return out, nil
}

// UnmarshalOf is a generic version of [Unmarshal] that parses JSON-encoded data
// and stores the result in v without boxing v into an interface value.
func UnmarshalOf[T any](data []byte, v *T) error {
	if v == nil {
		return Unmarshal(data, v)
	}
	return jsonv2.UnmarshalOf(data, v, DefaultOptionsV1())
}

// EncodeOf is a generic version of [Encoder.Encode] that writes the JSON
// encoding of v to enc without boxing v into an interface value.
func EncodeOf[T any](enc *Encoder, v T) error {
	if enc == nil {
		panic("json: nil Encoder")
	}
	if enc.err != nil {
		return enc.err
	}

	e := export.GetBufferedEncoder(enc.opts)
	defer export.PutBufferedEncoder(e)
	if err := jsonv2.MarshalEncodeOf(e, v); err != nil {
		return err
	}
	b := export.Encoder(e).Buf // b must not leak current scope
	if len(enc.indentPrefix)+len(enc.indentValue) > 0 {
		enc.indentBuf.Reset()
		if err := Indent(&enc.indentBuf, b, enc.indentPrefix, enc.indentValue); err != nil {
			return err
		}
		b = enc.indentBuf.Bytes()
	}
	b = append(b, '\n')

	if _, err := enc.w.Write(b); err != nil {
		enc.err = err
		return err
	}
	return nil
}

// DecodeOf is a generic version of [Decoder.Decode] that reads the next
// JSON-encoded value from dec and stores it in v without boxing v into an
// interface value.
func DecodeOf[T any](dec *Decoder, v *T) error {
	if dec == nil {
		panic("json: nil Decoder")
	}
	if dec.err != nil {
		return dec.err
	}
	b, err := dec.dec.ReadValue()
	if err != nil {
		dec.err = transformSyntacticError(err)
		if dec.err.Error() == errUnexpectedEnd.Error() {
			dec.err = io.ErrUnexpectedEOF
		}
		return dec.err
	}
	dec.hadPeeked = false
	dec.hadEOF = false
	return jsonv2.UnmarshalOf(b, v, dec.opts)
}
