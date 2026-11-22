// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.jsonv2

package json

import (
	"bytes"
	"internal/abi"
	"io"
	"runtime"
	"unsafe"

	"encoding/json/internal"
	"encoding/json/internal/jsonflags"
	"encoding/json/internal/jsonopts"
	"encoding/json/jsontext"
)

// MarshalOf is a generic version of [Marshal] that serializes in as JSON
// without boxing in into an interface value.
func MarshalOf[T any](in T, opts ...Options) (out []byte, err error) {
	var moBuf jsonopts.Struct
	mo := fastOpts(opts, &moBuf)
	if canFastMarshalFlags(mo) {
		at := abi.TypeFor[T]()
		if at.Kind() != abi.Interface {
			plan := lookupFastMarshalPlan(at)
			if plan.canFast {
				if out, ok := marshalFastAlloc(plan, noescape(unsafe.Pointer(&in)), mo); ok {
					runtime.KeepAlive(&in)
					return out, nil
				}
				runtime.KeepAlive(&in)
			}
		}
	}
	return Marshal(in, opts...)
}

// AppendMarshal serializes in as JSON and appends the result to dst.
// On error, it returns dst truncated to its original length alongside the error.
// See [Marshal] for details about the conversion of a Go value into JSON.
func AppendMarshal(dst []byte, in any, opts ...Options) ([]byte, error) {
	origLen := len(dst)
	var moBuf jsonopts.Struct
	mo := fastOpts(opts, &moBuf)
	if canFastMarshalFlags(mo) {
		at, ptr := efacePtr(&in)
		if out, ok := tryFastAppendMarshal(dst, at, ptr, mo); ok {
			runtime.KeepAlive(&in)
			return out, nil
		}
		runtime.KeepAlive(&in)
	}

	enc := export.GetAppendEncoder(dst, opts...)
	defer export.PutAppendEncoder(enc)
	xe := export.Encoder(enc)
	xe.Flags.Set(jsonflags.OmitTopLevelNewline | 1)
	err := marshalEncode(enc, in, &xe.Struct)
	if err != nil {
		if xe.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
			return dst[:origLen], internal.TransformMarshalError(in, err)
		}
		return dst[:origLen], err
	}
	return xe.Buf, nil
}

// AppendMarshalOf is a generic version of [AppendMarshal] that serializes in
// as JSON and appends the result to dst without boxing in into an interface value.
// On error, it returns dst truncated to its original length alongside the error.
func AppendMarshalOf[T any](dst []byte, in T, opts ...Options) ([]byte, error) {
	origLen := len(dst)
	var moBuf jsonopts.Struct
	mo := fastOpts(opts, &moBuf)
	if canFastMarshalFlags(mo) {
		at := abi.TypeFor[T]()
		if at.Kind() != abi.Interface {
			plan := lookupFastMarshalPlan(at)
			if plan.canFast {
				if b, ferr := plan.marshal(dst, noescape(unsafe.Pointer(&in)), mo, 0); ferr == nil {
					runtime.KeepAlive(&in)
					return b, nil
				}
				runtime.KeepAlive(&in)
				dst = dst[:origLen]
			}
		}
	}
	return AppendMarshal(dst, in, opts...)
}

// MarshalWriteOf is a generic version of [MarshalWrite] that serializes in
// into out without boxing in into an interface value.
func MarshalWriteOf[T any](out io.Writer, in T, opts ...Options) error {
	if out == nil {
		panic("nil io.Writer")
	}
	enc := export.GetStreamingEncoder(out, opts...)
	defer export.PutStreamingEncoder(enc)
	xe := export.Encoder(enc)
	xe.Flags.Set(jsonflags.OmitTopLevelNewline | 1)
	if canFastMarshalFlags(&xe.Struct) && isFastWritable(out) {
		at := abi.TypeFor[T]()
		if at.Kind() != abi.Interface {
			plan := lookupFastMarshalPlan(at)
			if plan.canFast {
				origBuf := xe.Buf
				if b, ferr := plan.marshal(origBuf, noescape(unsafe.Pointer(&in)), &xe.Struct, 0); ferr == nil {
					runtime.KeepAlive(&in)
					xe.CommitBuffer(b)
					xe.Tokens.Last.Increment()
					err := xe.Flush()
					if err != nil && xe.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
						return internal.TransformMarshalError(in, err)
					}
					return err
				}
				runtime.KeepAlive(&in)
				xe.Buf = origBuf
			}
		}
	}
	err := marshalEncode(enc, in, &xe.Struct)
	if err != nil && xe.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
		return internal.TransformMarshalError(in, err)
	}
	return err
}

// MarshalEncodeOf is a generic version of [MarshalEncode] that serializes in
// into out without boxing in into an interface value.
func MarshalEncodeOf[T any](out *jsontext.Encoder, in T, opts ...Options) error {
	if out == nil {
		panic("nil jsontext.Encoder")
	}
	if len(opts) == 0 {
		xe := export.Encoder(out)
		if xe.Tokens.Depth() == 1 && !xe.NeedFlush() && canFastMarshalFlags(&xe.Struct) {
			at := abi.TypeFor[T]()
			if at.Kind() != abi.Interface {
				plan := lookupFastMarshalPlan(at)
				if plan.canFast {
					origBuf := xe.Buf
					if b2, ferr := plan.marshal(origBuf, noescape(unsafe.Pointer(&in)), &xe.Struct, 0); ferr == nil {
						runtime.KeepAlive(&in)
						xe.CommitBuffer(b2)
						xe.Tokens.Last.Increment()
						if xe.NeedFlush() {
							return xe.Flush()
						}
						return nil
					}
					runtime.KeepAlive(&in)
					xe.Buf = origBuf
				}
			}
		}
	}
	return MarshalEncode(out, in, opts...)
}

// UnmarshalOf is a generic version of [Unmarshal] that decodes JSON data
// into out without boxing out into an interface value.
func UnmarshalOf[T any](in []byte, out *T, opts ...Options) error {
	if out == nil {
		return Unmarshal(in, out, opts...)
	}
	var uoBuf jsonopts.Struct
	uo := fastOpts(opts, &uoBuf)
	if tryFastUnmarshal(in, abi.TypeFor[T](), unsafe.Pointer(out), uo) {
		return nil
	}
	return Unmarshal(in, out, opts...)
}

// UnmarshalReadOf is a generic version of [UnmarshalRead] that decodes a JSON
// value from in into out without boxing out into an interface value.
func UnmarshalReadOf[T any](in io.Reader, out *T, opts ...Options) error {
	if in == nil {
		panic("nil io.Reader")
	}
	if out == nil {
		return UnmarshalRead(in, out, opts...)
	}
	// Fast path for *bytes.Buffer (consumed on success, matching UnmarshalRead).
	if bb, ok := in.(*bytes.Buffer); ok && bb != nil {
		data := bb.Bytes()
		var uoBuf jsonopts.Struct
		uo := fastOpts(opts, &uoBuf)
		if tryFastUnmarshal(data, abi.TypeFor[T](), unsafe.Pointer(out), uo) {
			bb.Next(len(data))
			return nil
		}
	}
	return UnmarshalRead(in, out, opts...)
}

// UnmarshalDecodeOf is a generic version of [UnmarshalDecode] that decodes the
// next JSON value from in into out without boxing out into an interface value.
func UnmarshalDecodeOf[T any](in *jsontext.Decoder, out *T, opts ...Options) error {
	if in == nil {
		panic("nil jsontext.Decoder")
	}
	return UnmarshalDecode(in, out, opts...)
}
