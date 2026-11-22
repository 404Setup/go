// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.jsonv2

package json

import (
	"bytes"
	"encoding/base64"
	"errors"
	"internal/abi"
	"internal/byteorder"
	"internal/unsafeheader"
	"math"
	"math/bits"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"sync/v2"
	"time"
	"unicode/utf8"
	"unsafe"
)

var errV1FastFallback = errors.New("v1 fast path fallback")

type v1Eface struct {
	typ  *abi.Type
	data unsafe.Pointer
}

//go:nosplit
//go:noinline
func v1Noescape(p unsafe.Pointer) unsafe.Pointer {
	x := uintptr(p)
	return unsafe.Pointer(x ^ 0)
}

func v1EfacePtr(a *any) (*abi.Type, unsafe.Pointer) {
	e := (*v1Eface)(unsafe.Pointer(a))
	if e.typ == nil {
		return nil, nil
	}
	if e.typ.IsDirectIface() {
		return e.typ, unsafe.Pointer(&e.data)
	}
	return e.typ, e.data
}

func v1AbiTypeOfReflect(rt reflect.Type) *abi.Type {
	return (*v1Eface)(unsafe.Pointer(&rt)).dataAsAbiType()
}

func (e *v1Eface) dataAsAbiType() *abi.Type {
	return (*abi.Type)(e.data)
}

var v1ReflectTypeIfaceTab = func() unsafe.Pointer {
	rt := reflect.TypeFor[int]()
	return (*[2]unsafe.Pointer)(unsafe.Pointer(&rt))[0]
}()

func v1AbiTypeToReflect(at *abi.Type) reflect.Type {
	var rt reflect.Type
	p := (*[2]unsafe.Pointer)(unsafe.Pointer(&rt))
	p[0] = v1ReflectTypeIfaceTab
	p[1] = unsafe.Pointer(at)
	return rt
}

const (
	v1SwarLo8  = 0x0101010101010101
	v1SwarHi8  = 0x8080808080808080
	v1SwarSp8  = 0x2020202020202020
	v1SwarQt8  = 0x2222222222222222 // '"'
	v1SwarBs8  = 0x5c5c5c5c5c5c5c5c // '\\'
	v1SwarLt8  = 0x3c3c3c3c3c3c3c3c // '<'
	v1SwarGt8  = 0x3e3e3e3e3e3e3e3e // '>'
	v1SwarAmp8 = 0x2626262626262626 // '&'
)

func v1EscapeMaskNoHTML(w uint64) uint64 {
	xq := w ^ v1SwarQt8
	xb := w ^ v1SwarBs8
	return (w & v1SwarHi8) |
		((w - v1SwarSp8) & v1SwarHi8) |
		((xq - v1SwarLo8) &^ xq & v1SwarHi8) |
		((xb - v1SwarLo8) &^ xb & v1SwarHi8)
}

func v1EscapeMaskHTML(w uint64) uint64 {
	xq := w ^ v1SwarQt8
	xb := w ^ v1SwarBs8
	xl := w ^ v1SwarLt8
	xg := w ^ v1SwarGt8
	xa := w ^ v1SwarAmp8
	return (w & v1SwarHi8) |
		((w - v1SwarSp8) & v1SwarHi8) |
		((xq - v1SwarLo8) &^ xq & v1SwarHi8) |
		((xb - v1SwarLo8) &^ xb & v1SwarHi8) |
		((xl - v1SwarLo8) &^ xl & v1SwarHi8) |
		((xg - v1SwarLo8) &^ xg & v1SwarHi8) |
		((xa - v1SwarLo8) &^ xa & v1SwarHi8)
}

func v1StringSafeHTML(s string) bool {
	b := unsafe.Slice(unsafe.StringData(s), len(s))
	i := 0
	for len(b)-i >= 8 {
		if v1EscapeMaskHTML(byteorder.LEUint64(b[i:])) != 0 {
			return false
		}
		i += 8
	}
	for ; i < len(b); i++ {
		c := b[i]
		if c >= utf8.RuneSelf || !htmlSafeSet[c] {
			return false
		}
	}
	return true
}

