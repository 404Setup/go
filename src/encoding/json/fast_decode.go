// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.jsonv2

package json

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"internal/abi"
	"internal/byteorder"
	"internal/runtime/maps"
	"internal/unsafeheader"
	"math"
	"math/bits"
	"reflect"
	"runtime"
	"strconv"
	"sync/v2"
	"time"
	"unicode/utf8"
	"unsafe"
)

// v1CheckValidFast performs a zero-allocation SWAR syntax validation matching
// legacy scanner.go rules (including maxNestingDepth = 10000 and allowing raw
// non-ASCII bytes inside JSON strings).
func v1CheckValidFast(data []byte) bool {
	n := len(data)
	pos := 0
	for pos < n && isSpace(data[pos]) {
		pos++
	}
	if pos >= n {
		return false
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
			for pos < n && isSpace(data[pos]) {
				pos++
			}
			if pos >= n {
				return false
			}
			switch data[pos] {
			case '{':
				if len(stack) >= maxNestingDepth {
					return false
				}
				pos++
				for pos < n && isSpace(data[pos]) {
					pos++
				}
				if pos >= n {
					return false
				}
				if data[pos] == '}' {
					pos++
					state = stAfterValue
					continue
				}
				// Expect object key string.
				if data[pos] != '"' {
					return false
				}
				pos = v1ScanStringEnd(data, pos)
				if pos < 0 {
					return false
				}
				for pos < n && isSpace(data[pos]) {
					pos++
				}
				if pos >= n || data[pos] != ':' {
					return false
				}
				pos++
				stack = append(stack, '}')
				continue

			case '[':
				if len(stack) >= maxNestingDepth {
					return false
				}
				pos++
				for pos < n && isSpace(data[pos]) {
					pos++
				}
				if pos >= n {
					return false
				}
				if data[pos] == ']' {
					pos++
					state = stAfterValue
					continue
				}
				stack = append(stack, ']')
				continue

			case '"':
				pos = v1ScanStringEnd(data, pos)
				if pos < 0 {
					return false
				}
				state = stAfterValue

			case 't':
				if pos+4 > n || data[pos+1] != 'r' || data[pos+2] != 'u' || data[pos+3] != 'e' {
					return false
				}
				pos += 4
				state = stAfterValue

			case 'f':
				if pos+5 > n || data[pos+1] != 'a' || data[pos+2] != 'l' || data[pos+3] != 's' || data[pos+4] != 'e' {
					return false
				}
				pos += 5
				state = stAfterValue

			case 'n':
				if pos+4 > n || data[pos+1] != 'u' || data[pos+2] != 'l' || data[pos+3] != 'l' {
					return false
				}
				pos += 4
				state = stAfterValue

			case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
				pos = v1ScanNumberEnd(data, pos)
				if pos < 0 {
					return false
				}
				state = stAfterValue

			default:
				return false
			}
		}

		// state == stAfterValue
		for pos < n && isSpace(data[pos]) {
			pos++
		}
		if len(stack) == 0 {
			return pos == n
		}
		if pos >= n {
			return false
		}
		top := stack[len(stack)-1]
		c := data[pos]
		if c == top {
			pos++
			stack = stack[:len(stack)-1]
			continue
		}
		if c != ',' {
			return false
		}
		pos++
		if top == '}' {
			for pos < n && isSpace(data[pos]) {
				pos++
			}
			if pos >= n || data[pos] != '"' {
				return false
			}
			pos = v1ScanStringEnd(data, pos)
			if pos < 0 {
				return false
			}
			for pos < n && isSpace(data[pos]) {
				pos++
			}
			if pos >= n || data[pos] != ':' {
				return false
			}
			pos++
		}
		state = stExpectValue
	}
}

func v1IsHex(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

// v1ScanStringEnd scans a JSON string starting at data[pos] == '"' and returns
// the index immediately after the closing '"', or -1 on syntax error.
func v1ScanStringEnd(data []byte, pos int) int {
	n := len(data)
	i := pos + 1
	for {
		for n-i >= 8 {
			if m := v1EscapeMaskNoHTML(byteorder.LEUint64(data[i:])); m != 0 {
				i += bits.TrailingZeros64(m) >> 3
				goto special
			}
			i += 8
		}
		for i < n {
			c := data[i]
			if c == '"' {
				return i + 1
			}
			if c == '\\' || c < 0x20 {
				goto special
			}
			i++
		}
		return -1

	special:
		c := data[i]
		if c == '"' {
			return i + 1
		}
		if c >= utf8.RuneSelf {
			// Legacy scanner.go permits raw non-ASCII bytes in strings.
			i++
			continue
		}
		if c < 0x20 {
			return -1
		}
		// c == '\\'
		i++
		if i >= n {
			return -1
		}
		switch data[i] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			i++
		case 'u':
			if i+4 >= n || !v1IsHex(data[i+1]) || !v1IsHex(data[i+2]) || !v1IsHex(data[i+3]) || !v1IsHex(data[i+4]) {
				return -1
			}
			i += 5
		default:
			return -1
		}
	}
}

// v1ScanNumberEnd scans a JSON number starting at data[pos] and returns the
// index immediately after the number, or -1 on syntax error.
func v1ScanNumberEnd(data []byte, pos int) int {
	n := len(data)
	i := pos
	if i < n && data[i] == '-' {
		i++
		if i >= n {
			return -1
		}
	}
	if data[i] == '0' {
		i++
	} else if '1' <= data[i] && data[i] <= '9' {
		i++
		for i < n && '0' <= data[i] && data[i] <= '9' {
			i++
		}
	} else {
		return -1
	}
	if i < n && data[i] == '.' {
		i++
		if i >= n || data[i] < '0' || data[i] > '9' {
			return -1
		}
		i++
		for i < n && '0' <= data[i] && data[i] <= '9' {
			i++
		}
	}
	if i < n && (data[i] == 'e' || data[i] == 'E') {
		i++
		if i < n && (data[i] == '+' || data[i] == '-') {
			i++
		}
		if i >= n || data[i] < '0' || data[i] > '9' {
			return -1
		}
		i++
		for i < n && '0' <= data[i] && data[i] <= '9' {
			i++
		}
	}
	return i
}

// v1StringCache is a 256-slot string interning cache matching v2's intern.go.
type v1StringCache = [256]string

var v1StringCachePool = sync.Pool[*v1StringCache]{
	New: func() *v1StringCache { return new(v1StringCache) },
}

func v1Hash64(lo, hi uint32) uint32 {
	const (
		prime3 = 0xc2b2ae3d
		prime4 = 0x27d4eb2f
		prime5 = 0x165667b1
	)
	h := prime5 + uint32(8)
	h += lo * prime3
	h = bits.RotateLeft32(h, 17) * prime4
	h += hi * prime3
	h = bits.RotateLeft32(h, 17) * prime4
	return h
}

func v1MakeString(c *v1StringCache, b []byte) string {
	const (
		minCachedLen = 2
		maxCachedLen = 256
	)
	if c == nil || len(b) < minCachedLen || len(b) > maxCachedLen {
		return string(b)
	}
	var h uint32
	switch {
	case len(b) >= 8:
		lo := binary.LittleEndian.Uint64(b[:8])
		hi := binary.LittleEndian.Uint64(b[len(b)-8:])
		h = v1Hash64(uint32(lo), uint32(lo>>32)) ^ v1Hash64(uint32(hi), uint32(hi>>32))
	case len(b) >= 4:
		lo := binary.LittleEndian.Uint32(b[:4])
		hi := binary.LittleEndian.Uint32(b[len(b)-4:])
		h = v1Hash64(lo, hi)
	default:
		lo := binary.LittleEndian.Uint16(b[:2])
		hi := binary.LittleEndian.Uint16(b[len(b)-2:])
		h = v1Hash64(uint32(lo), uint32(hi))
	}
	i := h & 255
	if s := (*c)[i]; s == string(b) {
		return s
	}
	s := string(b)
	(*c)[i] = s
	return s
}

