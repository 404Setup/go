// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.jsonv2

package json

import (
	"bytes"
	"encoding/base64"
	"errors"
	"internal/abi"
	"internal/byteorder"
	"internal/runtime/maps"
	"internal/unsafeheader"
	"math"
	"math/bits"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/v2"
	"time"
	"unicode/utf8"
	"unsafe"

	"encoding/json/internal/jsonflags"
	"encoding/json/internal/jsonopts"
	"encoding/json/internal/jsonwire"
)

var errFastFallback = errors.New("fast path fallback")

type eface struct {
	typ  *abi.Type
	data unsafe.Pointer
}

//go:nosplit
//go:noinline
func noescape(p unsafe.Pointer) unsafe.Pointer {
	x := uintptr(p)
	return unsafe.Pointer(x ^ 0)
}

func efacePtr(a *any) (*abi.Type, unsafe.Pointer) {
	e := (*eface)(unsafe.Pointer(a))
	if e.typ == nil {
		return nil, nil
	}
	if e.typ.IsDirectIface() {
		return e.typ, unsafe.Pointer(&e.data)
	}
	return e.typ, e.data
}

// fastBuf holds a pooled output buffer with utilization tracking matching jsontext.
type fastBuf struct {
	b       []byte
	strikes int
	prevLen int
}

var fastBufPool = sync.Pool[*fastBuf]{
	New: func() *fastBuf { return new(fastBuf) },
}

func getFastBuf() *fastBuf {
	fb := fastBufPool.Get()
	if fb.b == nil {
		n := 1 << bits.Len(uint(fb.prevLen|63))
		fb.b = make([]byte, 0, n)
	} else {
		fb.b = fb.b[:0]
	}
	return fb
}

func putFastBuf(fb *fastBuf) {
	switch {
	case cap(fb.b) <= 4<<10:
		fb.strikes = 0
	case cap(fb.b)/4 <= len(fb.b):
		fb.strikes = 0
	case fb.strikes < 4:
		fb.strikes++
	default:
		fb.strikes = 0
		fb.prevLen = len(fb.b)
		fb.b = nil
	}
	fastBufPool.Put(fb)
}

var fastStringCachePool = sync.Pool[*stringCache]{
	New: func() *stringCache { return new(stringCache) },
}

type fastKind uint8

const (
	fkUnsupported fastKind = iota
	fkBool
	fkString
	fkInt
	fkInt8
	fkInt16
	fkInt32
	fkInt64
	fkUint
	fkUint8
	fkUint16
	fkUint32
	fkUint64
	fkUintptr
	fkFloat32
	fkFloat64
	fkBytes
	fkByteArray
	fkSliceBool
	fkSliceString
	fkSliceInt
	fkSliceInt64
	fkSliceUint64
	fkSliceFloat64
	fkSliceFloat32
	fkSlice
	fkArray
	fkPtr
	fkStruct
	fkMapStringString
	fkMapStringInt
	fkMapStringAny
	fkMapStringElem
	fkMapIntElem
	fkMapUintElem
	fkAny
	fkTime
	fkMarshaler
	fkUnmarshaler
)

func isFastNumericKind(k fastKind) bool {
	return k >= fkInt && k <= fkFloat64
}

type fastMarshalField struct {
	offset               uintptr
	quotedNameColon      string // pre-quoted `"name":` when !nameNeedEscape
	commaQuotedNameColon string // pre-quoted `,"name":` when !nameNeedEscape
	name                 string
	nameNeedEscape       bool
	omitEmpty            bool
	omitZero             bool
	quoted               bool
	kind                 fastKind
	plan                 *fastMarshalPlan
}

type fastMarshalPlan struct {
	typ       *abi.Type
	mapType   *abi.MapType
	ifaceTab  unsafe.Pointer
	kind      fastKind
	keyBits   uint8
	canFast   bool
	hasOmit   bool
	hasQuoted bool
	minSize   int
	elemSize  uintptr
	arrayLen  int
	elemPlan  *fastMarshalPlan
	fields    []fastMarshalField
}

var fastMarshalCache sync.Map[*abi.Type, *fastMarshalPlan]

func lookupFastMarshalPlan(t *abi.Type) *fastMarshalPlan {
	if p, ok := fastMarshalCache.Load(t); ok {
		return p
	}
	visiting := make(map[*abi.Type]*fastMarshalPlan)
	p := buildFastMarshalPlan(t, visiting)
	finalizeFastMarshalPlans(visiting)
	for kt, kp := range visiting {
		fastMarshalCache.LoadOrStore(kt, kp)
	}
	return p
}

func finalizeFastMarshalPlans(visiting map[*abi.Type]*fastMarshalPlan) {
	// Sync field kinds now that all recursive plans have their final kind.
	for _, p := range visiting {
		if p.kind == fkStruct {
			for i := range p.fields {
				f := &p.fields[i]
				if f.plan != nil {
					f.kind = f.plan.kind
				}
			}
		}
	}
	// Propagate canFast == false across recursive dependencies.
	for changed := true; changed; {
		changed = false
		for _, p := range visiting {
			if !p.canFast {
				continue
			}
			switch p.kind {
			case fkPtr, fkSlice, fkArray, fkMapStringElem, fkMapIntElem, fkMapUintElem:
				if p.elemPlan == nil || !p.elemPlan.canFast {
					p.canFast = false
					changed = true
				}
			case fkStruct:
				for i := range p.fields {
					f := &p.fields[i]
					if f.plan != nil && !f.plan.canFast {
						p.canFast = false
						changed = true
						break
					}
				}
			}
		}
	}
}

var reflectTypeIfaceTab = func() unsafe.Pointer {
	rt := reflect.TypeFor[int]()
	return (*[2]unsafe.Pointer)(unsafe.Pointer(&rt))[0]
}()

func abiTypeOfReflect(rt reflect.Type) *abi.Type {
	// reflect.Type is an interface whose data word is *abi.Type.
	return (*abi.Type)((*eface)(unsafe.Pointer(&rt)).data)
}

func lookupFastMarshalPlanReflect(rt reflect.Type) *fastMarshalPlan {
	at := abiTypeOfReflect(rt)
	if p, ok := fastMarshalCache.Load(at); ok {
		return p
	}
	visiting := make(map[*abi.Type]*fastMarshalPlan)
	p := buildFastMarshalPlanReflect(rt, at, visiting)
	finalizeFastMarshalPlans(visiting)
	for kt, kp := range visiting {
		fastMarshalCache.LoadOrStore(kt, kp)
	}
	return p
}

func buildFastMarshalPlan(at *abi.Type, visiting map[*abi.Type]*fastMarshalPlan) *fastMarshalPlan {
	return buildFastMarshalPlanReflect(abiTypeToReflect(at), at, visiting)
}

func abiTypeToReflect(at *abi.Type) reflect.Type {
	var rt reflect.Type
	p := (*[2]unsafe.Pointer)(unsafe.Pointer(&rt))
	p[0] = reflectTypeIfaceTab
	p[1] = unsafe.Pointer(at)
	return rt
}

func hasAnyMarshalerMethod(rt reflect.Type) bool {
	if rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Interface {
		return false
	}
	return implementsAny(rt, allMarshalerTypes...)
}

func buildFastMarshalPlanReflect(rt reflect.Type, at *abi.Type, visiting map[*abi.Type]*fastMarshalPlan) *fastMarshalPlan {
	if p, ok := fastMarshalCache.Load(at); ok {
		return p
	}
	if p, ok := visiting[at]; ok {
		return p
	}
	p := &fastMarshalPlan{typ: at}
	visiting[at] = p

	if rt == timeTimeType {
		p.kind = fkTime
		p.canFast = true
		return p
	}
	if rt == timeDurationType {
		return p // canFast = false
	}
	if hasAnyMarshalerMethod(rt) {
		if !implementsAny(rt, jsonMarshalerToType) {
			if needAddr, ok := implements(rt, jsonMarshalerType); ok && !needAddr {
				m := reflect.New(rt).Interface().(Marshaler)
				p.ifaceTab = (*[2]unsafe.Pointer)(unsafe.Pointer(&m))[0]
				p.kind = fkMarshaler
				p.canFast = true
				return p
			}
		}
		return p // canFast = false
	}

	switch rt.Kind() {
	case reflect.Bool:
		p.kind = fkBool
		p.canFast = true
	case reflect.String:
		p.kind = fkString
		p.canFast = true
	case reflect.Int:
		p.kind = fkInt
		p.canFast = true
	case reflect.Int8:
		p.kind = fkInt8
		p.canFast = true
	case reflect.Int16:
		p.kind = fkInt16
		p.canFast = true
	case reflect.Int32:
		p.kind = fkInt32
		p.canFast = true
	case reflect.Int64:
		p.kind = fkInt64
		p.canFast = true
	case reflect.Uint:
		p.kind = fkUint
		p.canFast = true
	case reflect.Uint8:
		p.kind = fkUint8
		p.canFast = true
	case reflect.Uint16:
		p.kind = fkUint16
		p.canFast = true
	case reflect.Uint32:
		p.kind = fkUint32
		p.canFast = true
	case reflect.Uint64:
		p.kind = fkUint64
		p.canFast = true
	case reflect.Uintptr:
		p.kind = fkUintptr
		p.canFast = true
	case reflect.Float32:
		p.kind = fkFloat32
		p.canFast = true
	case reflect.Float64:
		p.kind = fkFloat64
		p.canFast = true

	case reflect.Slice:
		elem := rt.Elem()
		if elem.Kind() == reflect.Uint8 {
			// Only optimize plain []byte (unnamed uint8 without methods).
			if elem.PkgPath() == "" && !hasAnyMarshalerMethod(elem) {
				p.kind = fkBytes
				p.canFast = true
			}
			return p
		}
		ep := buildFastMarshalPlanReflect(elem, abiTypeOfReflect(elem), visiting)
		p.elemPlan = ep
		p.elemSize = elem.Size()
		switch ep.kind {
		case fkBool:
			p.kind = fkSliceBool
		case fkString:
			p.kind = fkSliceString
		case fkInt:
			p.kind = fkSliceInt
		case fkInt64:
			p.kind = fkSliceInt64
		case fkUint64:
			p.kind = fkSliceUint64
		case fkFloat64:
			p.kind = fkSliceFloat64
		case fkFloat32:
			p.kind = fkSliceFloat32
		default:
			p.kind = fkSlice
		}
		p.canFast = true

	case reflect.Array:
		elem := rt.Elem()
		if elem.Kind() == reflect.Uint8 {
			if elem.PkgPath() == "" && !hasAnyMarshalerMethod(elem) {
				p.kind = fkByteArray
				p.arrayLen = rt.Len()
				p.canFast = true
			}
			return p
		}
		ep := buildFastMarshalPlanReflect(elem, abiTypeOfReflect(elem), visiting)
		p.elemPlan = ep
		p.elemSize = elem.Size()
		p.arrayLen = rt.Len()
		p.kind = fkArray
		p.canFast = true

	case reflect.Pointer:
		elem := rt.Elem()
		ep := buildFastMarshalPlanReflect(elem, abiTypeOfReflect(elem), visiting)
		p.elemPlan = ep
		p.kind = fkPtr
		p.canFast = true

	case reflect.Map:
		if rt == reflect.TypeFor[map[string]string]() {
			p.kind = fkMapStringString
			p.canFast = true
		} else if rt == reflect.TypeFor[map[string]int]() {
			p.kind = fkMapStringInt
			p.canFast = true
		} else if rt == mapStringAnyType {
			p.kind = fkMapStringAny
			p.canFast = true
		} else {
			kt := rt.Key()
			if kt == timeDurationType || hasAnyMarshalerMethod(kt) {
				return p
			}
			switch kt.Kind() {
			case reflect.String:
				p.kind = fkMapStringElem
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				p.kind = fkMapIntElem
				p.keyBits = uint8(kt.Bits())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				p.kind = fkMapUintElem
				p.keyBits = uint8(kt.Bits())
			default:
				return p
			}
			elem := rt.Elem()
			p.elemPlan = buildFastMarshalPlanReflect(elem, abiTypeOfReflect(elem), visiting)
			p.mapType = (*abi.MapType)(unsafe.Pointer(at))
			p.canFast = true
		}

	case reflect.Interface:
		if rt == anyType {
			p.kind = fkAny
			p.canFast = true
		}

	case reflect.Struct:
		fields, errInit := lookupStructFields(rt)
		if errInit != nil || fields.errUnsupportedFormat != nil || fields.embeddedFallback != nil {
			return p
		}
		fastFields := make([]fastMarshalField, 0, len(fields.flattened))
		hasOmit := false
		hasQuoted := false
		minSize := 2
		for i := range fields.flattened {
			f := &fields.flattened[i]
			if f.format != "" {
				return p
			}
			// Custom IsZero() method (other than time.Time which we handle inline) falls back.
			if f.isZero != nil && f.typ != timeTimeType {
				return p
			}
			// Compute direct byte offset; abort if any embedded step crosses a pointer.
			off, ok := directFieldOffset(rt, f.index0, f.index, f.typ)
			if !ok {
				return p
			}
			fp := buildFastMarshalPlanReflect(f.typ, abiTypeOfReflect(f.typ), visiting)
			if f.string {
				if !isFastNumericKind(fp.kind) {
					return p
				}
				hasQuoted = true
			}
			if f.omitempty || f.omitzero {
				hasOmit = true
			}
			ff := fastMarshalField{
				offset:         off,
				name:           f.name,
				nameNeedEscape: f.nameNeedEscape,
				omitEmpty:      f.omitempty,
				omitZero:       f.omitzero,
				quoted:         f.string,
				kind:           fp.kind,
				plan:           fp,
			}
			if !f.nameNeedEscape {
				ff.quotedNameColon = f.quotedName + ":"
				if i == 0 {
					ff.commaQuotedNameColon = "{" + ff.quotedNameColon
				} else {
					ff.commaQuotedNameColon = "," + ff.quotedNameColon
				}
				minSize += len(ff.commaQuotedNameColon) + 4
			} else {
				minSize += len(f.name) + 8
			}
			fastFields = append(fastFields, ff)
		}
		p.kind = fkStruct
		p.fields = fastFields
		p.hasOmit = hasOmit
		p.hasQuoted = hasQuoted
		p.minSize = minSize
		p.canFast = true
	}
	return p
}

// directFieldOffset computes the flat byte offset of a struct field if no pointer
// indirection is needed along the embedded struct path.
func directFieldOffset(rt reflect.Type, index0 int, rest []int, wantType reflect.Type) (uintptr, bool) {
	sf := rt.Field(index0)
	off := sf.Offset
	cur := sf.Type
	for _, idx := range rest {
		if cur.Kind() != reflect.Struct {
			return 0, false
		}
		sf = cur.Field(idx)
		off += sf.Offset
		cur = sf.Type
	}
	if cur != wantType {
		return 0, false
	}
	return off, true
}

const fastMarshalDisallowedFlags = jsonflags.AnyWhitespace | jsonflags.StringifyNumbers | jsonflags.TagFlags

func canFastMarshalFlags(mo *jsonopts.Struct) bool {
	return mo.Marshalers == nil && !mo.Flags.Get(fastMarshalDisallowedFlags)
}

func fastOpts(opts []Options, storage *jsonopts.Struct) *jsonopts.Struct {
	if len(opts) == 0 {
		return storage
	}
	if len(opts) == 1 {
		if s, ok := opts[0].(*jsonopts.Struct); ok && s != nil {
			return s
		}
	}
	storage.Join(opts...)
	return storage
}

