// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.jsonv2

package json

import (
	"cmp"
	"errors"
	"math"
	"reflect"
	"slices"
	"strconv"

	"encoding/json/internal"
	"encoding/json/internal/jsonflags"
	"encoding/json/internal/jsonopts"
	"encoding/json/internal/jsonwire"
	"encoding/json/jsontext"
)

// This file contains an optimized marshal and unmarshal implementation
// for the any type. This type is often used when the Go program has
// no knowledge of the JSON schema. This is a common enough occurrence
// to justify the complexity of adding logic for this.

// marshalValueAny marshals a Go any as a JSON value.
// This assumes that there are no special formatting directives
// for any possible nested value.
func marshalValueAny(enc *jsontext.Encoder, val any, mo *jsonopts.Struct) error {
	xe := export.Encoder(enc)
	fast := !mo.Flags.Get(jsonflags.AnyWhitespace|jsonflags.StringifyNumbers|jsonflags.TagFlags) && !xe.Tokens.Last.NeedObjectName()
	switch val := val.(type) {
	case nil:
		if fast {
			xe.CommitBuffer(append(xe.Tokens.MayAppendDelim(xe.Buf, 'n'), "null"...))
			xe.Tokens.Last.Increment()
			if xe.NeedFlush() {
				return xe.Flush()
			}
			return nil
		}
		return enc.WriteToken(jsontext.Null)
	case bool:
		if fast {
			xe.CommitBuffer(strconv.AppendBool(xe.Tokens.MayAppendDelim(xe.Buf, 't'), val))
			xe.Tokens.Last.Increment()
			if xe.NeedFlush() {
				return xe.Flush()
			}
			return nil
		}
		return enc.WriteToken(jsontext.Bool(val))
	case string:
		if fast {
			b := xe.Tokens.MayAppendDelim(xe.Buf, '"')
			if b, err := jsonwire.AppendQuoteString(b, val, &mo.Flags); err == nil {
				xe.CommitBuffer(b)
				xe.Tokens.Last.Increment()
				if xe.NeedFlush() {
					return xe.Flush()
				}
				return nil
			}
		}
		return enc.WriteToken(jsontext.String(val))
	case float64:
		if math.IsNaN(val) || math.IsInf(val, 0) {
			break // use default logic below
		}
		if fast {
			xe.CommitBuffer(jsonwire.AppendFloat(xe.Tokens.MayAppendDelim(xe.Buf, '0'), val, 64))
			xe.Tokens.Last.Increment()
			if xe.NeedFlush() {
				return xe.Flush()
			}
			return nil
		}
		return enc.WriteToken(jsontext.Float(val))
	case map[string]any:
		return marshalObjectAny(enc, val, mo)
	case []any:
		return marshalArrayAny(enc, val, mo)
	case int:
		if mo.Marshalers == nil {
			if fast {
				xe.CommitBuffer(strconv.AppendInt(xe.Tokens.MayAppendDelim(xe.Buf, '0'), int64(val), 10))
				xe.Tokens.Last.Increment()
				if xe.NeedFlush() {
					return xe.Flush()
				}
				return nil
			}
			return enc.WriteToken(jsontext.Int(int64(val)))
		}
	case int64:
		if mo.Marshalers == nil {
			if fast {
				xe.CommitBuffer(strconv.AppendInt(xe.Tokens.MayAppendDelim(xe.Buf, '0'), val, 10))
				xe.Tokens.Last.Increment()
				if xe.NeedFlush() {
					return xe.Flush()
				}
				return nil
			}
			return enc.WriteToken(jsontext.Int(val))
		}
	case uint:
		if mo.Marshalers == nil {
			if fast {
				xe.CommitBuffer(strconv.AppendUint(xe.Tokens.MayAppendDelim(xe.Buf, '0'), uint64(val), 10))
				xe.Tokens.Last.Increment()
				if xe.NeedFlush() {
					return xe.Flush()
				}
				return nil
			}
			return enc.WriteToken(jsontext.Uint(uint64(val)))
		}
	case uint64:
		if mo.Marshalers == nil {
			if fast {
				xe.CommitBuffer(strconv.AppendUint(xe.Tokens.MayAppendDelim(xe.Buf, '0'), val, 10))
				xe.Tokens.Last.Increment()
				if xe.NeedFlush() {
					return xe.Flush()
				}
				return nil
			}
			return enc.WriteToken(jsontext.Uint(val))
		}
	case float32:
		if mo.Marshalers == nil && !math.IsNaN(float64(val)) && !math.IsInf(float64(val), 0) {
			if fast {
				xe.CommitBuffer(jsonwire.AppendFloat(xe.Tokens.MayAppendDelim(xe.Buf, '0'), float64(val), 32))
				xe.Tokens.Last.Increment()
				if xe.NeedFlush() {
					return xe.Flush()
				}
				return nil
			}
			return enc.WriteToken(jsontext.Float32(val))
		}
	}

	vm := maybeAddressableValue{reflect.ValueOf(val), true} // values retrieved from an interface are not addressable
	marshal := lookupMarshaler(vm.Type()).fnc
	if mo.Marshalers != nil {
		marshal, _ = mo.Marshalers.(*Marshalers).lookup(marshal, vm.Type())
	}
	return marshal(enc, vm, mo)
}

