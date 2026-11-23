// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.jsonv2

package json

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"internal/abi"
	"internal/byteorder"
	"internal/runtime/maps"
	istrconv "internal/strconv"
	"internal/unsafeheader"
	"math"
	"math/bits"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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
	e := (*v1Eface)(v1Noescape(unsafe.Pointer(a)))
	if e.typ == nil {
		return nil, nil
	}
	if e.typ.IsDirectIface() {
		return e.typ, v1Noescape(unsafe.Pointer(&e.data))
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
	need := len(src) + 2
	if cap(dst)-len(dst) < need {
		dst = slices.Grow(dst, need)
	}
	b := unsafe.Slice(unsafe.StringData(src), len(src))
	i := 0
	if escapeHTML {
		for len(b)-i >= 8 {
			if m := v1EscapeMaskHTML(byteorder.LEUint64(b[i:])); m != 0 {
				return v1AppendQuoteStringSlow(dst, src, b, i+(bits.TrailingZeros64(m)>>3), true)
			}
			i += 8
		}
		for i < len(b) {
			c := b[i]
			if c >= utf8.RuneSelf || !htmlSafeSet[c] {
				return v1AppendQuoteStringSlow(dst, src, b, i, true)
			}
			i++
		}
	} else {
		for len(b)-i >= 8 {
			if m := v1EscapeMaskNoHTML(byteorder.LEUint64(b[i:])); m != 0 {
				return v1AppendQuoteStringSlow(dst, src, b, i+(bits.TrailingZeros64(m)>>3), false)
			}
			i += 8
		}
		for i < len(b) {
			c := b[i]
			if c >= utf8.RuneSelf || !safeSet[c] {
				return v1AppendQuoteStringSlow(dst, src, b, i, false)
			}
			i++
		}
	}
	off := len(dst)
	dst = dst[:off+need]
	dst[off] = '"'
	copy(dst[off+1:], src)
	dst[off+1+len(src)] = '"'
	return dst
}