func (p *fastMarshalPlan) marshal(b []byte, ptr unsafe.Pointer, mo *jsonopts.Struct, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return nil, errFastFallback
	}
	switch p.kind {
	case fkBool:
		if *(*bool)(ptr) {
			return append(b, "true"...), nil
		}
		return append(b, "false"...), nil

	case fkString:
		b, err := jsonwire.AppendQuoteString(b, *(*string)(ptr), &mo.Flags)
		if err != nil {
			return nil, errFastFallback
		}
		return b, nil

	case fkInt:
		return strconv.AppendInt(b, int64(*(*int)(ptr)), 10), nil
	case fkInt8:
		return strconv.AppendInt(b, int64(*(*int8)(ptr)), 10), nil
	case fkInt16:
		return strconv.AppendInt(b, int64(*(*int16)(ptr)), 10), nil
	case fkInt32:
		return strconv.AppendInt(b, int64(*(*int32)(ptr)), 10), nil
	case fkInt64:
		return strconv.AppendInt(b, *(*int64)(ptr), 10), nil

	case fkUint:
		return strconv.AppendUint(b, uint64(*(*uint)(ptr)), 10), nil
	case fkUint8:
		return strconv.AppendUint(b, uint64(*(*uint8)(ptr)), 10), nil
	case fkUint16:
		return strconv.AppendUint(b, uint64(*(*uint16)(ptr)), 10), nil
	case fkUint32:
		return strconv.AppendUint(b, uint64(*(*uint32)(ptr)), 10), nil
	case fkUint64:
		return strconv.AppendUint(b, *(*uint64)(ptr), 10), nil
	case fkUintptr:
		return strconv.AppendUint(b, uint64(*(*uintptr)(ptr)), 10), nil

	case fkFloat32:
		fv := float64(*(*float32)(ptr))
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, errFastFallback
		}
		return jsonwire.AppendFloat(b, fv, 32), nil

	case fkFloat64:
		fv := *(*float64)(ptr)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, errFastFallback
		}
		return jsonwire.AppendFloat(b, fv, 64), nil

	case fkTime:
		tt := *(*time.Time)(ptr)
		// Years outside [0, 9999] or invalid timezone offsets error in RFC 3339;
		// delegate to standard path for exact error formatting.
		_, zoneOff := tt.Zone()
		if y := tt.Year(); y < 0 || y > 9999 || zoneOff <= -24*60*60 || zoneOff >= 24*60*60 || zoneOff%60 != 0 {
			return nil, errFastFallback
		}
		b = append(b, '"')
		b = tt.AppendFormat(b, time.RFC3339Nano)
		b = append(b, '"')
		return b, nil

	case fkBytes:
		s := *(*[]byte)(ptr)
		if s == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
			return append(b, "null"...), nil
		}
		b = append(b, '"')
		b = base64.StdEncoding.AppendEncode(b, s)
		b = append(b, '"')
		return b, nil

	case fkByteArray:
		s := unsafe.Slice((*byte)(ptr), p.arrayLen)
		if mo.Flags.Get(jsonflags.FormatByteArrayAsArray) {
			if len(s) == 0 {
				return append(b, "[]"...), nil
			}
			b = append(b, '[')
			for i, v := range s {
				if i > 0 {
					b = append(b, ',')
				}
				b = strconv.AppendUint(b, uint64(v), 10)
			}
			return append(b, ']'), nil
		}
		b = append(b, '"')
		b = base64.StdEncoding.AppendEncode(b, s)
		b = append(b, '"')
		return b, nil

	case fkPtr:
		elemPtr := *(*unsafe.Pointer)(ptr)
		if elemPtr == nil {
			return append(b, "null"...), nil
		}
		return p.elemPlan.marshal(b, elemPtr, mo, depth+1)

	case fkSliceBool:
		s := *(*[]bool)(ptr)
		if len(s) == 0 {
			if s == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
				return append(b, "null"...), nil
			}
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		for i, v := range s {
			if i > 0 {
				b = append(b, ',')
			}
			if v {
				b = append(b, "true"...)
			} else {
				b = append(b, "false"...)
			}
		}
		return append(b, ']'), nil

	case fkSliceString:
		s := *(*[]string)(ptr)
		if len(s) == 0 {
			if s == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
				return append(b, "null"...), nil
			}
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		var err error
		for i, v := range s {
			if i > 0 {
				b = append(b, ',')
			}
			if b, err = jsonwire.AppendQuoteString(b, v, &mo.Flags); err != nil {
				return nil, errFastFallback
			}
		}
		return append(b, ']'), nil

	case fkSliceInt:
		s := *(*[]int)(ptr)
		if len(s) == 0 {
			if s == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
				return append(b, "null"...), nil
			}
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		for i, v := range s {
			if i > 0 {
				b = append(b, ',')
			}
			b = strconv.AppendInt(b, int64(v), 10)
		}
		return append(b, ']'), nil

	case fkSliceInt64:
		s := *(*[]int64)(ptr)
		if len(s) == 0 {
			if s == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
				return append(b, "null"...), nil
			}
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		for i, v := range s {
			if i > 0 {
				b = append(b, ',')
			}
			b = strconv.AppendInt(b, v, 10)
		}
		return append(b, ']'), nil

	case fkSliceUint64:
		s := *(*[]uint64)(ptr)
		if len(s) == 0 {
			if s == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
				return append(b, "null"...), nil
			}
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		for i, v := range s {
			if i > 0 {
				b = append(b, ',')
			}
			b = strconv.AppendUint(b, v, 10)
		}
		return append(b, ']'), nil

	case fkSliceFloat64:
		s := *(*[]float64)(ptr)
		if len(s) == 0 {
			if s == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
				return append(b, "null"...), nil
			}
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		for i, v := range s {
			if i > 0 {
				b = append(b, ',')
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, errFastFallback
			}
			b = jsonwire.AppendFloat(b, v, 64)
		}
		return append(b, ']'), nil

	case fkSliceFloat32:
		s := *(*[]float32)(ptr)
		if len(s) == 0 {
			if s == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
				return append(b, "null"...), nil
			}
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		for i, v := range s {
			if i > 0 {
				b = append(b, ',')
			}
			fv := float64(v)
			if math.IsNaN(fv) || math.IsInf(fv, 0) {
				return nil, errFastFallback
			}
			b = jsonwire.AppendFloat(b, fv, 32)
		}
		return append(b, ']'), nil

	case fkSlice:
		sh := (*unsafeheader.Slice)(ptr)
		if sh.Len == 0 {
			if sh.Data == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
				return append(b, "null"...), nil
			}
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		elemPlan := p.elemPlan
		elemSize := p.elemSize
		data := sh.Data
		var err error
		for i := range sh.Len {
			if i > 0 {
				b = append(b, ',')
			}
			b, err = elemPlan.marshal(b, unsafe.Add(data, uintptr(i)*elemSize), mo, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil

	case fkArray:
		n := p.arrayLen
		if n == 0 {
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		elemPlan := p.elemPlan
		elemSize := p.elemSize
		var err error
		for i := range n {
			if i > 0 {
				b = append(b, ',')
			}
			b, err = elemPlan.marshal(b, unsafe.Add(ptr, uintptr(i)*elemSize), mo, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil

	case fkMapStringString:
		return marshalFastMapStringString(b, *(*map[string]string)(ptr), mo)

	case fkMapStringInt:
		return marshalFastMapStringInt(b, *(*map[string]int)(ptr), mo)

	case fkMapStringAny:
		return marshalFastMapStringAny(b, *(*map[string]any)(ptr), mo, depth+1)

	case fkMapStringElem:
		return p.marshalMapStringElem(b, ptr, mo, depth+1)

	case fkMapIntElem:
		return p.marshalMapIntElem(b, ptr, mo, depth+1)

	case fkMapUintElem:
		return p.marshalMapUintElem(b, ptr, mo, depth+1)

	case fkAny:
		return marshalFastAny(b, *(*any)(ptr), mo, depth+1)

	case fkMarshaler:
		return p.marshalCustomMarshaler(b, ptr, mo)

	case fkStruct:
		return p.marshalStruct(b, ptr, mo, depth+1)
	}
	return nil, errFastFallback
}

func (p *fastMarshalPlan) marshalCustomMarshaler(b []byte, ptr unsafe.Pointer, mo *jsonopts.Struct) ([]byte, error) {
	if ptr == nil {
		return nil, errFastFallback
	}
	var m Marshaler
	ip := (*[2]unsafe.Pointer)(unsafe.Pointer(&m))
	ip[0] = p.ifaceTab
	ip[1] = noescape(ptr)
	val, err := m.MarshalJSON()
	runtime.KeepAlive(ptr)
	if err != nil {
		return nil, errFastFallback
	}
	return appendCompactValidJSON(b, val, mo)
}

func appendCompactValidJSON(dst, src []byte, mo *jsonopts.Struct) ([]byte, error) {
	if mo.Flags.Get(jsonflags.CanonicalizeRawInts | jsonflags.CanonicalizeRawFloats) {
		return nil, errFastFallback
	}
	allowBadUTF8 := mo.Flags.Get(jsonflags.AllowInvalidUTF8)
	allowDup := mo.Flags.Get(jsonflags.AllowDuplicateNames)
	escHTML := mo.Flags.Get(jsonflags.EscapeForHTML)
	escJS := mo.Flags.Get(jsonflags.EscapeForJS)
	dst, i, err := appendCompactValidJSONValue(dst, src, 0, allowBadUTF8, allowDup, escHTML, escJS, 0)
	if err != nil {
		return nil, err
	}
	for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
		i++
	}
	if i != len(src) {
		return nil, errFastFallback
	}
	return dst, nil
}

func appendCompactValidJSONString(dst, src []byte, pos int, allowBadUTF8, escHTML, escJS bool) ([]byte, int, []byte, bool, error) {
	if pos >= len(src) || src[pos] != '"' {
		return nil, 0, nil, false, errFastFallback
	}
	start := pos + 1
	i := start
	for len(src)-i >= 8 {
		if m := fswarEscapeMask(byteorder.LEUint64(src[i:])); m != 0 {
			i += bits.TrailingZeros64(m) >> 3
			goto special
		}
		i += 8
	}
	for i < len(src) {
		c := src[i]
		if c >= utf8.RuneSelf || c < ' ' || c == '\\' || c == '"' {
			break
		}
		i++
	}
special:
	if i < len(src) && src[i] == '"' {
		raw := src[start:i]
		if escHTML {
			for _, c := range raw {
				if c == '<' || c == '>' || c == '&' {
					return nil, 0, nil, false, errFastFallback
				}
			}
		}
		return append(dst, src[pos:i+1]...), i + 1, raw, false, nil
	}
	if escHTML || escJS {
		return nil, 0, nil, false, errFastFallback
	}
	var flags jsonwire.ValueFlags
	n, err := jsonwire.ConsumeString(&flags, src[pos:], !allowBadUTF8)
	if err != nil {
		return nil, 0, nil, false, errFastFallback
	}
	return append(dst, src[pos:pos+n]...), pos + n, src[pos+1 : pos+n-1], !flags.IsVerbatim(), nil
}

func appendCompactValidJSONValue(dst, src []byte, i int, allowBadUTF8, allowDup, escHTML, escJS bool, depth int) ([]byte, int, error) {
	if depth >= 64 {
		return nil, 0, errFastFallback
	}
	for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
		i++
	}
	if i >= len(src) {
		return nil, 0, errFastFallback
	}
	switch src[i] {
	case 'n':
		if n := jsonwire.ConsumeNull(src[i:]); n > 0 {
			return append(dst, "null"...), i + n, nil
		}
		return nil, 0, errFastFallback
	case 't':
		if n := jsonwire.ConsumeTrue(src[i:]); n > 0 {
			return append(dst, "true"...), i + n, nil
		}
		return nil, 0, errFastFallback
	case 'f':
		if n := jsonwire.ConsumeFalse(src[i:]); n > 0 {
			return append(dst, "false"...), i + n, nil
		}
		return nil, 0, errFastFallback
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		n, err := jsonwire.ConsumeNumber(src[i:])
		if err != nil {
			return nil, 0, errFastFallback
		}
		return append(dst, src[i:i+n]...), i + n, nil
	case '"':
		dst, next, _, _, err := appendCompactValidJSONString(dst, src, i, allowBadUTF8, escHTML, escJS)
		return dst, next, err
	case '[':
		i++
		dst = append(dst, '[')
		for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
			i++
		}
		if i < len(src) && src[i] == ']' {
			return append(dst, ']'), i + 1, nil
		}
		var err error
		for {
			dst, i, err = appendCompactValidJSONValue(dst, src, i, allowBadUTF8, allowDup, escHTML, escJS, depth+1)
			if err != nil {
				return nil, 0, err
			}
			for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
				i++
			}
			if i >= len(src) {
				return nil, 0, errFastFallback
			}
			if src[i] == ',' {
				i++
				dst = append(dst, ',')
				continue
			}
			if src[i] == ']' {
				return append(dst, ']'), i + 1, nil
			}
			return nil, 0, errFastFallback
		}
	case '{':
		if allowBadUTF8 && !allowDup {
			return nil, 0, errFastFallback
		}
		i++
		dst = append(dst, '{')
		for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
			i++
		}
		if i < len(src) && src[i] == '}' {
			return append(dst, '}'), i + 1, nil
		}
		var keys [8][]byte
		numKeys := 0
		var err error
		for {
			for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
				i++
			}
			var kb []byte
			var escaped bool
			dst, i, kb, escaped, err = appendCompactValidJSONString(dst, src, i, allowBadUTF8, escHTML, escJS)
			if err != nil {
				return nil, 0, err
			}
			if !allowDup {
				if escaped || numKeys >= len(keys) {
					return nil, 0, errFastFallback
				}
				for j := range numKeys {
					if bytes.Equal(keys[j], kb) {
						return nil, 0, errFastFallback
					}
				}
				keys[numKeys] = kb
				numKeys++
			}
			for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
				i++
			}
			if i >= len(src) || src[i] != ':' {
				return nil, 0, errFastFallback
			}
			i++
			dst = append(dst, ':')
			dst, i, err = appendCompactValidJSONValue(dst, src, i, allowBadUTF8, allowDup, escHTML, escJS, depth+1)
			if err != nil {
				return nil, 0, err
			}
			for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
				i++
			}
			if i >= len(src) {
				return nil, 0, errFastFallback
			}
			if src[i] == ',' {
				i++
				dst = append(dst, ',')
				continue
			}
			if src[i] == '}' {
				return append(dst, '}'), i + 1, nil
			}
			return nil, 0, errFastFallback
		}
	}
	return nil, 0, errFastFallback
}

func (p *fastMarshalPlan) marshalStruct(b []byte, ptr unsafe.Pointer, mo *jsonopts.Struct, depth int) ([]byte, error) {
	fields := p.fields
	if len(fields) == 0 {
		return append(b, "{}"...), nil
	}
	omitZeroAll := mo.Flags.Get(jsonflags.OmitZeroStructFields)
	if !p.hasOmit && !p.hasQuoted && !omitZeroAll {
		if cap(b)-len(b) < p.minSize {
			b = slices.Grow(b, p.minSize)
		}
		var err error
		for i := range fields {
			f := &fields[i]
			fp := unsafe.Add(ptr, f.offset)
			if !f.nameNeedEscape {
				b = append(b, f.commaQuotedNameColon...)
			} else {
				if i == 0 {
					b = append(b, '{')
				} else {
					b = append(b, ',')
				}
				if b, err = jsonwire.AppendQuoteString(b, f.name, &mo.Flags); err != nil {
					return nil, errFastFallback
				}
				b = append(b, ':')
			}
			switch f.kind {
			case fkString:
				if b, err = jsonwire.AppendQuoteString(b, *(*string)(fp), &mo.Flags); err != nil {
					return nil, errFastFallback
				}
			case fkInt:
				b = strconv.AppendInt(b, int64(*(*int)(fp)), 10)
			case fkInt32:
				b = strconv.AppendInt(b, int64(*(*int32)(fp)), 10)
			case fkInt64:
				b = strconv.AppendInt(b, *(*int64)(fp), 10)
			case fkUint:
				b = strconv.AppendUint(b, uint64(*(*uint)(fp)), 10)
			case fkUint32:
				b = strconv.AppendUint(b, uint64(*(*uint32)(fp)), 10)
			case fkUint64:
				b = strconv.AppendUint(b, *(*uint64)(fp), 10)
			case fkFloat64:
				fv := *(*float64)(fp)
				if math.IsNaN(fv) || math.IsInf(fv, 0) {
					return nil, errFastFallback
				}
				b = jsonwire.AppendFloat(b, fv, 64)
			case fkFloat32:
				fv := float64(*(*float32)(fp))
				if math.IsNaN(fv) || math.IsInf(fv, 0) {
					return nil, errFastFallback
				}
				b = jsonwire.AppendFloat(b, fv, 32)
			case fkBool:
				if *(*bool)(fp) {
					b = append(b, "true"...)
				} else {
					b = append(b, "false"...)
				}
			case fkSliceString:
				s := *(*[]string)(fp)
				if len(s) == 0 {
					if s == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
						b = append(b, "null"...)
					} else {
						b = append(b, "[]"...)
					}
				} else {
					b = append(b, '[')
					for j, v := range s {
						if j > 0 {
							b = append(b, ',')
						}
						if b, err = jsonwire.AppendQuoteString(b, v, &mo.Flags); err != nil {
							return nil, errFastFallback
						}
					}
					b = append(b, ']')
				}
			default:
				if b, err = f.plan.marshal(b, fp, mo, depth); err != nil {
					return nil, err
				}
			}
		}
		return append(b, '}'), nil
	}

	b = append(b, '{')
	first := true
	legacyOmitEmpty := mo.Flags.Get(jsonflags.OmitEmptyWithLegacySemantics)
	var err error

	for i := range fields {
		f := &fields[i]
		fp := unsafe.Add(ptr, f.offset)

		if f.omitZero || omitZeroAll {
			if isFastZero(f.kind, f.plan, fp) {
				continue
			}
		}
		checkPostEmpty := false
		if f.omitEmpty {
			if legacyOmitEmpty {
				if isFastLegacyEmpty(f.kind, f.plan, fp) {
					continue
				}
			} else {
				switch f.kind {
				case fkString:
					if len(*(*string)(fp)) == 0 {
						continue
					}
				case fkBytes:
					if len(*(*[]byte)(fp)) == 0 {
						continue
					}
				case fkByteArray, fkArray:
					if f.plan.arrayLen == 0 {
						continue
					}
				case fkSliceBool, fkSliceString, fkSliceInt, fkSliceInt64, fkSliceUint64, fkSliceFloat64, fkSliceFloat32, fkSlice:
					if (*unsafeheader.Slice)(fp).Len == 0 {
						continue
					}
				case fkPtr:
					if *(*unsafe.Pointer)(fp) == nil {
						continue
					}
					checkPostEmpty = true
				case fkMapStringString:
					if len(*(*map[string]string)(fp)) == 0 {
						continue
					}
				case fkMapStringInt:
					if len(*(*map[string]int)(fp)) == 0 {
						continue
					}
				case fkMapStringAny:
					if len(*(*map[string]any)(fp)) == 0 {
						continue
					}
				case fkMapStringElem, fkMapIntElem, fkMapUintElem:
					if m := *(**maps.Map)(fp); m == nil || m.Used() == 0 {
						continue
					}
				case fkAny:
					if *(*any)(fp) == nil {
						continue
					}
					checkPostEmpty = true
				case fkStruct, fkMarshaler:
					checkPostEmpty = true
				}
			}
		}

		mark := len(b)
		if !first {
			b = append(b, ',')
		}
		if !f.nameNeedEscape {
			b = append(b, f.quotedNameColon...)
		} else {
			if b, err = jsonwire.AppendQuoteString(b, f.name, &mo.Flags); err != nil {
				return nil, errFastFallback
			}
			b = append(b, ':')
		}
		valStart := len(b)
		if f.quoted {
			b = append(b, '"')
		}

		switch f.kind {
		case fkString:
			if b, err = jsonwire.AppendQuoteString(b, *(*string)(fp), &mo.Flags); err != nil {
				return nil, errFastFallback
			}
		case fkInt:
			b = strconv.AppendInt(b, int64(*(*int)(fp)), 10)
		case fkInt32:
			b = strconv.AppendInt(b, int64(*(*int32)(fp)), 10)
		case fkInt64:
			b = strconv.AppendInt(b, *(*int64)(fp), 10)
		case fkUint:
			b = strconv.AppendUint(b, uint64(*(*uint)(fp)), 10)
		case fkUint32:
			b = strconv.AppendUint(b, uint64(*(*uint32)(fp)), 10)
		case fkUint64:
			b = strconv.AppendUint(b, *(*uint64)(fp), 10)
		case fkFloat64:
			fv := *(*float64)(fp)
			if math.IsNaN(fv) || math.IsInf(fv, 0) {
				return nil, errFastFallback
			}
			b = jsonwire.AppendFloat(b, fv, 64)
		case fkFloat32:
			fv := float64(*(*float32)(fp))
			if math.IsNaN(fv) || math.IsInf(fv, 0) {
				return nil, errFastFallback
			}
			b = jsonwire.AppendFloat(b, fv, 32)
		case fkBool:
			if *(*bool)(fp) {
				b = append(b, "true"...)
			} else {
				b = append(b, "false"...)
			}
		case fkSliceString:
			s := *(*[]string)(fp)
			if len(s) == 0 {
				if s == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
					b = append(b, "null"...)
				} else {
					b = append(b, "[]"...)
				}
			} else {
				b = append(b, '[')
				for j, v := range s {
					if j > 0 {
						b = append(b, ',')
					}
					if b, err = jsonwire.AppendQuoteString(b, v, &mo.Flags); err != nil {
						return nil, errFastFallback
					}
				}
				b = append(b, ']')
			}
		default:
			if b, err = f.plan.marshal(b, fp, mo, depth); err != nil {
				return nil, err
			}
		}
		if f.quoted {
			b = append(b, '"')
		}

		if checkPostEmpty && isMarshaledJSONEmpty(b[valStart:]) {
			b = b[:mark]
			continue
		}
		first = false
	}
	return append(b, '}'), nil
}

