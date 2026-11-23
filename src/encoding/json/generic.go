// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.jsonv2

package json

import (
	"internal/abi"
	"reflect"
	"runtime"
	"unsafe"
)

// MarshalOf is a generic version of [Marshal] that returns the JSON encoding of v
// without boxing v into an interface value.
func MarshalOf[T any](v T) ([]byte, error) {
	at := abi.TypeFor[T]()
	if at.Kind() != abi.Interface {
		plan := lookupV1FastMarshalPlan(at)
		if plan.canFast && !plan.hasCustom {
			out, err := v1MarshalFastAlloc(plan, v1Noescape(unsafe.Pointer(&v)), true)
			runtime.KeepAlive(v)
			if err == nil {
				return out, nil
			}
			if err != errV1FastFallback {
				return nil, err
			}
		}
	}
	return marshalOfSlow(v)
}

//go:noinline
func marshalOfSlow[T any](v T) ([]byte, error) {
	return Marshal(v)
}

// AppendMarshal appends the JSON encoding of v to dst and returns the extended buffer.
// On error, it returns dst truncated to its original length alongside the error.
func AppendMarshal(dst []byte, v any) ([]byte, error) {
	origLen := len(dst)
	at, ptr := v1EfacePtr(&v)
	if at == nil {
		return append(dst, "null"...), nil
	}
	plan := lookupV1FastMarshalPlan(at)
	if plan.canFast {
		out, err := plan.marshal(dst, ptr, true, 0)
		runtime.KeepAlive(v)
		if err == nil {
			return out, nil
		}
		dst = dst[:origLen]
		if err != errV1FastFallback {
			return dst, err
		}
	}

	e := newEncodeState()
	defer encodeStatePool.Put(e)
	if err := e.marshal(v, encOpts{escapeHTML: true}); err != nil {
		return dst[:origLen], err
	}
	return append(dst, e.Bytes()...), nil
}

// AppendMarshalOf is a generic version of [AppendMarshal] that appends the JSON
// encoding of v to dst without boxing v into an interface value.
// On error, it returns dst truncated to its original length alongside the error.
func AppendMarshalOf[T any](dst []byte, v T) ([]byte, error) {
	origLen := len(dst)
	at := abi.TypeFor[T]()
	if at.Kind() != abi.Interface {
		plan := lookupV1FastMarshalPlan(at)
		if plan.canFast && !plan.hasCustom {
			out, err := plan.marshal(dst, v1Noescape(unsafe.Pointer(&v)), true, 0)
			runtime.KeepAlive(v)
			if err == nil {
				return out, nil
			}
			dst = dst[:origLen]
			if err != errV1FastFallback {
				return dst, err
			}
		}
	}
	return appendMarshalOfSlow(dst, v)
}

//go:noinline
func appendMarshalOfSlow[T any](dst []byte, v T) ([]byte, error) {
	return AppendMarshal(dst, v)
}

// MarshalIndentOf is a generic version of [MarshalIndent] without boxing v
// into an interface value.
func MarshalIndentOf[T any](v T, prefix, indent string) ([]byte, error) {
	fb := getV1FastBuf()
	b, err := AppendMarshalOf(fb.b, v)
	if err != nil {
		fb.b = b[:0]
		putV1FastBuf(fb)
		return nil, err
	}
	fb.b = b
	b2 := make([]byte, 0, indentGrowthFactor*len(b))
	b2, err = appendIndent(b2, b, prefix, indent)
	putV1FastBuf(fb)
	if err != nil {
		return nil, err
	}
	return b2, nil
}

// AppendMarshalIndent is like [MarshalIndent] but appends the formatted JSON
// encoding of v to dst. On error, it returns dst truncated to its original
// length alongside the error.
func AppendMarshalIndent(dst []byte, v any, prefix, indent string) ([]byte, error) {
	origLen := len(dst)
	fb := getV1FastBuf()
	b, err := AppendMarshal(fb.b, v)
	if err != nil {
		fb.b = b[:0]
		putV1FastBuf(fb)
		return dst[:origLen], err
	}
	fb.b = b
	out, err := appendIndent(dst, b, prefix, indent)
	putV1FastBuf(fb)
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
	fb := getV1FastBuf()
	b, err := AppendMarshalOf(fb.b, v)
	if err != nil {
		fb.b = b[:0]
		putV1FastBuf(fb)
		return dst[:origLen], err
	}
	fb.b = b
	out, err := appendIndent(dst, b, prefix, indent)
	putV1FastBuf(fb)
	if err != nil {
		return dst[:origLen], err
	}
	return out, nil
}

// UnmarshalOf is a generic version of [Unmarshal] that parses JSON-encoded data
// and stores the result in v without boxing v into an interface value.
func UnmarshalOf[T any](data []byte, v *T) error {
	if v == nil {
		if !v1CheckValidFast(data) {
			var d decodeState
			if err := checkValid(data, &d.scan); err != nil {
				return err
			}
		}
		return &InvalidUnmarshalError{reflect.TypeFor[*T]()}
	}
	if handled, err := tryV1FastUnmarshal(data, abi.TypeFor[T](), unsafe.Pointer(v), false, false, false); handled {
		return err
	}
	return Unmarshal(data, v)
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
	at := abi.TypeFor[T]()
	if at.Kind() != abi.Interface {
		plan := lookupV1FastMarshalPlan(at)
		if plan.canFast && !plan.hasCustom {
			fb := getV1FastBuf()
			b, err := plan.marshal(fb.b, v1Noescape(unsafe.Pointer(&v)), enc.escapeHTML, 0)
			runtime.KeepAlive(v)
			if err == nil {
				b = append(b, '\n')
				fb.b = b
				out := b
				if enc.indentPrefix != "" || enc.indentValue != "" {
					var ierr error
					enc.indentBuf, ierr = appendIndent(enc.indentBuf[:0], b, enc.indentPrefix, enc.indentValue)
					if ierr != nil {
						putV1FastBuf(fb)
						return ierr
					}
					out = enc.indentBuf
				}
				_, werr := enc.w.Write(out)
				putV1FastBuf(fb)
				if werr != nil {
					enc.err = werr
				}
				return werr
			}
			putV1FastBuf(fb)
			if err != errV1FastFallback {
				return err
			}
		}
	}
	return encodeOfSlow(enc, v)
}

//go:noinline
func encodeOfSlow[T any](enc *Encoder, v T) error {
	return enc.Encode(v)
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
	if err := dec.tokenPrepareForDecode(); err != nil {
		return err
	}
	if !dec.tokenValueAllowed() {
		return &SyntaxError{msg: "not at beginning of value", Offset: dec.InputOffset()}
	}
	n, err := dec.readValue()
	if err != nil {
		return err
	}
	raw := dec.buf[dec.scanp : dec.scanp+n]
	dec.scanp += n

	if v != nil {
		if handled, err := tryV1FastUnmarshal(raw, abi.TypeFor[T](), unsafe.Pointer(v), true, dec.d.useNumber, dec.d.disallowUnknownFields); handled {
			dec.tokenValueEnd()
			return err
		}
	}

	dec.d.init(raw)
	err = dec.d.unmarshal(v)
	dec.tokenValueEnd()
	return err
}