func v1AppendQuoteStringSlow(dst []byte, src string, b []byte, i int, escapeHTML bool) []byte {
	dst = append(dst, '"')
	start := 0
	first := true
	for i < len(b) {
		if !first {
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
		}
		first = false
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
		for i < len(b) {
			b0 := b[i]
			if b0 < utf8.RuneSelf {
				break
			}
			if b0 >= 0xC2 && b0 <= 0xDF {
				if i+1 < len(b) && b[i+1]&0xC0 == 0x80 {
					i += 2
					continue
				}
			} else if b0 >= 0xE0 && b0 <= 0xEF {
				if i+2 < len(b) && b[i+2]&0xC0 == 0x80 {
					b1 := b[i+1]
					if b0 == 0xE2 {
						if b1&0xC0 == 0x80 && (b1 != 0x80 || (b[i+2] != 0xA8 && b[i+2] != 0xA9)) {
							i += 3
							continue
						}
					} else if b0 == 0xE0 {
						if b1 >= 0xA0 && b1 <= 0xBF {
							i += 3
							continue
						}
					} else if b0 == 0xED {
						if b1 >= 0x80 && b1 <= 0x9F {
							i += 3
							continue
						}
					} else if b1&0xC0 == 0x80 {
						i += 3
						continue
					}
				}
			} else if b0 >= 0xF0 && b0 <= 0xF4 {
				if i+3 < len(b) && b[i+2]&0xC0 == 0x80 && b[i+3]&0xC0 == 0x80 {
					b1 := b[i+1]
					if (b0 == 0xF0 && b1 >= 0x90 && b1 <= 0xBF) ||
						(b0 >= 0xF1 && b0 <= 0xF3 && b1&0xC0 == 0x80) ||
						(b0 == 0xF4 && b1 >= 0x80 && b1 <= 0x8F) {
						i += 4
						continue
					}
				}
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
	}
	dst = append(dst, src[start:]...)
	dst = append(dst, '"')
	return dst
}

func v1AppendFloat(b []byte, f float64, bits int) []byte {
	return istrconv.AppendFloatECMA(b, f, bits)
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
	v1fkSlicePoint2F64
	v1fkSlice
	v1fkArray
	v1fkPtr
	v1fkStruct
	v1fkMapStringString
	v1fkMapStringInt
	v1fkMapStringAny
	v1fkMapStringElem
	v1fkMapIntElem
	v1fkMapUintElem
	v1fkAny
	v1fkTime
	v1fkMarshaler
	v1fkUnmarshaler
)

func v1IsFastNumericKind(k v1FastKind) bool {
	return k >= v1fkInt && k <= v1fkFloat64
}

var (
	v1TimeType           = reflect.TypeFor[time.Time]()
	v1SlicePoint2F64Type = reflect.TypeFor[[][2]float64]()
)

type v1FastMarshalField struct {
	offset           uintptr
	nameNonEsc       string
	nameEscHTML      string
	commaNameNonEsc  string
	commaNameEscHTML string
	omitEmpty        bool
	omitZero         bool
	quoted           bool
	kind             v1FastKind
	plan             *v1FastMarshalPlan
}

type v1FastMarshalPlan struct {
	typ         *abi.Type
	rt          reflect.Type
	mapType     *abi.MapType
	ifaceTab    unsafe.Pointer
	kind        v1FastKind
	keyBits     uint8
	canFast     bool
	hasCustom   bool
	hasCycle    bool
	building    bool
	hasOmit     bool
	hasQuoted   bool
	isFlatSmall bool
	minSize     int
	sizeHint    atomic.Uint32
	elemSize    uintptr
	arrayLen    int
	elemPlan    *v1FastMarshalPlan
	fields      []v1FastMarshalField
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
			switch p.kind {
			case v1fkPtr, v1fkSlice, v1fkArray, v1fkMapStringElem, v1fkMapIntElem, v1fkMapUintElem:
				if p.elemPlan == nil || !p.elemPlan.canFast {
					if p.canFast {
						p.canFast = false
						changed = true
					}
				} else {
					if p.elemPlan.hasCustom && !p.hasCustom {
						p.hasCustom = true
						changed = true
					}
					if p.elemPlan.hasCycle && !p.hasCycle {
						p.hasCycle = true
						changed = true
					}
				}
			case v1fkStruct:
				for i := range p.fields {
					f := &p.fields[i]
					if f.plan == nil || !f.plan.canFast {
						if p.canFast {
							p.canFast = false
							changed = true
						}
						break
					}
					if f.plan.hasCustom && !p.hasCustom {
						p.hasCustom = true
						changed = true
					}
					if f.plan.hasCycle && !p.hasCycle {
						p.hasCycle = true
						changed = true
					}
				}
			}
		}
	}
	for _, p := range visiting {
		if p.kind == v1fkStruct && p.canFast && !p.hasCustom && !p.hasOmit && !p.hasCycle && p.minSize > 0 && p.minSize <= 256 {
			flat := true
			for i := range p.fields {
				k := p.fields[i].kind
				if k < v1fkBool || k > v1fkSliceFloat32 {
					flat = false
					break
				}
			}
			p.isFlatSmall = flat
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
		if p.building {
			p.hasCycle = true
		}
		return p
	}
	p := &v1FastMarshalPlan{typ: at, rt: rt, building: true}
	visiting[at] = p
	defer func() { p.building = false }()

	if rt == v1TimeType {
		p.kind = v1fkTime
		p.canFast = true
		return p
	}
	if rt == numberType {
		return p
	}
	if v1HasCustomMarshaler(rt) {
		if rt.Kind() != reflect.Pointer && rt.Kind() != reflect.Interface && rt.Implements(marshalerType) {
			m := reflect.New(rt).Interface().(Marshaler)
			p.ifaceTab = (*[2]unsafe.Pointer)(unsafe.Pointer(&m))[0]
			p.kind = v1fkMarshaler
			p.hasCustom = true
			p.canFast = true
			return p
		}
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
		case v1fkArray:
			if rt == v1SlicePoint2F64Type && ep.arrayLen == 2 && ep.elemPlan != nil && ep.elemPlan.kind == v1fkFloat64 {
				p.kind = v1fkSlicePoint2F64
			} else {
				p.kind = v1fkSlice
			}
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
			p.hasCustom = true
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
			p.hasCustom = true
			p.canFast = true
		} else {
			kt := rt.Key()
			if kt == numberType || v1HasCustomMarshaler(kt) {
				return p
			}
			switch kt.Kind() {
			case reflect.String:
				p.kind = v1fkMapStringElem
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				p.kind = v1fkMapIntElem
				p.keyBits = uint8(kt.Bits())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				p.kind = v1fkMapUintElem
				p.keyBits = uint8(kt.Bits())
			default:
				return p
			}
			elem := rt.Elem()
			p.elemPlan = buildV1FastMarshalPlan(elem, v1AbiTypeOfReflect(elem), visiting)
			p.mapType = (*abi.MapType)(unsafe.Pointer(at))
			p.canFast = true
		}

	case reflect.Struct:
		sf := cachedTypeFields(rt)
		fastFields := make([]v1FastMarshalField, 0, len(sf.list))
		hasOmit := false
		hasQuoted := false
		minSize := 2
		for i := range sf.list {
			f := &sf.list[i]
			if f.isZero != nil {
				return p
			}
			off, actualType, ok := v1DirectFieldOffset(rt, f.index)
			if !ok {
				return p
			}
			fp := buildV1FastMarshalPlan(actualType, v1AbiTypeOfReflect(actualType), visiting)
			if f.quoted {
				if !v1IsFastNumericKind(fp.kind) && fp.kind != v1fkString {
					return p
				}
				hasQuoted = true
			}
			if f.omitEmpty || f.omitZero {
				hasOmit = true
			}
			prefix := ","
			if i == 0 {
				prefix = "{"
			}
			cNonEsc := prefix + f.nameNonEsc
			cEscHTML := prefix + f.nameEscHTML
			minSize += len(cEscHTML) + 4
			fastFields = append(fastFields, v1FastMarshalField{
				offset:           off,
				nameNonEsc:       f.nameNonEsc,
				nameEscHTML:      f.nameEscHTML,
				commaNameNonEsc:  cNonEsc,
				commaNameEscHTML: cEscHTML,
				omitEmpty:        f.omitEmpty,
				omitZero:         f.omitZero,
				quoted:           f.quoted,
				kind:             fp.kind,
				plan:             fp,
			})
		}
		p.kind = v1fkStruct
		p.fields = fastFields
		p.hasOmit = hasOmit
		p.hasQuoted = hasQuoted
		p.minSize = minSize
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
		return istrconv.AppendInt10(b, int64(*(*int)(ptr))), nil
	case v1fkInt8:
		return istrconv.AppendInt10(b, int64(*(*int8)(ptr))), nil
	case v1fkInt16:
		return istrconv.AppendInt10(b, int64(*(*int16)(ptr))), nil
	case v1fkInt32:
		return istrconv.AppendInt10(b, int64(*(*int32)(ptr))), nil
	case v1fkInt64:
		return istrconv.AppendInt10(b, *(*int64)(ptr)), nil
	case v1fkUint:
		return istrconv.AppendUint10(b, uint64(*(*uint)(ptr))), nil
	case v1fkUint8:
		return istrconv.AppendUint10(b, uint64(*(*uint8)(ptr))), nil
	case v1fkUint16:
		return istrconv.AppendUint10(b, uint64(*(*uint16)(ptr))), nil
	case v1fkUint32:
		return istrconv.AppendUint10(b, uint64(*(*uint32)(ptr))), nil
	case v1fkUint64:
		return istrconv.AppendUint10(b, *(*uint64)(ptr)), nil
	case v1fkUintptr:
		return istrconv.AppendUint10(b, uint64(*(*uintptr)(ptr))), nil
	case v1fkFloat32:
		fv := float64(*(*float32)(ptr))
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, v1UnsupportedFloatError(p.rt, ptr, fv, 32)
		}
		return v1AppendFloat(b, fv, 32), nil
	case v1fkFloat64:
		fv := *(*float64)(ptr)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, v1UnsupportedFloatError(p.rt, ptr, fv, 64)
		}
		return v1AppendFloat(b, fv, 64), nil
	case v1fkTime:
		t := *(*time.Time)(ptr)
		if y := t.Year(); y < 0 || y >= 10000 {
			if _, err := t.MarshalJSON(); err != nil {
				return nil, &MarshalerError{v1TimeType, err, "MarshalJSON"}
			}
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
		if len(s) >= 8 {
			b = slices.Grow(b, len(s)*4+2)
		}
		b = append(b, '[')
		for i, v := range s {
			if i > 0 {
				b = append(b, ',')
			}
			b = istrconv.AppendInt10(b, int64(v))
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
		if len(s) >= 8 {
			b = slices.Grow(b, len(s)*4+2)
		}
		b = append(b, '[')
		for i, v := range s {
			if i > 0 {
				b = append(b, ',')
			}
			b = istrconv.AppendInt10(b, v)
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
		if len(s) >= 8 {
			b = slices.Grow(b, len(s)*4+2)
		}
		b = append(b, '[')
		for i, v := range s {
			if i > 0 {
				b = append(b, ',')
			}
			b = istrconv.AppendUint10(b, v)
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
				return nil, v1UnsupportedFloatError(p.elemPlan.rt, unsafe.Pointer(&s[i]), v, 64)
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
				return nil, v1UnsupportedFloatError(p.elemPlan.rt, unsafe.Pointer(&s[i]), fv, 32)
			}
			b = v1AppendFloat(b, fv, 32)
		}
		return append(b, ']'), nil
	case v1fkSlicePoint2F64:
		s := *(*[][2]float64)(ptr)
		if s == nil {
			return append(b, "null"...), nil
		}
		if len(s) == 0 {
			return append(b, "[]"...), nil
		}
		b = slices.Grow(b, len(s)*38+2)
		b = append(b, '[')
		for i, pt := range s {
			if i > 0 {
				b = append(b, ',')
			}
			v0, v1 := pt[0], pt[1]
			if math.IsNaN(v0) || math.IsInf(v0, 0) {
				return nil, v1UnsupportedFloatError(p.elemPlan.elemPlan.rt, unsafe.Pointer(&s[i][0]), v0, 64)
			}
			if math.IsNaN(v1) || math.IsInf(v1, 0) {
				return nil, v1UnsupportedFloatError(p.elemPlan.elemPlan.rt, unsafe.Pointer(&s[i][1]), v1, 64)
			}
			b = append(b, '[')
			b = istrconv.AppendFloatECMA(b, v0, 64)
			b = append(b, ',')
			b = istrconv.AppendFloatECMA(b, v1, 64)
			b = append(b, ']')
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
		elemPlan := p.elemPlan
		if elemPlan.kind == v1fkFloat64 && n == 2 {
			arr := (*[2]float64)(ptr)
			v0, v1 := arr[0], arr[1]
			if !math.IsNaN(v0) && !math.IsInf(v0, 0) && !math.IsNaN(v1) && !math.IsInf(v1, 0) {
				b = append(b, '[')
				b = v1AppendFloat(b, v0, 64)
				b = append(b, ',')
				b = v1AppendFloat(b, v1, 64)
				return append(b, ']'), nil
			}
		}
		b = append(b, '[')
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
	case v1fkMapStringElem:
		return p.marshalMapStringElem(b, ptr, escapeHTML, depth+1)
	case v1fkMapIntElem:
		return p.marshalMapIntElem(b, ptr, escapeHTML, depth+1)
	case v1fkMapUintElem:
		return p.marshalMapUintElem(b, ptr, escapeHTML, depth+1)
	case v1fkAny:
		return v1MarshalFastAny(b, *(*any)(ptr), escapeHTML, depth+1)
	case v1fkMarshaler:
		return p.marshalCustomMarshaler(b, ptr, escapeHTML)
	case v1fkStruct:
		return p.marshalStruct(b, ptr, escapeHTML, depth+1)
	}
	return nil, errV1FastFallback
}

func v1UnsupportedFloatError(rt reflect.Type, ptr unsafe.Pointer, fv float64, bits int) error {
	return &UnsupportedValueError{
		Value: reflect.NewAt(rt, ptr).Elem(),
		Str:   strconv.FormatFloat(fv, 'g', -1, bits),
	}
}

func (p *v1FastMarshalPlan) marshalCustomMarshaler(b []byte, ptr unsafe.Pointer, escapeHTML bool) ([]byte, error) {
	if ptr == nil {
		return nil, errV1FastFallback
	}
	var m Marshaler
	ip := (*[2]unsafe.Pointer)(unsafe.Pointer(&m))
	ip[0] = p.ifaceTab
	ip[1] = ptr
	val, err := m.MarshalJSON()
	runtime.KeepAlive(ptr)
	if err != nil {
		return nil, &MarshalerError{p.rt, err, "MarshalJSON"}
	}
	if out, cerr := v1AppendCompactValidJSON(b, val, escapeHTML); cerr == nil {
		return out, nil
	}
	out, cerr := appendCompact(b, val, escapeHTML)
	if cerr != nil {
		return nil, &MarshalerError{p.rt, cerr, "MarshalJSON"}
	}
	return out, nil
}

func v1AppendCompactValidJSON(dst, src []byte, escapeHTML bool) ([]byte, error) {
	dst, i, err := v1AppendCompactValidJSONValue(dst, src, 0, escapeHTML, 0)
	if err != nil {
		return nil, err
	}
	for i < len(src) && isSpace(src[i]) {
		i++
	}
	if i != len(src) {
		return nil, errV1FastFallback
	}
	return dst, nil
}

func v1AppendCompactValidJSONString(dst, src []byte, pos int, escapeHTML bool) ([]byte, int, error) {
	if pos >= len(src) || src[pos] != '"' {
		return nil, 0, errV1FastFallback
	}
	start := pos + 1
	i := start
	if escapeHTML {
		for len(src)-i >= 8 {
			if m := v1EscapeMaskHTML(byteorder.LEUint64(src[i:])); m != 0 {
				i += bits.TrailingZeros64(m) >> 3
				goto special
			}
			i += 8
		}
		for i < len(src) {
			c := src[i]
			if c >= utf8.RuneSelf || !htmlSafeSet[c] {
				break
			}
			i++
		}
	} else {
		for len(src)-i >= 8 {
			if m := v1EscapeMaskNoHTML(byteorder.LEUint64(src[i:])); m != 0 {
				i += bits.TrailingZeros64(m) >> 3
				goto special
			}
			i += 8
		}
		for i < len(src) {
			c := src[i]
			if c >= utf8.RuneSelf || c < 0x20 || c == '\\' || c == '"' {
				break
			}
			i++
		}
	}
special:
	if i < len(src) && src[i] == '"' {
		return append(dst, src[pos:i+1]...), i + 1, nil
	}
	if escapeHTML {
		return nil, 0, errV1FastFallback
	}
	end := v1ScanStringEnd(src, pos)
	if end < 0 {
		return nil, 0, errV1FastFallback
	}
	return append(dst, src[pos:end]...), end, nil
}

func v1AppendCompactValidJSONValue(dst, src []byte, i int, escapeHTML bool, depth int) ([]byte, int, error) {
	if depth >= 64 {
		return nil, 0, errV1FastFallback
	}
	for i < len(src) && isSpace(src[i]) {
		i++
	}
	if i >= len(src) {
		return nil, 0, errV1FastFallback
	}
	switch src[i] {
	case 'n':
		if i+4 <= len(src) && string(src[i:i+4]) == "null" {
			return append(dst, "null"...), i + 4, nil
		}
		return nil, 0, errV1FastFallback
	case 't':
		if i+4 <= len(src) && string(src[i:i+4]) == "true" {
			return append(dst, "true"...), i + 4, nil
		}
		return nil, 0, errV1FastFallback
	case 'f':
		if i+5 <= len(src) && string(src[i:i+5]) == "false" {
			return append(dst, "false"...), i + 5, nil
		}
		return nil, 0, errV1FastFallback
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		end := v1ScanNumberEnd(src, i)
		if end < 0 {
			return nil, 0, errV1FastFallback
		}
		return append(dst, src[i:end]...), end, nil
	case '"':
		return v1AppendCompactValidJSONString(dst, src, i, escapeHTML)
	case '[':
		i++
		dst = append(dst, '[')
		for i < len(src) && isSpace(src[i]) {
			i++
		}
		if i < len(src) && src[i] == ']' {
			return append(dst, ']'), i + 1, nil
		}
		var err error
		for {
			dst, i, err = v1AppendCompactValidJSONValue(dst, src, i, escapeHTML, depth+1)
			if err != nil {
				return nil, 0, err
			}
			for i < len(src) && isSpace(src[i]) {
				i++
			}
			if i >= len(src) {
				return nil, 0, errV1FastFallback
			}
			if src[i] == ',' {
				i++
				dst = append(dst, ',')
				continue
			}
			if src[i] == ']' {
				return append(dst, ']'), i + 1, nil
			}
			return nil, 0, errV1FastFallback
		}
	case '{':
		i++
		dst = append(dst, '{')
		for i < len(src) && isSpace(src[i]) {
			i++
		}
		if i < len(src) && src[i] == '}' {
			return append(dst, '}'), i + 1, nil
		}
		var err error
		for {
			for i < len(src) && isSpace(src[i]) {
				i++
			}
			dst, i, err = v1AppendCompactValidJSONString(dst, src, i, escapeHTML)
			if err != nil {
				return nil, 0, err
			}
			for i < len(src) && isSpace(src[i]) {
				i++
			}
			if i >= len(src) || src[i] != ':' {
				return nil, 0, errV1FastFallback
			}
			i++
			dst = append(dst, ':')
			dst, i, err = v1AppendCompactValidJSONValue(dst, src, i, escapeHTML, depth+1)
			if err != nil {
				return nil, 0, err
			}
			for i < len(src) && isSpace(src[i]) {
				i++
			}
			if i >= len(src) {
				return nil, 0, errV1FastFallback
			}
			if src[i] == ',' {
				i++
				dst = append(dst, ',')
				continue
			}
			if src[i] == '}' {
				return append(dst, '}'), i + 1, nil
			}
			return nil, 0, errV1FastFallback
		}
	}
	return nil, 0, errV1FastFallback
}

func (p *v1FastMarshalPlan) marshalStruct(b []byte, ptr unsafe.Pointer, escapeHTML bool, depth int) ([]byte, error) {
	fields := p.fields
	if len(fields) == 0 {
		return append(b, "{}"...), nil
	}
	if !p.hasOmit && !p.hasQuoted {
		if cap(b)-len(b) < p.minSize {
			b = slices.Grow(b, p.minSize)
		}
		var err error
		for i := range fields {
			f := &fields[i]
			fp := unsafe.Add(ptr, f.offset)
			if escapeHTML {
				b = append(b, f.commaNameEscHTML...)
			} else {
				b = append(b, f.commaNameNonEsc...)
			}
			switch f.kind {
			case v1fkString:
				b = v1AppendQuoteString(b, *(*string)(fp), escapeHTML)
			case v1fkInt:
				b = istrconv.AppendInt10(b, int64(*(*int)(fp)))
			case v1fkInt32:
				b = istrconv.AppendInt10(b, int64(*(*int32)(fp)))
			case v1fkInt64:
				b = istrconv.AppendInt10(b, *(*int64)(fp))
			case v1fkUint:
				b = istrconv.AppendUint10(b, uint64(*(*uint)(fp)))
			case v1fkUint32:
				b = istrconv.AppendUint10(b, uint64(*(*uint32)(fp)))
			case v1fkUint64:
				b = istrconv.AppendUint10(b, *(*uint64)(fp))
			case v1fkFloat64:
				fv := *(*float64)(fp)
				if math.IsNaN(fv) || math.IsInf(fv, 0) {
					return nil, v1UnsupportedFloatError(f.plan.rt, fp, fv, 64)
				}
				b = istrconv.AppendFloatECMA(b, fv, 64)
			case v1fkFloat32:
				fv := float64(*(*float32)(fp))
				if math.IsNaN(fv) || math.IsInf(fv, 0) {
					return nil, v1UnsupportedFloatError(f.plan.rt, fp, fv, 32)
				}
				b = istrconv.AppendFloatECMA(b, fv, 32)
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

	b = append(b, '{')
	first := true
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
		if f.quoted && f.kind != v1fkString {
			b = append(b, '"')
		}
		switch f.kind {
		case v1fkString:
			if f.quoted {
				var innerBuf [128]byte
				inner := v1AppendQuoteString(innerBuf[:0], *(*string)(fp), escapeHTML)
				b = v1AppendQuoteString(b, unsafe.String(unsafe.SliceData(inner), len(inner)), false)
			} else {
				b = v1AppendQuoteString(b, *(*string)(fp), escapeHTML)
			}
		case v1fkInt:
			b = istrconv.AppendInt10(b, int64(*(*int)(fp)))
		case v1fkInt32:
			b = istrconv.AppendInt10(b, int64(*(*int32)(fp)))
		case v1fkInt64:
			b = istrconv.AppendInt10(b, *(*int64)(fp))
		case v1fkUint:
			b = istrconv.AppendUint10(b, uint64(*(*uint)(fp)))
		case v1fkUint32:
			b = istrconv.AppendUint10(b, uint64(*(*uint32)(fp)))
		case v1fkUint64:
			b = istrconv.AppendUint10(b, *(*uint64)(fp))
		case v1fkFloat64:
			fv := *(*float64)(fp)
			if math.IsNaN(fv) || math.IsInf(fv, 0) {
				return nil, v1UnsupportedFloatError(f.plan.rt, fp, fv, 64)
			}
			b = istrconv.AppendFloatECMA(b, fv, 64)
		case v1fkFloat32:
			fv := float64(*(*float32)(fp))
			if math.IsNaN(fv) || math.IsInf(fv, 0) {
				return nil, v1UnsupportedFloatError(f.plan.rt, fp, fv, 32)
			}
			b = istrconv.AppendFloatECMA(b, fv, 32)
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
		if f.quoted && f.kind != v1fkString {
			b = append(b, '"')
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
	case v1fkSliceBool, v1fkSliceString, v1fkSliceInt, v1fkSliceInt64, v1fkSliceUint64, v1fkSliceFloat64, v1fkSliceFloat32, v1fkSlicePoint2F64, v1fkSlice:
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
	case v1fkMapStringElem, v1fkMapIntElem, v1fkMapUintElem:
		m := *(**maps.Map)(ptr)
		return m == nil || m.Used() == 0
	case v1fkAny:
		return *(*any)(ptr) == nil
	case v1fkMarshaler:
		v := reflect.NewAt(plan.rt, ptr).Elem()
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
		return *(*float32)(ptr) == 0
	case v1fkFloat64:
		return *(*float64)(ptr) == 0
	case v1fkTime:
		return (*time.Time)(ptr).IsZero()
	case v1fkBytes:
		return *(*[]byte)(ptr) == nil
	case v1fkSliceBool, v1fkSliceString, v1fkSliceInt, v1fkSliceInt64, v1fkSliceUint64, v1fkSliceFloat64, v1fkSliceFloat32, v1fkSlicePoint2F64, v1fkSlice:
		return (*unsafeheader.Slice)(ptr).Data == nil
	case v1fkPtr:
		return *(*unsafe.Pointer)(ptr) == nil
	case v1fkMapStringString:
		return *(*map[string]string)(ptr) == nil
	case v1fkMapStringInt:
		return *(*map[string]int)(ptr) == nil
	case v1fkMapStringAny:
		return *(*map[string]any)(ptr) == nil
	case v1fkMapStringElem, v1fkMapIntElem, v1fkMapUintElem:
		return *(**maps.Map)(ptr) == nil
	case v1fkAny:
		return *(*any)(ptr) == nil
	case v1fkArray, v1fkStruct, v1fkMarshaler:
		return reflect.NewAt(plan.rt, ptr).Elem().IsZero()
	}
	return false
}

type v1MapStringEntry struct {
	k string
	v unsafe.Pointer
}

var v1MapStringEntryPool = sync.Pool[*[]v1MapStringEntry]{
	New: func() *[]v1MapStringEntry { s := make([]v1MapStringEntry, 0, 16); return &s },
}

func getV1MapStringEntries(n int) *[]v1MapStringEntry {
	p := v1MapStringEntryPool.Get()
	if cap(*p) < n {
		*p = make([]v1MapStringEntry, n)
	} else {
		*p = (*p)[:n]
	}
	return p
}

func putV1MapStringEntries(p *[]v1MapStringEntry) {
	if cap(*p) > 4096 {
		return
	}
	clear(*p)
	*p = (*p)[:0]
	v1MapStringEntryPool.Put(p)
}

type v1MapNumEntry struct {
	v   unsafe.Pointer
	buf [24]byte
	n   uint8
}

var v1MapNumEntryPool = sync.Pool[*[]v1MapNumEntry]{
	New: func() *[]v1MapNumEntry { s := make([]v1MapNumEntry, 0, 16); return &s },
}

func getV1MapNumEntries(n int) *[]v1MapNumEntry {
	p := v1MapNumEntryPool.Get()
	if cap(*p) < n {
		*p = make([]v1MapNumEntry, n)
	} else {
		*p = (*p)[:n]
	}
	return p
}

func putV1MapNumEntries(p *[]v1MapNumEntry) {
	if cap(*p) > 4096 {
		return
	}
	clear(*p)
	*p = (*p)[:0]
	v1MapNumEntryPool.Put(p)
}

func v1ReadSignedKey(kp unsafe.Pointer, keyBits uint8) int64 {
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

func v1ReadUnsignedKey(kp unsafe.Pointer, keyBits uint8) uint64 {
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

func (p *v1FastMarshalPlan) marshalMapStringElem(b []byte, ptr unsafe.Pointer, escapeHTML bool, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return nil, errV1FastFallback
	}
	m := *(**maps.Map)(ptr)
	if m == nil {
		return append(b, "null"...), nil
	}
	n := int(m.Used())
	if n == 0 {
		return append(b, "{}"...), nil
	}
	b = append(b, '{')
	elemPlan := p.elemPlan
	var err error
	if n == 1 {
		var it maps.Iter
		it.Init(p.mapType, m)
		for it.Next(); it.Key() != nil; it.Next() {
			b = v1AppendQuoteString(b, *(*string)(it.Key()), escapeHTML)
			b = append(b, ':')
			if b, err = elemPlan.marshal(b, it.Elem(), escapeHTML, depth); err != nil {
				return nil, err
			}
		}
		return append(b, '}'), nil
	}
	ep := getV1MapStringEntries(n)
	entries := *ep
	var it maps.Iter
	it.Init(p.mapType, m)
	i := 0
	for it.Next(); it.Key() != nil && i < n; it.Next() {
		entries[i] = v1MapStringEntry{k: *(*string)(it.Key()), v: it.Elem()}
		i++
	}
	entries = entries[:i]
	slices.SortFunc(entries, func(x, y v1MapStringEntry) int {
		return strings.Compare(x.k, y.k)
	})
	for idx, e := range entries {
		if idx > 0 {
			b = append(b, ',')
		}
		b = v1AppendQuoteString(b, e.k, escapeHTML)
		b = append(b, ':')
		if b, err = elemPlan.marshal(b, e.v, escapeHTML, depth); err != nil {
			putV1MapStringEntries(ep)
			return nil, err
		}
	}
	putV1MapStringEntries(ep)
	return append(b, '}'), nil
}

func (p *v1FastMarshalPlan) marshalMapIntElem(b []byte, ptr unsafe.Pointer, escapeHTML bool, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return nil, errV1FastFallback
	}
	m := *(**maps.Map)(ptr)
	if m == nil {
		return append(b, "null"...), nil
	}
	n := int(m.Used())
	if n == 0 {
		return append(b, "{}"...), nil
	}
	b = append(b, '{')
	elemPlan := p.elemPlan
	keyBits := p.keyBits
	var err error
	if n == 1 {
		var it maps.Iter
		it.Init(p.mapType, m)
		for it.Next(); it.Key() != nil; it.Next() {
			b = append(b, '"')
			b = istrconv.AppendInt10(b, v1ReadSignedKey(it.Key(), keyBits))
			b = append(b, '"', ':')
			if b, err = elemPlan.marshal(b, it.Elem(), escapeHTML, depth); err != nil {
				return nil, err
			}
		}
		return append(b, '}'), nil
	}
	ep := getV1MapNumEntries(n)
	entries := *ep
	var it maps.Iter
	it.Init(p.mapType, m)
	i := 0
	for it.Next(); it.Key() != nil && i < n; it.Next() {
		e := &entries[i]
		e.v = it.Elem()
		buf := append(e.buf[:0], '"')
		buf = istrconv.AppendInt10(buf, v1ReadSignedKey(it.Key(), keyBits))
		buf = append(buf, '"', ':')
		e.n = uint8(len(buf))
		i++
	}
	entries = entries[:i]
	slices.SortFunc(entries, func(x, y v1MapNumEntry) int {
		return bytes.Compare(x.buf[:x.n], y.buf[:y.n])
	})
	for idx := range entries {
		e := &entries[idx]
		if idx > 0 {
			b = append(b, ',')
		}
		b = append(b, e.buf[:e.n]...)
		if b, err = elemPlan.marshal(b, e.v, escapeHTML, depth); err != nil {
			putV1MapNumEntries(ep)
			return nil, err
		}
	}
	putV1MapNumEntries(ep)
	return append(b, '}'), nil
}

func (p *v1FastMarshalPlan) marshalMapUintElem(b []byte, ptr unsafe.Pointer, escapeHTML bool, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return nil, errV1FastFallback
	}
	m := *(**maps.Map)(ptr)
	if m == nil {
		return append(b, "null"...), nil
	}
	n := int(m.Used())
	if n == 0 {
		return append(b, "{}"...), nil
	}
	b = append(b, '{')
	elemPlan := p.elemPlan
	keyBits := p.keyBits
	var err error
	if n == 1 {
		var it maps.Iter
		it.Init(p.mapType, m)
		for it.Next(); it.Key() != nil; it.Next() {
			b = append(b, '"')
			b = istrconv.AppendUint10(b, v1ReadUnsignedKey(it.Key(), keyBits))
			b = append(b, '"', ':')
			if b, err = elemPlan.marshal(b, it.Elem(), escapeHTML, depth); err != nil {
				return nil, err
			}
		}
		return append(b, '}'), nil
	}
	ep := getV1MapNumEntries(n)
	entries := *ep
	var it maps.Iter
	it.Init(p.mapType, m)
	i := 0
	for it.Next(); it.Key() != nil && i < n; it.Next() {
		e := &entries[i]
		e.v = it.Elem()
		buf := append(e.buf[:0], '"')
		buf = istrconv.AppendUint10(buf, v1ReadUnsignedKey(it.Key(), keyBits))
		buf = append(buf, '"', ':')
		e.n = uint8(len(buf))
		i++
	}
	entries = entries[:i]
	slices.SortFunc(entries, func(x, y v1MapNumEntry) int {
		return bytes.Compare(x.buf[:x.n], y.buf[:y.n])
	})
	for idx := range entries {
		e := &entries[idx]
		if idx > 0 {
			b = append(b, ',')
		}
		b = append(b, e.buf[:e.n]...)
		if b, err = elemPlan.marshal(b, e.v, escapeHTML, depth); err != nil {
			putV1MapNumEntries(ep)
			return nil, err
		}
	}
	putV1MapNumEntries(ep)
	return append(b, '}'), nil
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
			b = istrconv.AppendInt10(b, int64(v))
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
		b = istrconv.AppendInt10(b, int64(m[k]))
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
		return istrconv.AppendInt10(b, int64(v)), nil
	case int64:
		return istrconv.AppendInt10(b, v), nil
	case int32:
		return istrconv.AppendInt10(b, int64(v)), nil
	case uint:
		return istrconv.AppendUint10(b, uint64(v)), nil
	case uint64:
		return istrconv.AppendUint10(b, v), nil
	case uint32:
		return istrconv.AppendUint10(b, uint64(v)), nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, &UnsupportedValueError{Value: reflect.ValueOf(v), Str: strconv.FormatFloat(v, 'g', -1, 64)}
		}
		return v1AppendFloat(b, v, 64), nil
	case float32:
		fv := float64(v)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, &UnsupportedValueError{Value: reflect.ValueOf(v), Str: strconv.FormatFloat(fv, 'g', -1, 32)}
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
			b = istrconv.AppendInt10(b, int64(x))
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
		numStr := string(v)
		if numStr == "" {
			numStr = "0"
		}
		if !isValidNumber(numStr) {
			return nil, fmt.Errorf("json: invalid number literal %q", numStr)
		}
		return append(b, numStr...), nil
	}
	return v1MarshalFastAnyEface(b, val, escapeHTML, depth)
}

//go:noinline
func v1MarshalFastAnyEface(b []byte, val any, escapeHTML bool, depth int) ([]byte, error) {
	at, ptr := v1EfacePtr(&val)
	if at == nil {
		return append(b, "null"...), nil
	}
	plan := lookupV1FastMarshalPlan(at)
	if plan.canFast {
		out, err := plan.marshal(b, ptr, escapeHTML, depth+1)
		runtime.KeepAlive(val)
		return out, err
	}
	return v1MarshalReflect(b, val, escapeHTML, depth+1)
}

func v1MarshalReflect(b []byte, val any, escapeHTML bool, depth int) ([]byte, error) {
	e := newEncodeState()
	defer encodeStatePool.Put(e)
	e.ptrLevel = uint(depth)
	if err := e.marshal(val, encOpts{escapeHTML: escapeHTML}); err != nil {
		return nil, err
	}
	return append(b, e.Bytes()...), nil
}

func v1MarshalFastAlloc(plan *v1FastMarshalPlan, ptr unsafe.Pointer, escapeHTML bool) ([]byte, error) {
	for depth := 0; plan.kind == v1fkPtr && depth < startDetectingCyclesAfter; depth++ {
		p := *(*unsafe.Pointer)(ptr)
		if p == nil {
			return []byte("null"), nil
		}
		ptr = p
		plan = plan.elemPlan
	}
	switch plan.kind {
	case v1fkBool:
		if *(*bool)(ptr) {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case v1fkString:
		s := *(*string)(ptr)
		if v1StringSafeHTML(s) {
			out := make([]byte, len(s)+2)
			out[0] = '"'
			copy(out[1:], s)
			out[len(s)+1] = '"'
			return out, nil
		}
	case v1fkInt:
		var scratch [32]byte
		return bytes.Clone(istrconv.AppendInt10(scratch[:0], int64(*(*int)(ptr)))), nil
	case v1fkInt8:
		var scratch [8]byte
		return bytes.Clone(istrconv.AppendInt10(scratch[:0], int64(*(*int8)(ptr)))), nil
	case v1fkInt16:
		var scratch [8]byte
		return bytes.Clone(istrconv.AppendInt10(scratch[:0], int64(*(*int16)(ptr)))), nil
	case v1fkInt32:
		var scratch [16]byte
		return bytes.Clone(istrconv.AppendInt10(scratch[:0], int64(*(*int32)(ptr)))), nil
	case v1fkInt64:
		var scratch [32]byte
		return bytes.Clone(istrconv.AppendInt10(scratch[:0], *(*int64)(ptr))), nil
	case v1fkUint:
		var scratch [32]byte
		return bytes.Clone(istrconv.AppendUint10(scratch[:0], uint64(*(*uint)(ptr)))), nil
	case v1fkUint8:
		var scratch [8]byte
		return bytes.Clone(istrconv.AppendUint10(scratch[:0], uint64(*(*uint8)(ptr)))), nil
	case v1fkUint16:
		var scratch [8]byte
		return bytes.Clone(istrconv.AppendUint10(scratch[:0], uint64(*(*uint16)(ptr)))), nil
	case v1fkUint32:
		var scratch [16]byte
		return bytes.Clone(istrconv.AppendUint10(scratch[:0], uint64(*(*uint32)(ptr)))), nil
	case v1fkUint64:
		var scratch [32]byte
		return bytes.Clone(istrconv.AppendUint10(scratch[:0], *(*uint64)(ptr))), nil
	case v1fkUintptr:
		var scratch [32]byte
		return bytes.Clone(istrconv.AppendUint10(scratch[:0], uint64(*(*uintptr)(ptr)))), nil
	case v1fkFloat32:
		fv := float64(*(*float32)(ptr))
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, v1UnsupportedFloatError(plan.rt, ptr, fv, 32)
		}
		var scratch [32]byte
		return bytes.Clone(v1AppendFloat(scratch[:0], fv, 32)), nil
	case v1fkFloat64:
		fv := *(*float64)(ptr)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, v1UnsupportedFloatError(plan.rt, ptr, fv, 64)
		}
		var scratch [32]byte
		return bytes.Clone(v1AppendFloat(scratch[:0], fv, 64)), nil
	}
	if plan.isFlatSmall {
		est := min(max(plan.minSize+80, 128), 384)
		b, err := plan.marshal(make([]byte, 0, est), ptr, escapeHTML, 0)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	if hint := int(plan.sizeHint.Load()); hint > 0 {
		b, err := plan.marshal(make([]byte, 0, hint), ptr, escapeHTML, 0)
		if err != nil {
			return nil, err
		}
		if len(b) > hint {
			plan.sizeHint.Store(uint32(min(len(b)+len(b)>>5+64, 64<<20)))
		} else if len(b)*2 < hint {
			plan.sizeHint.Store(uint32(max(len(b)+len(b)>>4+64, 256)))
		}
		return b, nil
	}
	fb := getV1FastBuf()
	b, err := plan.marshal(fb.b, ptr, escapeHTML, 0)
	if err != nil {
		if cap(b) > cap(fb.b) {
			fb.b = b[:0]
		} else {
			fb.b = fb.b[:0]
		}
		v1FastBufPool.Put(fb)
		return nil, err
	}
	if len(b) >= 64 {
		plan.sizeHint.CompareAndSwap(0, uint32(min(len(b)+len(b)>>5+64, 64<<20)))
	}
	fb.b = b
	out := bytes.Clone(b)
	putV1FastBuf(fb)
	return out, nil
}