func isMarshaledJSONEmpty(v []byte) bool {
	switch len(v) {
	case 2:
		return (v[0] == '"' && v[1] == '"') ||
			(v[0] == '{' && v[1] == '}') ||
			(v[0] == '[' && v[1] == ']')
	case 4:
		return string(v) == "null"
	}
	return false
}

func isFastZero(k fastKind, plan *fastMarshalPlan, ptr unsafe.Pointer) bool {
	switch k {
	case fkBool:
		return !*(*bool)(ptr)
	case fkString:
		return len(*(*string)(ptr)) == 0
	case fkInt:
		return *(*int)(ptr) == 0
	case fkInt8:
		return *(*int8)(ptr) == 0
	case fkInt16:
		return *(*int16)(ptr) == 0
	case fkInt32:
		return *(*int32)(ptr) == 0
	case fkInt64:
		return *(*int64)(ptr) == 0
	case fkUint:
		return *(*uint)(ptr) == 0
	case fkUint8:
		return *(*uint8)(ptr) == 0
	case fkUint16:
		return *(*uint16)(ptr) == 0
	case fkUint32:
		return *(*uint32)(ptr) == 0
	case fkUint64:
		return *(*uint64)(ptr) == 0
	case fkUintptr:
		return *(*uintptr)(ptr) == 0
	case fkFloat32:
		return math.Float32bits(*(*float32)(ptr)) == 0
	case fkFloat64:
		return math.Float64bits(*(*float64)(ptr)) == 0
	case fkTime:
		return (*time.Time)(ptr).IsZero()
	case fkBytes:
		return *(*[]byte)(ptr) == nil
	case fkByteArray:
		for _, v := range unsafe.Slice((*byte)(ptr), plan.arrayLen) {
			if v != 0 {
				return false
			}
		}
		return true
	case fkSliceBool, fkSliceString, fkSliceInt, fkSliceInt64, fkSliceUint64, fkSliceFloat64, fkSliceFloat32, fkSlice:
		return (*unsafeheader.Slice)(ptr).Data == nil
	case fkPtr:
		return *(*unsafe.Pointer)(ptr) == nil
	case fkMapStringString:
		return *(*map[string]string)(ptr) == nil
	case fkMapStringInt:
		return *(*map[string]int)(ptr) == nil
	case fkMapStringAny:
		return *(*map[string]any)(ptr) == nil
	case fkMapStringElem, fkMapIntElem, fkMapUintElem:
		return *(**maps.Map)(ptr) == nil
	case fkAny:
		return *(*any)(ptr) == nil
	case fkArray:
		elemSize := plan.elemSize
		ep := plan.elemPlan
		for i := range plan.arrayLen {
			if !isFastZero(ep.kind, ep, unsafe.Add(ptr, uintptr(i)*elemSize)) {
				return false
			}
		}
		return true
	case fkStruct, fkMarshaler:
		// Check all bytes in struct are zero? Beware padding bytes or unexported fields!
		// Struct IsZero checks every Go struct field (including unexported ones).
		// To be 100% exact, check via reflect if struct has unexported fields or just check size == 0.
		if plan.typ.Size() == 0 {
			return true
		}
		return isStructZeroReflect(plan.typ, ptr)
	}
	return false
}

func isStructZeroReflect(at *abi.Type, ptr unsafe.Pointer) bool {
	rt := abiTypeToReflect(at)
	return reflect.NewAt(rt, ptr).Elem().IsZero()
}

func isFastLegacyEmpty(k fastKind, plan *fastMarshalPlan, ptr unsafe.Pointer) bool {
	switch k {
	case fkBool:
		return !*(*bool)(ptr)
	case fkString:
		return len(*(*string)(ptr)) == 0
	case fkInt:
		return *(*int)(ptr) == 0
	case fkInt8:
		return *(*int8)(ptr) == 0
	case fkInt16:
		return *(*int16)(ptr) == 0
	case fkInt32:
		return *(*int32)(ptr) == 0
	case fkInt64:
		return *(*int64)(ptr) == 0
	case fkUint:
		return *(*uint)(ptr) == 0
	case fkUint8:
		return *(*uint8)(ptr) == 0
	case fkUint16:
		return *(*uint16)(ptr) == 0
	case fkUint32:
		return *(*uint32)(ptr) == 0
	case fkUint64:
		return *(*uint64)(ptr) == 0
	case fkUintptr:
		return *(*uintptr)(ptr) == 0
	case fkFloat32:
		return *(*float32)(ptr) == 0
	case fkFloat64:
		return *(*float64)(ptr) == 0
	case fkBytes:
		return len(*(*[]byte)(ptr)) == 0
	case fkByteArray, fkArray:
		return plan.arrayLen == 0
	case fkSliceBool, fkSliceString, fkSliceInt, fkSliceInt64, fkSliceUint64, fkSliceFloat64, fkSliceFloat32, fkSlice:
		return (*unsafeheader.Slice)(ptr).Len == 0
	case fkPtr:
		return *(*unsafe.Pointer)(ptr) == nil
	case fkMapStringString:
		return len(*(*map[string]string)(ptr)) == 0
	case fkMapStringInt:
		return len(*(*map[string]int)(ptr)) == 0
	case fkMapStringAny:
		return len(*(*map[string]any)(ptr)) == 0
	case fkMapStringElem, fkMapIntElem, fkMapUintElem:
		m := *(**maps.Map)(ptr)
		return m == nil || m.Used() == 0
	case fkAny:
		return *(*any)(ptr) == nil
	case fkMarshaler:
		v := reflect.NewAt(abiTypeToReflect(plan.typ), ptr).Elem()
		switch v.Kind() {
		case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
			return v.Len() == 0
		case reflect.Bool:
			return !v.Bool()
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return v.Int() == 0
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			return v.Uint() == 0
		case reflect.Float32, reflect.Float64:
			return v.Float() == 0
		case reflect.Interface, reflect.Pointer:
			return v.IsNil()
		}
	}
	return false
}

type mapStringEntry struct {
	k string
	v unsafe.Pointer
}

var mapStringEntryPool = sync.Pool[*[]mapStringEntry]{
	New: func() *[]mapStringEntry { s := make([]mapStringEntry, 0, 16); return &s },
}

func getMapStringEntries(n int) *[]mapStringEntry {
	p := mapStringEntryPool.Get()
	if cap(*p) < n {
		*p = make([]mapStringEntry, n)
	} else {
		*p = (*p)[:n]
	}
	return p
}

func putMapStringEntries(p *[]mapStringEntry) {
	if cap(*p) > 4096 {
		return
	}
	clear(*p)
	*p = (*p)[:0]
	mapStringEntryPool.Put(p)
}

type mapNumEntry struct {
	v   unsafe.Pointer
	buf [24]byte // formatted `"<digits>":`
	n   uint8
}

var mapNumEntryPool = sync.Pool[*[]mapNumEntry]{
	New: func() *[]mapNumEntry { s := make([]mapNumEntry, 0, 16); return &s },
}

func getMapNumEntries(n int) *[]mapNumEntry {
	p := mapNumEntryPool.Get()
	if cap(*p) < n {
		*p = make([]mapNumEntry, n)
	} else {
		*p = (*p)[:n]
	}
	return p
}

func putMapNumEntries(p *[]mapNumEntry) {
	if cap(*p) > 4096 {
		return
	}
	clear(*p)
	*p = (*p)[:0]
	mapNumEntryPool.Put(p)
}

func readSignedKey(kp unsafe.Pointer, keyBits uint8) int64 {
	switch keyBits {
	case 8:
		return int64(*(*int8)(kp))
	case 16:
		return int64(*(*int16)(kp))
	case 32:
		return int64(*(*int32)(kp))
	default:
		return *(*int64)(kp)
	}
}

func readUnsignedKey(kp unsafe.Pointer, keyBits uint8) uint64 {
	switch keyBits {
	case 8:
		return uint64(*(*uint8)(kp))
	case 16:
		return uint64(*(*uint16)(kp))
	case 32:
		return uint64(*(*uint32)(kp))
	default:
		return *(*uint64)(kp)
	}
}

func (p *fastMarshalPlan) marshalMapStringElem(b []byte, ptr unsafe.Pointer, mo *jsonopts.Struct, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return nil, errFastFallback
	}
	m := *(**maps.Map)(ptr)
	if m == nil || m.Used() == 0 {
		if m == nil && mo.Flags.Get(jsonflags.FormatNilMapAsNull) {
			return append(b, "null"...), nil
		}
		return append(b, "{}"...), nil
	}
	n := int(m.Used())
	if mo.Flags.Get(jsonflags.AllowInvalidUTF8) && !mo.Flags.Get(jsonflags.AllowDuplicateNames) && n > 1 {
		return nil, errFastFallback
	}
	b = append(b, '{')
	elemPlan := p.elemPlan
	var err error
	if !mo.Flags.Get(jsonflags.Deterministic) || n <= 1 {
		var it maps.Iter
		it.Init(p.mapType, m)
		first := true
		for it.Next(); it.Key() != nil; it.Next() {
			if !first {
				b = append(b, ',')
			}
			first = false
			if b, err = jsonwire.AppendQuoteString(b, *(*string)(it.Key()), &mo.Flags); err != nil {
				return nil, errFastFallback
			}
			b = append(b, ':')
			if b, err = elemPlan.marshal(b, it.Elem(), mo, depth); err != nil {
				return nil, err
			}
		}
	} else {
		ep := getMapStringEntries(n)
		entries := *ep
		var it maps.Iter
		it.Init(p.mapType, m)
		i := 0
		for it.Next(); it.Key() != nil && i < n; it.Next() {
			entries[i] = mapStringEntry{k: *(*string)(it.Key()), v: it.Elem()}
			i++
		}
		entries = entries[:i]
		slices.SortFunc(entries, func(x, y mapStringEntry) int {
			return strings.Compare(x.k, y.k)
		})
		for idx, e := range entries {
			if idx > 0 {
				b = append(b, ',')
			}
			if b, err = jsonwire.AppendQuoteString(b, e.k, &mo.Flags); err != nil {
				putMapStringEntries(ep)
				return nil, errFastFallback
			}
			b = append(b, ':')
			if b, err = elemPlan.marshal(b, e.v, mo, depth); err != nil {
				putMapStringEntries(ep)
				return nil, err
			}
		}
		putMapStringEntries(ep)
	}
	return append(b, '}'), nil
}

func (p *fastMarshalPlan) marshalMapIntElem(b []byte, ptr unsafe.Pointer, mo *jsonopts.Struct, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return nil, errFastFallback
	}
	m := *(**maps.Map)(ptr)
	if m == nil || m.Used() == 0 {
		if m == nil && mo.Flags.Get(jsonflags.FormatNilMapAsNull) {
			return append(b, "null"...), nil
		}
		return append(b, "{}"...), nil
	}
	n := int(m.Used())
	b = append(b, '{')
	elemPlan := p.elemPlan
	keyBits := p.keyBits
	var err error
	if !mo.Flags.Get(jsonflags.Deterministic) || n <= 1 {
		var it maps.Iter
		it.Init(p.mapType, m)
		first := true
		for it.Next(); it.Key() != nil; it.Next() {
			if !first {
				b = append(b, ',')
			}
			first = false
			b = append(b, '"')
			b = strconv.AppendInt(b, readSignedKey(it.Key(), keyBits), 10)
			b = append(b, '"', ':')
			if b, err = elemPlan.marshal(b, it.Elem(), mo, depth); err != nil {
				return nil, err
			}
		}
	} else {
		ep := getMapNumEntries(n)
		entries := *ep
		var it maps.Iter
		it.Init(p.mapType, m)
		i := 0
		for it.Next(); it.Key() != nil && i < n; it.Next() {
			e := &entries[i]
			e.v = it.Elem()
			buf := append(e.buf[:0], '"')
			buf = strconv.AppendInt(buf, readSignedKey(it.Key(), keyBits), 10)
			buf = append(buf, '"', ':')
			e.n = uint8(len(buf))
			i++
		}
		entries = entries[:i]
		slices.SortFunc(entries, func(x, y mapNumEntry) int {
			return bytes.Compare(x.buf[:x.n], y.buf[:y.n])
		})
		for idx := range entries {
			e := &entries[idx]
			if idx > 0 {
				b = append(b, ',')
			}
			b = append(b, e.buf[:e.n]...)
			if b, err = elemPlan.marshal(b, e.v, mo, depth); err != nil {
				putMapNumEntries(ep)
				return nil, err
			}
		}
		putMapNumEntries(ep)
	}
	return append(b, '}'), nil
}

func (p *fastMarshalPlan) marshalMapUintElem(b []byte, ptr unsafe.Pointer, mo *jsonopts.Struct, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return nil, errFastFallback
	}
	m := *(**maps.Map)(ptr)
	if m == nil || m.Used() == 0 {
		if m == nil && mo.Flags.Get(jsonflags.FormatNilMapAsNull) {
			return append(b, "null"...), nil
		}
		return append(b, "{}"...), nil
	}
	n := int(m.Used())
	b = append(b, '{')
	elemPlan := p.elemPlan
	keyBits := p.keyBits
	var err error
	if !mo.Flags.Get(jsonflags.Deterministic) || n <= 1 {
		var it maps.Iter
		it.Init(p.mapType, m)
		first := true
		for it.Next(); it.Key() != nil; it.Next() {
			if !first {
				b = append(b, ',')
			}
			first = false
			b = append(b, '"')
			b = strconv.AppendUint(b, readUnsignedKey(it.Key(), keyBits), 10)
			b = append(b, '"', ':')
			if b, err = elemPlan.marshal(b, it.Elem(), mo, depth); err != nil {
				return nil, err
			}
		}
	} else {
		ep := getMapNumEntries(n)
		entries := *ep
		var it maps.Iter
		it.Init(p.mapType, m)
		i := 0
		for it.Next(); it.Key() != nil && i < n; it.Next() {
			e := &entries[i]
			e.v = it.Elem()
			buf := append(e.buf[:0], '"')
			buf = strconv.AppendUint(buf, readUnsignedKey(it.Key(), keyBits), 10)
			buf = append(buf, '"', ':')
			e.n = uint8(len(buf))
			i++
		}
		entries = entries[:i]
		slices.SortFunc(entries, func(x, y mapNumEntry) int {
			return bytes.Compare(x.buf[:x.n], y.buf[:y.n])
		})
		for idx := range entries {
			e := &entries[idx]
			if idx > 0 {
				b = append(b, ',')
			}
			b = append(b, e.buf[:e.n]...)
			if b, err = elemPlan.marshal(b, e.v, mo, depth); err != nil {
				putMapNumEntries(ep)
				return nil, err
			}
		}
		putMapNumEntries(ep)
	}
	return append(b, '}'), nil
}

func marshalFastMapStringString(b []byte, m map[string]string, mo *jsonopts.Struct) ([]byte, error) {
	n := len(m)
	if n == 0 {
		if m == nil && mo.Flags.Get(jsonflags.FormatNilMapAsNull) {
			return append(b, "null"...), nil
		}
		return append(b, "{}"...), nil
	}
	// If AllowInvalidUTF8 && !AllowDuplicateNames, two different invalid-UTF8 keys
	// could mangle to the same JSON name; fall back to standard duplicate-name check.
	if mo.Flags.Get(jsonflags.AllowInvalidUTF8) && !mo.Flags.Get(jsonflags.AllowDuplicateNames) && n > 1 {
		return nil, errFastFallback
	}
	b = append(b, '{')
	var err error
	if !mo.Flags.Get(jsonflags.Deterministic) || n <= 1 {
		first := true
		for k, v := range m {
			if !first {
				b = append(b, ',')
			}
			first = false
			if b, err = jsonwire.AppendQuoteString(b, k, &mo.Flags); err != nil {
				return nil, errFastFallback
			}
			b = append(b, ':')
			if b, err = jsonwire.AppendQuoteString(b, v, &mo.Flags); err != nil {
				return nil, errFastFallback
			}
		}
	} else {
		names := getStrings(n)
		i := 0
		for k := range m {
			(*names)[i] = k
			i++
		}
		slices.Sort(*names)
		for idx, k := range *names {
			if idx > 0 {
				b = append(b, ',')
			}
			if b, err = jsonwire.AppendQuoteString(b, k, &mo.Flags); err != nil {
				putStrings(names)
				return nil, errFastFallback
			}
			b = append(b, ':')
			if b, err = jsonwire.AppendQuoteString(b, m[k], &mo.Flags); err != nil {
				putStrings(names)
				return nil, errFastFallback
			}
		}
		putStrings(names)
	}
	return append(b, '}'), nil
}