var v1UnmarshalerType = reflect.TypeFor[Unmarshaler]()

func v1HasCustomUnmarshaler(rt reflect.Type) bool {
	if rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Interface {
		return false
	}
	pt := reflect.PointerTo(rt)
	return pt.Implements(v1UnmarshalerType) || pt.Implements(textUnmarshalerType)
}

type v1FastUnmarshalField struct {
	orderIdx        int
	offset          uintptr
	name            string
	quotedNameColon string // pre-quoted `"name":` when name is unescaped ASCII
	quoted          bool
	kind            v1FastKind
	plan            *v1FastUnmarshalPlan
}

type v1FastUnmarshalPlan struct {
	typ            *abi.Type
	rt             reflect.Type
	mapType        *abi.MapType
	ifaceTab       unsafe.Pointer
	kind           v1FastKind
	keyBits        uint8
	canFast        bool
	hasCustom      bool
	hasCycle       bool
	building       bool
	hasQuoted      bool
	elemSize       uintptr
	arrayLen       int
	emptySliceData unsafe.Pointer
	elemPlan       *v1FastUnmarshalPlan
	elemRT         reflect.Type
	fields         []v1FastUnmarshalField
	byExactName    map[string]*v1FastUnmarshalField
	byFoldedName   map[string]*v1FastUnmarshalField
}

var v1FastUnmarshalCache sync.Map[*abi.Type, *v1FastUnmarshalPlan]

func lookupV1FastUnmarshalPlan(at *abi.Type) *v1FastUnmarshalPlan {
	if p, ok := v1FastUnmarshalCache.Load(at); ok {
		return p
	}
	visiting := make(map[*abi.Type]*v1FastUnmarshalPlan)
	p := buildV1FastUnmarshalPlan(v1AbiTypeToReflect(at), at, visiting)
	finalizeV1FastUnmarshalPlans(visiting)
	for kt, kp := range visiting {
		v1FastUnmarshalCache.LoadOrStore(kt, kp)
	}
	return p
}

