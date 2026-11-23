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
			if plan.canFast && !plan.hasCustom {
				out, ferr := marshalFastAlloc(plan, noescape(unsafe.Pointer(&in)), mo)
				runtime.KeepAlive(in)
				if ferr == nil {
					return out, nil
				}
				return marshalOfSlowErr(in, out, at, ferr, mo, opts)
			}
		}
	}
	return marshalOfSlow(in, opts)
}

//go:noinline
func marshalOfSlowErr[T any](in T, out []byte, at *abi.Type, ferr error, mo *jsonopts.Struct, opts []Options) ([]byte, error) {
	if ferr != errFastFallback {
		return out, materializeFastErr(ferr, in, at, 0, mo)
	}
	return Marshal(in, opts...)
}

//go:noinline
func marshalOfSlow[T any](in T, opts []Options) ([]byte, error) {
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
		e := (*eface)(noescape(unsafe.Pointer(&in)))
		at := e.typ
		if at == nil {
			return append(dst, "null"...), nil
		}
		plan := lookupFastMarshalPlan(at)
		if plan.canFast {
			var out []byte
			var ferr error
			if !plan.hasCustom {
				var ptr unsafe.Pointer
				if at.IsDirectIface() {
					ptr = noescape(unsafe.Pointer(&e.data))
				} else {
					ptr = noescape(e.data)
				}
				out, ferr = plan.marshal(dst, ptr, mo, 0)
				runtime.KeepAlive(in)
			} else if at.Kind() == abi.Pointer && plan.kind == fkPtr {
				if e.data == nil {
					return append(dst, "null"...), nil
				}
				out, ferr = plan.elemPlan.marshal(dst, e.data, mo, 0)
			} else {
				out, ferr = marshalFastEface(plan, dst, in, mo)
			}
			if ferr == nil {
				return out, nil
			}
			if ferr != errFastFallback {
				return dst[:origLen], materializeFastErr(ferr, in, at, origLen, mo)
			}
			dst = dst[:origLen]
		}
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
			if plan.canFast && !plan.hasCustom {
				b, ferr := plan.marshal(dst, noescape(unsafe.Pointer(&in)), mo, 0)
				runtime.KeepAlive(in)
				if ferr == nil {
					return b, nil
				}
				return appendMarshalOfSlowErr(dst[:origLen], in, at, origLen, ferr, mo, opts)
			}
		}
	}
	return appendMarshalOfSlow(dst[:origLen], in, opts)
}

//go:noinline
func appendMarshalOfSlowErr[T any](dst []byte, in T, at *abi.Type, origLen int, ferr error, mo *jsonopts.Struct, opts []Options) ([]byte, error) {
	if ferr != errFastFallback {
		return dst, materializeFastErr(ferr, in, at, origLen, mo)
	}
	return AppendMarshal(dst, in, opts...)
}

//go:noinline
func appendMarshalOfSlow[T any](dst []byte, in T, opts []Options) ([]byte, error) {
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
				var ptr unsafe.Pointer
				if plan.hasCustom {
					ptr = unsafe.Pointer(&in)
				} else {
					ptr = noescape(unsafe.Pointer(&in))
				}
				b, ferr := plan.marshal(origBuf, ptr, &xe.Struct, 0)
				runtime.KeepAlive(&in)
				if ferr == nil {
					xe.CommitBuffer(b)
					xe.Tokens.Last.Increment()
					err := xe.Flush()
					if err != nil && xe.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
						return internal.TransformMarshalError(in, err)
					}
					return err
				}
				if ferr != errFastFallback {
					if !xe.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
						if p := fastPartialOutput(b, ferr); len(p) > len(origBuf) {
							xe.CommitBuffer(p)
							_ = xe.Flush()
						}
					}
					if se, ok := ferr.(*fastSemanticError); !ok || se.partialBuf == nil {
						xe.Buf = origBuf
					}
					return materializeFastErr(ferr, in, at, len(origBuf), &xe.Struct)
				}
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
		if xe.Tokens.Depth() == 1 && xe.Tokens.Last.Length() == 0 && !xe.NeedFlush() && canFastMarshalFlags(&xe.Struct) {
			at := abi.TypeFor[T]()
			if at.Kind() != abi.Interface {
				plan := lookupFastMarshalPlan(at)
				if plan.canFast && !plan.hasCustom {
					origBuf := xe.Buf
					b2, ferr := plan.marshal(origBuf, noescape(unsafe.Pointer(&in)), &xe.Struct, 0)
					runtime.KeepAlive(in)
					if ferr == nil {
						xe.CommitBuffer(b2)
						xe.Tokens.Last.Increment()
						if xe.NeedFlush() {
							return xe.Flush()
						}
						return nil
					}
					return marshalEncodeOfSlowErr(out, origBuf, b2, in, at, ferr, opts)
				}
			}
		}
	}
	return marshalEncodeOfSlow(out, in, opts)
}

//go:noinline
func marshalEncodeOfSlowErr[T any](out *jsontext.Encoder, origBuf, b2 []byte, in T, at *abi.Type, ferr error, opts []Options) error {
	xe := export.Encoder(out)
	if ferr != errFastFallback {
		if !xe.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
			if p := fastPartialOutput(b2, ferr); len(p) > len(origBuf) {
				xe.CommitBuffer(p)
			} else {
				xe.Buf = origBuf
			}
		} else {
			xe.Buf = origBuf
		}
		return materializeFastErr(ferr, in, at, len(origBuf), &xe.Struct)
	}
	xe.Buf = origBuf
	return MarshalEncode(out, in, opts...)
}

//go:noinline
func marshalEncodeOfSlow[T any](out *jsontext.Encoder, in T, opts []Options) error {
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
	if handled, ferr := tryFastUnmarshal(in, abi.TypeFor[T](), unsafe.Pointer(out), uo); handled {
		return ferr
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
		if handled, ferr := tryFastUnmarshal(data, abi.TypeFor[T](), unsafe.Pointer(out), uo); handled {
			bb.Next(len(data))
			return ferr
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
	if out != nil && len(opts) == 0 {
		xd := export.Decoder(in)
		if handled, ferr := tryFastUnmarshalDecode(in, abi.TypeFor[T](), unsafe.Pointer(out), &xd.Struct, false); handled {
			return ferr
		}
	}
	return UnmarshalDecode(in, out, opts...)
}