func marshalFastMapStringInt(b []byte, m map[string]int, mo *jsonopts.Struct) ([]byte, error) {
	n := len(m)
	if n == 0 {
		if m == nil && mo.Flags.Get(jsonflags.FormatNilMapAsNull) {
			return append(b, "null"...), nil
		}
		return append(b, "{}"...), nil
	}
	if mo.Flags.Get(jsonflags.AllowInvalidUTF8) && !mo.Flags.Get(jsonflags.AllowDuplicateNames) && n > 1 {
		return nil, errFastFallback
	}
	b = append(b, '{')
	var err error
	if !mo.Flags.Get(jsonflags.Deterministic) || n <= 1 {
		first := true
		for k, v := range m {
			if !first {
				b = append(b, ',')
			}
			first = false
			if b, err = jsonwire.AppendQuoteString(b, k, &mo.Flags); err != nil {
				return nil, errFastFallback
			}
			b = append(b, ':')
			b = strconv.AppendInt(b, int64(v), 10)
		}
	} else {
		names := getStrings(n)
		i := 0
		for k := range m {
			(*names)[i] = k
			i++
		}
		slices.Sort(*names)
		for idx, k := range *names {
			if idx > 0 {
				b = append(b, ',')
			}
			if b, err = jsonwire.AppendQuoteString(b, k, &mo.Flags); err != nil {
				putStrings(names)
				return nil, errFastFallback
			}
			b = append(b, ':')
			b = strconv.AppendInt(b, int64(m[k]), 10)
		}
		putStrings(names)
	}
	return append(b, '}'), nil
}

func marshalFastMapStringAny(b []byte, m map[string]any, mo *jsonopts.Struct, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return nil, errFastFallback
	}
	n := len(m)
	if n == 0 {
		if m == nil && mo.Flags.Get(jsonflags.FormatNilMapAsNull) {
			return append(b, "null"...), nil
		}
		return append(b, "{}"...), nil
	}
	if mo.Flags.Get(jsonflags.AllowInvalidUTF8) && !mo.Flags.Get(jsonflags.AllowDuplicateNames) && n > 1 {
		return nil, errFastFallback
	}
	b = append(b, '{')
	var err error
	if !mo.Flags.Get(jsonflags.Deterministic) || n <= 1 {
		first := true
		for k, v := range m {
			if !first {
				b = append(b, ',')
			}
			first = false
			if b, err = jsonwire.AppendQuoteString(b, k, &mo.Flags); err != nil {
				return nil, errFastFallback
			}
			b = append(b, ':')
			if b, err = marshalFastAny(b, v, mo, depth); err != nil {
				return nil, err
			}
		}
	} else {
		names := getStrings(n)
		i := 0
		for k := range m {
			(*names)[i] = k
			i++
		}
		slices.Sort(*names)
		for idx, k := range *names {
			if idx > 0 {
				b = append(b, ',')
			}
			if b, err = jsonwire.AppendQuoteString(b, k, &mo.Flags); err != nil {
				putStrings(names)
				return nil, errFastFallback
			}
			b = append(b, ':')
			if b, err = marshalFastAny(b, m[k], mo, depth); err != nil {
				putStrings(names)
				return nil, err
			}
		}
		putStrings(names)
	}
	return append(b, '}'), nil
}

func marshalFastAny(b []byte, val any, mo *jsonopts.Struct, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return nil, errFastFallback
	}
	switch v := val.(type) {
	case nil:
		return append(b, "null"...), nil
	case bool:
		if v {
			return append(b, "true"...), nil
		}
		return append(b, "false"...), nil
	case string:
		b, err := jsonwire.AppendQuoteString(b, v, &mo.Flags)
		if err != nil {
			return nil, errFastFallback
		}
		return b, nil
	case int:
		return strconv.AppendInt(b, int64(v), 10), nil
	case int64:
		return strconv.AppendInt(b, v, 10), nil
	case uint:
		return strconv.AppendUint(b, uint64(v), 10), nil
	case uint64:
		return strconv.AppendUint(b, v, 10), nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, errFastFallback
		}
		return jsonwire.AppendFloat(b, v, 64), nil
	case float32:
		fv := float64(v)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, errFastFallback
		}
		return jsonwire.AppendFloat(b, fv, 32), nil
	case []any:
		if len(v) == 0 {
			if v == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
				return append(b, "null"...), nil
			}
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		var err error
		for i, elem := range v {
			if i > 0 {
				b = append(b, ',')
			}
			if b, err = marshalFastAny(b, elem, mo, depth+1); err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	case map[string]any:
		return marshalFastMapStringAny(b, v, mo, depth+1)
	case []int:
		if len(v) == 0 {
			if v == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
				return append(b, "null"...), nil
			}
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		for i, x := range v {
			if i > 0 {
				b = append(b, ',')
			}
			b = strconv.AppendInt(b, int64(x), 10)
		}
		return append(b, ']'), nil
	case []string:
		if len(v) == 0 {
			if v == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
				return append(b, "null"...), nil
			}
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		var err error
		for i, s := range v {
			if i > 0 {
				b = append(b, ',')
			}
			if b, err = jsonwire.AppendQuoteString(b, s, &mo.Flags); err != nil {
				return nil, errFastFallback
			}
		}
		return append(b, ']'), nil
	case map[string]string:
		return marshalFastMapStringString(b, v, mo)
	}

	at, ptr := efacePtr(&val)
	if at == nil {
		return append(b, "null"...), nil
	}
	plan := lookupFastMarshalPlan(at)
	if !plan.canFast {
		return nil, errFastFallback
	}
	b, err := plan.marshal(b, ptr, mo, depth+1)
	runtime.KeepAlive(&val)
	return b, err
}

// tryFastAppendMarshal attempts to marshal (at, ptr) directly into dst.
// It returns (out, true) on success, or (dst[:origLen], false) if the standard path should run.
func tryFastAppendMarshal(dst []byte, at *abi.Type, ptr unsafe.Pointer, mo *jsonopts.Struct) ([]byte, bool) {
	if !canFastMarshalFlags(mo) {
		return dst, false
	}
	if at == nil {
		return append(dst, "null"...), true
	}
	plan := lookupFastMarshalPlan(at)
	if !plan.canFast {
		return dst, false
	}
	origLen := len(dst)
	out, err := plan.marshal(dst, ptr, mo, 0)
	if err != nil {
		return dst[:origLen], false
	}
	return out, true
}

// marshalFastAlloc marshals (plan, ptr) into a freshly allocated byte slice.
// Scalar values bypass fastBufPool using a stack scratch buffer or direct sizing.
func marshalFastAlloc(plan *fastMarshalPlan, ptr unsafe.Pointer, mo *jsonopts.Struct) ([]byte, bool) {
	// Dereference leading pointers quickly (bounded to guard against cycles).
	for depth := 0; plan.kind == fkPtr && depth < startDetectingCyclesAfter; depth++ {
		p := *(*unsafe.Pointer)(ptr)
		if p == nil {
			return []byte("null"), true
		}
		ptr = p
		plan = plan.elemPlan
	}
	switch plan.kind {
	case fkBool:
		if *(*bool)(ptr) {
			return []byte("true"), true
		}
		return []byte("false"), true
	case fkString:
		s := *(*string)(ptr)
		if !jsonwire.NeedEscape(unsafe.Slice(unsafe.StringData(s), len(s))) {
			out := make([]byte, len(s)+2)
			out[0] = '"'
			copy(out[1:], s)
			out[len(s)+1] = '"'
			return out, true
		}
	case fkInt:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], int64(*(*int)(ptr)), 10)), true
	case fkInt8:
		var scratch [8]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], int64(*(*int8)(ptr)), 10)), true
	case fkInt16:
		var scratch [8]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], int64(*(*int16)(ptr)), 10)), true
	case fkInt32:
		var scratch [16]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], int64(*(*int32)(ptr)), 10)), true
	case fkInt64:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], *(*int64)(ptr), 10)), true
	case fkUint:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uint)(ptr)), 10)), true
	case fkUint8:
		var scratch [8]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uint8)(ptr)), 10)), true
	case fkUint16:
		var scratch [8]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uint16)(ptr)), 10)), true
	case fkUint32:
		var scratch [16]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uint32)(ptr)), 10)), true
	case fkUint64:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], *(*uint64)(ptr), 10)), true
	case fkUintptr:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uintptr)(ptr)), 10)), true
	case fkFloat32:
		fv := float64(*(*float32)(ptr))
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, false
		}
		var scratch [32]byte
		return bytes.Clone(jsonwire.AppendFloat(scratch[:0], fv, 32)), true
	case fkFloat64:
		fv := *(*float64)(ptr)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, false
		}
		var scratch [32]byte
		return bytes.Clone(jsonwire.AppendFloat(scratch[:0], fv, 64)), true
	case fkByteArray:
		if !mo.Flags.Get(jsonflags.FormatByteArrayAsArray) {
			s := unsafe.Slice((*byte)(ptr), plan.arrayLen)
			encLen := base64.StdEncoding.EncodedLen(len(s))
			out := make([]byte, encLen+2)
			out[0] = '"'
			base64.StdEncoding.Encode(out[1:], s)
			out[encLen+1] = '"'
			return out, true
		}
	}
	fb := getFastBuf()
	b, err := plan.marshal(fb.b, ptr, mo, 0)
	if err != nil {
		putFastBuf(fb)
		return nil, false
	}
	fb.b = b
	out := bytes.Clone(b)
	putFastBuf(fb)
	return out, true
}

// ============================================================================
// Fast Direct-Memory Unmarshal Engine
// ============================================================================

type fastUnmarshalField struct {
	id              int
	orderIdx        int
	offset          uintptr
	name            string
	quotedNameColon string // pre-quoted `"name":` when !nameNeedEscape
	casing          int8
	quoted          bool
	kind            fastKind
	plan            *fastUnmarshalPlan
}

type fastUnmarshalPlan struct {
	typ            *abi.Type
	rt             reflect.Type
	mapType        *abi.MapType
	ifaceTab       unsafe.Pointer
	kind           fastKind
	keyBits        uint8
	canFast        bool
	hasQuoted      bool
	elemSize       uintptr
	arrayLen       int
	emptySliceData unsafe.Pointer
	elemPlan       *fastUnmarshalPlan
	elemRT         reflect.Type
	fields         []fastUnmarshalField
	byActualName   map[string]*fastUnmarshalField
	byFoldedName   map[string][]*fastUnmarshalField
	hasCaseIgnore  bool
}

var fastUnmarshalCache sync.Map[*abi.Type, *fastUnmarshalPlan]

func lookupFastUnmarshalPlan(at *abi.Type) *fastUnmarshalPlan {
	if p, ok := fastUnmarshalCache.Load(at); ok {
		return p
	}
	visiting := make(map[*abi.Type]*fastUnmarshalPlan)
	p := buildFastUnmarshalPlan(abiTypeToReflect(at), at, visiting)
	finalizeFastUnmarshalPlans(visiting)
	for kt, kp := range visiting {
		fastUnmarshalCache.LoadOrStore(kt, kp)
	}
	return p
}