func v1AppendQuoteString(dst []byte, src string, escapeHTML bool) []byte {
	dst = append(dst, '"')
	b := unsafe.Slice(unsafe.StringData(src), len(src))
	start := 0
	i := 0
	for i < len(b) {
		if escapeHTML {
			for len(b)-i >= 8 {
				if m := v1EscapeMaskHTML(byteorder.LEUint64(b[i:])); m != 0 {
					i += bits.TrailingZeros64(m) >> 3
					break
				}
				i += 8
			}
		} else {
			for len(b)-i >= 8 {
				if m := v1EscapeMaskNoHTML(byteorder.LEUint64(b[i:])); m != 0 {
					i += bits.TrailingZeros64(m) >> 3
					break
				}
				i += 8
			}
		}
		if i >= len(b) {
			break
		}
		if c := b[i]; c < utf8.RuneSelf {
			if htmlSafeSet[c] || (!escapeHTML && safeSet[c]) {
				i++
				continue
			}
			dst = append(dst, src[start:i]...)
			switch c {
			case '\\', '"':
				dst = append(dst, '\\', c)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(src[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, src[start:i]...)
			dst = append(dst, `\ufffd`...)
			i += size
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			dst = append(dst, src[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hex[r&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, src[start:]...)
	dst = append(dst, '"')
	return dst
}

func v1AppendFloat(b []byte, f float64, bits int) []byte {
	abs := math.Abs(f)
	fmt := byte('f')
	if abs != 0 {
		if bits == 64 && (abs < 1e-6 || abs >= 1e21) || bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21) {
			fmt = 'e'
		}
	}
	b = strconv.AppendFloat(b, f, fmt, -1, bits)
	if fmt == 'e' {
		n := len(b)
		if n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	return b
}

type v1FastBuf struct {
	b       []byte
	strikes int
	prevLen int
}

var v1FastBufPool = sync.Pool[*v1FastBuf]{
	New: func() *v1FastBuf { return new(v1FastBuf) },
}

func getV1FastBuf() *v1FastBuf {
	fb := v1FastBufPool.Get()
	if fb.b == nil {
		n := 1 << bits.Len(uint(fb.prevLen|63))
		fb.b = make([]byte, 0, n)
	} else {
		fb.b = fb.b[:0]
	}
	return fb
}

func putV1FastBuf(fb *v1FastBuf) {
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
	v1FastBufPool.Put(fb)
}

var v1StringsPool = sync.Pool[*[]string]{
	New: func() *[]string {
		s := make([]string, 0, 16)
		return &s
	},
}

func getV1Strings(n int) *[]string {
	sp := v1StringsPool.Get()
	if cap(*sp) < n {
		*sp = make([]string, n)
	} else {
		*sp = (*sp)[:n]
	}
	return sp
}

func putV1Strings(sp *[]string) {
	clear(*sp)
	if cap(*sp) > 1024 {
		*sp = nil
	}
	v1StringsPool.Put(sp)
}

type v1FastKind uint8

const (
	v1fkUnsupported v1FastKind = iota
	v1fkBool
	v1fkString
	v1fkInt
	v1fkInt8
	v1fkInt16
	v1fkInt32
	v1fkInt64
	v1fkUint
	v1fkUint8
	v1fkUint16
	v1fkUint32
	v1fkUint64
	v1fkUintptr
	v1fkFloat32
	v1fkFloat64
	v1fkBytes
	v1fkSliceBool
	v1fkSliceString
	v1fkSliceInt
	v1fkSliceInt64
	v1fkSliceUint64
	v1fkSliceFloat64
	v1fkSliceFloat32
	v1fkSlice
	v1fkArray
	v1fkPtr
	v1fkStruct
	v1fkMapStringString
	v1fkMapStringInt
	v1fkMapStringAny
	v1fkAny
	v1fkTime
)

var v1TimeType = reflect.TypeFor[time.Time]()

type v1FastMarshalField struct {
	offset      uintptr
	nameNonEsc  string
	nameEscHTML string
	omitEmpty   bool
	omitZero    bool
	kind        v1FastKind
	plan        *v1FastMarshalPlan
}

type v1FastMarshalPlan struct {
	typ      *abi.Type
	rt       reflect.Type
	kind     v1FastKind
	canFast  bool
	elemSize uintptr
	arrayLen int
	elemPlan *v1FastMarshalPlan
	fields   []v1FastMarshalField
}

var v1FastMarshalCache sync.Map[*abi.Type, *v1FastMarshalPlan]

func lookupV1FastMarshalPlan(at *abi.Type) *v1FastMarshalPlan {
	if p, ok := v1FastMarshalCache.Load(at); ok {
		return p
	}
	visiting := make(map[*abi.Type]*v1FastMarshalPlan)
	p := buildV1FastMarshalPlan(v1AbiTypeToReflect(at), at, visiting)
	finalizeV1FastMarshalPlans(visiting)
	for kt, kp := range visiting {
		v1FastMarshalCache.LoadOrStore(kt, kp)
	}
	return p
}

func finalizeV1FastMarshalPlans(visiting map[*abi.Type]*v1FastMarshalPlan) {
	for _, p := range visiting {
		if p.kind == v1fkStruct {
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
			case v1fkPtr, v1fkSlice, v1fkArray:
				if p.elemPlan == nil || !p.elemPlan.canFast {
					p.canFast = false
					changed = true
				}
			case v1fkStruct:
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

func v1HasCustomMarshaler(rt reflect.Type) bool {
	if rt.Implements(marshalerType) || rt.Implements(textMarshalerType) {
		return true
	}
	if rt.Kind() != reflect.Pointer && rt.Kind() != reflect.Interface {
		pt := reflect.PointerTo(rt)
		if pt.Implements(marshalerType) || pt.Implements(textMarshalerType) {
			return true
		}
	}
	return false
}

func v1DirectFieldOffset(rt reflect.Type, index []int) (uintptr, reflect.Type, bool) {
	cur := rt
	var off uintptr
	for _, idx := range index {
		if cur.Kind() != reflect.Struct {
			return 0, nil, false
		}
		sf := cur.Field(idx)
		off += sf.Offset
		cur = sf.Type
	}
	return off, cur, true
}

func buildV1FastMarshalPlan(rt reflect.Type, at *abi.Type, visiting map[*abi.Type]*v1FastMarshalPlan) *v1FastMarshalPlan {
	if p, ok := v1FastMarshalCache.Load(at); ok {
		return p
	}
	if p, ok := visiting[at]; ok {
		return p
	}
	p := &v1FastMarshalPlan{typ: at, rt: rt}
	visiting[at] = p

	if rt == v1TimeType {
		p.kind = v1fkTime
		p.canFast = true
		return p
	}
	if rt == numberType {
		return p
	}
	if v1HasCustomMarshaler(rt) {
		return p
	}

	switch rt.Kind() {
	case reflect.Bool:
		p.kind = v1fkBool
		p.canFast = true
	case reflect.String:
		p.kind = v1fkString
		p.canFast = true
	case reflect.Int:
		p.kind = v1fkInt
		p.canFast = true
	case reflect.Int8:
		p.kind = v1fkInt8
		p.canFast = true
	case reflect.Int16:
		p.kind = v1fkInt16
		p.canFast = true
	case reflect.Int32:
		p.kind = v1fkInt32
		p.canFast = true
	case reflect.Int64:
		p.kind = v1fkInt64
		p.canFast = true
	case reflect.Uint:
		p.kind = v1fkUint
		p.canFast = true
	case reflect.Uint8:
		p.kind = v1fkUint8
		p.canFast = true
	case reflect.Uint16:
		p.kind = v1fkUint16
		p.canFast = true
	case reflect.Uint32:
		p.kind = v1fkUint32
		p.canFast = true
	case reflect.Uint64:
		p.kind = v1fkUint64
		p.canFast = true
	case reflect.Uintptr:
		p.kind = v1fkUintptr
		p.canFast = true
	case reflect.Float32:
		p.kind = v1fkFloat32
		p.canFast = true
	case reflect.Float64:
		p.kind = v1fkFloat64
		p.canFast = true

	case reflect.Slice:
		elem := rt.Elem()
		if elem.Kind() == reflect.Uint8 {
			if rt == reflect.TypeFor[[]byte]() {
				p.kind = v1fkBytes
				p.canFast = true
			}
			return p
		}
		ep := buildV1FastMarshalPlan(elem, v1AbiTypeOfReflect(elem), visiting)
		p.elemPlan = ep
		p.elemSize = elem.Size()
		switch ep.kind {
		case v1fkBool:
			p.kind = v1fkSliceBool
		case v1fkString:
			p.kind = v1fkSliceString
		case v1fkInt:
			p.kind = v1fkSliceInt
		case v1fkInt64:
			p.kind = v1fkSliceInt64
		case v1fkUint64:
			p.kind = v1fkSliceUint64
		case v1fkFloat64:
			p.kind = v1fkSliceFloat64
		case v1fkFloat32:
			p.kind = v1fkSliceFloat32
		default:
			p.kind = v1fkSlice
		}
		p.canFast = true

	case reflect.Array:
		elem := rt.Elem()
		ep := buildV1FastMarshalPlan(elem, v1AbiTypeOfReflect(elem), visiting)
		p.elemPlan = ep
		p.elemSize = elem.Size()
		p.arrayLen = rt.Len()
		p.kind = v1fkArray
		p.canFast = true

	case reflect.Pointer:
		elem := rt.Elem()
		ep := buildV1FastMarshalPlan(elem, v1AbiTypeOfReflect(elem), visiting)
		p.elemPlan = ep
		p.kind = v1fkPtr
		p.canFast = true

	case reflect.Interface:
		if rt == reflect.TypeFor[any]() {
			p.kind = v1fkAny
			p.canFast = true
		}

	case reflect.Map:
		if rt == reflect.TypeFor[map[string]string]() {
			p.kind = v1fkMapStringString
			p.canFast = true
		} else if rt == reflect.TypeFor[map[string]int]() {
			p.kind = v1fkMapStringInt
			p.canFast = true
		} else if rt == reflect.TypeFor[map[string]any]() {
			p.kind = v1fkMapStringAny
			p.canFast = true
		}

	case reflect.Struct:
		sf := cachedTypeFields(rt)
		fastFields := make([]v1FastMarshalField, 0, len(sf.list))
		for i := range sf.list {
			f := &sf.list[i]
			if f.quoted || f.isZero != nil {
				return p
			}
			off, actualType, ok := v1DirectFieldOffset(rt, f.index)
			if !ok {
				return p
			}
			fp := buildV1FastMarshalPlan(actualType, v1AbiTypeOfReflect(actualType), visiting)
			fastFields = append(fastFields, v1FastMarshalField{
				offset:      off,
				nameNonEsc:  f.nameNonEsc,
				nameEscHTML: f.nameEscHTML,
				omitEmpty:   f.omitEmpty,
				omitZero:    f.omitZero,
				kind:        fp.kind,
				plan:        fp,
			})
		}
		p.kind = v1fkStruct
		p.fields = fastFields
		p.canFast = true
	}
	return p
}

func (p *v1FastMarshalPlan) marshal(b []byte, ptr unsafe.Pointer, escapeHTML bool, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return nil, errV1FastFallback
	}
	switch p.kind {
	case v1fkBool:
		if *(*bool)(ptr) {
			return append(b, "true"...), nil
		}
		return append(b, "false"...), nil
	case v1fkString:
		return v1AppendQuoteString(b, *(*string)(ptr), escapeHTML), nil
	case v1fkInt:
		return strconv.AppendInt(b, int64(*(*int)(ptr)), 10), nil
	case v1fkInt8:
		return strconv.AppendInt(b, int64(*(*int8)(ptr)), 10), nil
	case v1fkInt16:
		return strconv.AppendInt(b, int64(*(*int16)(ptr)), 10), nil
	case v1fkInt32:
		return strconv.AppendInt(b, int64(*(*int32)(ptr)), 10), nil
	case v1fkInt64:
		return strconv.AppendInt(b, *(*int64)(ptr), 10), nil
	case v1fkUint:
		return strconv.AppendUint(b, uint64(*(*uint)(ptr)), 10), nil
	case v1fkUint8:
		return strconv.AppendUint(b, uint64(*(*uint8)(ptr)), 10), nil
	case v1fkUint16:
		return strconv.AppendUint(b, uint64(*(*uint16)(ptr)), 10), nil
	case v1fkUint32:
		return strconv.AppendUint(b, uint64(*(*uint32)(ptr)), 10), nil
	case v1fkUint64:
		return strconv.AppendUint(b, *(*uint64)(ptr), 10), nil
	case v1fkUintptr:
		return strconv.AppendUint(b, uint64(*(*uintptr)(ptr)), 10), nil
	case v1fkFloat32:
		fv := float64(*(*float32)(ptr))
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, errV1FastFallback
		}
		return v1AppendFloat(b, fv, 32), nil
	case v1fkFloat64:
		fv := *(*float64)(ptr)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, errV1FastFallback
		}
		return v1AppendFloat(b, fv, 64), nil
	case v1fkTime:
		t := *(*time.Time)(ptr)
		if y := t.Year(); y < 0 || y >= 10000 {
			return nil, errV1FastFallback
		}
		b = append(b, '"')
		b = t.AppendFormat(b, time.RFC3339Nano)
		return append(b, '"'), nil
	case v1fkBytes:
		s := *(*[]byte)(ptr)
		if s == nil {
			return append(b, "null"...), nil
		}
		b = append(b, '"')
		b = base64.StdEncoding.AppendEncode(b, s)
		return append(b, '"'), nil
	case v1fkPtr:
		elemPtr := *(*unsafe.Pointer)(ptr)
		if elemPtr == nil {
			return append(b, "null"...), nil
		}
		return p.elemPlan.marshal(b, elemPtr, escapeHTML, depth+1)
	case v1fkSliceBool:
		s := *(*[]bool)(ptr)
		if s == nil {
			return append(b, "null"...), nil
		}
		if len(s) == 0 {
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
	case v1fkSliceString:
		s := *(*[]string)(ptr)
		if s == nil {
			return append(b, "null"...), nil
		}
		if len(s) == 0 {
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		for i, v := range s {
			if i > 0 {
				b = append(b, ',')
			}
			b = v1AppendQuoteString(b, v, escapeHTML)
		}
		return append(b, ']'), nil
	case v1fkSliceInt:
		s := *(*[]int)(ptr)
		if s == nil {
			return append(b, "null"...), nil
		}
		if len(s) == 0 {
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
	case v1fkSliceInt64:
		s := *(*[]int64)(ptr)
		if s == nil {
			return append(b, "null"...), nil
		}
		if len(s) == 0 {
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
	case v1fkSliceUint64:
		s := *(*[]uint64)(ptr)
		if s == nil {
			return append(b, "null"...), nil
		}
		if len(s) == 0 {
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
	case v1fkSliceFloat64:
		s := *(*[]float64)(ptr)
		if s == nil {
			return append(b, "null"...), nil
		}
		if len(s) == 0 {
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		for i, v := range s {
			if i > 0 {
				b = append(b, ',')
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, errV1FastFallback
			}
			b = v1AppendFloat(b, v, 64)
		}
		return append(b, ']'), nil
	case v1fkSliceFloat32:
		s := *(*[]float32)(ptr)
		if s == nil {
			return append(b, "null"...), nil
		}
		if len(s) == 0 {
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		for i, v := range s {
			if i > 0 {
				b = append(b, ',')
			}
			fv := float64(v)
			if math.IsNaN(fv) || math.IsInf(fv, 0) {
				return nil, errV1FastFallback
			}
			b = v1AppendFloat(b, fv, 32)
		}
		return append(b, ']'), nil
	case v1fkSlice:
		sh := (*unsafeheader.Slice)(ptr)
		if sh.Data == nil {
			return append(b, "null"...), nil
		}
		if sh.Len == 0 {
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
			b, err = elemPlan.marshal(b, unsafe.Add(data, uintptr(i)*elemSize), escapeHTML, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	case v1fkArray:
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
			b, err = elemPlan.marshal(b, unsafe.Add(ptr, uintptr(i)*elemSize), escapeHTML, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	case v1fkMapStringString:
		return v1MarshalFastMapStringString(b, *(*map[string]string)(ptr), escapeHTML)
	case v1fkMapStringInt:
		return v1MarshalFastMapStringInt(b, *(*map[string]int)(ptr), escapeHTML)
	case v1fkMapStringAny:
		return v1MarshalFastMapStringAny(b, *(*map[string]any)(ptr), escapeHTML, depth+1)
	case v1fkAny:
		return v1MarshalFastAny(b, *(*any)(ptr), escapeHTML, depth+1)
	case v1fkStruct:
		return p.marshalStruct(b, ptr, escapeHTML, depth+1)
	}
	return nil, errV1FastFallback
}

func (p *v1FastMarshalPlan) marshalStruct(b []byte, ptr unsafe.Pointer, escapeHTML bool, depth int) ([]byte, error) {
	b = append(b, '{')
	first := true
	fields := p.fields
	var err error
	for i := range fields {
		f := &fields[i]
		fp := unsafe.Add(ptr, f.offset)
		if f.omitEmpty && v1IsFastEmpty(f.kind, f.plan, fp) {
			continue
		}
		if f.omitZero && v1IsFastZero(f.kind, f.plan, fp) {
			continue
		}
		if !first {
			b = append(b, ',')
		}
		first = false
		if escapeHTML {
			b = append(b, f.nameEscHTML...)
		} else {
			b = append(b, f.nameNonEsc...)
		}
		switch f.kind {
		case v1fkString:
			b = v1AppendQuoteString(b, *(*string)(fp), escapeHTML)
		case v1fkInt:
			b = strconv.AppendInt(b, int64(*(*int)(fp)), 10)
		case v1fkInt32:
			b = strconv.AppendInt(b, int64(*(*int32)(fp)), 10)
		case v1fkInt64:
			b = strconv.AppendInt(b, *(*int64)(fp), 10)
		case v1fkUint:
			b = strconv.AppendUint(b, uint64(*(*uint)(fp)), 10)
		case v1fkUint32:
			b = strconv.AppendUint(b, uint64(*(*uint32)(fp)), 10)
		case v1fkUint64:
			b = strconv.AppendUint(b, *(*uint64)(fp), 10)
		case v1fkFloat64:
			fv := *(*float64)(fp)
			if math.IsNaN(fv) || math.IsInf(fv, 0) {
				return nil, errV1FastFallback
			}
			b = v1AppendFloat(b, fv, 64)
		case v1fkFloat32:
			fv := float64(*(*float32)(fp))
			if math.IsNaN(fv) || math.IsInf(fv, 0) {
				return nil, errV1FastFallback
			}
			b = v1AppendFloat(b, fv, 32)
		case v1fkBool:
			if *(*bool)(fp) {
				b = append(b, "true"...)
			} else {
				b = append(b, "false"...)
			}
		default:
			if b, err = f.plan.marshal(b, fp, escapeHTML, depth); err != nil {
				return nil, err
			}
		}
	}
	return append(b, '}'), nil
}

func v1IsFastEmpty(k v1FastKind, plan *v1FastMarshalPlan, ptr unsafe.Pointer) bool {
	switch k {
	case v1fkBool:
		return !*(*bool)(ptr)
	case v1fkString:
		return len(*(*string)(ptr)) == 0
	case v1fkInt:
		return *(*int)(ptr) == 0
	case v1fkInt8:
		return *(*int8)(ptr) == 0
	case v1fkInt16:
		return *(*int16)(ptr) == 0
	case v1fkInt32:
		return *(*int32)(ptr) == 0
	case v1fkInt64:
		return *(*int64)(ptr) == 0
	case v1fkUint:
		return *(*uint)(ptr) == 0
	case v1fkUint8:
		return *(*uint8)(ptr) == 0
	case v1fkUint16:
		return *(*uint16)(ptr) == 0
	case v1fkUint32:
		return *(*uint32)(ptr) == 0
	case v1fkUint64:
		return *(*uint64)(ptr) == 0
	case v1fkUintptr:
		return *(*uintptr)(ptr) == 0
	case v1fkFloat32:
		return *(*float32)(ptr) == 0
	case v1fkFloat64:
		return *(*float64)(ptr) == 0
	case v1fkBytes:
		return len(*(*[]byte)(ptr)) == 0
	case v1fkSliceBool, v1fkSliceString, v1fkSliceInt, v1fkSliceInt64, v1fkSliceUint64, v1fkSliceFloat64, v1fkSliceFloat32, v1fkSlice:
		return (*unsafeheader.Slice)(ptr).Len == 0
	case v1fkArray:
		return plan.arrayLen == 0
	case v1fkPtr:
		return *(*unsafe.Pointer)(ptr) == nil
	case v1fkMapStringString:
		return len(*(*map[string]string)(ptr)) == 0
	case v1fkMapStringInt:
		return len(*(*map[string]int)(ptr)) == 0
	case v1fkMapStringAny:
		return len(*(*map[string]any)(ptr)) == 0
	case v1fkAny:
		return *(*any)(ptr) == nil
	}
	return false
}

func v1IsFastZero(k v1FastKind, plan *v1FastMarshalPlan, ptr unsafe.Pointer) bool {
	switch k {
	case v1fkBool:
		return !*(*bool)(ptr)
	case v1fkString:
		return len(*(*string)(ptr)) == 0
	case v1fkInt:
		return *(*int)(ptr) == 0
	case v1fkInt8:
		return *(*int8)(ptr) == 0
	case v1fkInt16:
		return *(*int16)(ptr) == 0
	case v1fkInt32:
		return *(*int32)(ptr) == 0
	case v1fkInt64:
		return *(*int64)(ptr) == 0
	case v1fkUint:
		return *(*uint)(ptr) == 0
	case v1fkUint8:
		return *(*uint8)(ptr) == 0
	case v1fkUint16:
		return *(*uint16)(ptr) == 0
	case v1fkUint32:
		return *(*uint32)(ptr) == 0
	case v1fkUint64:
		return *(*uint64)(ptr) == 0
	case v1fkUintptr:
		return *(*uintptr)(ptr) == 0
	case v1fkFloat32:
		return math.Float32bits(*(*float32)(ptr)) == 0
	case v1fkFloat64:
		return math.Float64bits(*(*float64)(ptr)) == 0
	case v1fkTime:
		return (*time.Time)(ptr).IsZero()
	case v1fkBytes:
		return *(*[]byte)(ptr) == nil
	case v1fkSliceBool, v1fkSliceString, v1fkSliceInt, v1fkSliceInt64, v1fkSliceUint64, v1fkSliceFloat64, v1fkSliceFloat32, v1fkSlice:
		return (*unsafeheader.Slice)(ptr).Data == nil
	case v1fkPtr:
		return *(*unsafe.Pointer)(ptr) == nil
	case v1fkMapStringString:
		return *(*map[string]string)(ptr) == nil
	case v1fkMapStringInt:
		return *(*map[string]int)(ptr) == nil
	case v1fkMapStringAny:
		return *(*map[string]any)(ptr) == nil
	case v1fkAny:
		return *(*any)(ptr) == nil
	case v1fkArray, v1fkStruct:
		return reflect.NewAt(plan.rt, ptr).Elem().IsZero()
	}
	return false
}

func v1MarshalFastMapStringString(b []byte, m map[string]string, escapeHTML bool) ([]byte, error) {
	if m == nil {
		return append(b, "null"...), nil
	}
	n := len(m)
	if n == 0 {
		return append(b, "{}"...), nil
	}
	b = append(b, '{')
	if n == 1 {
		for k, v := range m {
			b = v1AppendQuoteString(b, k, escapeHTML)
			b = append(b, ':')
			b = v1AppendQuoteString(b, v, escapeHTML)
		}
		return append(b, '}'), nil
	}
	names := getV1Strings(n)
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
		b = v1AppendQuoteString(b, k, escapeHTML)
		b = append(b, ':')
		b = v1AppendQuoteString(b, m[k], escapeHTML)
	}
	putV1Strings(names)
	return append(b, '}'), nil
}

func v1MarshalFastMapStringInt(b []byte, m map[string]int, escapeHTML bool) ([]byte, error) {
	if m == nil {
		return append(b, "null"...), nil
	}
	n := len(m)
	if n == 0 {
		return append(b, "{}"...), nil
	}
	b = append(b, '{')
	if n == 1 {
		for k, v := range m {
			b = v1AppendQuoteString(b, k, escapeHTML)
			b = append(b, ':')
			b = strconv.AppendInt(b, int64(v), 10)
		}
		return append(b, '}'), nil
	}
	names := getV1Strings(n)
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
		b = v1AppendQuoteString(b, k, escapeHTML)
		b = append(b, ':')
		b = strconv.AppendInt(b, int64(m[k]), 10)
	}
	putV1Strings(names)
	return append(b, '}'), nil
}

func v1MarshalFastMapStringAny(b []byte, m map[string]any, escapeHTML bool, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return nil, errV1FastFallback
	}
	if m == nil {
		return append(b, "null"...), nil
	}
	n := len(m)
	if n == 0 {
		return append(b, "{}"...), nil
	}
	b = append(b, '{')
	var err error
	if n == 1 {
		for k, v := range m {
			b = v1AppendQuoteString(b, k, escapeHTML)
			b = append(b, ':')
			if b, err = v1MarshalFastAny(b, v, escapeHTML, depth); err != nil {
				return nil, err
			}
		}
		return append(b, '}'), nil
	}
	names := getV1Strings(n)
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
		b = v1AppendQuoteString(b, k, escapeHTML)
		b = append(b, ':')
		if b, err = v1MarshalFastAny(b, m[k], escapeHTML, depth); err != nil {
			putV1Strings(names)
			return nil, err
		}
	}
	putV1Strings(names)
	return append(b, '}'), nil
}

func v1MarshalFastAny(b []byte, val any, escapeHTML bool, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return nil, errV1FastFallback
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
		return v1AppendQuoteString(b, v, escapeHTML), nil
	case int:
		return strconv.AppendInt(b, int64(v), 10), nil
	case int64:
		return strconv.AppendInt(b, v, 10), nil
	case int32:
		return strconv.AppendInt(b, int64(v), 10), nil
	case uint:
		return strconv.AppendUint(b, uint64(v), 10), nil
	case uint64:
		return strconv.AppendUint(b, v, 10), nil
	case uint32:
		return strconv.AppendUint(b, uint64(v), 10), nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, errV1FastFallback
		}
		return v1AppendFloat(b, v, 64), nil
	case float32:
		fv := float64(v)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, errV1FastFallback
		}
		return v1AppendFloat(b, fv, 32), nil
	case []any:
		if v == nil {
			return append(b, "null"...), nil
		}
		if len(v) == 0 {
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		var err error
		for i, elem := range v {
			if i > 0 {
				b = append(b, ',')
			}
			if b, err = v1MarshalFastAny(b, elem, escapeHTML, depth+1); err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	case map[string]any:
		return v1MarshalFastMapStringAny(b, v, escapeHTML, depth+1)
	case []int:
		if v == nil {
			return append(b, "null"...), nil
		}
		if len(v) == 0 {
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
		if v == nil {
			return append(b, "null"...), nil
		}
		if len(v) == 0 {
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		for i, s := range v {
			if i > 0 {
				b = append(b, ',')
			}
			b = v1AppendQuoteString(b, s, escapeHTML)
		}
		return append(b, ']'), nil
	case map[string]string:
		return v1MarshalFastMapStringString(b, v, escapeHTML)
	case Number:
		return nil, errV1FastFallback
	}
	at, ptr := v1EfacePtr(&val)
	if at == nil {
		return append(b, "null"...), nil
	}
	plan := lookupV1FastMarshalPlan(at)
	if !plan.canFast {
		return nil, errV1FastFallback
	}
	b, err := plan.marshal(b, ptr, escapeHTML, depth+1)
	runtime.KeepAlive(&val)
	return b, err
}

func v1MarshalFastAlloc(plan *v1FastMarshalPlan, ptr unsafe.Pointer, escapeHTML bool) ([]byte, bool) {
	for depth := 0; plan.kind == v1fkPtr && depth < startDetectingCyclesAfter; depth++ {
		p := *(*unsafe.Pointer)(ptr)
		if p == nil {
			return []byte("null"), true
		}
		ptr = p
		plan = plan.elemPlan
	}
	switch plan.kind {
	case v1fkBool:
		if *(*bool)(ptr) {
			return []byte("true"), true
		}
		return []byte("false"), true
	case v1fkString:
		s := *(*string)(ptr)
		if v1StringSafeHTML(s) {
			out := make([]byte, len(s)+2)
			out[0] = '"'
			copy(out[1:], s)
			out[len(s)+1] = '"'
			return out, true
		}
	case v1fkInt:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], int64(*(*int)(ptr)), 10)), true
	case v1fkInt8:
		var scratch [8]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], int64(*(*int8)(ptr)), 10)), true
	case v1fkInt16:
		var scratch [8]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], int64(*(*int16)(ptr)), 10)), true
	case v1fkInt32:
		var scratch [16]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], int64(*(*int32)(ptr)), 10)), true
	case v1fkInt64:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], *(*int64)(ptr), 10)), true
	case v1fkUint:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uint)(ptr)), 10)), true
	case v1fkUint8:
		var scratch [8]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uint8)(ptr)), 10)), true
	case v1fkUint16:
		var scratch [8]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uint16)(ptr)), 10)), true
	case v1fkUint32:
		var scratch [16]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uint32)(ptr)), 10)), true
	case v1fkUint64:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], *(*uint64)(ptr), 10)), true
	case v1fkUintptr:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uintptr)(ptr)), 10)), true
	case v1fkFloat32:
		fv := float64(*(*float32)(ptr))
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, false
		}
		var scratch [32]byte
		return bytes.Clone(v1AppendFloat(scratch[:0], fv, 32)), true
	case v1fkFloat64:
		fv := *(*float64)(ptr)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, false
		}
		var scratch [32]byte
		return bytes.Clone(v1AppendFloat(scratch[:0], fv, 64)), true
	}
	fb := getV1FastBuf()
	b, err := plan.marshal(fb.b, ptr, escapeHTML, 0)
	if err != nil {
		putV1FastBuf(fb)
		return nil, false
	}
	fb.b = b
	out := bytes.Clone(b)
	putV1FastBuf(fb)
	return out, true
}