// unmarshalValueAny unmarshals a JSON value as a Go any.
// This assumes that there are no special formatting directives
// for any possible nested value.
// Duplicate names must be rejected since this does not implement merging.
func unmarshalValueAny(dec *jsontext.Decoder, uo *jsonopts.Struct) (any, error) {
	switch k := dec.PeekKind(); k {
	case '{':
		return unmarshalObjectAny(dec, uo)
	case '[':
		return unmarshalArrayAny(dec, uo)
	default:
		xd := export.Decoder(dec)
		var flags jsonwire.ValueFlags
		val, err := xd.ReadValue(&flags)
		if err != nil {
			return nil, err
		}
		switch val.Kind() {
		case 'n':
			return nil, nil
		case 'f':
			return false, nil
		case 't':
			return true, nil
		case '"':
			val = jsonwire.UnquoteMayCopy(val, flags.IsVerbatim())
			if xd.StringCache == nil {
				xd.StringCache = new(stringCache)
			}
			return makeString(xd.StringCache, val), nil
		case '0':
			if uo.Flags.Get(jsonflags.UnmarshalAnyWithRawNumber) {
				return internal.RawNumberOf(val), nil
			}
			fv, err := strconv.ParseFloat(string(val), 64)
			if err != nil {
				return 0.0, newUnmarshalErrorAfterWithValue(dec, float64Type, errors.Unwrap(err))
			}
			return fv, nil
		default:
			panic("BUG: invalid kind: " + k.String())
		}
	}
}

// marshalObjectAny marshals a Go map[string]any as a JSON object
// (or as a JSON null if nil and [jsonflags.FormatNilMapAsNull]).
func marshalObjectAny(enc *jsontext.Encoder, obj map[string]any, mo *jsonopts.Struct) error {
	// Check for cycles.
	xe := export.Encoder(enc)
	if xe.Tokens.Depth() > startDetectingCyclesAfter {
		v := reflect.ValueOf(obj)
		if err := visitPointer(&xe.SeenPointers, v, mo); err != nil {
			return newMarshalErrorBefore(enc, mapStringAnyType, err)
		}
		defer leavePointer(&xe.SeenPointers, v)
	}

	// Handle empty maps.
	if len(obj) == 0 {
		if mo.Flags.Get(jsonflags.FormatNilMapAsNull) && obj == nil {
			return enc.WriteToken(jsontext.Null)
		}
		// Optimize for marshaling an empty map without any preceding whitespace.
		if !mo.Flags.Get(jsonflags.AnyWhitespace) && !xe.Tokens.Last.NeedObjectName() {
			xe.Buf = append(xe.Tokens.MayAppendDelim(xe.Buf, '{'), "{}"...)
			xe.Tokens.Last.Increment()
			if xe.NeedFlush() {
				return xe.Flush()
			}
			return nil
		}
	}

	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	// A Go map guarantees that each entry has a unique key.
	// The only possibility of duplicates is due to invalid UTF-8.
	if !mo.Flags.Get(jsonflags.AllowInvalidUTF8) {
		xe.Tokens.Last.DisableNamespace()
	}
	if !mo.Flags.Get(jsonflags.Deterministic) || len(obj) <= 1 {
		for name, val := range obj {
			if err := enc.WriteToken(jsontext.String(name)); err != nil {
				return err
			}
			if err := marshalValueAny(enc, val, mo); err != nil {
				return err
			}
		}
	} else {
		names := getStrings(len(obj))
		var i int
		for name := range obj {
			(*names)[i] = name
			i++
		}
		slices.Sort(*names)
		for _, name := range *names {
			if err := enc.WriteToken(jsontext.String(name)); err != nil {
				return err
			}
			if err := marshalValueAny(enc, obj[name], mo); err != nil {
				return err
			}
		}
		putStrings(names)
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return err
	}
	return nil
}