func finalizeFastUnmarshalPlans(visiting map[*abi.Type]*fastUnmarshalPlan) {
	for _, p := range visiting {
		if p.kind == fkStruct {
			for i := range p.fields {
				f := &p.fields[i]
				if f.plan != nil {
					f.kind = f.plan.kind
				}
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, p := range visiting {
			if !p.canFast {
				continue
			}
			switch p.kind {
			case fkPtr, fkSlice, fkArray, fkMapStringElem, fkMapIntElem, fkMapUintElem:
				if p.elemPlan == nil || !p.elemPlan.canFast {
					p.canFast = false
					changed = true
				}
			case fkStruct:
				for i := range p.fields {
					f := &p.fields[i]
					if f.plan != nil && !f.plan.canFast {
						p.canFast = false
						changed = true
						break
					}
				}
			}
		}
	}
}

func hasAnyUnmarshalerMethod(rt reflect.Type) bool {
	if rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Interface {
		return false
	}
	return implementsAny(rt, allUnmarshalerTypes...)
}

func buildFastUnmarshalPlan(rt reflect.Type, at *abi.Type, visiting map[*abi.Type]*fastUnmarshalPlan) *fastUnmarshalPlan {
	if p, ok := fastUnmarshalCache.Load(at); ok {
		return p
	}
	if p, ok := visiting[at]; ok {
		return p
	}
	p := &fastUnmarshalPlan{typ: at, rt: rt}
	visiting[at] = p

	if rt == timeTimeType {
		p.kind = fkTime
		p.canFast = true
		return p
	}
	if rt == timeDurationType {
		return p // standard path handles duration formats
	}
	if hasAnyUnmarshalerMethod(rt) {
		if !implementsAny(rt, jsonUnmarshalerFromType) {
			if _, ok := implements(rt, jsonUnmarshalerType); ok {
				u := reflect.New(rt).Interface().(Unmarshaler)
				p.ifaceTab = (*[2]unsafe.Pointer)(unsafe.Pointer(&u))[0]
				p.kind = fkUnmarshaler
				p.canFast = true
				return p
			}
		}
		return p
	}

	switch rt.Kind() {
	case reflect.Bool:
		p.kind = fkBool
		p.canFast = true
	case reflect.String:
		p.kind = fkString
		p.canFast = true
	case reflect.Int:
		p.kind = fkInt
		p.canFast = true
	case reflect.Int8:
		p.kind = fkInt8
		p.canFast = true
	case reflect.Int16:
		p.kind = fkInt16
		p.canFast = true
	case reflect.Int32:
		p.kind = fkInt32
		p.canFast = true
	case reflect.Int64:
		p.kind = fkInt64
		p.canFast = true
	case reflect.Uint:
		p.kind = fkUint
		p.canFast = true
	case reflect.Uint8:
		p.kind = fkUint8
		p.canFast = true
	case reflect.Uint16:
		p.kind = fkUint16
		p.canFast = true
	case reflect.Uint32:
		p.kind = fkUint32
		p.canFast = true
	case reflect.Uint64:
		p.kind = fkUint64
		p.canFast = true
	case reflect.Uintptr:
		p.kind = fkUintptr
		p.canFast = true
	case reflect.Float32:
		p.kind = fkFloat32
		p.canFast = true
	case reflect.Float64:
		p.kind = fkFloat64
		p.canFast = true

	case reflect.Slice:
		elem := rt.Elem()
		if elem.Kind() == reflect.Uint8 {
			if rt == reflect.TypeFor[[]byte]() {
				p.kind = fkBytes
				p.canFast = true
			}
			return p
		}
		ep := buildFastUnmarshalPlan(elem, abiTypeOfReflect(elem), visiting)
		p.elemPlan = ep
		p.elemRT = elem
		p.elemSize = elem.Size()
		p.emptySliceData = reflect.MakeSlice(rt, 0, 0).UnsafePointer()
		switch ep.kind {
		case fkBool:
			p.kind = fkSliceBool
		case fkString:
			p.kind = fkSliceString
		case fkInt:
			p.kind = fkSliceInt
		case fkInt64:
			p.kind = fkSliceInt64
		case fkUint64:
			p.kind = fkSliceUint64
		case fkFloat64:
			p.kind = fkSliceFloat64
		case fkFloat32:
			p.kind = fkSliceFloat32
		default:
			p.kind = fkSlice
		}
		p.canFast = true

	case reflect.Array:
		elem := rt.Elem()
		if elem.Kind() == reflect.Uint8 {
			if elem.PkgPath() == "" && !hasAnyUnmarshalerMethod(elem) {
				p.kind = fkByteArray
				p.arrayLen = rt.Len()
				p.canFast = true
			}
			return p
		}
		ep := buildFastUnmarshalPlan(elem, abiTypeOfReflect(elem), visiting)
		p.elemPlan = ep
		p.elemRT = elem
		p.elemSize = elem.Size()
		p.arrayLen = rt.Len()
		p.kind = fkArray
		p.canFast = true

	case reflect.Pointer:
		elem := rt.Elem()
		ep := buildFastUnmarshalPlan(elem, abiTypeOfReflect(elem), visiting)
		p.elemPlan = ep
		p.elemRT = elem
		p.kind = fkPtr
		p.canFast = true

	case reflect.Interface:
		if rt == reflect.TypeFor[any]() {
			p.kind = fkAny
			p.canFast = true
		}

	case reflect.Map:
		if rt == reflect.TypeFor[map[string]string]() {
			p.kind = fkMapStringString
			p.canFast = true
		} else if rt == reflect.TypeFor[map[string]int]() {
			p.kind = fkMapStringInt
			p.canFast = true
		} else if rt == reflect.TypeFor[map[string]any]() {
			p.kind = fkMapStringAny
			p.canFast = true
		} else {
			kt := rt.Key()
			if kt == timeDurationType || hasAnyUnmarshalerMethod(kt) {
				return p
			}
			switch kt.Kind() {
			case reflect.String:
				p.kind = fkMapStringElem
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				p.kind = fkMapIntElem
				p.keyBits = uint8(kt.Bits())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				p.kind = fkMapUintElem
				p.keyBits = uint8(kt.Bits())
			default:
				return p
			}
			elem := rt.Elem()
			p.elemPlan = buildFastUnmarshalPlan(elem, abiTypeOfReflect(elem), visiting)
			p.elemRT = elem
			p.elemSize = elem.Size()
			p.mapType = (*abi.MapType)(unsafe.Pointer(at))
			p.canFast = true
		}

	case reflect.Struct:
		fields, errInit := lookupStructFields(rt)
		if errInit != nil || fields.errUnsupportedFormat != nil || fields.embeddedFallback != nil {
			return p
		}
		fastFields := make([]fastUnmarshalField, 0, len(fields.flattened))
		hasCaseIgnore := false
		hasQuoted := false
		for i := range fields.flattened {
			f := &fields.flattened[i]
			if f.format != "" {
				return p
			}
			off, ok := directFieldOffset(rt, f.index0, f.index, f.typ)
			if !ok {
				return p
			}
			fp := buildFastUnmarshalPlan(f.typ, abiTypeOfReflect(f.typ), visiting)
			if f.string {
				if !isFastNumericKind(fp.kind) {
					return p
				}
				hasQuoted = true
			}
			if f.casing == caseIgnore {
				hasCaseIgnore = true
			}
			ff := fastUnmarshalField{
				id:       f.id,
				orderIdx: i,
				offset:   off,
				name:     f.name,
				casing:   f.casing,
				quoted:   f.string,
				kind:     fp.kind,
				plan:     fp,
			}
			if !f.nameNeedEscape {
				ff.quotedNameColon = f.quotedName + ":"
			}
			fastFields = append(fastFields, ff)
		}
		byActual := make(map[string]*fastUnmarshalField, len(fastFields))
		byFolded := make(map[string][]*fastUnmarshalField, len(fastFields))
		for i := range fastFields {
			ff := &fastFields[i]
			byActual[ff.name] = ff
			folded := string(foldName([]byte(ff.name)))
			byFolded[folded] = append(byFolded[folded], ff)
		}
		for folded, list := range byFolded {
			if len(list) > 1 {
				slices.SortFunc(list, func(x, y *fastUnmarshalField) int {
					if x.id < y.id {
						return -1
					}
					if x.id > y.id {
						return 1
					}
					return 0
				})
				byFolded[folded] = list
			}
		}
		p.kind = fkStruct
		p.fields = fastFields
		p.byActualName = byActual
		p.byFoldedName = byFolded
		p.hasCaseIgnore = hasCaseIgnore
		p.hasQuoted = hasQuoted
		p.canFast = true
	}
	return p
}

const fastUnmarshalDisallowedFlags = jsonflags.StringifyNumbers | jsonflags.TagFlags

func canFastUnmarshalFlags(uo *jsonopts.Struct) bool {
	return uo.Unmarshalers == nil && !uo.Flags.Get(fastUnmarshalDisallowedFlags)
}

type fastDecoder struct {
	data         []byte
	pos          int
	flags        jsonflags.Flags
	strCache     *stringCache
	allowDup     bool
	allowBadUTF8 bool
	legacyMerge  bool
	caseInsens   bool
	caseSensDel  bool
	rejectUnk    bool
}

const (
	fswarLo8 = 0x0101010101010101
	fswarHi8 = 0x8080808080808080
	fswarSp8 = 0x2020202020202020
	fswarQt8 = 0x2222222222222222
	fswarBs8 = 0x5c5c5c5c5c5c5c5c
)

func fswarEscapeMask(w uint64) uint64 {
	xq := w ^ fswarQt8
	xb := w ^ fswarBs8
	return (w & fswarHi8) |
		((w - fswarSp8) & fswarHi8) |
		((xq - fswarLo8) &^ xq & fswarHi8) |
		((xb - fswarLo8) &^ xb & fswarHi8)
}

func (d *fastDecoder) skipWS() {
	data, pos := d.data, d.pos
	for pos < len(data) {
		c := data[pos]
		if c > ' ' || (c != ' ' && c != '\t' && c != '\r' && c != '\n') {
			break
		}
		pos++
	}
	d.pos = pos
}

// readString reads a JSON string starting at d.pos (which must point to '"').
// Returns the unquoted bytes (pointing directly into d.data if unescaped, or newly allocated).
func (d *fastDecoder) readString() ([]byte, error) {
	data := d.data
	pos := d.pos
	if pos >= len(data) || data[pos] != '"' {
		return nil, errFastFallback
	}
	start := pos + 1
	i := start
	for len(data)-i >= 8 {
		if m := fswarEscapeMask(byteorder.LEUint64(data[i:])); m != 0 {
			i += bits.TrailingZeros64(m) >> 3
			goto special
		}
		i += 8
	}
	for i < len(data) {
		c := data[i]
		if c >= utf8.RuneSelf || c < ' ' || c == '\\' || c == '"' {
			break
		}
		i++
	}
special:
	if i < len(data) && data[i] == '"' {
		d.pos = i + 1
		return data[start:i], nil
	}
	// Has escape or non-ASCII: use jsonwire.ConsumeString + UnquoteMayCopy.
	var flags jsonwire.ValueFlags
	n, err := jsonwire.ConsumeString(&flags, data[pos:], !d.allowBadUTF8)
	if err != nil {
		return nil, errFastFallback
	}
	d.pos = pos + n
	return jsonwire.UnquoteMayCopy(data[pos:pos+n], flags.IsVerbatim()), nil
}

func (d *fastDecoder) readInt64(bits int) (int64, error) {
	data := d.data
	pos := d.pos
	if pos >= len(data) {
		return 0, errFastFallback
	}
	neg := false
	if data[pos] == '-' {
		neg = true
		pos++
		if pos >= len(data) {
			return 0, errFastFallback
		}
	}
	c := data[pos]
	var v uint64
	if c == '0' {
		pos++
		if pos < len(data) {
			nc := data[pos]
			if ('0' <= nc && nc <= '9') || nc == '.' || nc == 'e' || nc == 'E' {
				return 0, errFastFallback
			}
		}
	} else if '1' <= c && c <= '9' {
		v = uint64(c - '0')
		pos++
		startDigits := pos - 1
		for pos < len(data) {
			dc := data[pos]
			if dc < '0' || dc > '9' {
				break
			}
			v = v*10 + uint64(dc-'0')
			pos++
		}
		if pos < len(data) && (data[pos] == '.' || data[pos] == 'e' || data[pos] == 'E') {
			return 0, errFastFallback
		}
		nDigits := pos - startDigits
		if nDigits >= 20 && (data[startDigits] != '1' || v < 1e19 || nDigits > 20) {
			return 0, errFastFallback
		}
	} else {
		return 0, errFastFallback
	}
	maxInt := uint64(1) << (bits - 1)
	if (neg && v > maxInt) || (!neg && v > maxInt-1) {
		return 0, errFastFallback
	}
	d.pos = pos
	if neg {
		return int64(-v), nil
	}
	return int64(v), nil
}

func (d *fastDecoder) readUint64(bits int) (uint64, error) {
	data := d.data
	pos := d.pos
	if pos >= len(data) {
		return 0, errFastFallback
	}
	c := data[pos]
	var v uint64
	if c == '0' {
		pos++
		if pos < len(data) {
			nc := data[pos]
			if ('0' <= nc && nc <= '9') || nc == '.' || nc == 'e' || nc == 'E' {
				return 0, errFastFallback
			}
		}
	} else if '1' <= c && c <= '9' {
		v = uint64(c - '0')
		pos++
		startDigits := pos - 1
		for pos < len(data) {
			dc := data[pos]
			if dc < '0' || dc > '9' {
				break
			}
			v = v*10 + uint64(dc-'0')
			pos++
		}
		if pos < len(data) && (data[pos] == '.' || data[pos] == 'e' || data[pos] == 'E') {
			return 0, errFastFallback
		}
		nDigits := pos - startDigits
		if nDigits >= 20 && (data[startDigits] != '1' || v < 1e19 || nDigits > 20) {
			return 0, errFastFallback
		}
	} else {
		return 0, errFastFallback
	}
	maxUint := uint64(1)<<bits - 1
	if bits == 64 {
		maxUint = math.MaxUint64
	}
	if v > maxUint {
		return 0, errFastFallback
	}
	d.pos = pos
	return v, nil
}

var exactPow10F64 = [23]float64{
	1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10,
	1e11, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18, 1e19, 1e20,
	1e21, 1e22,
}

var exactPow10F32 = [11]float32{
	1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10,
}

func (d *fastDecoder) readFloat(bits int) (float64, error) {
	data := d.data
	start := d.pos
	i := start
	n := len(data)
	if i >= n {
		return 0, errFastFallback
	}
	neg := false
	if data[i] == '-' {
		neg = true
		i++
		if i >= n {
			return 0, errFastFallback
		}
	}
	var mant uint64
	exact := true
	const maxMantDiv10 = (1<<53 - 1) / 10 // 900719925474099; maxMantDiv10*10 + 9 < 1<<53
	c := data[i]
	if c == '0' {
		i++
		if i < n && '0' <= data[i] && data[i] <= '9' {
			return 0, errFastFallback
		}
	} else if '1' <= c && c <= '9' {
		mant = uint64(c - '0')
		i++
		for i < n {
			dc := data[i]
			if dc < '0' || dc > '9' {
				break
			}
			if exact {
				if mant < maxMantDiv10 {
					mant = mant*10 + uint64(dc-'0')
				} else {
					exact = false
				}
			}
			i++
		}
	} else {
		return 0, errFastFallback
	}

	exp := 0
	if i < n && data[i] == '.' {
		i++
		if i >= n || data[i] < '0' || data[i] > '9' {
			return 0, errFastFallback
		}
		for i < n {
			dc := data[i]
			if dc < '0' || dc > '9' {
				break
			}
			if exact {
				if mant < maxMantDiv10 {
					mant = mant*10 + uint64(dc-'0')
					exp--
				} else {
					exact = false
				}
			}
			i++
		}
	}

	if i < n && (data[i] == 'e' || data[i] == 'E') {
		i++
		expNeg := false
		if i < n && (data[i] == '+' || data[i] == '-') {
			expNeg = data[i] == '-'
			i++
		}
		if i >= n || data[i] < '0' || data[i] > '9' {
			return 0, errFastFallback
		}
		expVal := 0
		for i < n {
			dc := data[i]
			if dc < '0' || dc > '9' {
				break
			}
			if expVal < 1000 {
				expVal = expVal*10 + int(dc-'0')
			}
			i++
		}
		if expNeg {
			exp -= expVal
		} else {
			exp += expVal
		}
	}

	if exact {
		if bits == 64 {
			if exp >= -22 && exp <= 22 {
				f := float64(mant)
				if exp > 0 {
					f *= exactPow10F64[exp]
				} else if exp < 0 {
					f /= exactPow10F64[-exp]
				}
				if neg {
					f = -f
				}
				d.pos = i
				return f, nil
			}
		} else if mant < 1<<24 && exp >= -10 && exp <= 10 {
			f := float32(mant)
			if exp > 0 {
				f *= exactPow10F32[exp]
			} else if exp < 0 {
				f /= exactPow10F32[-exp]
			}
			if neg {
				f = -f
			}
			d.pos = i
			return float64(f), nil
		}
	}

	raw := data[start:i]
	fv, err := strconv.ParseFloat(unsafe.String(unsafe.SliceData(raw), len(raw)), bits)
	if err != nil {
		return 0, errFastFallback
	}
	d.pos = i
	return fv, nil
}

func (d *fastDecoder) consumeNull() bool {
	if n := jsonwire.ConsumeNull(d.data[d.pos:]); n > 0 {
		d.pos += n
		return true
	}
	return false
}

func (d *fastDecoder) skipValue(depth int) error {
	if depth >= 10000 {
		return errFastFallback
	}
	var stackBuf [64]byte
	stack := stackBuf[:0]
	const (
		stExpectValue = 0
		stAfterValue  = 1
	)
	state := stExpectValue

	for {
		if state == stExpectValue {
			d.skipWS()
			if d.pos >= len(d.data) {
				return errFastFallback
			}
			switch d.data[d.pos] {
			case '"':
				if _, err := d.readString(); err != nil {
					return err
				}
				state = stAfterValue
			case 't':
				if n := jsonwire.ConsumeTrue(d.data[d.pos:]); n > 0 {
					d.pos += n
					state = stAfterValue
				} else {
					return errFastFallback
				}
			case 'f':
				if n := jsonwire.ConsumeFalse(d.data[d.pos:]); n > 0 {
					d.pos += n
					state = stAfterValue
				} else {
					return errFastFallback
				}
			case 'n':
				if n := jsonwire.ConsumeNull(d.data[d.pos:]); n > 0 {
					d.pos += n
					state = stAfterValue
				} else {
					return errFastFallback
				}
			case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
				n, err := jsonwire.ConsumeNumber(d.data[d.pos:])
				if err != nil {
					return errFastFallback
				}
				d.pos += n
				state = stAfterValue
			case '[':
				if depth+len(stack) >= 10000 {
					return errFastFallback
				}
				d.pos++
				d.skipWS()
				if d.pos < len(d.data) && d.data[d.pos] == ']' {
					d.pos++
					state = stAfterValue
					continue
				}
				stack = append(stack, ']')
				continue
			case '{':
				if !d.allowDup {
					return errFastFallback
				}
				if depth+len(stack) >= 10000 {
					return errFastFallback
				}
				d.pos++
				d.skipWS()
				if d.pos < len(d.data) && d.data[d.pos] == '}' {
					d.pos++
					state = stAfterValue
					continue
				}
				if _, err := d.readString(); err != nil {
					return err
				}
				d.skipWS()
				if d.pos >= len(d.data) || d.data[d.pos] != ':' {
					return errFastFallback
				}
				d.pos++
				stack = append(stack, '}')
				continue
			default:
				return errFastFallback
			}
		}

		if len(stack) == 0 {
			return nil
		}
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		top := stack[len(stack)-1]
		c := d.data[d.pos]
		if c == top {
			d.pos++
			stack = stack[:len(stack)-1]
			continue
		}
		if c != ',' {
			return errFastFallback
		}
		d.pos++
		if top == '}' {
			d.skipWS()
			if _, err := d.readString(); err != nil {
				return err
			}
			d.skipWS()
			if d.pos >= len(d.data) || d.data[d.pos] != ':' {
				return errFastFallback
			}
			d.pos++
		}
		state = stExpectValue
	}
}

func (d *fastDecoder) unmarshal(p *fastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	if depth >= 10000 {
		return errFastFallback
	}
	d.skipWS()
	if d.pos >= len(d.data) {
		return errFastFallback
	}
	c := d.data[d.pos]

	switch p.kind {
	case fkBool:
		switch c {
		case 't':
			if n := jsonwire.ConsumeTrue(d.data[d.pos:]); n > 0 {
				d.pos += n
				*(*bool)(ptr) = true
				return nil
			}
		case 'f':
			if n := jsonwire.ConsumeFalse(d.data[d.pos:]); n > 0 {
				d.pos += n
				*(*bool)(ptr) = false
				return nil
			}
		case 'n':
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*bool)(ptr) = false
				}
				return nil
			}
		}
		return errFastFallback

	case fkString:
		switch c {
		case '"':
			b, err := d.readString()
			if err != nil {
				return err
			}
			*(*string)(ptr) = makeString(d.strCache, b)
			return nil
		case 'n':
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*string)(ptr) = ""
				}
				return nil
			}
		}
		return errFastFallback

	case fkInt:
		if c == 'n' {
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*int)(ptr) = 0
				}
				return nil
			}
			return errFastFallback
		}
		v, err := d.readInt64(strconv.IntSize)
		if err != nil {
			return err
		}
		*(*int)(ptr) = int(v)
		return nil

	case fkInt8:
		if c == 'n' {
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*int8)(ptr) = 0
				}
				return nil
			}
			return errFastFallback
		}
		v, err := d.readInt64(8)
		if err != nil {
			return err
		}
		*(*int8)(ptr) = int8(v)
		return nil

	case fkInt16:
		if c == 'n' {
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*int16)(ptr) = 0
				}
				return nil
			}
			return errFastFallback
		}
		v, err := d.readInt64(16)
		if err != nil {
			return err
		}
		*(*int16)(ptr) = int16(v)
		return nil

	case fkInt32:
		if c == 'n' {
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*int32)(ptr) = 0
				}
				return nil
			}
			return errFastFallback
		}
		v, err := d.readInt64(32)
		if err != nil {
			return err
		}
		*(*int32)(ptr) = int32(v)
		return nil

	case fkInt64:
		if c == 'n' {
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*int64)(ptr) = 0
				}
				return nil
			}
			return errFastFallback
		}
		v, err := d.readInt64(64)
		if err != nil {
			return err
		}
		*(*int64)(ptr) = v
		return nil

	case fkUint:
		if c == 'n' {
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*uint)(ptr) = 0
				}
				return nil
			}
			return errFastFallback
		}
		v, err := d.readUint64(strconv.IntSize)
		if err != nil {
			return err
		}
		*(*uint)(ptr) = uint(v)
		return nil

	case fkUint8:
		if c == 'n' {
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*uint8)(ptr) = 0
				}
				return nil
			}
			return errFastFallback
		}
		v, err := d.readUint64(8)
		if err != nil {
			return err
		}
		*(*uint8)(ptr) = uint8(v)
		return nil

	case fkUint16:
		if c == 'n' {
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*uint16)(ptr) = 0
				}
				return nil
			}
			return errFastFallback
		}
		v, err := d.readUint64(16)
		if err != nil {
			return err
		}
		*(*uint16)(ptr) = uint16(v)
		return nil

	case fkUint32:
		if c == 'n' {
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*uint32)(ptr) = 0
				}
				return nil
			}
			return errFastFallback
		}
		v, err := d.readUint64(32)
		if err != nil {
			return err
		}
		*(*uint32)(ptr) = uint32(v)
		return nil

	case fkUint64:
		if c == 'n' {
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*uint64)(ptr) = 0
				}
				return nil
			}
			return errFastFallback
		}
		v, err := d.readUint64(64)
		if err != nil {
			return err
		}
		*(*uint64)(ptr) = v
		return nil

	case fkUintptr:
		if c == 'n' {
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*uintptr)(ptr) = 0
				}
				return nil
			}
			return errFastFallback
		}
		v, err := d.readUint64(int(unsafe.Sizeof(uintptr(0)) * 8))
		if err != nil {
			return err
		}
		*(*uintptr)(ptr) = uintptr(v)
		return nil

	case fkFloat32:
		if c == 'n' {
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*float32)(ptr) = 0
				}
				return nil
			}
			return errFastFallback
		}
		v, err := d.readFloat(32)
		if err != nil {
			return err
		}
		*(*float32)(ptr) = float32(v)
		return nil

	case fkFloat64:
		if c == 'n' {
			if d.consumeNull() {
				if !d.legacyMerge {
					*(*float64)(ptr) = 0
				}
				return nil
			}
			return errFastFallback
		}
		v, err := d.readFloat(64)
		if err != nil {
			return err
		}
		*(*float64)(ptr) = v
		return nil

	case fkPtr:
		if c == 'n' {
			if d.consumeNull() {
				*(*unsafe.Pointer)(ptr) = nil
				return nil
			}
			return errFastFallback
		}
		elemPtr := *(*unsafe.Pointer)(ptr)
		if elemPtr == nil {
			nv := reflect.New(p.elemRT)
			elemPtr = nv.UnsafePointer()
			*(*unsafe.Pointer)(ptr) = elemPtr
		}
		return d.unmarshal(p.elemPlan, elemPtr, depth+1)

	case fkSliceString:
		return d.unmarshalSliceString(ptr)

	case fkSliceInt:
		return d.unmarshalSliceInt(ptr)

	case fkSliceInt64:
		return d.unmarshalSliceInt64(ptr)

	case fkSliceUint64:
		return d.unmarshalSliceUint64(ptr)

	case fkSliceFloat64:
		return d.unmarshalSliceFloat64(ptr)

	case fkSliceFloat32:
		return d.unmarshalSliceFloat32(ptr)

	case fkSliceBool:
		return d.unmarshalSliceBool(ptr)

	case fkTime:
		return d.unmarshalTime(ptr)

	case fkBytes:
		return d.unmarshalBytes(ptr)

	case fkByteArray:
		return d.unmarshalByteArray(p, ptr)

	case fkSlice:
		return d.unmarshalSlice(p, ptr, depth+1)

	case fkArray:
		return d.unmarshalArray(p, ptr, depth+1)

	case fkMapStringString:
		return d.unmarshalMapStringString(ptr)

	case fkMapStringInt:
		return d.unmarshalMapStringInt(ptr)

	case fkMapStringAny:
		return d.unmarshalMapStringAny(ptr, depth+1)

	case fkMapStringElem:
		return d.unmarshalMapStringElem(p, ptr, depth+1)

	case fkMapIntElem:
		return d.unmarshalMapIntElem(p, ptr, depth+1)

	case fkMapUintElem:
		return d.unmarshalMapUintElem(p, ptr, depth+1)

	case fkAny:
		return d.unmarshalAny(ptr, depth+1)

	case fkUnmarshaler:
		return d.unmarshalCustomUnmarshaler(p, ptr, depth+1)

	case fkStruct:
		return d.unmarshalStruct(p, ptr, depth+1)
	}
	return errFastFallback
}