func finalizeV1FastUnmarshalPlans(visiting map[*abi.Type]*v1FastUnmarshalPlan) {
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
			case v1fkPtr, v1fkSlice, v1fkArray:
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
			case v1fkMapStringElem, v1fkMapIntElem, v1fkMapUintElem:
				if p.elemPlan == nil || !p.elemPlan.canFast || p.elemPlan.hasCustom {
					if p.canFast {
						p.canFast = false
						changed = true
					}
				} else {
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
}

func buildV1FastUnmarshalPlan(rt reflect.Type, at *abi.Type, visiting map[*abi.Type]*v1FastUnmarshalPlan) *v1FastUnmarshalPlan {
	if p, ok := v1FastUnmarshalCache.Load(at); ok {
		return p
	}
	if p, ok := visiting[at]; ok {
		if p.building {
			p.hasCycle = true
		}
		return p
	}
	p := &v1FastUnmarshalPlan{typ: at, rt: rt, building: true}
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
	if v1HasCustomUnmarshaler(rt) {
		if reflect.PointerTo(rt).Implements(v1UnmarshalerType) {
			u := reflect.New(rt).Interface().(Unmarshaler)
			p.ifaceTab = (*[2]unsafe.Pointer)(unsafe.Pointer(&u))[0]
			p.kind = v1fkUnmarshaler
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
		ep := buildV1FastUnmarshalPlan(elem, v1AbiTypeOfReflect(elem), visiting)
		p.elemPlan = ep
		p.elemRT = elem
		p.elemSize = elem.Size()
		p.emptySliceData = reflect.MakeSlice(rt, 0, 0).UnsafePointer()
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
		ep := buildV1FastUnmarshalPlan(elem, v1AbiTypeOfReflect(elem), visiting)
		p.elemPlan = ep
		p.elemRT = elem
		p.elemSize = elem.Size()
		p.arrayLen = rt.Len()
		p.kind = v1fkArray
		p.canFast = true

	case reflect.Pointer:
		elem := rt.Elem()
		ep := buildV1FastUnmarshalPlan(elem, v1AbiTypeOfReflect(elem), visiting)
		p.elemPlan = ep
		p.elemRT = elem
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
		} else {
			kt := rt.Key()
			if kt == numberType || v1HasCustomUnmarshaler(kt) {
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
			p.elemPlan = buildV1FastUnmarshalPlan(elem, v1AbiTypeOfReflect(elem), visiting)
			p.elemRT = elem
			p.elemSize = elem.Size()
			p.mapType = (*abi.MapType)(unsafe.Pointer(at))
			p.canFast = true
		}

	case reflect.Struct:
		sf := cachedTypeFields(rt)
		fastFields := make([]v1FastUnmarshalField, 0, len(sf.list))
		hasQuoted := false
		for i := range sf.list {
			f := &sf.list[i]
			off, actualType, ok := v1DirectFieldOffset(rt, f.index)
			if !ok {
				return p
			}
			fp := buildV1FastUnmarshalPlan(actualType, v1AbiTypeOfReflect(actualType), visiting)
			if f.quoted {
				if !v1IsFastNumericKind(fp.kind) {
					return p
				}
				hasQuoted = true
			}
			ff := v1FastUnmarshalField{
				orderIdx: i,
				offset:   off,
				name:     f.name,
				quoted:   f.quoted,
				kind:     fp.kind,
				plan:     fp,
			}
			if v1StringSafeHTML(f.name) {
				ff.quotedNameColon = `"` + f.name + `":`
			}
			fastFields = append(fastFields, ff)
		}
		byExact := make(map[string]*v1FastUnmarshalField, len(fastFields))
		byFolded := make(map[string]*v1FastUnmarshalField, len(fastFields))
		for i := range fastFields {
			ff := &fastFields[i]
			byExact[ff.name] = ff
			folded := string(foldName([]byte(ff.name)))
			if _, exists := byFolded[folded]; !exists {
				byFolded[folded] = ff
			}
		}
		p.kind = v1fkStruct
		p.fields = fastFields
		p.byExactName = byExact
		p.byFoldedName = byFolded
		p.hasQuoted = hasQuoted
		p.canFast = true
	}
	return p
}

type v1FastDecoder struct {
	data                  []byte
	pos                   int
	strCache              *v1StringCache
	useNumber             bool
	disallowUnknownFields bool
	invokedCustom         int
}

func (d *v1FastDecoder) skipWS() {
	if d.pos < len(d.data) && d.data[d.pos] <= ' ' {
		d.v1SkipWSSlow()
	}
}

//go:noinline
func (d *v1FastDecoder) v1SkipWSSlow() {
	data, pos := d.data, d.pos
	if data[pos] == ' ' {
		pos++
		if pos < len(data) && data[pos] > ' ' {
			d.pos = pos
			return
		}
	}
	for pos < len(data) {
		c := data[pos]
		if c == ' ' || c == '\n' || c == '\r' || c == '\t' {
			pos++
			for len(data)-pos >= 8 && byteorder.LEUint64(data[pos:]) == 0x2020202020202020 {
				pos += 8
			}
			if len(data)-pos >= 4 && byteorder.LEUint32(data[pos:]) == 0x20202020 {
				pos += 4
			}
			continue
		}
		break
	}
	d.pos = pos
}

func (d *v1FastDecoder) consumeNull() bool {
	if d.pos+4 <= len(d.data) &&
		d.data[d.pos] == 'n' && d.data[d.pos+1] == 'u' &&
		d.data[d.pos+2] == 'l' && d.data[d.pos+3] == 'l' {
		d.pos += 4
		return true
	}
	return false
}

func (d *v1FastDecoder) readString() ([]byte, error) {
	data := d.data
	pos := d.pos
	if pos >= len(data) || data[pos] != '"' {
		return nil, errV1FastFallback
	}
	start := pos + 1
	i := start
	for len(data)-i >= 8 {
		if m := v1EscapeMaskNoHTML(byteorder.LEUint64(data[i:])); m != 0 {
			i += bits.TrailingZeros64(m) >> 3
			goto special
		}
		i += 8
	}
	for i < len(data) {
		c := data[i]
		if c >= utf8.RuneSelf || c < 0x20 || c == '\\' || c == '"' {
			break
		}
		i++
	}
special:
	if i < len(data) && data[i] == '"' {
		d.pos = i + 1
		return data[start:i], nil
	}
	end := v1ScanStringEnd(data, pos)
	if end < 0 {
		return nil, errV1FastFallback
	}
	out, ok := unquoteBytes(data[pos:end])
	if !ok {
		return nil, errV1FastFallback
	}
	d.pos = end
	return out, nil
}

func (d *v1FastDecoder) readInt64(bits int) (int64, error) {
	data := d.data
	pos := d.pos
	if pos >= len(data) {
		return 0, errV1FastFallback
	}
	neg := false
	if data[pos] == '-' {
		neg = true
		pos++
		if pos >= len(data) {
			return 0, errV1FastFallback
		}
	}
	c := data[pos]
	var v uint64
	if c == '0' {
		pos++
		if pos < len(data) {
			nc := data[pos]
			if ('0' <= nc && nc <= '9') || nc == '.' || nc == 'e' || nc == 'E' {
				return 0, errV1FastFallback
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
			return 0, errV1FastFallback
		}
		nDigits := pos - startDigits
		if nDigits >= 20 && (data[startDigits] != '1' || v < 1e19 || nDigits > 20) {
			return 0, errV1FastFallback
		}
	} else {
		return 0, errV1FastFallback
	}
	maxInt := uint64(1) << (bits - 1)
	if (neg && v > maxInt) || (!neg && v > maxInt-1) {
		return 0, errV1FastFallback
	}
	d.pos = pos
	if neg {
		return int64(-v), nil
	}
	return int64(v), nil
}

func (d *v1FastDecoder) readUint64(bits int) (uint64, error) {
	data := d.data
	pos := d.pos
	if pos >= len(data) {
		return 0, errV1FastFallback
	}
	c := data[pos]
	var v uint64
	if c == '0' {
		pos++
		if pos < len(data) {
			nc := data[pos]
			if ('0' <= nc && nc <= '9') || nc == '.' || nc == 'e' || nc == 'E' {
				return 0, errV1FastFallback
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
			return 0, errV1FastFallback
		}
		nDigits := pos - startDigits
		if nDigits >= 20 && (data[startDigits] != '1' || v < 1e19 || nDigits > 20) {
			return 0, errV1FastFallback
		}
	} else {
		return 0, errV1FastFallback
	}
	maxUint := uint64(1)<<bits - 1
	if bits == 64 {
		maxUint = math.MaxUint64
	}
	if v > maxUint {
		return 0, errV1FastFallback
	}
	d.pos = pos
	return v, nil
}

var v1ExactPow10F64 = [23]float64{
	1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10,
	1e11, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18, 1e19, 1e20,
	1e21, 1e22,
}

var v1ExactPow10F32 = [11]float32{
	1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10,
}

func (d *v1FastDecoder) readFloat(bits int) (float64, error) {
	data := d.data
	start := d.pos
	i := start
	n := len(data)
	if i >= n {
		return 0, errV1FastFallback
	}
	neg := false
	if data[i] == '-' {
		neg = true
		i++
		if i >= n {
			return 0, errV1FastFallback
		}
	}
	var mant uint64
	exact := true
	const maxMantDiv10 = (1<<53 - 1) / 10
	c := data[i]
	if c == '0' {
		i++
		if i < n && '0' <= data[i] && data[i] <= '9' {
			return 0, errV1FastFallback
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
		return 0, errV1FastFallback
	}

	exp := 0
	if i < n && data[i] == '.' {
		i++
		if i >= n || data[i] < '0' || data[i] > '9' {
			return 0, errV1FastFallback
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
			return 0, errV1FastFallback
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
					f *= v1ExactPow10F64[exp]
				} else if exp < 0 {
					f /= v1ExactPow10F64[-exp]
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
				f *= v1ExactPow10F32[exp]
			} else if exp < 0 {
				f /= v1ExactPow10F32[-exp]
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
		return 0, errV1FastFallback
	}
	d.pos = i
	return fv, nil
}

func (d *v1FastDecoder) skipValue(depth int) error {
	if depth >= maxNestingDepth {
		return errV1FastFallback
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
				return errV1FastFallback
			}
			switch d.data[d.pos] {
			case '"':
				end := v1ScanStringEnd(d.data, d.pos)
				if end < 0 {
					return errV1FastFallback
				}
				d.pos = end
				state = stAfterValue
			case 't':
				if d.pos+4 <= len(d.data) && string(d.data[d.pos:d.pos+4]) == "true" {
					d.pos += 4
					state = stAfterValue
				} else {
					return errV1FastFallback
				}
			case 'f':
				if d.pos+5 <= len(d.data) && string(d.data[d.pos:d.pos+5]) == "false" {
					d.pos += 5
					state = stAfterValue
				} else {
					return errV1FastFallback
				}
			case 'n':
				if d.consumeNull() {
					state = stAfterValue
				} else {
					return errV1FastFallback
				}
			case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
				end := v1ScanNumberEnd(d.data, d.pos)
				if end < 0 {
					return errV1FastFallback
				}
				d.pos = end
				state = stAfterValue
			case '[':
				if depth+len(stack) >= maxNestingDepth {
					return errV1FastFallback
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
				if depth+len(stack) >= maxNestingDepth {
					return errV1FastFallback
				}
				d.pos++
				d.skipWS()
				if d.pos < len(d.data) && d.data[d.pos] == '}' {
					d.pos++
					state = stAfterValue
					continue
				}
				if d.pos >= len(d.data) || d.data[d.pos] != '"' {
					return errV1FastFallback
				}
				end := v1ScanStringEnd(d.data, d.pos)
				if end < 0 {
					return errV1FastFallback
				}
				d.pos = end
				d.skipWS()
				if d.pos >= len(d.data) || d.data[d.pos] != ':' {
					return errV1FastFallback
				}
				d.pos++
				stack = append(stack, '}')
				continue
			default:
				return errV1FastFallback
			}
		}

		if len(stack) == 0 {
			return nil
		}
		d.skipWS()
		if d.pos >= len(d.data) {
			return errV1FastFallback
		}
		top := stack[len(stack)-1]
		c := d.data[d.pos]
		if c == top {
			d.pos++
			stack = stack[:len(stack)-1]
			continue
		}
		if c != ',' {
			return errV1FastFallback
		}
		d.pos++
		if top == '}' {
			d.skipWS()
			if d.pos >= len(d.data) || d.data[d.pos] != '"' {
				return errV1FastFallback
			}
			end := v1ScanStringEnd(d.data, d.pos)
			if end < 0 {
				return errV1FastFallback
			}
			d.pos = end
			d.skipWS()
			if d.pos >= len(d.data) || d.data[d.pos] != ':' {
				return errV1FastFallback
			}
			d.pos++
		}
		state = stExpectValue
	}
}

func (d *v1FastDecoder) unmarshal(p *v1FastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	if depth >= maxNestingDepth {
		return errV1FastFallback
	}
	d.skipWS()
	if d.pos >= len(d.data) {
		return errV1FastFallback
	}
	c := d.data[d.pos]

	switch p.kind {
	case v1fkBool:
		switch c {
		case 't':
			if d.pos+4 <= len(d.data) && string(d.data[d.pos:d.pos+4]) == "true" {
				d.pos += 4
				*(*bool)(ptr) = true
				return nil
			}
		case 'f':
			if d.pos+5 <= len(d.data) && string(d.data[d.pos:d.pos+5]) == "false" {
				d.pos += 5
				*(*bool)(ptr) = false
				return nil
			}
		case 'n':
			if d.consumeNull() {
				return nil // legacy v1: null into bool is a no-op
			}
		}
		return errV1FastFallback

	case v1fkString:
		if c == '"' {
			b, err := d.readString()
			if err != nil {
				return err
			}
			*(*string)(ptr) = v1MakeString(d.strCache, b)
			return nil
		}
		if c == 'n' && d.consumeNull() {
			return nil
		}
		return errV1FastFallback

	case v1fkInt:
		if c == 'n' && d.consumeNull() {
			return nil
		}
		v, err := d.readInt64(strconv.IntSize)
		if err != nil {
			return err
		}
		*(*int)(ptr) = int(v)
		return nil

	case v1fkInt8:
		if c == 'n' && d.consumeNull() {
			return nil
		}
		v, err := d.readInt64(8)
		if err != nil {
			return err
		}
		*(*int8)(ptr) = int8(v)
		return nil

	case v1fkInt16:
		if c == 'n' && d.consumeNull() {
			return nil
		}
		v, err := d.readInt64(16)
		if err != nil {
			return err
		}
		*(*int16)(ptr) = int16(v)
		return nil

	case v1fkInt32:
		if c == 'n' && d.consumeNull() {
			return nil
		}
		v, err := d.readInt64(32)
		if err != nil {
			return err
		}
		*(*int32)(ptr) = int32(v)
		return nil

	case v1fkInt64:
		if c == 'n' && d.consumeNull() {
			return nil
		}
		v, err := d.readInt64(64)
		if err != nil {
			return err
		}
		*(*int64)(ptr) = v
		return nil

	case v1fkUint:
		if c == 'n' && d.consumeNull() {
			return nil
		}
		v, err := d.readUint64(strconv.IntSize)
		if err != nil {
			return err
		}
		*(*uint)(ptr) = uint(v)
		return nil

	case v1fkUint8:
		if c == 'n' && d.consumeNull() {
			return nil
		}
		v, err := d.readUint64(8)
		if err != nil {
			return err
		}
		*(*uint8)(ptr) = uint8(v)
		return nil

	case v1fkUint16:
		if c == 'n' && d.consumeNull() {
			return nil
		}
		v, err := d.readUint64(16)
		if err != nil {
			return err
		}
		*(*uint16)(ptr) = uint16(v)
		return nil

	case v1fkUint32:
		if c == 'n' && d.consumeNull() {
			return nil
		}
		v, err := d.readUint64(32)
		if err != nil {
			return err
		}
		*(*uint32)(ptr) = uint32(v)
		return nil

	case v1fkUint64:
		if c == 'n' && d.consumeNull() {
			return nil
		}
		v, err := d.readUint64(64)
		if err != nil {
			return err
		}
		*(*uint64)(ptr) = v
		return nil

	case v1fkUintptr:
		if c == 'n' && d.consumeNull() {
			return nil
		}
		v, err := d.readUint64(int(unsafe.Sizeof(uintptr(0)) * 8))
		if err != nil {
			return err
		}
		*(*uintptr)(ptr) = uintptr(v)
		return nil

	case v1fkFloat32:
		if c == 'n' && d.consumeNull() {
			return nil
		}
		v, err := d.readFloat(32)
		if err != nil {
			return err
		}
		*(*float32)(ptr) = float32(v)
		return nil

	case v1fkFloat64:
		if c == 'n' && d.consumeNull() {
			return nil
		}
		v, err := d.readFloat(64)
		if err != nil {
			return err
		}
		*(*float64)(ptr) = v
		return nil

	case v1fkPtr:
		if c == 'n' && d.consumeNull() {
			*(*unsafe.Pointer)(ptr) = nil
			return nil
		}
		elemPtr := *(*unsafe.Pointer)(ptr)
		if elemPtr == nil {
			nv := reflect.New(p.elemRT)
			elemPtr = nv.UnsafePointer()
			*(*unsafe.Pointer)(ptr) = elemPtr
		}
		return d.unmarshal(p.elemPlan, elemPtr, depth+1)

	case v1fkBytes:
		return d.unmarshalBytes(ptr)

	case v1fkSliceString:
		return d.unmarshalSliceString(ptr)

	case v1fkSliceInt:
		return d.unmarshalSliceInt(ptr)

	case v1fkSliceInt64:
		return d.unmarshalSliceInt64(ptr)

	case v1fkSliceUint64:
		return d.unmarshalSliceUint64(ptr)

	case v1fkSliceFloat64:
		return d.unmarshalSliceFloat64(ptr)

	case v1fkSliceFloat32:
		return d.unmarshalSliceFloat32(ptr)

	case v1fkSlicePoint2F64:
		return d.unmarshalSlicePoint2F64(ptr)

	case v1fkSliceBool:
		return d.unmarshalSliceBool(ptr)

	case v1fkSlice:
		return d.unmarshalSlice(p, ptr, depth+1)

	case v1fkArray:
		return d.unmarshalArray(p, ptr, depth+1)

	case v1fkTime:
		return d.unmarshalTime(ptr)

	case v1fkMapStringString:
		return d.unmarshalMapStringString(ptr)

	case v1fkMapStringInt:
		return d.unmarshalMapStringInt(ptr)

	case v1fkMapStringAny:
		return d.unmarshalMapStringAny(ptr, depth+1)

	case v1fkMapStringElem:
		return d.unmarshalMapStringElem(p, ptr, depth+1)

	case v1fkMapIntElem:
		return d.unmarshalMapIntElem(p, ptr, depth+1)

	case v1fkMapUintElem:
		return d.unmarshalMapUintElem(p, ptr, depth+1)

	case v1fkAny:
		return d.unmarshalAny(ptr, depth+1)

	case v1fkUnmarshaler:
		return d.unmarshalCustomUnmarshaler(p, ptr, depth+1)

	case v1fkStruct:
		return d.unmarshalStruct(p, ptr, depth+1)
	}
	return errV1FastFallback
}

type v1DirectError struct {
	err error
}

func (e *v1DirectError) Error() string { return e.err.Error() }

type v1CustomTypeError struct {
	err *UnmarshalTypeError
}

func (e *v1CustomTypeError) Error() string { return e.err.Error() }

func (d *v1FastDecoder) unmarshalTime(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			d.invokedCustom++
			return nil
		}
		return errV1FastFallback
	}
	if c != '"' {
		start := d.pos
		if err := d.skipValue(0); err != nil {
			return err
		}
		var tt time.Time
		if err := tt.UnmarshalJSON(d.data[start:d.pos]); err != nil {
			return &v1DirectError{err: err}
		}
		*(*time.Time)(ptr) = tt
		d.invokedCustom++
		return nil
	}
	end := v1ScanStringEnd(d.data, d.pos)
	if end < 0 {
		return errV1FastFallback
	}
	var tt time.Time
	if err := tt.UnmarshalJSON(d.data[d.pos:end]); err != nil {
		return &v1DirectError{err: err}
	}
	*(*time.Time)(ptr) = tt
	d.pos = end
	d.invokedCustom++
	return nil
}

func (d *v1FastDecoder) unmarshalCustomUnmarshaler(p *v1FastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	start := d.pos
	if err := d.skipValue(depth); err != nil {
		return err
	}
	raw := d.data[start:d.pos]
	var u Unmarshaler
	ip := (*[2]unsafe.Pointer)(unsafe.Pointer(&u))
	ip[0] = p.ifaceTab
	ip[1] = ptr
	if err := u.UnmarshalJSON(raw); err != nil {
		if ute, ok := err.(*UnmarshalTypeError); ok {
			return &v1CustomTypeError{err: ute}
		}
		return &v1DirectError{err: err}
	}
	d.invokedCustom++
	return nil
}

func (d *v1FastDecoder) unmarshalQuotedNumber(kind v1FastKind, fp unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			return nil
		}
		return errV1FastFallback
	}
	if c != '"' {
		return errV1FastFallback
	}
	s, err := d.readString()
	if err != nil {
		return err
	}
	if len(s) == 4 && string(s) == "null" {
		return nil
	}
	sub := v1FastDecoder{data: s}
	switch kind {
	case v1fkInt:
		v, err := sub.readInt64(strconv.IntSize)
		if err != nil || sub.pos != len(s) {
			return errV1FastFallback
		}
		*(*int)(fp) = int(v)
	case v1fkInt8:
		v, err := sub.readInt64(8)
		if err != nil || sub.pos != len(s) {
			return errV1FastFallback
		}
		*(*int8)(fp) = int8(v)
	case v1fkInt16:
		v, err := sub.readInt64(16)
		if err != nil || sub.pos != len(s) {
			return errV1FastFallback
		}
		*(*int16)(fp) = int16(v)
	case v1fkInt32:
		v, err := sub.readInt64(32)
		if err != nil || sub.pos != len(s) {
			return errV1FastFallback
		}
		*(*int32)(fp) = int32(v)
	case v1fkInt64:
		v, err := sub.readInt64(64)
		if err != nil || sub.pos != len(s) {
			return errV1FastFallback
		}
		*(*int64)(fp) = v
	case v1fkUint:
		v, err := sub.readUint64(strconv.IntSize)
		if err != nil || sub.pos != len(s) {
			return errV1FastFallback
		}
		*(*uint)(fp) = uint(v)
	case v1fkUint8:
		v, err := sub.readUint64(8)
		if err != nil || sub.pos != len(s) {
			return errV1FastFallback
		}
		*(*uint8)(fp) = uint8(v)
	case v1fkUint16:
		v, err := sub.readUint64(16)
		if err != nil || sub.pos != len(s) {
			return errV1FastFallback
		}
		*(*uint16)(fp) = uint16(v)
	case v1fkUint32:
		v, err := sub.readUint64(32)
		if err != nil || sub.pos != len(s) {
			return errV1FastFallback
		}
		*(*uint32)(fp) = uint32(v)
	case v1fkUint64:
		v, err := sub.readUint64(64)
		if err != nil || sub.pos != len(s) {
			return errV1FastFallback
		}
		*(*uint64)(fp) = v
	case v1fkUintptr:
		v, err := sub.readUint64(int(unsafe.Sizeof(uintptr(0)) * 8))
		if err != nil || sub.pos != len(s) {
			return errV1FastFallback
		}
		*(*uintptr)(fp) = uintptr(v)
	case v1fkFloat32:
		v, err := sub.readFloat(32)
		if err != nil || sub.pos != len(s) {
			return errV1FastFallback
		}
		*(*float32)(fp) = float32(v)
	case v1fkFloat64:
		v, err := sub.readFloat(64)
		if err != nil || sub.pos != len(s) {
			return errV1FastFallback
		}
		*(*float64)(fp) = v
	default:
		return errV1FastFallback
	}
	return nil
}

func v1ParseStrictInt64Key(kb []byte, bits int) (int64, bool) {
	if len(kb) == 0 || (kb[0] == '-' && len(kb) == 2 && kb[1] == '0') {
		return 0, false
	}
	sub := v1FastDecoder{data: kb}
	v, err := sub.readInt64(bits)
	if err != nil || sub.pos != len(kb) {
		return 0, false
	}
	return v, true
}

func v1ParseStrictUint64Key(kb []byte, bits int) (uint64, bool) {
	if len(kb) == 0 {
		return 0, false
	}
	sub := v1FastDecoder{data: kb}
	v, err := sub.readUint64(bits)
	if err != nil || sub.pos != len(kb) {
		return 0, false
	}
	return v, true
}

func v1ClearBytes(ptr unsafe.Pointer, size uintptr, rt reflect.Type) {
	if size == 0 {
		return
	}
	if rt.Kind() == reflect.Pointer {
		*(*unsafe.Pointer)(ptr) = nil
		return
	}
	reflect.NewAt(rt, ptr).Elem().SetZero()
}

func (d *v1FastDecoder) unmarshalMapStringElem(p *v1FastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(**maps.Map)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '{' {
		return errV1FastFallback
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
		return errV1FastFallback
	}
	elemPlan := p.elemPlan
	var key string
	keyPtr := v1Noescape(unsafe.Pointer(&key))
	for {
		d.skipWS()
		kb, err := d.readString()
		if err != nil {
			return err
		}
		key = v1MakeString(d.strCache, kb)
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ':' {
			return errV1FastFallback
		}
		d.pos++
		existing, exists := m.Get(mt, keyPtr)
		var elemPtr unsafe.Pointer
		if exists {
			v1ClearBytes(existing, p.elemSize, p.elemRT)
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
			return errV1FastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalMapIntElem(p *v1FastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(**maps.Map)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '{' {
		return errV1FastFallback
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
		return errV1FastFallback
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
		keyPtr = v1Noescape(unsafe.Pointer(&k8))
	case 16:
		keyPtr = v1Noescape(unsafe.Pointer(&k16))
	case 32:
		keyPtr = v1Noescape(unsafe.Pointer(&k32))
	default:
		keyPtr = v1Noescape(unsafe.Pointer(&k64))
	}
	for {
		d.skipWS()
		kb, err := d.readString()
		if err != nil {
			return err
		}
		kv, ok := v1ParseStrictInt64Key(kb, int(keyBits))
		if !ok {
			return errV1FastFallback
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
			return errV1FastFallback
		}
		d.pos++
		existing, exists := m.Get(mt, keyPtr)
		var elemPtr unsafe.Pointer
		if exists {
			v1ClearBytes(existing, p.elemSize, p.elemRT)
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
			return errV1FastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalMapUintElem(p *v1FastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(**maps.Map)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '{' {
		return errV1FastFallback
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
		return errV1FastFallback
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
		keyPtr = v1Noescape(unsafe.Pointer(&k8))
	case 16:
		keyPtr = v1Noescape(unsafe.Pointer(&k16))
	case 32:
		keyPtr = v1Noescape(unsafe.Pointer(&k32))
	default:
		keyPtr = v1Noescape(unsafe.Pointer(&k64))
	}
	for {
		d.skipWS()
		kb, err := d.readString()
		if err != nil {
			return err
		}
		kv, ok := v1ParseStrictUint64Key(kb, int(keyBits))
		if !ok {
			return errV1FastFallback
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
			return errV1FastFallback
		}
		d.pos++
		existing, exists := m.Get(mt, keyPtr)
		var elemPtr unsafe.Pointer
		if exists {
			v1ClearBytes(existing, p.elemSize, p.elemRT)
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
			return errV1FastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalBytes(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]byte)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '"' {
		return errV1FastFallback
	}
	b, err := d.readString()
	if err != nil {
		return err
	}
	if len(b) == 0 {
		*(*[]byte)(ptr) = []byte{}
		return nil
	}
	if bytes.IndexAny(b, "\r\n") >= 0 {
		return errV1FastFallback
	}
	dst := make([]byte, base64.StdEncoding.DecodedLen(len(b)))
	n, decErr := base64.StdEncoding.Decode(dst, b)
	if decErr != nil {
		return errV1FastFallback
	}
	*(*[]byte)(ptr) = dst[:n]
	return nil
}

func (d *v1FastDecoder) unmarshalSliceString(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]string)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '[' {
		return errV1FastFallback
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
			return errV1FastFallback
		}
		var elem string
		if i < oldCap {
			elem = s[i]
		}
		switch d.data[d.pos] {
		case '"':
			b, err := d.readString()
			if err != nil {
				return err
			}
			elem = v1MakeString(d.strCache, b)
		case 'n':
			if !d.consumeNull() {
				return errV1FastFallback
			}
		default:
			return errV1FastFallback
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
			return errV1FastFallback
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
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalSliceInt(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]int)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '[' {
		return errV1FastFallback
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
			return errV1FastFallback
		}
		var elem int
		if i < oldCap {
			elem = s[i]
		}
		if d.data[d.pos] == 'n' {
			if !d.consumeNull() {
				return errV1FastFallback
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
			return errV1FastFallback
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
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalSliceInt64(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]int64)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '[' {
		return errV1FastFallback
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
			return errV1FastFallback
		}
		var elem int64
		if i < oldCap {
			elem = s[i]
		}
		if d.data[d.pos] == 'n' {
			if !d.consumeNull() {
				return errV1FastFallback
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
			return errV1FastFallback
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
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalSliceUint64(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]uint64)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '[' {
		return errV1FastFallback
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
			return errV1FastFallback
		}
		var elem uint64
		if i < oldCap {
			elem = s[i]
		}
		if d.data[d.pos] == 'n' {
			if !d.consumeNull() {
				return errV1FastFallback
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
			return errV1FastFallback
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
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalSliceFloat64(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]float64)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '[' {
		return errV1FastFallback
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
			return errV1FastFallback
		}
		var elem float64
		if i < oldCap {
			elem = s[i]
		}
		if d.data[d.pos] == 'n' {
			if !d.consumeNull() {
				return errV1FastFallback
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
			return errV1FastFallback
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
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalSliceFloat32(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]float32)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '[' {
		return errV1FastFallback
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
			return errV1FastFallback
		}
		var elem float32
		if i < oldCap {
			elem = s[i]
		}
		if d.data[d.pos] == 'n' {
			if !d.consumeNull() {
				return errV1FastFallback
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
			return errV1FastFallback
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
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalSliceBool(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[]bool)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '[' {
		return errV1FastFallback
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
			return errV1FastFallback
		}
		var elem bool
		if i < oldCap {
			elem = s[i]
		}
		switch d.data[d.pos] {
		case 't':
			if d.pos+4 <= len(d.data) && string(d.data[d.pos:d.pos+4]) == "true" {
				d.pos += 4
				elem = true
			} else {
				return errV1FastFallback
			}
		case 'f':
			if d.pos+5 <= len(d.data) && string(d.data[d.pos:d.pos+5]) == "false" {
				d.pos += 5
				elem = false
			} else {
				return errV1FastFallback
			}
		case 'n':
			if !d.consumeNull() {
				return errV1FastFallback
			}
		default:
			return errV1FastFallback
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
			return errV1FastFallback
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
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalSlicePoint2F64(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[][2]float64)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '[' {
		return errV1FastFallback
	}
	d.pos++
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		*(*[][2]float64)(ptr) = [][2]float64{}
		return nil
	}
	sp := (*[][2]float64)(ptr)
	s := *sp
	s = s[:cap(s)]
	i := 0
	for {
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != '[' {
			return errV1FastFallback
		}
		d.pos++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errV1FastFallback
		}
		v0, err := d.readFloat(64)
		if err != nil {
			return err
		}
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ',' {
			return errV1FastFallback
		}
		d.pos++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errV1FastFallback
		}
		v1, err := d.readFloat(64)
		if err != nil {
			return err
		}
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ']' {
			return errV1FastFallback
		}
		d.pos++
		pt := [2]float64{v0, v1}
		if i < len(s) {
			s[i] = pt
		} else {
			s = append(s, pt)
			s = s[:cap(s)]
		}
		i++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errV1FastFallback
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
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalSlice(p *v1FastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*unsafeheader.Slice)(ptr) = unsafeheader.Slice{}
			return nil
		}
		return errV1FastFallback
	}
	if c != '[' {
		return errV1FastFallback
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
		}
		elemPtr := unsafe.Add(sh.Data, uintptr(i)*elemSize)
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
			return errV1FastFallback
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
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalArray(p *v1FastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			return nil // legacy v1: null into array is a no-op
		}
		return errV1FastFallback
	}
	if c != '[' {
		return errV1FastFallback
	}
	d.pos++
	d.skipWS()
	n := p.arrayLen
	elemPlan := p.elemPlan
	if elemPlan.kind == v1fkFloat64 && n == 2 && d.pos < len(d.data) {
		savedPos := d.pos
		if c0 := d.data[d.pos]; c0 == '-' || ('0' <= c0 && c0 <= '9') {
			if v0, err := d.readFloat(64); err == nil {
				d.skipWS()
				if d.pos < len(d.data) && d.data[d.pos] == ',' {
					d.pos++
					d.skipWS()
					if d.pos < len(d.data) {
						if c1 := d.data[d.pos]; c1 == '-' || ('0' <= c1 && c1 <= '9') {
							if v1, err := d.readFloat(64); err == nil {
								d.skipWS()
								if d.pos < len(d.data) && d.data[d.pos] == ']' {
									arr := (*[2]float64)(ptr)
									arr[0] = v0
									arr[1] = v1
									d.pos++
									return nil
								}
							}
						}
					}
				}
			}
		}
		d.pos = savedPos
	}
	elemSize := p.elemSize
	i := 0
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
	} else {
		for {
			if i < n {
				elemPtr := unsafe.Add(ptr, uintptr(i)*elemSize)
				if err := d.unmarshal(elemPlan, elemPtr, depth); err != nil {
					return err
				}
			} else {
				if err := d.skipValue(depth); err != nil {
					return err
				}
			}
			i++
			d.skipWS()
			if d.pos >= len(d.data) {
				return errV1FastFallback
			}
			if d.data[d.pos] == ',' {
				d.pos++
				continue
			}
			if d.data[d.pos] == ']' {
				d.pos++
				break
			}
			return errV1FastFallback
		}
	}
	// Legacy v1: zero out any remaining array elements.
	if i < n {
		rv := reflect.NewAt(p.rt, ptr).Elem()
		for ; i < n; i++ {
			rv.Index(i).SetZero()
		}
	}
	return nil
}

func (d *v1FastDecoder) unmarshalMapStringString(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*map[string]string)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '{' {
		return errV1FastFallback
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
	for {
		d.skipWS()
		kb, err := d.readString()
		if err != nil {
			return err
		}
		key := v1MakeString(d.strCache, kb)
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ':' {
			return errV1FastFallback
		}
		d.pos++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errV1FastFallback
		}
		switch d.data[d.pos] {
		case '"':
			vb, err := d.readString()
			if err != nil {
				return err
			}
			m[key] = v1MakeString(d.strCache, vb)
		case 'n':
			if !d.consumeNull() {
				return errV1FastFallback
			}
			// Legacy v1 decodes null into map element zero value ("").
			m[key] = ""
		default:
			return errV1FastFallback
		}
		d.skipWS()
		if d.pos >= len(d.data) {
			return errV1FastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalMapStringInt(ptr unsafe.Pointer) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*map[string]int)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '{' {
		return errV1FastFallback
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
	for {
		d.skipWS()
		kb, err := d.readString()
		if err != nil {
			return err
		}
		key := v1MakeString(d.strCache, kb)
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ':' {
			return errV1FastFallback
		}
		d.pos++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errV1FastFallback
		}
		if d.data[d.pos] == 'n' {
			if !d.consumeNull() {
				return errV1FastFallback
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
			return errV1FastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalMapStringAny(ptr unsafe.Pointer, depth int) error {
	if depth >= maxNestingDepth {
		return errV1FastFallback
	}
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*map[string]any)(ptr) = nil
			return nil
		}
		return errV1FastFallback
	}
	if c != '{' {
		return errV1FastFallback
	}
	d.pos++
	d.skipWS()
	mp := (*map[string]any)(ptr)
	m := *mp
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
		key := v1MakeString(d.strCache, kb)
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ':' {
			return errV1FastFallback
		}
		d.pos++
		val, err := d.readAny(depth)
		if err != nil {
			return err
		}
		m[key] = val
		d.skipWS()
		if d.pos >= len(d.data) {
			return errV1FastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		return errV1FastFallback
	}
}

func (d *v1FastDecoder) unmarshalAny(ptr unsafe.Pointer, depth int) error {
	if *(*any)(ptr) != nil {
		return errV1FastFallback
	}
	v, err := d.readAny(depth)
	if err != nil {
		return err
	}
	*(*any)(ptr) = v
	return nil
}

func (d *v1FastDecoder) readAny(depth int) (any, error) {
	if depth >= maxNestingDepth {
		return nil, errV1FastFallback
	}
	d.skipWS()
	if d.pos >= len(d.data) {
		return nil, errV1FastFallback
	}
	switch d.data[d.pos] {
	case 'n':
		if d.consumeNull() {
			return nil, nil
		}
		return nil, errV1FastFallback
	case 't':
		if d.pos+4 <= len(d.data) && string(d.data[d.pos:d.pos+4]) == "true" {
			d.pos += 4
			return true, nil
		}
		return nil, errV1FastFallback
	case 'f':
		if d.pos+5 <= len(d.data) && string(d.data[d.pos:d.pos+5]) == "false" {
			d.pos += 5
			return false, nil
		}
		return nil, errV1FastFallback
	case '"':
		b, err := d.readString()
		if err != nil {
			return nil, err
		}
		return v1MakeString(d.strCache, b), nil
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		if d.useNumber {
			end := v1ScanNumberEnd(d.data, d.pos)
			if end < 0 {
				return nil, errV1FastFallback
			}
			num := Number(d.data[d.pos:end])
			d.pos = end
			return num, nil
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
				return nil, errV1FastFallback
			}
			if d.data[d.pos] == ',' {
				d.pos++
				continue
			}
			if d.data[d.pos] == ']' {
				d.pos++
				return s, nil
			}
			return nil, errV1FastFallback
		}
	case '{':
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
			key := v1MakeString(d.strCache, kb)
			d.skipWS()
			if d.pos >= len(d.data) || d.data[d.pos] != ':' {
				return nil, errV1FastFallback
			}
			d.pos++
			val, err := d.readAny(depth + 1)
			if err != nil {
				return nil, err
			}
			m[key] = val
			d.skipWS()
			if d.pos >= len(d.data) {
				return nil, errV1FastFallback
			}
			if d.data[d.pos] == ',' {
				d.pos++
				continue
			}
			if d.data[d.pos] == '}' {
				d.pos++
				return m, nil
			}
			return nil, errV1FastFallback
		}
	}
	return nil, errV1FastFallback
}

func (d *v1FastDecoder) unmarshalStruct(p *v1FastUnmarshalPlan, ptr unsafe.Pointer, depth int) error {
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			return nil // legacy v1: null into struct is a no-op
		}
		return errV1FastFallback
	}
	if c != '{' {
		return errV1FastFallback
	}
	d.pos++
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == '}' {
		d.pos++
		return nil
	}

	fields := p.fields
	nextIdx := 0
	for {
		data := d.data
		pos := d.pos
		if pos < len(data) && data[pos] <= ' ' {
			for pos < len(data) && data[pos] <= ' ' && isSpace(data[pos]) {
				pos++
			}
			d.pos = pos
		}

		var f *v1FastUnmarshalField
		if nextIdx < len(fields) {
			if qf := fields[nextIdx].quotedNameColon; len(qf) > 0 && pos+len(qf) <= len(data) && string(data[pos:pos+len(qf)]) == qf {
				d.pos = pos + len(qf)
				f = &fields[nextIdx]
				nextIdx++
				goto gotField
			}
		}

		{
			keyBytes, err := d.readString()
			if err != nil {
				return err
			}
			d.skipWS()
			if d.pos >= len(d.data) || d.data[d.pos] != ':' {
				return errV1FastFallback
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
				f = p.byExactName[string(keyBytes)]
				if f != nil {
					nextIdx = f.orderIdx + 1
				}
			}
			if f == nil {
				f = p.byFoldedName[string(foldName(keyBytes))]
				if f != nil {
					nextIdx = f.orderIdx + 1
				}
			}
		}

	gotField:
		if f == nil {
			if d.disallowUnknownFields {
				return errV1FastFallback
			}
			if err := d.skipValue(depth); err != nil {
				return err
			}
		} else {
			fp := unsafe.Add(ptr, f.offset)
			d.skipWS()
			if d.pos >= len(d.data) {
				return errV1FastFallback
			}
			if p.hasQuoted && f.quoted {
				if err := d.unmarshalQuotedNumber(f.kind, fp); err != nil {
					return err
				}
			} else {
				vc := d.data[d.pos]
				switch f.kind {
				case v1fkString:
					if vc == '"' {
						b, err := d.readString()
						if err != nil {
							return err
						}
						*(*string)(fp) = v1MakeString(d.strCache, b)
					} else if vc == 'n' && d.consumeNull() {
						// no-op
					} else {
						return errV1FastFallback
					}
				case v1fkInt:
					if vc == 'n' && d.consumeNull() {
						// no-op
					} else {
						v, err := d.readInt64(strconv.IntSize)
						if err != nil {
							return err
						}
						*(*int)(fp) = int(v)
					}
				case v1fkInt32:
					if vc == 'n' && d.consumeNull() {
						// no-op
					} else {
						v, err := d.readInt64(32)
						if err != nil {
							return err
						}
						*(*int32)(fp) = int32(v)
					}
				case v1fkInt64:
					if vc == 'n' && d.consumeNull() {
						// no-op
					} else {
						v, err := d.readInt64(64)
						if err != nil {
							return err
						}
						*(*int64)(fp) = v
					}
				case v1fkUint:
					if vc == 'n' && d.consumeNull() {
						// no-op
					} else {
						v, err := d.readUint64(strconv.IntSize)
						if err != nil {
							return err
						}
						*(*uint)(fp) = uint(v)
					}
				case v1fkUint32:
					if vc == 'n' && d.consumeNull() {
						// no-op
					} else {
						v, err := d.readUint64(32)
						if err != nil {
							return err
						}
						*(*uint32)(fp) = uint32(v)
					}
				case v1fkUint64:
					if vc == 'n' && d.consumeNull() {
						// no-op
					} else {
						v, err := d.readUint64(64)
						if err != nil {
							return err
						}
						*(*uint64)(fp) = v
					}
				case v1fkFloat64:
					if vc == 'n' && d.consumeNull() {
						// no-op
					} else {
						v, err := d.readFloat(64)
						if err != nil {
							return err
						}
						*(*float64)(fp) = v
					}
				case v1fkFloat32:
					if vc == 'n' && d.consumeNull() {
						// no-op
					} else {
						v, err := d.readFloat(32)
						if err != nil {
							return err
						}
						*(*float32)(fp) = float32(v)
					}
				case v1fkBool:
					if vc == 't' && d.pos+4 <= len(d.data) && string(d.data[d.pos:d.pos+4]) == "true" {
						d.pos += 4
						*(*bool)(fp) = true
					} else if vc == 'f' && d.pos+5 <= len(d.data) && string(d.data[d.pos:d.pos+5]) == "false" {
						d.pos += 5
						*(*bool)(fp) = false
					} else if vc == 'n' && d.consumeNull() {
						// no-op
					} else {
						return errV1FastFallback
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
			return errV1FastFallback
		}
		if d.data[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.data[d.pos] == '}' {
			d.pos++
			return nil
		}
		return errV1FastFallback
	}
}

func v1IsTargetZero(p *v1FastUnmarshalPlan, ptr unsafe.Pointer) bool {
	switch p.kind {
	case v1fkPtr:
		return *(*unsafe.Pointer)(ptr) == nil
	case v1fkTime:
		return (*time.Time)(ptr).IsZero()
	case v1fkBytes, v1fkSliceBool, v1fkSliceString, v1fkSliceInt, v1fkSliceInt64, v1fkSliceUint64, v1fkSliceFloat64, v1fkSliceFloat32, v1fkSlicePoint2F64, v1fkSlice:
		return (*unsafeheader.Slice)(ptr).Data == nil
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
	case v1fkStruct, v1fkArray, v1fkUnmarshaler:
		return reflect.NewAt(p.rt, ptr).Elem().IsZero()
	}
	return false
}

// tryV1FastUnmarshal attempts to unmarshal valid JSON data into (at, ptr).
// Caller MUST ensure data is syntactically valid (or pass validated=false to validate when needed).
func tryV1FastUnmarshal(data []byte, at *abi.Type, ptr unsafe.Pointer, validated, useNumber, disallowUnknown bool) (bool, error) {
	plan := lookupV1FastUnmarshalPlan(at)
	if !plan.canFast {
		return false, nil
	}

	switch plan.kind {
	case v1fkBool, v1fkString, v1fkInt, v1fkInt8, v1fkInt16, v1fkInt32, v1fkInt64,
		v1fkUint, v1fkUint8, v1fkUint16, v1fkUint32, v1fkUint64, v1fkUintptr,
		v1fkFloat32, v1fkFloat64:
		return tryV1FastUnmarshalScalar(data, plan, ptr), nil
	}

	wasZero := v1IsTargetZero(plan, ptr)
	if !validated && (!wasZero || plan.hasCustom) {
		if !v1CheckValidFast(data) {
			return false, nil
		}
	}

	sc := v1StringCachePool.Get()
	d := v1FastDecoder{
		data:                  data,
		strCache:              sc,
		useNumber:             useNumber,
		disallowUnknownFields: disallowUnknown,
	}
	err := d.unmarshal(plan, ptr, 0)
	if err == nil {
		d.skipWS()
		if d.pos == len(data) {
			v1StringCachePool.Put(sc)
			return true, nil
		}
	}
	v1StringCachePool.Put(sc)
	if de, ok := err.(*v1DirectError); ok {
		if !validated && wasZero && !plan.hasCustom && !v1CheckValidFast(data) {
			reflect.NewAt(plan.rt, ptr).Elem().SetZero()
			return false, nil
		}
		return true, de.err
	}
	if cte, ok := err.(*v1CustomTypeError); ok {
		return true, v1ReplayUnmarshal(data, plan.rt, ptr, useNumber, disallowUnknown, d.invokedCustom, cte.err)
	}
	if d.invokedCustom > 0 {
		return true, v1ReplayUnmarshal(data, plan.rt, ptr, useNumber, disallowUnknown, d.invokedCustom, nil)
	}
	if wasZero {
		reflect.NewAt(plan.rt, ptr).Elem().SetZero()
	}
	return false, nil
}

//go:noinline
func v1ReplayUnmarshal(data []byte, rt reflect.Type, ptr unsafe.Pointer, useNumber, disallowUnknown bool, skipUnmarshaler int, replayErr error) error {
	var d decodeState
	d.init(data)
	d.useNumber = useNumber
	d.disallowUnknownFields = disallowUnknown
	d.skipUnmarshaler = skipUnmarshaler
	d.replayErr = replayErr
	return d.unmarshal(reflect.NewAt(rt, ptr).Interface())
}

func tryV1FastUnmarshalScalar(data []byte, plan *v1FastUnmarshalPlan, ptr unsafe.Pointer) bool {
	d := v1FastDecoder{data: data}
	d.skipWS()
	if d.pos >= len(data) {
		return false
	}
	c := data[d.pos]

	switch plan.kind {
	case v1fkBool:
		var v bool
		var isNull bool
		switch c {
		case 't':
			if d.pos+4 <= len(data) && string(data[d.pos:d.pos+4]) == "true" {
				d.pos += 4
				v = true
			} else {
				return false
			}
		case 'f':
			if d.pos+5 <= len(data) && string(data[d.pos:d.pos+5]) == "false" {
				d.pos += 5
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
		if d.pos != len(data) {
			return false
		}
		if !isNull {
			*(*bool)(ptr) = v
		}
		return true

	case v1fkString:
		if c == '"' {
			b, err := d.readString()
			if err != nil {
				return false
			}
			d.skipWS()
			if d.pos != len(data) {
				return false
			}
			sc := v1StringCachePool.Get()
			*(*string)(ptr) = v1MakeString(sc, b)
			v1StringCachePool.Put(sc)
			return true
		}
		if c == 'n' && d.consumeNull() {
			d.skipWS()
			return d.pos == len(data)
		}
		return false

	case v1fkInt, v1fkInt8, v1fkInt16, v1fkInt32, v1fkInt64:
		if c == 'n' && d.consumeNull() {
			d.skipWS()
			return d.pos == len(data)
		}
		var bits int
		switch plan.kind {
		case v1fkInt:
			bits = strconv.IntSize
		case v1fkInt8:
			bits = 8
		case v1fkInt16:
			bits = 16
		case v1fkInt32:
			bits = 32
		case v1fkInt64:
			bits = 64
		}
		v, err := d.readInt64(bits)
		if err != nil {
			return false
		}
		d.skipWS()
		if d.pos != len(data) {
			return false
		}
		switch plan.kind {
		case v1fkInt:
			*(*int)(ptr) = int(v)
		case v1fkInt8:
			*(*int8)(ptr) = int8(v)
		case v1fkInt16:
			*(*int16)(ptr) = int16(v)
		case v1fkInt32:
			*(*int32)(ptr) = int32(v)
		case v1fkInt64:
			*(*int64)(ptr) = v
		}
		return true

	case v1fkUint, v1fkUint8, v1fkUint16, v1fkUint32, v1fkUint64, v1fkUintptr:
		if c == 'n' && d.consumeNull() {
			d.skipWS()
			return d.pos == len(data)
		}
		var bits int
		switch plan.kind {
		case v1fkUint:
			bits = strconv.IntSize
		case v1fkUint8:
			bits = 8
		case v1fkUint16:
			bits = 16
		case v1fkUint32:
			bits = 32
		case v1fkUint64:
			bits = 64
		case v1fkUintptr:
			bits = int(unsafe.Sizeof(uintptr(0)) * 8)
		}
		v, err := d.readUint64(bits)
		if err != nil {
			return false
		}
		d.skipWS()
		if d.pos != len(data) {
			return false
		}
		switch plan.kind {
		case v1fkUint:
			*(*uint)(ptr) = uint(v)
		case v1fkUint8:
			*(*uint8)(ptr) = uint8(v)
		case v1fkUint16:
			*(*uint16)(ptr) = uint16(v)
		case v1fkUint32:
			*(*uint32)(ptr) = uint32(v)
		case v1fkUint64:
			*(*uint64)(ptr) = v
		case v1fkUintptr:
			*(*uintptr)(ptr) = uintptr(v)
		}
		return true

	case v1fkFloat32, v1fkFloat64:
		if c == 'n' && d.consumeNull() {
			d.skipWS()
			return d.pos == len(data)
		}
		bits := 64
		if plan.kind == v1fkFloat32 {
			bits = 32
		}
		v, err := d.readFloat(bits)
		if err != nil {
			return false
		}
		d.skipWS()
		if d.pos != len(data) {
			return false
		}
		if plan.kind == v1fkFloat32 {
			*(*float32)(ptr) = float32(v)
		} else {
			*(*float64)(ptr) = v
		}
		return true
	}
	return false
}