// unmarshalObjectAny unmarshals a JSON object as a Go map[string]any.
// It panics if not decoding a JSON object.
func unmarshalObjectAny(dec *jsontext.Decoder, uo *jsonopts.Struct) (map[string]any, error) {
	switch tok, err := dec.ReadToken(); {
	case err != nil:
		return nil, err
	case tok.Kind() != '{':
		panic("BUG: invalid kind: " + tok.Kind().String())
	}
	xd := export.Decoder(dec)
	obj := make(map[string]any)
	// A Go map guarantees that each entry has a unique key.
	// The only possibility of duplicates is due to invalid UTF-8.
	if !uo.Flags.Get(jsonflags.AllowInvalidUTF8) {
		xd.Tokens.Last.DisableNamespace()
	}
	var errUnmarshal error
	for dec.PeekKind() != '}' {
		var flags jsonwire.ValueFlags
		rawName, err := xd.ReadValue(&flags)
		if err != nil {
			return obj, err
		}
		nameBytes := jsonwire.UnquoteMayCopy(rawName, flags.IsVerbatim())
		if xd.StringCache == nil {
			xd.StringCache = new(stringCache)
		}
		name := makeString(xd.StringCache, nameBytes)

		// Manually check for duplicate names.
		if _, ok := obj[name]; ok {
			// TODO: Unread the object name.
			err := newDuplicateNameError(dec.StackPointer(), nil, dec.InputOffset()-len64(rawName))
			return obj, err
		}

		val, err := unmarshalValueAny(dec, uo)
		obj[name] = val
		if err != nil {
			if isFatalError(err, uo.Flags) {
				return obj, err
			}
			errUnmarshal = cmp.Or(errUnmarshal, err)
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return obj, err
	}
	return obj, errUnmarshal
}

// marshalArrayAny marshals a Go []any as a JSON array
// (or as a JSON null if nil and [jsonflags.FormatNilSliceAsNull]).
func marshalArrayAny(enc *jsontext.Encoder, arr []any, mo *jsonopts.Struct) error {
	// Check for cycles.
	xe := export.Encoder(enc)
	if xe.Tokens.Depth() > startDetectingCyclesAfter {
		v := reflect.ValueOf(arr)
		if err := visitPointer(&xe.SeenPointers, v, mo); err != nil {
			return newMarshalErrorBefore(enc, sliceAnyType, err)
		}
		defer leavePointer(&xe.SeenPointers, v)
	}

	// Handle empty slices.
	if len(arr) == 0 {
		if mo.Flags.Get(jsonflags.FormatNilSliceAsNull) && arr == nil {
			return enc.WriteToken(jsontext.Null)
		}
		// Optimize for marshaling an empty slice without any preceding whitespace.
		if !mo.Flags.Get(jsonflags.AnyWhitespace) && !xe.Tokens.Last.NeedObjectName() {
			xe.Buf = append(xe.Tokens.MayAppendDelim(xe.Buf, '['), "[]"...)
			xe.Tokens.Last.Increment()
			if xe.NeedFlush() {
				return xe.Flush()
			}
			return nil
		}
	}

	if err := enc.WriteToken(jsontext.BeginArray); err != nil {
		return err
	}
	for _, val := range arr {
		if err := marshalValueAny(enc, val, mo); err != nil {
			return err
		}
	}
	if err := enc.WriteToken(jsontext.EndArray); err != nil {
		return err
	}
	return nil
}

// unmarshalArrayAny unmarshals a JSON array as a Go []any.
// It panics if not decoding a JSON array.
func unmarshalArrayAny(dec *jsontext.Decoder, uo *jsonopts.Struct) ([]any, error) {
	switch tok, err := dec.ReadToken(); {
	case err != nil:
		return nil, err
	case tok.Kind() != '[':
		panic("BUG: invalid kind: " + tok.Kind().String())
	}
	arr := []any{}
	var errUnmarshal error
	for dec.PeekKind() != ']' {
		val, err := unmarshalValueAny(dec, uo)
		arr = append(arr, val)
		if err != nil {
			if isFatalError(err, uo.Flags) {
				return arr, err
			}
			errUnmarshal = cmp.Or(errUnmarshal, err)
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return arr, err
	}
	return arr, errUnmarshal
}