func (d *fastDecoder) unmarshalCustomUnmarshaler(p *fastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	start := d.pos
	if err := d.skipValueChecked(depth); err != nil {
		return err
	}
	raw := d.data[start:d.pos]
	var u Unmarshaler
	ip := (*[2]unsafe.Pointer)(unsafe.Pointer(&u))
	ip[0] = p.ifaceTab
	ip[1] = ptr
	if err := u.UnmarshalJSON(raw); err != nil {
		return errFastFallback
	}
	return nil
}

func (d *fastDecoder) skipValueChecked(depth int) error {
	if d.allowDup {
		return d.skipValue(depth)
	}
	if depth >= 64 {
		return errFastFallback
	}
	d.skipWS()
	if d.pos >= len(d.data) {
		return errFastFallback
	}
	switch d.data[d.pos] {
	case '"':
		_, err := d.readString()
		return err
	case 't':
		if n := jsonwire.ConsumeTrue(d.data[d.pos:]); n > 0 {
			d.pos += n
			return nil
		}
		return errFastFallback
	case 'f':
		if n := jsonwire.ConsumeFalse(d.data[d.pos:]); n > 0 {
			d.pos += n
			return nil
		}
		return errFastFallback
	case 'n':
		if n := jsonwire.ConsumeNull(d.data[d.pos:]); n > 0 {
			d.pos += n
			return nil
		}
		return errFastFallback
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		n, err := jsonwire.ConsumeNumber(d.data[d.pos:])
		if err != nil {
			return errFastFallback
		}
		d.pos += n
		return nil
	case '[':
		d.pos++
		d.skipWS()
		if d.pos < len(d.data) && d.data[d.pos] == ']' {
			d.pos++
			return nil
		}
		for {
			if err := d.skipValueChecked(depth + 1); err != nil {
				return err
			}
			d.skipWS()
			if d.pos >= len(d.data) {
				return errFastFallback
			}
			if d.data[d.pos] == ',' {
				d.pos++
				continue
			}
			if d.data[d.pos] == ']' {
				d.pos++
				return nil
			}
			return errFastFallback
		}
	case '{':
		if d.allowBadUTF8 {
			return errFastFallback
		}
		d.pos++
		d.skipWS()
		if d.pos < len(d.data) && d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		var keys [16][]byte
		numKeys := 0
		for {
			d.skipWS()
			kb, err := d.readString()
			if err != nil {
				return err
			}
			if numKeys >= len(keys) {
				return errFastFallback
			}
			for j := range numKeys {
				if bytes.Equal(keys[j], kb) {
					return errFastFallback
				}
			}
			keys[numKeys] = kb
			numKeys++
			d.skipWS()
			if d.pos >= len(d.data) || d.data[d.pos] != ':' {
				return errFastFallback
			}
			d.pos++
			if err := d.skipValueChecked(depth + 1); err != nil {
				return err
			}
			d.skipWS()
			if d.pos >= len(d.data) {
				return errFastFallback
			}
			if d.data[d.pos] == ',' {
				d.pos++
				continue
			}
			if d.data[d.pos] == '}' {
				d.pos++
				return nil
			}
			return errFastFallback
		}
	}
	return errFastFallback
}

func (d *fastDecoder) unmarshalTime(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			if !d.legacyMerge {
				*(*time.Time)(ptr) = time.Time{}
			}
			return nil
		}
		return errFastFallback
	}
	if c != '"' {
		return errFastFallback
	}
	b, err := d.readString()
	if err != nil {
		return err
	}
	var tt time.Time
	if err := tt.UnmarshalText(b); err != nil {
		return errFastFallback
	}
	if !d.flags.Get(jsonflags.ParseTimeWithLooseRFC3339) {
		switch {
		case b[len("2006-01-02T")+1] == ':':
			return errFastFallback
		case b[len("2006-01-02T15:04:05")] == ',':
			return errFastFallback
		case b[len(b)-1] != 'Z':
			if parseDec2(b[len(b)-len("07:00"):]) >= 24 || parseDec2(b[len(b)-len("00"):]) >= 60 {
				return errFastFallback
			}
		}
	}
	*(*time.Time)(ptr) = tt
	return nil
}

func (d *fastDecoder) unmarshalByteArray(p *fastUnmarshalPlan, ptr unsafe.Pointer) error {
	if d.flags.Get(jsonflags.FormatByteArrayAsArray) {
		return errFastFallback
	}
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			if !d.legacyMerge {
				clear(unsafe.Slice((*byte)(ptr), p.arrayLen))
			}
			return nil
		}
		return errFastFallback
	}
	if c != '"' {
		return errFastFallback
	}
	b, err := d.readString()
	if err != nil {
		return err
	}
	dst := unsafe.Slice((*byte)(ptr), p.arrayLen)
	if len(b) == 0 {
		if p.arrayLen != 0 && !d.flags.Get(jsonflags.UnmarshalArrayFromAnyLength) {
			return errFastFallback
		}
		clear(dst)
		return nil
	}
	if len(b)%4 != 0 {
		return errFastFallback
	}
	maxLen := base64StdEncoding.DecodedLen(len(b))
	var stackBuf [64]byte
	var tmp []byte
	if maxLen <= len(stackBuf) {
		tmp = stackBuf[:maxLen]
	} else {
		tmp = make([]byte, maxLen)
	}
	n, decErr := base64StdEncoding.Decode(tmp, b)
	if decErr != nil {
		return errFastFallback
	}
	if len(b) != base64StdEncoding.EncodedLen(n) && !d.flags.Get(jsonflags.ParseBytesWithLooseRFC4648) {
		return errFastFallback
	}
	if n != p.arrayLen && !d.flags.Get(jsonflags.UnmarshalArrayFromAnyLength) {
		return errFastFallback
	}
	clear(dst[copy(dst, tmp[:n]):])
	return nil
}

func (d *fastDecoder) unmarshalQuotedNumber(kind fastKind, fp unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if !d.consumeNull() {
			return errFastFallback
		}
		if !d.legacyMerge {
			zeroNumericField(kind, fp)
		}
		return nil
	}
	if c != '"' {
		return errFastFallback
	}
	s, err := d.readString()
	if err != nil {
		return err
	}
	if len(s) == 4 && string(s) == "null" {
		if d.flags.Get(jsonflags.StringifyWithLegacySemantics) {
			if !d.legacyMerge {
				zeroNumericField(kind, fp)
			}
			return nil
		}
		return errFastFallback
	}
	sub := fastDecoder{data: s}
	switch kind {
	case fkInt:
		v, err := sub.readInt64(strconv.IntSize)
		if err != nil || sub.pos != len(s) {
			return errFastFallback
		}
		*(*int)(fp) = int(v)
	case fkInt8:
		v, err := sub.readInt64(8)
		if err != nil || sub.pos != len(s) {
			return errFastFallback
		}
		*(*int8)(fp) = int8(v)
	case fkInt16:
		v, err := sub.readInt64(16)
		if err != nil || sub.pos != len(s) {
			return errFastFallback
		}
		*(*int16)(fp) = int16(v)
	case fkInt32:
		v, err := sub.readInt64(32)
		if err != nil || sub.pos != len(s) {
			return errFastFallback
		}
		*(*int32)(fp) = int32(v)
	case fkInt64:
		v, err := sub.readInt64(64)
		if err != nil || sub.pos != len(s) {
			return errFastFallback
		}
		*(*int64)(fp) = v
	case fkUint:
		v, err := sub.readUint64(strconv.IntSize)
		if err != nil || sub.pos != len(s) {
			return errFastFallback
		}
		*(*uint)(fp) = uint(v)
	case fkUint8:
		v, err := sub.readUint64(8)
		if err != nil || sub.pos != len(s) {
			return errFastFallback
		}
		*(*uint8)(fp) = uint8(v)
	case fkUint16:
		v, err := sub.readUint64(16)
		if err != nil || sub.pos != len(s) {
			return errFastFallback
		}
		*(*uint16)(fp) = uint16(v)
	case fkUint32:
		v, err := sub.readUint64(32)
		if err != nil || sub.pos != len(s) {
			return errFastFallback
		}
		*(*uint32)(fp) = uint32(v)
	case fkUint64:
		v, err := sub.readUint64(64)
		if err != nil || sub.pos != len(s) {
			return errFastFallback
		}
		*(*uint64)(fp) = v
	case fkUintptr:
		v, err := sub.readUint64(int(unsafe.Sizeof(uintptr(0)) * 8))
		if err != nil || sub.pos != len(s) {
			return errFastFallback
		}
		*(*uintptr)(fp) = uintptr(v)
	case fkFloat32:
		v, err := sub.readFloat(32)
		if err != nil || sub.pos != len(s) {
			return errFastFallback
		}
		*(*float32)(fp) = float32(v)
	case fkFloat64:
		v, err := sub.readFloat(64)
		if err != nil || sub.pos != len(s) {
			return errFastFallback
		}
		*(*float64)(fp) = v
	default:
		return errFastFallback
	}
	return nil
}

func zeroNumericField(kind fastKind, fp unsafe.Pointer) {
	switch kind {
	case fkInt:
		*(*int)(fp) = 0
	case fkInt8:
		*(*int8)(fp) = 0
	case fkInt16:
		*(*int16)(fp) = 0
	case fkInt32:
		*(*int32)(fp) = 0
	case fkInt64:
		*(*int64)(fp) = 0
	case fkUint:
		*(*uint)(fp) = 0
	case fkUint8:
		*(*uint8)(fp) = 0
	case fkUint16:
		*(*uint16)(fp) = 0
	case fkUint32:
		*(*uint32)(fp) = 0
	case fkUint64:
		*(*uint64)(fp) = 0
	case fkUintptr:
		*(*uintptr)(fp) = 0
	case fkFloat32:
		*(*float32)(fp) = 0
	case fkFloat64:
		*(*float64)(fp) = 0
	}
}

func parseStrictInt64Key(kb []byte, bits int) (int64, bool) {
	if len(kb) == 0 {
		return 0, false
	}
	if kb[0] == '-' && len(kb) == 2 && kb[1] == '0' {
		return 0, false // let standard path handle "-0" / duplicate "0" nuances
	}
	sub := fastDecoder{data: kb}
	v, err := sub.readInt64(bits)
	if err != nil || sub.pos != len(kb) {
		return 0, false
	}
	return v, true
}

func parseStrictUint64Key(kb []byte, bits int) (uint64, bool) {
	if len(kb) == 0 {
		return 0, false
	}
	sub := fastDecoder{data: kb}
	v, err := sub.readUint64(bits)
	if err != nil || sub.pos != len(kb) {
		return 0, false
	}
	return v, true
}

func (d *fastDecoder) unmarshalMapStringElem(p *fastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(**maps.Map)(ptr) = nil
			return nil
		}
		return errFastFallback
	}
	if c != '{' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	mt := p.mapType
	mp := (**maps.Map)(ptr)
	m := *mp
	if m == nil {
		m = maps.NewMap(mt, 0, nil)
		*mp = m
	}
	if d.pos < len(d.data) && d.data[d.pos] == '}' {
		d.pos++
		return nil
	}
	if m.Used() > 0 {
		return errFastFallback
	}
	if d.allowBadUTF8 && !d.allowDup {
		return errFastFallback
	}
	elemPlan := p.elemPlan
	var key string
	keyPtr := noescape(unsafe.Pointer(&key))
	for {
		d.skipWS()
		kb, err := d.readString()
		if err != nil {
			return err
		}
		key = makeString(d.strCache, kb)
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ':' {
			return errFastFallback
		}
		d.pos++
		existing, exists := m.Get(mt, keyPtr)
		var elemPtr unsafe.Pointer
		if exists {
			if !d.allowDup {
				return errFastFallback
			}
			if d.legacyMerge {
				clearBytes(existing, p.elemSize, p.elemRT)
			}
			elemPtr = existing
		} else {
			elemPtr = m.PutSlot(mt, keyPtr)
		}
		if err := d.unmarshal(elemPlan, elemPtr, depth); err != nil {
			if !exists {
				m.Delete(mt, keyPtr)
			}
			runtime.KeepAlive(&key)
			return err
		}
		runtime.KeepAlive(&key)
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		return errFastFallback
	}
}

func (d *fastDecoder) unmarshalMapIntElem(p *fastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(**maps.Map)(ptr) = nil
			return nil
		}
		return errFastFallback
	}
	if c != '{' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	mt := p.mapType
	mp := (**maps.Map)(ptr)
	m := *mp
	if m == nil {
		m = maps.NewMap(mt, 0, nil)
		*mp = m
	}
	if d.pos < len(d.data) && d.data[d.pos] == '}' {
		d.pos++
		return nil
	}
	if m.Used() > 0 {
		return errFastFallback
	}
	elemPlan := p.elemPlan
	keyBits := p.keyBits
	var k8 int8
	var k16 int16
	var k32 int32
	var k64 int64
	var keyPtr unsafe.Pointer
	switch keyBits {
	case 8:
		keyPtr = noescape(unsafe.Pointer(&k8))
	case 16:
		keyPtr = noescape(unsafe.Pointer(&k16))
	case 32:
		keyPtr = noescape(unsafe.Pointer(&k32))
	default:
		keyPtr = noescape(unsafe.Pointer(&k64))
	}
	for {
		d.skipWS()
		kb, err := d.readString()
		if err != nil {
			return err
		}
		kv, ok := parseStrictInt64Key(kb, int(keyBits))
		if !ok {
			return errFastFallback
		}
		switch keyBits {
		case 8:
			k8 = int8(kv)
		case 16:
			k16 = int16(kv)
		case 32:
			k32 = int32(kv)
		default:
			k64 = kv
		}
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ':' {
			return errFastFallback
		}
		d.pos++
		existing, exists := m.Get(mt, keyPtr)
		var elemPtr unsafe.Pointer
		if exists {
			if !d.allowDup {
				return errFastFallback
			}
			if d.legacyMerge {
				clearBytes(existing, p.elemSize, p.elemRT)
			}
			elemPtr = existing
		} else {
			elemPtr = m.PutSlot(mt, keyPtr)
		}
		if err := d.unmarshal(elemPlan, elemPtr, depth); err != nil {
			if !exists {
				m.Delete(mt, keyPtr)
			}
			runtime.KeepAlive(&k8)
			runtime.KeepAlive(&k16)
			runtime.KeepAlive(&k32)
			runtime.KeepAlive(&k64)
			return err
		}
		runtime.KeepAlive(&k8)
		runtime.KeepAlive(&k16)
		runtime.KeepAlive(&k32)
		runtime.KeepAlive(&k64)
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		return errFastFallback
	}
}

func (d *fastDecoder) unmarshalMapUintElem(p *fastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(**maps.Map)(ptr) = nil
			return nil
		}
		return errFastFallback
	}
	if c != '{' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	mt := p.mapType
	mp := (**maps.Map)(ptr)
	m := *mp
	if m == nil {
		m = maps.NewMap(mt, 0, nil)
		*mp = m
	}
	if d.pos < len(d.data) && d.data[d.pos] == '}' {
		d.pos++
		return nil
	}
	if m.Used() > 0 {
		return errFastFallback
	}
	elemPlan := p.elemPlan
	keyBits := p.keyBits
	var k8 uint8
	var k16 uint16
	var k32 uint32
	var k64 uint64
	var keyPtr unsafe.Pointer
	switch keyBits {
	case 8:
		keyPtr = noescape(unsafe.Pointer(&k8))
	case 16:
		keyPtr = noescape(unsafe.Pointer(&k16))
	case 32:
		keyPtr = noescape(unsafe.Pointer(&k32))
	default:
		keyPtr = noescape(unsafe.Pointer(&k64))
	}
	for {
		d.skipWS()
		kb, err := d.readString()
		if err != nil {
			return err
		}
		kv, ok := parseStrictUint64Key(kb, int(keyBits))
		if !ok {
			return errFastFallback
		}
		switch keyBits {
		case 8:
			k8 = uint8(kv)
		case 16:
			k16 = uint16(kv)
		case 32:
			k32 = uint32(kv)
		default:
			k64 = kv
		}
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ':' {
			return errFastFallback
		}
		d.pos++
		existing, exists := m.Get(mt, keyPtr)
		var elemPtr unsafe.Pointer
		if exists {
			if !d.allowDup {
				return errFastFallback
			}
			if d.legacyMerge {
				clearBytes(existing, p.elemSize, p.elemRT)
			}
			elemPtr = existing
		} else {
			elemPtr = m.PutSlot(mt, keyPtr)
		}
		if err := d.unmarshal(elemPlan, elemPtr, depth); err != nil {
			if !exists {
				m.Delete(mt, keyPtr)
			}
			runtime.KeepAlive(&k8)
			runtime.KeepAlive(&k16)
			runtime.KeepAlive(&k32)
			runtime.KeepAlive(&k64)
			return err
		}
		runtime.KeepAlive(&k8)
		runtime.KeepAlive(&k16)
		runtime.KeepAlive(&k32)
		runtime.KeepAlive(&k64)
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		return errFastFallback
	}
}

func (d *fastDecoder) unmarshalSliceString(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]string)(ptr) = nil
			return nil
		}
		return errFastFallback
	}
	if c != '[' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		*(*[]string)(ptr) = []string{}
		return nil
	}
	sp := (*[]string)(ptr)
	s := *sp
	oldCap := cap(s)
	s = s[:oldCap]
	i := 0
	for {
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		var elem string
		if i < oldCap && d.legacyMerge {
			elem = s[i]
		}
		switch d.data[d.pos] {
		case '"':
			b, err := d.readString()
			if err != nil {
				return err
			}
			elem = makeString(d.strCache, b)
		case 'n':
			if !d.consumeNull() {
				return errFastFallback
			}
			if !d.legacyMerge {
				elem = ""
			}
		default:
			return errFastFallback
		}
		if i < len(s) {
			s[i] = elem
		} else {
			s = append(s, elem)
			oldCap = 0 // newly grown capacity is clean
			s = s[:cap(s)]
		}
		i++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == ']' {
			d.pos++
			*sp = s[:i]
			return nil
		}
		return errFastFallback
	}
}

func (d *fastDecoder) unmarshalSliceInt(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]int)(ptr) = nil
			return nil
		}
		return errFastFallback
	}
	if c != '[' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		*(*[]int)(ptr) = []int{}
		return nil
	}
	sp := (*[]int)(ptr)
	s := *sp
	oldCap := cap(s)
	s = s[:oldCap]
	i := 0
	for {
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		var elem int
		if i < oldCap && d.legacyMerge {
			elem = s[i]
		}
		if d.data[d.pos] == 'n' {
			if !d.consumeNull() {
				return errFastFallback
			}
			if !d.legacyMerge {
				elem = 0
			}
		} else {
			v, err := d.readInt64(strconv.IntSize)
			if err != nil {
				return err
			}
			elem = int(v)
		}
		if i < len(s) {
			s[i] = elem
		} else {
			s = append(s, elem)
			oldCap = 0
			s = s[:cap(s)]
		}
		i++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == ']' {
			d.pos++
			*sp = s[:i]
			return nil
		}
		return errFastFallback
	}
}

func (d *fastDecoder) unmarshalSliceInt64(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]int64)(ptr) = nil
			return nil
		}
		return errFastFallback
	}
	if c != '[' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		*(*[]int64)(ptr) = []int64{}
		return nil
	}
	sp := (*[]int64)(ptr)
	s := *sp
	oldCap := cap(s)
	s = s[:oldCap]
	i := 0
	for {
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		var elem int64
		if i < oldCap && d.legacyMerge {
			elem = s[i]
		}
		if d.data[d.pos] == 'n' {
			if !d.consumeNull() {
				return errFastFallback
			}
			if !d.legacyMerge {
				elem = 0
			}
		} else {
			v, err := d.readInt64(64)
			if err != nil {
				return err
			}
			elem = v
		}
		if i < len(s) {
			s[i] = elem
		} else {
			s = append(s, elem)
			oldCap = 0
			s = s[:cap(s)]
		}
		i++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == ']' {
			d.pos++
			*sp = s[:i]
			return nil
		}
		return errFastFallback
	}
}

func (d *fastDecoder) unmarshalSliceUint64(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]uint64)(ptr) = nil
			return nil
		}
		return errFastFallback
	}
	if c != '[' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		*(*[]uint64)(ptr) = []uint64{}
		return nil
	}
	sp := (*[]uint64)(ptr)
	s := *sp
	oldCap := cap(s)
	s = s[:oldCap]
	i := 0
	for {
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		var elem uint64
		if i < oldCap && d.legacyMerge {
			elem = s[i]
		}
		if d.data[d.pos] == 'n' {
			if !d.consumeNull() {
				return errFastFallback
			}
			if !d.legacyMerge {
				elem = 0
			}
		} else {
			v, err := d.readUint64(64)
			if err != nil {
				return err
			}
			elem = v
		}
		if i < len(s) {
			s[i] = elem
		} else {
			s = append(s, elem)
			oldCap = 0
			s = s[:cap(s)]
		}
		i++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == ']' {
			d.pos++
			*sp = s[:i]
			return nil
		}
		return errFastFallback
	}
}

func (d *fastDecoder) unmarshalSliceFloat64(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]float64)(ptr) = nil
			return nil
		}
		return errFastFallback
	}
	if c != '[' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		*(*[]float64)(ptr) = []float64{}
		return nil
	}
	sp := (*[]float64)(ptr)
	s := *sp
	oldCap := cap(s)
	s = s[:oldCap]
	i := 0
	for {
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		var elem float64
		if i < oldCap && d.legacyMerge {
			elem = s[i]
		}
		if d.data[d.pos] == 'n' {
			if !d.consumeNull() {
				return errFastFallback
			}
			if !d.legacyMerge {
				elem = 0
			}
		} else {
			v, err := d.readFloat(64)
			if err != nil {
				return err
			}
			elem = v
		}
		if i < len(s) {
			s[i] = elem
		} else {
			s = append(s, elem)
			oldCap = 0
			s = s[:cap(s)]
		}
		i++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == ']' {
			d.pos++
			*sp = s[:i]
			return nil
		}
		return errFastFallback
	}
}

func (d *fastDecoder) unmarshalSliceFloat32(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]float32)(ptr) = nil
			return nil
		}
		return errFastFallback
	}
	if c != '[' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		*(*[]float32)(ptr) = []float32{}
		return nil
	}
	sp := (*[]float32)(ptr)
	s := *sp
	oldCap := cap(s)
	s = s[:oldCap]
	i := 0
	for {
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		var elem float32
		if i < oldCap && d.legacyMerge {
			elem = s[i]
		}
		if d.data[d.pos] == 'n' {
			if !d.consumeNull() {
				return errFastFallback
			}
			if !d.legacyMerge {
				elem = 0
			}
		} else {
			v, err := d.readFloat(32)
			if err != nil {
				return err
			}
			elem = float32(v)
		}
		if i < len(s) {
			s[i] = elem
		} else {
			s = append(s, elem)
			oldCap = 0
			s = s[:cap(s)]
		}
		i++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == ']' {
			d.pos++
			*sp = s[:i]
			return nil
		}
		return errFastFallback
	}
}

func (d *fastDecoder) unmarshalSliceBool(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]bool)(ptr) = nil
			return nil
		}
		return errFastFallback
	}
	if c != '[' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		*(*[]bool)(ptr) = []bool{}
		return nil
	}
	sp := (*[]bool)(ptr)
	s := *sp
	oldCap := cap(s)
	s = s[:oldCap]
	i := 0
	for {
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		var elem bool
		if i < oldCap && d.legacyMerge {
			elem = s[i]
		}
		switch d.data[d.pos] {
		case 't':
			if n := jsonwire.ConsumeTrue(d.data[d.pos:]); n > 0 {
				d.pos += n
				elem = true
			} else {
				return errFastFallback
			}
		case 'f':
			if n := jsonwire.ConsumeFalse(d.data[d.pos:]); n > 0 {
				d.pos += n
				elem = false
			} else {
				return errFastFallback
			}
		case 'n':
			if !d.consumeNull() {
				return errFastFallback
			}
			if !d.legacyMerge {
				elem = false
			}
		default:
			return errFastFallback
		}
		if i < len(s) {
			s[i] = elem
		} else {
			s = append(s, elem)
			oldCap = 0
			s = s[:cap(s)]
		}
		i++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == ']' {
			d.pos++
			*sp = s[:i]
			return nil
		}
		return errFastFallback
	}
}

func (d *fastDecoder) unmarshalBytes(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]byte)(ptr) = nil
			return nil
		}
		return errFastFallback
	}
	if c != '"' {
		return errFastFallback
	}
	b, err := d.readString()
	if err != nil {
		return err
	}
	bp := (*[]byte)(ptr)
	if len(b) == 0 {
		if *bp == nil {
			*bp = []byte{}
		} else {
			*bp = (*bp)[:0]
		}
		return nil
	}
	if len(b)%4 != 0 {
		return errFastFallback
	}
	maxLen := base64StdEncoding.DecodedLen(len(b))
	dst := *bp
	if cap(dst) >= maxLen {
		dst = dst[:maxLen]
	} else {
		dst = make([]byte, maxLen)
	}
	n, decErr := base64StdEncoding.Decode(dst, b)
	if decErr != nil {
		return errFastFallback
	}
	if len(b) != base64StdEncoding.EncodedLen(n) && !d.flags.Get(jsonflags.ParseBytesWithLooseRFC4648) {
		return errFastFallback
	}
	*bp = dst[:n]
	return nil
}

func (d *fastDecoder) unmarshalSlice(p *fastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*unsafeheader.Slice)(ptr) = unsafeheader.Slice{}
			return nil
		}
		return errFastFallback
	}
	if c != '[' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		*(*unsafeheader.Slice)(ptr) = unsafeheader.Slice{Data: p.emptySliceData}
		return nil
	}

	sh := (*unsafeheader.Slice)(ptr)
	origLen := sh.Len
	mustZero := !d.legacyMerge
	sliceCap := sh.Cap
	elemPlan := p.elemPlan
	elemSize := p.elemSize
	i := 0
	for {
		if i == sliceCap {
			rv := reflect.NewAt(p.rt, ptr).Elem()
			rv.SetLen(sliceCap)
			rv.Grow(1)
			sliceCap = rv.Cap()
			mustZero = false // reflect.Value.Grow zero-initializes new capacity
		}
		elemPtr := unsafe.Add(sh.Data, uintptr(i)*elemSize)
		if mustZero {
			clearBytes(elemPtr, elemSize, p.elemRT)
		}
		if err := d.unmarshal(elemPlan, elemPtr, depth); err != nil {
			if origLen <= sh.Cap {
				sh.Len = origLen
			}
			return err
		}
		i++
		d.skipWS()
		if d.pos >= len(d.data) {
			if origLen <= sh.Cap {
				sh.Len = origLen
			}
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == ']' {
			d.pos++
			sh.Len = i
			return nil
		}
		if origLen <= sh.Cap {
			sh.Len = origLen
		}
		return errFastFallback
	}
}

func (d *fastDecoder) unmarshalArray(p *fastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			if !d.legacyMerge {
				reflect.NewAt(p.rt, ptr).Elem().SetZero()
			}
			return nil
		}
		return errFastFallback
	}
	if c != '[' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	n := p.arrayLen
	if n == 0 {
		if d.pos < len(d.data) && d.data[d.pos] == ']' {
			d.pos++
			return nil
		}
		return errFastFallback
	}
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		return errFastFallback // underflow (or UnmarshalArrayFromAnyLength)
	}
	elemPlan := p.elemPlan
	elemSize := p.elemSize
	mustZero := !d.legacyMerge && elemPlan.kind >= fkBytes
	for i := range n {
		if i > 0 {
			d.skipWS()
			if d.pos >= len(d.data) || d.data[d.pos] != ',' {
				return errFastFallback
			}
			d.pos++
		}
		elemPtr := unsafe.Add(ptr, uintptr(i)*elemSize)
		if mustZero {
			clearBytes(elemPtr, elemSize, p.elemRT)
		}
		if err := d.unmarshal(elemPlan, elemPtr, depth); err != nil {
			return err
		}
	}
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		return nil
	}
	return errFastFallback
}

func (d *fastDecoder) unmarshalMapStringAny(ptr unsafe.Pointer, depth int) error {
	if depth >= 10000 {
		return errFastFallback
	}
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*map[string]any)(ptr) = nil
			return nil
		}
		return errFastFallback
	}
	if c != '{' {
		return errFastFallback
	}
	mp := (*map[string]any)(ptr)
	m := *mp
	if len(m) > 0 {
		return errFastFallback
	}
	if d.allowBadUTF8 && !d.allowDup {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	if m == nil {
		m = make(map[string]any)
		*mp = m
	}
	if d.pos < len(d.data) && d.data[d.pos] == '}' {
		d.pos++
		return nil
	}
	for {
		d.skipWS()
		kb, err := d.readString()
		if err != nil {
			return err
		}
		key := makeString(d.strCache, kb)
		if _, exists := m[key]; exists {
			return errFastFallback
		}
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ':' {
			return errFastFallback
		}
		d.pos++
		val, err := d.readAny(depth)
		if err != nil {
			return err
		}
		m[key] = val
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		return errFastFallback
	}
}

func (d *fastDecoder) unmarshalAny(ptr unsafe.Pointer, depth int) error {
	if *(*any)(ptr) != nil {
		return errFastFallback
	}
	v, err := d.readAny(depth)
	if err != nil {
		return err
	}
	*(*any)(ptr) = v
	return nil
}

func (d *fastDecoder) readAny(depth int) (any, error) {
	if depth >= 10000 {
		return nil, errFastFallback
	}
	d.skipWS()
	if d.pos >= len(d.data) {
		return nil, errFastFallback
	}
	switch d.data[d.pos] {
	case 'n':
		if d.consumeNull() {
			return nil, nil
		}
		return nil, errFastFallback
	case 't':
		if n := jsonwire.ConsumeTrue(d.data[d.pos:]); n > 0 {
			d.pos += n
			return true, nil
		}
		return nil, errFastFallback
	case 'f':
		if n := jsonwire.ConsumeFalse(d.data[d.pos:]); n > 0 {
			d.pos += n
			return false, nil
		}
		return nil, errFastFallback
	case '"':
		b, err := d.readString()
		if err != nil {
			return nil, err
		}
		return makeString(d.strCache, b), nil
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		if d.flags.Get(jsonflags.UnmarshalAnyWithRawNumber) {
			return nil, errFastFallback
		}
		v, err := d.readFloat(64)
		if err != nil {
			return nil, err
		}
		return v, nil
	case '[':
		d.pos++
		d.skipWS()
		if d.pos < len(d.data) && d.data[d.pos] == ']' {
			d.pos++
			return []any{}, nil
		}
		s := make([]any, 0, 4)
		for {
			elem, err := d.readAny(depth + 1)
			if err != nil {
				return nil, err
			}
			s = append(s, elem)
			d.skipWS()
			if d.pos >= len(d.data) {
				return nil, errFastFallback
			}
			if d.data[d.pos] == ',' {
				d.pos++
				continue
			}
			if d.data[d.pos] == ']' {
				d.pos++
				return s, nil
			}
			return nil, errFastFallback
		}
	case '{':
		if d.allowBadUTF8 && !d.allowDup {
			return nil, errFastFallback
		}
		d.pos++
		d.skipWS()
		m := make(map[string]any)
		if d.pos < len(d.data) && d.data[d.pos] == '}' {
			d.pos++
			return m, nil
		}
		for {
			d.skipWS()
			kb, err := d.readString()
			if err != nil {
				return nil, err
			}
			key := makeString(d.strCache, kb)
			if _, exists := m[key]; exists {
				return nil, errFastFallback
			}
			d.skipWS()
			if d.pos >= len(d.data) || d.data[d.pos] != ':' {
				return nil, errFastFallback
			}
			d.pos++
			val, err := d.readAny(depth + 1)
			if err != nil {
				return nil, err
			}
			m[key] = val
			d.skipWS()
			if d.pos >= len(d.data) {
				return nil, errFastFallback
			}
			if d.data[d.pos] == ',' {
				d.pos++
				continue
			}
			if d.data[d.pos] == '}' {
				d.pos++
				return m, nil
			}
			return nil, errFastFallback
		}
	}
	return nil, errFastFallback
}

func clearBytes(ptr unsafe.Pointer, size uintptr, rt reflect.Type) {
	if size == 0 {
		return
	}
	if rt.Kind() == reflect.Pointer {
		*(*unsafe.Pointer)(ptr) = nil
		return
	}
	// reflect.NewAt(rt, ptr).Elem().SetZero() handles GC write barriers safely!
	reflect.NewAt(rt, ptr).Elem().SetZero()
}

func (d *fastDecoder) unmarshalMapStringString(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*map[string]string)(ptr) = nil
			return nil
		}
		return errFastFallback
	}
	if c != '{' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	mp := (*map[string]string)(ptr)
	m := *mp
	if m == nil {
		m = make(map[string]string)
		*mp = m
	}
	if d.pos < len(d.data) && d.data[d.pos] == '}' {
		d.pos++
		return nil
	}
	// If map is pre-populated and !allowDup, delegate to standard path for `seen` tracking.
	if !d.allowDup && len(m) > 0 {
		return errFastFallback
	}
	// If AllowInvalidUTF8 && !AllowDuplicateNames, fall back for namespace check.
	if d.allowBadUTF8 && !d.allowDup {
		return errFastFallback
	}
	for {
		d.skipWS()
		kb, err := d.readString()
		if err != nil {
			return err
		}
		key := makeString(d.strCache, kb)
		if !d.allowDup {
			if _, exists := m[key]; exists {
				return errFastFallback
			}
		}
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ':' {
			return errFastFallback
		}
		d.pos++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		switch d.data[d.pos] {
		case '"':
			vb, err := d.readString()
			if err != nil {
				return err
			}
			m[key] = makeString(d.strCache, vb)
		case 'n':
			if !d.consumeNull() {
				return errFastFallback
			}
			m[key] = ""
		default:
			return errFastFallback
		}
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		return errFastFallback
	}
}

func (d *fastDecoder) unmarshalMapStringInt(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*map[string]int)(ptr) = nil
			return nil
		}
		return errFastFallback
	}
	if c != '{' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	mp := (*map[string]int)(ptr)
	m := *mp
	if m == nil {
		m = make(map[string]int)
		*mp = m
	}
	if d.pos < len(d.data) && d.data[d.pos] == '}' {
		d.pos++
		return nil
	}
	if !d.allowDup && len(m) > 0 {
		return errFastFallback
	}
	if d.allowBadUTF8 && !d.allowDup {
		return errFastFallback
	}
	for {
		d.skipWS()
		kb, err := d.readString()
		if err != nil {
			return err
		}
		key := makeString(d.strCache, kb)
		if !d.allowDup {
			if _, exists := m[key]; exists {
				return errFastFallback
			}
		}
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ':' {
			return errFastFallback
		}
		d.pos++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == 'n' {
			if !d.consumeNull() {
				return errFastFallback
			}
			m[key] = 0
		} else {
			v, err := d.readInt64(strconv.IntSize)
			if err != nil {
				return err
			}
			m[key] = int(v)
		}
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		return errFastFallback
	}
}

func (d *fastDecoder) unmarshalStruct(p *fastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			if !d.legacyMerge {
				reflect.NewAt(p.rt, ptr).Elem().SetZero()
			}
			return nil
		}
		return errFastFallback
	}
	if c != '{' {
		return errFastFallback
	}
	d.pos++
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == '}' {
		d.pos++
		return nil
	}

	fields := p.fields
	var seenLo uint64
	var seenSet uintSet
	useBitset64 := len(fields) <= 64
	nextIdx := 0
	var unkKeys [4]string
	numUnk := 0

	for {
		data := d.data
		pos := d.pos
		if pos < len(data) && data[pos] <= ' ' {
			for pos < len(data) && data[pos] <= ' ' && (data[pos] == ' ' || data[pos] == '\t' || data[pos] == '\r' || data[pos] == '\n') {
				pos++
			}
			d.pos = pos
		}

		var f *fastUnmarshalField
		var keyBytes []byte
		if nextIdx < len(fields) {
			if qf := fields[nextIdx].quotedNameColon; len(qf) > 0 && pos+len(qf) <= len(data) && string(data[pos:pos+len(qf)]) == qf {
				d.pos = pos + len(qf)
				f = &fields[nextIdx]
				nextIdx++
				goto gotField
			}
		}

		{
			var err error
			keyBytes, err = d.readString()
			if err != nil {
				return err
			}
			d.skipWS()
			if d.pos >= len(d.data) || d.data[d.pos] != ':' {
				return errFastFallback
			}
			d.pos++

			if nextIdx < len(fields) && string(keyBytes) == fields[nextIdx].name {
				f = &fields[nextIdx]
				nextIdx++
			} else if len(fields) <= 8 {
				for j := range fields {
					if string(keyBytes) == fields[j].name {
						f = &fields[j]
						nextIdx = j + 1
						break
					}
				}
			} else {
				f = p.byActualName[string(keyBytes)]
				if f != nil {
					nextIdx = f.orderIdx + 1
				}
			}
			if f == nil && (p.hasCaseIgnore || d.caseInsens) {
				cands := p.byFoldedName[string(foldName(keyBytes))]
				numMatch := 0
				for _, cand := range cands {
					if cand.casing == caseIgnore || (d.caseInsens && cand.casing != caseStrict) {
						if !d.caseSensDel || strings.EqualFold(string(keyBytes), cand.name) {
							if f == nil {
								f = cand
							}
							numMatch++
						}
					}
				}
				if numMatch > 1 && !d.flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
					return errFastFallback
				}
			}
		}

	gotField:
		if f == nil {
			if d.rejectUnk {
				return errFastFallback
			}
			if !d.allowDup {
				if numUnk >= len(unkKeys) {
					return errFastFallback
				}
				ks := string(keyBytes)
				for j := range numUnk {
					if unkKeys[j] == ks {
						return errFastFallback
					}
				}
				unkKeys[numUnk] = ks
				numUnk++
			}
			if err := d.skipValueChecked(depth); err != nil {
				return err
			}
		} else {
			if !d.allowDup {
				if useBitset64 {
					bit := uint64(1) << uint(f.id)
					if seenLo&bit != 0 {
						return errFastFallback
					}
					seenLo |= bit
				} else if !seenSet.insert(uint(f.id)) {
					return errFastFallback
				}
			}

			fp := unsafe.Add(ptr, f.offset)
			d.skipWS()
			if d.pos >= len(d.data) {
				return errFastFallback
			}
			if p.hasQuoted && f.quoted {
				if err := d.unmarshalQuotedNumber(f.kind, fp); err != nil {
					return err
				}
			} else {
				vc := d.data[d.pos]
				switch f.kind {
				case fkString:
					if vc == '"' {
						b, err := d.readString()
						if err != nil {
							return err
						}
						*(*string)(fp) = makeString(d.strCache, b)
					} else if vc == 'n' && d.consumeNull() {
						if !d.legacyMerge {
							*(*string)(fp) = ""
						}
					} else {
						return errFastFallback
					}
				case fkInt:
					if vc == 'n' && d.consumeNull() {
						if !d.legacyMerge {
							*(*int)(fp) = 0
						}
					} else {
						v, err := d.readInt64(strconv.IntSize)
						if err != nil {
							return err
						}
						*(*int)(fp) = int(v)
					}
				case fkInt32:
					if vc == 'n' && d.consumeNull() {
						if !d.legacyMerge {
							*(*int32)(fp) = 0
						}
					} else {
						v, err := d.readInt64(32)
						if err != nil {
							return err
						}
						*(*int32)(fp) = int32(v)
					}
				case fkInt64:
					if vc == 'n' && d.consumeNull() {
						if !d.legacyMerge {
							*(*int64)(fp) = 0
						}
					} else {
						v, err := d.readInt64(64)
						if err != nil {
							return err
						}
						*(*int64)(fp) = v
					}
				case fkUint:
					if vc == 'n' && d.consumeNull() {
						if !d.legacyMerge {
							*(*uint)(fp) = 0
						}
					} else {
						v, err := d.readUint64(strconv.IntSize)
						if err != nil {
							return err
						}
						*(*uint)(fp) = uint(v)
					}
				case fkUint32:
					if vc == 'n' && d.consumeNull() {
						if !d.legacyMerge {
							*(*uint32)(fp) = 0
						}
					} else {
						v, err := d.readUint64(32)
						if err != nil {
							return err
						}
						*(*uint32)(fp) = uint32(v)
					}
				case fkUint64:
					if vc == 'n' && d.consumeNull() {
						if !d.legacyMerge {
							*(*uint64)(fp) = 0
						}
					} else {
						v, err := d.readUint64(64)
						if err != nil {
							return err
						}
						*(*uint64)(fp) = v
					}
				case fkFloat64:
					if vc == 'n' && d.consumeNull() {
						if !d.legacyMerge {
							*(*float64)(fp) = 0
						}
					} else {
						v, err := d.readFloat(64)
						if err != nil {
							return err
						}
						*(*float64)(fp) = v
					}
				case fkFloat32:
					if vc == 'n' && d.consumeNull() {
						if !d.legacyMerge {
							*(*float32)(fp) = 0
						}
					} else {
						v, err := d.readFloat(32)
						if err != nil {
							return err
						}
						*(*float32)(fp) = float32(v)
					}
				case fkBool:
					if vc == 't' {
						if n := jsonwire.ConsumeTrue(d.data[d.pos:]); n > 0 {
							d.pos += n
							*(*bool)(fp) = true
						} else {
							return errFastFallback
						}
					} else if vc == 'f' {
						if n := jsonwire.ConsumeFalse(d.data[d.pos:]); n > 0 {
							d.pos += n
							*(*bool)(fp) = false
						} else {
							return errFastFallback
						}
					} else if vc == 'n' && d.consumeNull() {
						if !d.legacyMerge {
							*(*bool)(fp) = false
						}
					} else {
						return errFastFallback
					}
				default:
					if err := d.unmarshal(f.plan, fp, depth); err != nil {
						return err
					}
				}
			}
		}

		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		return errFastFallback
	}
}

// tryFastUnmarshal attempts to unmarshal JSON bytes directly into (at, ptr).
// Returns true on complete success, or false if the standard path should run.
// To guarantee zero corruption of pre-populated destination values on error,
// composite destinations are only fast-decoded when we can safely restore or
// when pre-validated (or empty/zero).
func tryFastUnmarshal(in []byte, at *abi.Type, ptr unsafe.Pointer, uo *jsonopts.Struct) bool {
	if !canFastUnmarshalFlags(uo) {
		return false
	}
	plan := lookupFastUnmarshalPlan(at)
	if !plan.canFast {
		return false
	}

	// Fast scalar path: parse + check EOF BEFORE writing to ptr, so any error
	// leaves ptr completely untouched and falls back cleanly.
	switch plan.kind {
	case fkBool, fkString, fkInt, fkInt8, fkInt16, fkInt32, fkInt64,
		fkUint, fkUint8, fkUint16, fkUint32, fkUint64, fkUintptr,
		fkFloat32, fkFloat64:
		return tryFastUnmarshalScalar(in, plan, ptr, uo)
	}

	wasZero := isFastTargetZero(plan, ptr)
	if !wasZero {
		// Pre-validate syntax for non-zero targets so syntax errors never mutate ptr.
		if !fastCheckValid(in, uo.Flags.Get(jsonflags.AllowInvalidUTF8)) {
			return false
		}
	}

	sc := fastStringCachePool.Get()
	d := fastDecoder{
		data:         in,
		flags:        uo.Flags,
		strCache:     sc,
		allowDup:     uo.Flags.Get(jsonflags.AllowDuplicateNames),
		allowBadUTF8: uo.Flags.Get(jsonflags.AllowInvalidUTF8),
		legacyMerge:  uo.Flags.Get(jsonflags.MergeWithLegacySemantics),
		caseInsens:   uo.Flags.Get(jsonflags.MatchCaseInsensitiveNames),
		caseSensDel:  uo.Flags.Get(jsonflags.MatchCaseSensitiveDelimiter),
		rejectUnk:    uo.Flags.Get(jsonflags.RejectUnknownMembers),
	}
	err := d.unmarshal(plan, ptr, 0)
	if err == nil {
		d.skipWS()
		if d.pos == len(in) {
			fastStringCachePool.Put(sc)
			return true
		}
	}
	fastStringCachePool.Put(sc)
	if wasZero {
		// Restore zero state before delegating to standard unmarshalDecode!
		reflect.NewAt(plan.rt, ptr).Elem().SetZero()
	}
	return false
}

func isFastTargetZero(p *fastUnmarshalPlan, ptr unsafe.Pointer) bool {
	switch p.kind {
	case fkPtr:
		return *(*unsafe.Pointer)(ptr) == nil
	case fkTime:
		return (*time.Time)(ptr).IsZero()
	case fkByteArray:
		for _, v := range unsafe.Slice((*byte)(ptr), p.arrayLen) {
			if v != 0 {
				return false
			}
		}
		return true
	case fkBytes, fkSliceBool, fkSliceString, fkSliceInt, fkSliceInt64, fkSliceUint64, fkSliceFloat64, fkSliceFloat32, fkSlice:
		return (*unsafeheader.Slice)(ptr).Data == nil
	case fkMapStringString:
		return *(*map[string]string)(ptr) == nil
	case fkMapStringInt:
		return *(*map[string]int)(ptr) == nil
	case fkMapStringAny:
		return *(*map[string]any)(ptr) == nil
	case fkMapStringElem, fkMapIntElem, fkMapUintElem:
		return *(**maps.Map)(ptr) == nil
	case fkAny:
		return *(*any)(ptr) == nil
	case fkStruct, fkArray, fkUnmarshaler:
		return reflect.NewAt(p.rt, ptr).Elem().IsZero()
	}
	return false
}

func tryFastUnmarshalScalar(in []byte, plan *fastUnmarshalPlan, ptr unsafe.Pointer, uo *jsonopts.Struct) bool {
	d := fastDecoder{
		data:         in,
		flags:        uo.Flags,
		allowBadUTF8: uo.Flags.Get(jsonflags.AllowInvalidUTF8),
		legacyMerge:  uo.Flags.Get(jsonflags.MergeWithLegacySemantics),
	}
	d.skipWS()
	if d.pos >= len(in) {
		return false
	}
	c := in[d.pos]

	switch plan.kind {
	case fkBool:
		var v bool
		var isNull bool
		switch c {
		case 't':
			if n := jsonwire.ConsumeTrue(in[d.pos:]); n > 0 {
				d.pos += n
				v = true
			} else {
				return false
			}
		case 'f':
			if n := jsonwire.ConsumeFalse(in[d.pos:]); n > 0 {
				d.pos += n
				v = false
			} else {
				return false
			}
		case 'n':
			if !d.consumeNull() {
				return false
			}
			isNull = true
		default:
			return false
		}
		d.skipWS()
		if d.pos != len(in) {
			return false
		}
		if !isNull {
			*(*bool)(ptr) = v
		} else if !d.legacyMerge {
			*(*bool)(ptr) = false
		}
		return true

	case fkString:
		if c == '"' {
			b, err := d.readString()
			if err != nil {
				return false
			}
			d.skipWS()
			if d.pos != len(in) {
				return false
			}
			sc := fastStringCachePool.Get()
			*(*string)(ptr) = makeString(sc, b)
			fastStringCachePool.Put(sc)
			return true
		}
		if c == 'n' && d.consumeNull() {
			d.skipWS()
			if d.pos != len(in) {
				return false
			}
			if !d.legacyMerge {
				*(*string)(ptr) = ""
			}
			return true
		}
		return false

	case fkInt, fkInt8, fkInt16, fkInt32, fkInt64:
		if c == 'n' {
			if !d.consumeNull() {
				return false
			}
			d.skipWS()
			if d.pos != len(in) {
				return false
			}
			if !d.legacyMerge {
				switch plan.kind {
				case fkInt:
					*(*int)(ptr) = 0
				case fkInt8:
					*(*int8)(ptr) = 0
				case fkInt16:
					*(*int16)(ptr) = 0
				case fkInt32:
					*(*int32)(ptr) = 0
				case fkInt64:
					*(*int64)(ptr) = 0
				}
			}
			return true
		}
		var bits int
		switch plan.kind {
		case fkInt:
			bits = strconv.IntSize
		case fkInt8:
			bits = 8
		case fkInt16:
			bits = 16
		case fkInt32:
			bits = 32
		case fkInt64:
			bits = 64
		}
		v, err := d.readInt64(bits)
		if err != nil {
			return false
		}
		d.skipWS()
		if d.pos != len(in) {
			return false
		}
		switch plan.kind {
		case fkInt:
			*(*int)(ptr) = int(v)
		case fkInt8:
			*(*int8)(ptr) = int8(v)
		case fkInt16:
			*(*int16)(ptr) = int16(v)
		case fkInt32:
			*(*int32)(ptr) = int32(v)
		case fkInt64:
			*(*int64)(ptr) = v
		}
		return true

	case fkUint, fkUint8, fkUint16, fkUint32, fkUint64, fkUintptr:
		if c == 'n' {
			if !d.consumeNull() {
				return false
			}
			d.skipWS()
			if d.pos != len(in) {
				return false
			}
			if !d.legacyMerge {
				switch plan.kind {
				case fkUint:
					*(*uint)(ptr) = 0
				case fkUint8:
					*(*uint8)(ptr) = 0
				case fkUint16:
					*(*uint16)(ptr) = 0
				case fkUint32:
					*(*uint32)(ptr) = 0
				case fkUint64:
					*(*uint64)(ptr) = 0
				case fkUintptr:
					*(*uintptr)(ptr) = 0
				}
			}
			return true
		}
		var bits int
		switch plan.kind {
		case fkUint:
			bits = strconv.IntSize
		case fkUint8:
			bits = 8
		case fkUint16:
			bits = 16
		case fkUint32:
			bits = 32
		case fkUint64:
			bits = 64
		case fkUintptr:
			bits = int(unsafe.Sizeof(uintptr(0)) * 8)
		}
		v, err := d.readUint64(bits)
		if err != nil {
			return false
		}
		d.skipWS()
		if d.pos != len(in) {
			return false
		}
		switch plan.kind {
		case fkUint:
			*(*uint)(ptr) = uint(v)
		case fkUint8:
			*(*uint8)(ptr) = uint8(v)
		case fkUint16:
			*(*uint16)(ptr) = uint16(v)
		case fkUint32:
			*(*uint32)(ptr) = uint32(v)
		case fkUint64:
			*(*uint64)(ptr) = v
		case fkUintptr:
			*(*uintptr)(ptr) = uintptr(v)
		}
		return true

	case fkFloat32, fkFloat64:
		if c == 'n' {
			if !d.consumeNull() {
				return false
			}
			d.skipWS()
			if d.pos != len(in) {
				return false
			}
			if !d.legacyMerge {
				if plan.kind == fkFloat32 {
					*(*float32)(ptr) = 0
				} else {
					*(*float64)(ptr) = 0
				}
			}
			return true
		}
		bits := 64
		if plan.kind == fkFloat32 {
			bits = 32
		}
		v, err := d.readFloat(bits)
		if err != nil {
			return false
		}
		d.skipWS()
		if d.pos != len(in) {
			return false
		}
		if plan.kind == fkFloat32 {
			*(*float32)(ptr) = float32(v)
		} else {
			*(*float64)(ptr) = v
		}
		return true
	}
	return false
}

// fastCheckValid performs a zero-allocation SWAR syntax check of a single JSON value.
func fastCheckValid(data []byte, allowBadUTF8 bool) bool {
	d := fastDecoder{data: data, allowDup: true, allowBadUTF8: allowBadUTF8}
	if err := d.skipValue(0); err != nil {
		return false
	}
	d.skipWS()
	return d.pos == len(data)
}
