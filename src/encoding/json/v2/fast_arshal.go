// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.jsonv2

package json

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
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

	"encoding/json/internal"
	"encoding/json/internal/jsonflags"
	"encoding/json/internal/jsonopts"
	"encoding/json/internal/jsonwire"
	"encoding/json/jsontext"
)

var errFastFallback = errors.New("fast path fallback")

type fastSemanticError struct {
	action        string
	goType        *abi.Type
	offset        int
	synOffset     int
	partialEnd    int
	partialBuf    []byte
	err           error
	fn            string
	addr          bool
	lockedAddr    bool
	collapse      bool
	syntacticOnly bool
	directLegacy  error
	jsonKind      jsontext.Kind
	jsonValue     []byte
	suffixPtr     jsontext.Pointer
	reversePath   []string
}

func (e *fastSemanticError) Error() string { return e.err.Error() }

func (e *fastSemanticError) prepend(seg string, indirect bool) {
	e.reversePath = append(e.reversePath, seg)
	if indirect && !e.lockedAddr {
		e.addr = true
	}
}

func (e *fastSemanticError) setPartialEnd(end int) {
	if e.partialEnd == 0 && len(e.partialBuf) == 0 {
		e.partialEnd = end
	}
}

func newFastMarshalErr(goType *abi.Type, offset int, err error) *fastSemanticError {
	return &fastSemanticError{
		action:     "marshal",
		goType:     goType,
		offset:     offset,
		partialEnd: -1,
		err:        err,
	}
}

func newFastSyntacticErr(offset int, err error) *fastSemanticError {
	return &fastSemanticError{
		action:        "marshal",
		offset:        offset,
		partialEnd:    -1,
		err:           err,
		syntacticOnly: true,
	}
}

//go:noinline
func newFastFloatErr(goType *abi.Type, ptr unsafe.Pointer, offset int, fv float64, mo *jsonopts.Struct) *fastSemanticError {
	err := fmt.Errorf("unsupported value: %v", fv)
	if mo.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) && goType != nil && ptr != nil {
		val := reflect.NewAt(abiTypeToReflect(goType), ptr).Elem().Interface()
		err = &internal.ValueError{Val: val, Err: err}
	}
	return newFastMarshalErr(goType, offset, err)
}

//go:noinline
func newFastFloatValErr(goType *abi.Type, val any, offset int, fv float64, mo *jsonopts.Struct) *fastSemanticError {
	err := fmt.Errorf("unsupported value: %v", fv)
	if mo.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
		err = &internal.ValueError{Val: val, Err: err}
	}
	return newFastMarshalErr(goType, offset, err)
}

func wrapFastMarshalPartial(err error, partialEnd int) error {
	if se, ok := err.(*fastSemanticError); ok && se.partialEnd < 0 && se.partialBuf == nil {
		se.partialEnd = partialEnd
	}
	return err
}

func wrapFastMarshalPath(err error, seg string, indirect bool) error {
	if se, ok := err.(*fastSemanticError); ok {
		se.prepend(seg, indirect)
	}
	return err
}

func wrapFastMarshalKey(err error, seg string) error {
	if se, ok := err.(*fastSemanticError); ok {
		se.reversePath = append(se.reversePath, seg)
		se.lockedAddr = true
	}
	return err
}

func wrapFastMarshalIndirect(err error) error {
	if se, ok := err.(*fastSemanticError); ok && !se.lockedAddr {
		se.addr = true
	}
	return err
}

func lockFastMarshalAddr(err error) error {
	if se, ok := err.(*fastSemanticError); ok {
		se.lockedAddr = true
	}
	return err
}

func materializeFastErr(err error, in any, rootAt *abi.Type, startLen int, mo *jsonopts.Struct) error {
	if err == nil || err == errFastFallback {
		return err
	}
	se, ok := err.(*fastSemanticError)
	if !ok {
		return err
	}
	if se.directLegacy != nil && mo.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
		return se.directLegacy
	}
	var ptr jsontext.Pointer
	for i := len(se.reversePath) - 1; i >= 0; i-- {
		ptr = ptr.AppendToken(se.reversePath[i])
	}
	outerPtr := ptr
	if se.suffixPtr != "" {
		ptr += se.suffixPtr
	}
	if se.syntacticOnly {
		syn := &jsontext.SyntacticError{
			ByteOffset:  int64(se.offset - startLen),
			JSONPointer: ptr,
			Err:         se.err,
		}
		if mo.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
			if se.action == "unmarshal" {
				return internal.TransformUnmarshalError(in, syn)
			}
			return internal.TransformMarshalError(in, syn)
		}
		return syn
	}
	if syn, ok := se.err.(*jsontext.SyntacticError); ok {
		synCopy := *syn
		synCopy.ByteOffset += int64(se.synOffset - startLen)
		synCopy.JSONPointer = outerPtr + synCopy.JSONPointer
		se.err = &synCopy
	}
	var rt reflect.Type
	if se.goType != nil {
		rt = abiTypeToReflect(se.goType)
	}
	if mo.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
		if se.action == "marshal" && se.fn != "" && rt != nil {
			addressable := se.addr || (!se.lockedAddr && rootAt != nil && rootAt.Kind() == abi.Pointer)
			var val any
			if addressable {
				val = reflect.New(rt).Interface()
			} else {
				val = reflect.Zero(rt).Interface()
			}
			return internal.NewMarshalerError(val, se.err, se.fn)
		}
		if se.action == "unmarshal" && se.fn != "" {
			return internal.TransformUnmarshalError(in, se.err)
		}
	}
	serr := &SemanticError{
		action:      se.action,
		GoType:      rt,
		ByteOffset:  int64(se.offset - startLen),
		JSONPointer: ptr,
		JSONKind:    se.jsonKind,
		JSONValue:   jsontext.Value(se.jsonValue).Clone(),
		Err:         se.err,
	}
	var outErr error = serr
	if se.collapse {
		outErr = collapseSemanticErrors(serr)
	}
	if mo.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
		if se.action == "marshal" {
			return internal.TransformMarshalError(in, outErr)
		}
		return internal.TransformUnmarshalError(in, outErr)
	}
	return outErr
}

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

//go:noinline
func marshalFastAllocEface(plan *fastMarshalPlan, in any, mo *jsonopts.Struct) ([]byte, error) {
	_, ptr := efacePtr(&in)
	out, err := marshalFastAlloc(plan, ptr, mo)
	runtime.KeepAlive(&in)
	return out, err
}

//go:noinline
func marshalFastEface(plan *fastMarshalPlan, b []byte, in any, mo *jsonopts.Struct) ([]byte, error) {
	_, ptr := efacePtr(&in)
	out, err := plan.marshal(b, ptr, mo, 0)
	runtime.KeepAlive(&in)
	return out, err
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
	fkSlicePoint2F64
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
	hasCustom bool
	hasCycle  bool
	building  bool
	hasOmit     bool
	hasQuoted   bool
	isFlatSmall bool
	minSize     int
	elemSize    uintptr
	arrayLen    int
	elemPlan    *fastMarshalPlan
	fields      []fastMarshalField
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
	// Propagate canFast == false, hasCustom, and hasCycle across recursive dependencies.
	for changed := true; changed; {
		changed = false
		for _, p := range visiting {
			switch p.kind {
			case fkPtr, fkSlice, fkArray, fkMapStringElem, fkMapIntElem, fkMapUintElem:
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
			case fkStruct:
				for i := range p.fields {
					f := &p.fields[i]
					if f.plan == nil {
						continue
					}
					if !f.plan.canFast {
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
		if p.kind == fkStruct && p.canFast && !p.hasCustom && !p.hasOmit && !p.hasCycle && p.minSize > 0 && p.minSize <= 256 {
			flat := true
			for i := range p.fields {
				k := p.fields[i].kind
				if k < fkBool || k > fkSliceFloat32 {
					flat = false
					break
				}
			}
			p.isFlatSmall = flat
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
		if p.building {
			p.hasCycle = true
		}
		return p
	}
	p := &fastMarshalPlan{typ: at, building: true}
	visiting[at] = p
	defer func() { p.building = false }()

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
				p.hasCustom = true
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
		case fkArray:
			if rt == slicePoint2F64Type && ep.arrayLen == 2 && ep.elemPlan != nil && ep.elemPlan.kind == fkFloat64 {
				p.kind = fkSlicePoint2F64
			} else {
				p.kind = fkSlice
			}
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
			p.hasCustom = true
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
			p.hasCustom = true
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
				if !isFastNumericKind(fp.kind) && fp.kind != fkString {
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

const fastMarshalDisallowedFlags = jsonflags.AnyWhitespace | jsonflags.StringifyNumbers | jsonflags.TagFlags | jsonflags.CanonicalizeNumbers | jsonflags.ReorderRawObjects

func canFastMarshalFlags(mo *jsonopts.Struct) bool {
	return mo.Marshalers == nil && !mo.Flags.Get(fastMarshalDisallowedFlags) && !mo.Flags.Has(jsonflags.ByteLimit|jsonflags.DepthLimit)
}

var slicePoint2F64Type = reflect.TypeFor[[][2]float64]()

func fastOpts(opts []Options, storage *jsonopts.Struct) *jsonopts.Struct {
	if len(opts) == 0 {
		return &jsonopts.DefaultOptionsV2
	}
	if len(opts) == 1 {
		if s, ok := opts[0].(*jsonopts.Struct); ok && s != nil {
			return s
		}
	}
	storage.Join(opts...)
	return storage
}

func parseDec2Fast(b []byte) byte {
	if len(b) < 2 {
		return 99
	}
	return (b[0]-'0')*10 + (b[1] - '0')
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
		off := len(b)
		b, err := jsonwire.AppendQuoteString(b, *(*string)(ptr), &mo.Flags)
		if err != nil {
			return nil, newFastSyntacticErr(off, err)
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
			return nil, newFastFloatErr(p.typ, ptr, len(b), fv, mo)
		}
		return jsonwire.AppendFloat(b, fv, 32), nil

	case fkFloat64:
		fv := *(*float64)(ptr)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, newFastFloatErr(p.typ, ptr, len(b), fv, mo)
		}
		return jsonwire.AppendFloat(b, fv, 64), nil

	case fkTime:
		tt := *(*time.Time)(ptr)
		off := len(b)
		b = append(b, '"')
		b = tt.AppendFormat(b, time.RFC3339Nano)
		tb := b[off+1:]
		if len(tb) <= len("9999") || tb[len("9999")] != '-' {
			return b[:off], &fastSemanticError{
				action:     "marshal",
				goType:     p.typ,
				offset:     off,
				partialEnd: -1,
				err:        errors.New("year outside of range [0,9999]"),
				fn:         "MarshalJSON",
			}
		}
		if tb[len(tb)-1] != 'Z' {
			if len(tb) < len("Z07:00") {
				return b[:off], &fastSemanticError{
					action:     "marshal",
					goType:     p.typ,
					offset:     off,
					partialEnd: -1,
					err:        errors.New("timezone hour outside of range [0,23]"),
					fn:         "MarshalJSON",
				}
			}
			c := tb[len(tb)-len("Z07:00")]
			if ('0' <= c && c <= '9') || parseDec2Fast(tb[len(tb)-len("07:00"):]) >= 24 {
				return b[:off], &fastSemanticError{
					action:     "marshal",
					goType:     p.typ,
					offset:     off,
					partialEnd: -1,
					err:        errors.New("timezone hour outside of range [0,23]"),
					fn:         "MarshalJSON",
				}
			}
		}
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
		b, err := p.elemPlan.marshal(b, elemPtr, mo, depth+1)
		if err != nil {
			return b, wrapFastMarshalIndirect(err)
		}
		return b, nil

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
			partialEnd := len(b)
			if i > 0 {
				b = append(b, ',')
			}
			off := len(b)
			if b, err = jsonwire.AppendQuoteString(b, v, &mo.Flags); err != nil {
				return b, wrapFastMarshalPartial(wrapFastMarshalPath(newFastSyntacticErr(off, err), strconv.Itoa(i), true), partialEnd)
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
		if len(s) >= 8 {
			b = slices.Grow(b, len(s)*4+2)
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
		if len(s) >= 8 {
			b = slices.Grow(b, len(s)*4+2)
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
		if len(s) >= 8 {
			b = slices.Grow(b, len(s)*4+2)
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
			partialEnd := len(b)
			if i > 0 {
				b = append(b, ',')
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				var elemTyp *abi.Type
				if p.elemPlan != nil {
					elemTyp = p.elemPlan.typ
				}
				return b, wrapFastMarshalPartial(wrapFastMarshalPath(newFastFloatErr(elemTyp, unsafe.Pointer(&s[i]), len(b), v, mo), strconv.Itoa(i), true), partialEnd)
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
			partialEnd := len(b)
			if i > 0 {
				b = append(b, ',')
			}
			fv := float64(v)
			if math.IsNaN(fv) || math.IsInf(fv, 0) {
				var elemTyp *abi.Type
				if p.elemPlan != nil {
					elemTyp = p.elemPlan.typ
				}
				return b, wrapFastMarshalPartial(wrapFastMarshalPath(newFastFloatErr(elemTyp, unsafe.Pointer(&s[i]), len(b), fv, mo), strconv.Itoa(i), true), partialEnd)
			}
			b = jsonwire.AppendFloat(b, fv, 32)
		}
		return append(b, ']'), nil

	case fkSlicePoint2F64:
		s := *(*[][2]float64)(ptr)
		if len(s) == 0 {
			if s == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
				return append(b, "null"...), nil
			}
			return append(b, "[]"...), nil
		}
		b = slices.Grow(b, len(s)*24+2)
		b = append(b, '[')
		for i, pt := range s {
			partialEnd := len(b)
			if i > 0 {
				b = append(b, ',')
			}
			v0, v1 := pt[0], pt[1]
			if math.IsNaN(v0) || math.IsInf(v0, 0) {
				var elemTyp *abi.Type
				if p.elemPlan != nil && p.elemPlan.elemPlan != nil {
					elemTyp = p.elemPlan.elemPlan.typ
				}
				return b, wrapFastMarshalPartial(wrapFastMarshalPath(wrapFastMarshalPath(newFastFloatErr(elemTyp, unsafe.Pointer(&s[i][0]), len(b)+1, v0, mo), "0", false), strconv.Itoa(i), true), partialEnd)
			}
			if math.IsNaN(v1) || math.IsInf(v1, 0) {
				var elemTyp *abi.Type
				if p.elemPlan != nil && p.elemPlan.elemPlan != nil {
					elemTyp = p.elemPlan.elemPlan.typ
				}
				b = append(b, '[')
				b = jsonwire.AppendFloat(b, v0, 64)
				b = append(b, ',')
				return b, wrapFastMarshalPartial(wrapFastMarshalPath(wrapFastMarshalPath(newFastFloatErr(elemTyp, unsafe.Pointer(&s[i][1]), len(b), v1, mo), "1", false), strconv.Itoa(i), true), partialEnd)
			}
			b = append(b, '[')
			b = jsonwire.AppendFloat(b, v0, 64)
			b = append(b, ',')
			b = jsonwire.AppendFloat(b, v1, 64)
			b = append(b, ']')
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
			partialEnd := len(b)
			if i > 0 {
				b = append(b, ',')
			}
			b, err = elemPlan.marshal(b, unsafe.Add(data, uintptr(i)*elemSize), mo, depth+1)
			if err != nil {
				return b, wrapFastMarshalPartial(wrapFastMarshalPath(err, strconv.Itoa(i), true), partialEnd)
			}
		}
		return append(b, ']'), nil

	case fkArray:
		n := p.arrayLen
		if n == 0 {
			return append(b, "[]"...), nil
		}
		elemPlan := p.elemPlan
		switch elemPlan.kind {
		case fkInt64:
			if n >= 8 {
				b = slices.Grow(b, n*4+2)
			}
			b = append(b, '[')
			s := unsafe.Slice((*int64)(ptr), n)
			for i, v := range s {
				if i > 0 {
					b = append(b, ',')
				}
				b = strconv.AppendInt(b, v, 10)
			}
			return append(b, ']'), nil
		case fkInt:
			if n >= 8 {
				b = slices.Grow(b, n*4+2)
			}
			b = append(b, '[')
			s := unsafe.Slice((*int)(ptr), n)
			for i, v := range s {
				if i > 0 {
					b = append(b, ',')
				}
				b = strconv.AppendInt(b, int64(v), 10)
			}
			return append(b, ']'), nil
		case fkUint64:
			if n >= 8 {
				b = slices.Grow(b, n*4+2)
			}
			b = append(b, '[')
			s := unsafe.Slice((*uint64)(ptr), n)
			for i, v := range s {
				if i > 0 {
					b = append(b, ',')
				}
				b = strconv.AppendUint(b, v, 10)
			}
			return append(b, ']'), nil
		case fkFloat64:
			if n == 2 {
				arr := (*[2]float64)(ptr)
				v0, v1 := arr[0], arr[1]
				if !math.IsNaN(v0) && !math.IsInf(v0, 0) && !math.IsNaN(v1) && !math.IsInf(v1, 0) {
					b = append(b, '[')
					b = jsonwire.AppendFloat(b, v0, 64)
					b = append(b, ',')
					b = jsonwire.AppendFloat(b, v1, 64)
					return append(b, ']'), nil
				}
			}
		}
		b = append(b, '[')
		elemSize := p.elemSize
		var err error
		for i := range n {
			partialEnd := len(b)
			if i > 0 {
				b = append(b, ',')
			}
			b, err = elemPlan.marshal(b, unsafe.Add(ptr, uintptr(i)*elemSize), mo, depth+1)
			if err != nil {
				return b, wrapFastMarshalPartial(wrapFastMarshalPath(err, strconv.Itoa(i), false), partialEnd)
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
		return p.marshalCustomMarshaler(b, ptr, mo, depth)

	case fkStruct:
		return p.marshalStruct(b, ptr, mo, depth+1)
	}
	return b, errFastFallback
}

var errInvalidJSON = errors.New("invalid JSON")

//go:noinline
func validateMarshalJSONSyntaxErr(goType *abi.Type, offset int, val []byte, mo *jsonopts.Struct) error {
	moCopy := *mo
	enc := export.GetBufferedEncoder(&moCopy)
	err := enc.WriteValue(val)
	export.PutBufferedEncoder(enc)
	if err == nil {
		err = &jsontext.SyntacticError{Err: errors.New("exceeded max depth")}
	}
	return &fastSemanticError{
		action:     "marshal",
		goType:     goType,
		offset:     offset,
		synOffset:  offset,
		partialEnd: -1,
		err:        err,
		fn:         "MarshalJSON",
	}
}

func (p *fastMarshalPlan) marshalCustomMarshaler(b []byte, ptr unsafe.Pointer, mo *jsonopts.Struct, depth int) ([]byte, error) {
	if ptr == nil {
		return b, errFastFallback
	}
	var m Marshaler
	ip := (*[2]unsafe.Pointer)(unsafe.Pointer(&m))
	ip[0] = p.ifaceTab
	ip[1] = ptr
	val, err := m.MarshalJSON()
	runtime.KeepAlive(ptr)
	if err != nil {
		err = wrapErrUnsupported(err, "MarshalJSON method")
		return b, &fastSemanticError{
			action:     "marshal",
			goType:     p.typ,
			offset:     len(b),
			partialEnd: -1,
			err:        err,
			fn:         "MarshalJSON",
			collapse:   true,
		}
	}
	off := len(b)
	out, err := appendCompactValidJSON(b, val, mo, depth)
	if err != nil {
		return b, validateMarshalJSONSyntaxErr(p.typ, off, val, mo)
	}
	return out, nil
}

func appendCompactValidJSON(dst, src []byte, mo *jsonopts.Struct, depth int) ([]byte, error) {
	allowBadUTF8 := mo.Flags.Get(jsonflags.AllowInvalidUTF8)
	allowDup := mo.Flags.Get(jsonflags.AllowDuplicateNames)
	escHTML := mo.Flags.Get(jsonflags.EscapeForHTML)
	escJS := mo.Flags.Get(jsonflags.EscapeForJS)
	dst, i, err := appendCompactValidJSONValue(dst, src, 0, mo, allowBadUTF8, allowDup, escHTML, escJS, depth)
	if err != nil {
		return nil, err
	}
	for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
		i++
	}
	if i != len(src) {
		return nil, errInvalidJSON
	}
	return dst, nil
}

func appendCompactValidJSONString(dst, src []byte, pos int, mo *jsonopts.Struct, escHTML, needKey bool) ([]byte, int, []byte, error) {
	if pos >= len(src) || src[pos] != '"' {
		return nil, 0, nil, errInvalidJSON
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
					goto reformat
				}
			}
		}
		return append(dst, src[pos:i+1]...), i + 1, raw, nil
	}
reformat:
	dstStart := len(dst)
	dst2, n, err := jsonwire.ReformatString(dst, src[pos:], &mo.Flags)
	if err != nil {
		return nil, 0, nil, err
	}
	var kb []byte
	if needKey {
		kb, _ = jsonwire.AppendUnquote(nil, dst2[dstStart:])
	}
	return dst2, pos + n, kb, nil
}

func appendCompactValidJSONValue(dst, src []byte, i int, mo *jsonopts.Struct, allowBadUTF8, allowDup, escHTML, escJS bool, depth int) ([]byte, int, error) {
	for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
		i++
	}
	if i >= len(src) {
		return nil, 0, errInvalidJSON
	}
	switch src[i] {
	case 'n':
		if n := jsonwire.ConsumeNull(src[i:]); n > 0 {
			return append(dst, "null"...), i + n, nil
		}
		return nil, 0, errInvalidJSON
	case 't':
		if n := jsonwire.ConsumeTrue(src[i:]); n > 0 {
			return append(dst, "true"...), i + n, nil
		}
		return nil, 0, errInvalidJSON
	case 'f':
		if n := jsonwire.ConsumeFalse(src[i:]); n > 0 {
			return append(dst, "false"...), i + n, nil
		}
		return nil, 0, errInvalidJSON
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		n, err := jsonwire.ConsumeNumber(src[i:])
		if err != nil {
			return nil, 0, errInvalidJSON
		}
		return append(dst, src[i:i+n]...), i + n, nil
	case '"':
		dst, next, _, err := appendCompactValidJSONString(dst, src, i, mo, escHTML, false)
		return dst, next, err
	case '[':
		if depth >= 10000 {
			return nil, 0, errInvalidJSON
		}
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
			dst, i, err = appendCompactValidJSONValue(dst, src, i, mo, allowBadUTF8, allowDup, escHTML, escJS, depth+1)
			if err != nil {
				return nil, 0, err
			}
			for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
				i++
			}
			if i >= len(src) {
				return nil, 0, errInvalidJSON
			}
			if src[i] == ',' {
				i++
				dst = append(dst, ',')
				continue
			}
			if src[i] == ']' {
				return append(dst, ']'), i + 1, nil
			}
			return nil, 0, errInvalidJSON
		}
	case '{':
		if depth >= 10000 {
			return nil, 0, errInvalidJSON
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
		var seen map[string]struct{}
		numKeys := 0
		var err error
		for {
			for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
				i++
			}
			var kb []byte
			dst, i, kb, err = appendCompactValidJSONString(dst, src, i, mo, escHTML, !allowDup)
			if err != nil {
				return nil, 0, err
			}
			if !allowDup {
				if seen != nil {
					if _, dup := seen[string(kb)]; dup {
						return nil, 0, errInvalidJSON
					}
					seen[string(kb)] = struct{}{}
				} else {
					for j := range numKeys {
						if bytes.Equal(keys[j], kb) {
							return nil, 0, errInvalidJSON
						}
					}
					if numKeys < len(keys) {
						keys[numKeys] = kb
						numKeys++
					} else {
						seen = make(map[string]struct{}, 16)
						for j := range numKeys {
							seen[string(keys[j])] = struct{}{}
						}
						seen[string(kb)] = struct{}{}
					}
				}
			}
			for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
				i++
			}
			if i >= len(src) || src[i] != ':' {
				return nil, 0, errInvalidJSON
			}
			i++
			dst = append(dst, ':')
			dst, i, err = appendCompactValidJSONValue(dst, src, i, mo, allowBadUTF8, allowDup, escHTML, escJS, depth+1)
			if err != nil {
				return nil, 0, err
			}
			for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
				i++
			}
			if i >= len(src) {
				return nil, 0, errInvalidJSON
			}
			if src[i] == ',' {
				i++
				dst = append(dst, ',')
				continue
			}
			if src[i] == '}' {
				return append(dst, '}'), i + 1, nil
			}
			return nil, 0, errInvalidJSON
		}
	}
	return nil, 0, errInvalidJSON
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
			mark := len(b)
			if i == 0 {
				mark++ // include opening '{' when failing on first field's key
			}
			if !f.nameNeedEscape {
				b = append(b, f.commaQuotedNameColon...)
			} else {
				if i == 0 {
					b = append(b, '{')
				} else {
					b = append(b, ',')
				}
				nameOff := len(b)
				if b, err = jsonwire.AppendQuoteString(b, f.name, &mo.Flags); err != nil {
					return b, wrapFastMarshalPartial(newFastSyntacticErr(nameOff, err), mark)
				}
				b = append(b, ':')
			}
			nameEnd := len(b) - 1 // before ':'
			switch f.kind {
			case fkString:
				off := len(b)
				if b, err = jsonwire.AppendQuoteString(b, *(*string)(fp), &mo.Flags); err != nil {
					return b, wrapFastMarshalPartial(wrapFastMarshalPath(newFastSyntacticErr(off, err), f.name, false), nameEnd)
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
					return b, wrapFastMarshalPartial(wrapFastMarshalPath(newFastFloatErr(f.plan.typ, fp, len(b), fv, mo), f.name, false), nameEnd)
				}
				b = jsonwire.AppendFloat(b, fv, 64)
			case fkFloat32:
				fv := float64(*(*float32)(fp))
				if math.IsNaN(fv) || math.IsInf(fv, 0) {
					return b, wrapFastMarshalPartial(wrapFastMarshalPath(newFastFloatErr(f.plan.typ, fp, len(b), fv, mo), f.name, false), nameEnd)
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
						elemPartial := len(b)
						if j > 0 {
							b = append(b, ',')
						}
						off := len(b)
						if b, err = jsonwire.AppendQuoteString(b, v, &mo.Flags); err != nil {
							return b, wrapFastMarshalPartial(wrapFastMarshalPath(wrapFastMarshalPath(newFastSyntacticErr(off, err), strconv.Itoa(j), true), f.name, false), elemPartial)
						}
					}
					b = append(b, ']')
				}
			default:
				if b, err = f.plan.marshal(b, fp, mo, depth); err != nil {
					return b, wrapFastMarshalPartial(wrapFastMarshalPath(err, f.name, false), nameEnd)
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
					if !f.quoted {
						if len(*(*string)(fp)) == 0 {
							continue
						}
					} else {
						checkPostEmpty = true
					}
				case fkBytes:
					if len(*(*[]byte)(fp)) == 0 {
						continue
					}
				case fkByteArray, fkArray:
					if f.plan.arrayLen == 0 {
						continue
					}
				case fkSliceBool, fkSliceString, fkSliceInt, fkSliceInt64, fkSliceUint64, fkSliceFloat64, fkSliceFloat32, fkSlicePoint2F64, fkSlice:
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
			nameOff := len(b)
			if b, err = jsonwire.AppendQuoteString(b, f.name, &mo.Flags); err != nil {
				return b, wrapFastMarshalPartial(newFastSyntacticErr(nameOff, err), mark)
			}
			b = append(b, ':')
		}
		nameEnd := len(b) - 1
		valStart := len(b)
		if f.quoted && f.kind != fkString {
			b = append(b, '"')
		}

		switch f.kind {
		case fkString:
			if f.quoted {
				if mo.Flags.Get(jsonflags.StringifyWithLegacySemantics) {
					var innerBuf [128]byte
					inner, ierr := jsonwire.AppendQuoteString(innerBuf[:0], *(*string)(fp), &mo.Flags)
					if ierr != nil {
						return b, wrapFastMarshalPartial(wrapFastMarshalPath(newFastSyntacticErr(valStart, ierr), f.name, false), nameEnd)
					}
					var outerFlags jsonflags.Flags
					b, err = jsonwire.AppendQuoteBytes(b, inner, &outerFlags)
					if err != nil {
						return b, wrapFastMarshalPartial(wrapFastMarshalPath(newFastSyntacticErr(valStart, err), f.name, false), nameEnd)
					}
					break
				}
				if !mo.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
					return b, wrapFastMarshalPartial(wrapFastMarshalPath(newFastMarshalErr(f.plan.typ, valStart, errInvalidStringTag), f.name, false), nameEnd)
				}
			}
			off := len(b)
			if b, err = jsonwire.AppendQuoteString(b, *(*string)(fp), &mo.Flags); err != nil {
				return b, wrapFastMarshalPartial(wrapFastMarshalPath(newFastSyntacticErr(off, err), f.name, false), nameEnd)
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
				return b, wrapFastMarshalPartial(wrapFastMarshalPath(newFastFloatErr(f.plan.typ, fp, valStart, fv, mo), f.name, false), nameEnd)
			}
			b = jsonwire.AppendFloat(b, fv, 64)
		case fkFloat32:
			fv := float64(*(*float32)(fp))
			if math.IsNaN(fv) || math.IsInf(fv, 0) {
				return b, wrapFastMarshalPartial(wrapFastMarshalPath(newFastFloatErr(f.plan.typ, fp, valStart, fv, mo), f.name, false), nameEnd)
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
					elemPartial := len(b)
					if j > 0 {
						b = append(b, ',')
					}
					off := len(b)
					if b, err = jsonwire.AppendQuoteString(b, v, &mo.Flags); err != nil {
						return b, wrapFastMarshalPartial(wrapFastMarshalPath(wrapFastMarshalPath(newFastSyntacticErr(off, err), strconv.Itoa(j), true), f.name, false), elemPartial)
					}
				}
				b = append(b, ']')
			}
		default:
			if b, err = f.plan.marshal(b, fp, mo, depth); err != nil {
				return b, wrapFastMarshalPartial(wrapFastMarshalPath(err, f.name, false), nameEnd)
			}
		}
		if f.quoted && f.kind != fkString {
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
		return *(*float32)(ptr) == 0
	case fkFloat64:
		return *(*float64)(ptr) == 0
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
	case fkSliceBool, fkSliceString, fkSliceInt, fkSliceInt64, fkSliceUint64, fkSliceFloat64, fkSliceFloat32, fkSlicePoint2F64, fkSlice:
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
	case fkSliceBool, fkSliceString, fkSliceInt, fkSliceInt64, fkSliceUint64, fkSliceFloat64, fkSliceFloat32, fkSlicePoint2F64, fkSlice:
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

//go:noinline
func checkDuplicateMapKey(seen map[string]struct{}, quoted []byte, off, partialEnd int) (map[string]struct{}, *fastSemanticError) {
	uk, _ := jsonwire.AppendUnquote(nil, quoted)
	key := string(uk)
	if seen == nil {
		seen = make(map[string]struct{}, 8)
	} else if _, dup := seen[key]; dup {
		return seen, &fastSemanticError{
			action:        "marshal",
			offset:        off,
			partialEnd:    partialEnd,
			err:           jsontext.ErrDuplicateName,
			syntacticOnly: true,
			lockedAddr:    true,
			reversePath:   []string{key},
		}
	}
	seen[key] = struct{}{}
	return seen, nil
}

//go:noinline
func appendMarshalAnyFallback(b []byte, val any, mo *jsonopts.Struct) ([]byte, error) {
	start := len(b)
	moCopy := *mo
	enc := export.GetBufferedEncoder(&moCopy)
	xe := export.Encoder(enc)
	xe.Flags.Set(jsonflags.OmitTopLevelNewline | 1)
	err := marshalEncode(enc, val, &xe.Struct)
	if err != nil {
		partialBuf := make([]byte, len(b)+len(xe.Buf))
		copy(partialBuf, b)
		copy(partialBuf[len(b):], xe.Buf)
		export.PutBufferedEncoder(enc)
		switch e := err.(type) {
		case *SemanticError:
			return b, &fastSemanticError{
				action:     e.action,
				goType:     abiTypeOfReflect(e.GoType),
				offset:     start + int(e.ByteOffset),
				synOffset:  start,
				partialEnd: -1,
				partialBuf: partialBuf,
				err:        e.Err,
				lockedAddr: true,
				jsonKind:   e.JSONKind,
				jsonValue:  e.JSONValue,
				suffixPtr:  e.JSONPointer,
			}
		case *jsontext.SyntacticError:
			return b, &fastSemanticError{
				action:        "marshal",
				offset:        start + int(e.ByteOffset),
				partialEnd:    -1,
				partialBuf:    partialBuf,
				err:           e.Err,
				syntacticOnly: true,
				lockedAddr:    true,
				suffixPtr:     e.JSONPointer,
			}
		default:
			return b, &fastSemanticError{
				partialEnd:   -1,
				partialBuf:   partialBuf,
				directLegacy: err,
				lockedAddr:   true,
			}
		}
	}
	b = append(b, xe.Buf...)
	export.PutBufferedEncoder(enc)
	return b, nil
}

func (p *fastMarshalPlan) marshalMapStringElem(b []byte, ptr unsafe.Pointer, mo *jsonopts.Struct, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return b, errFastFallback
	}
	m := *(**maps.Map)(ptr)
	if m == nil || m.Used() == 0 {
		if m == nil && mo.Flags.Get(jsonflags.FormatNilMapAsNull) {
			return append(b, "null"...), nil
		}
		return append(b, "{}"...), nil
	}
	n := int(m.Used())
	checkDupInvalidUTF8 := mo.Flags.Get(jsonflags.AllowInvalidUTF8) && !mo.Flags.Get(jsonflags.AllowDuplicateNames) && n > 1
	var seen map[string]struct{}
	b = append(b, '{')
	elemPlan := p.elemPlan
	var err error
	if !mo.Flags.Get(jsonflags.Deterministic) || n <= 1 {
		var it maps.Iter
		it.Init(p.mapType, m)
		first := true
		for it.Next(); it.Key() != nil; it.Next() {
			beforeComma := len(b)
			if !first {
				b = append(b, ',')
			}
			first = false
			key := *(*string)(it.Key())
			keyOff := len(b)
			if b, err = jsonwire.AppendQuoteString(b, key, &mo.Flags); err != nil {
				return b, wrapFastMarshalPartial(newFastSyntacticErr(keyOff, err), beforeComma)
			}
			if checkDupInvalidUTF8 {
				var ferr *fastSemanticError
				if seen, ferr = checkDuplicateMapKey(seen, b[keyOff:], keyOff, beforeComma); ferr != nil {
					return b, ferr
				}
			}
			keyEnd := len(b)
			b = append(b, ':')
			if b, err = elemPlan.marshal(b, it.Elem(), mo, depth); err != nil {
				return b, wrapFastMarshalPartial(wrapFastMarshalKey(err, key), keyEnd)
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
			beforeComma := len(b)
			if idx > 0 {
				b = append(b, ',')
			}
			keyOff := len(b)
			if b, err = jsonwire.AppendQuoteString(b, e.k, &mo.Flags); err != nil {
				putMapStringEntries(ep)
				return b, wrapFastMarshalPartial(newFastSyntacticErr(keyOff, err), beforeComma)
			}
			if checkDupInvalidUTF8 {
				var ferr *fastSemanticError
				if seen, ferr = checkDuplicateMapKey(seen, b[keyOff:], keyOff, beforeComma); ferr != nil {
					putMapStringEntries(ep)
					return b, ferr
				}
			}
			keyEnd := len(b)
			b = append(b, ':')
			if b, err = elemPlan.marshal(b, e.v, mo, depth); err != nil {
				putMapStringEntries(ep)
				return b, wrapFastMarshalPartial(wrapFastMarshalKey(err, e.k), keyEnd)
			}
		}
		putMapStringEntries(ep)
	}
	return append(b, '}'), nil
}

func (p *fastMarshalPlan) marshalMapIntElem(b []byte, ptr unsafe.Pointer, mo *jsonopts.Struct, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return b, errFastFallback
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
			keyStart := len(b)
			b = strconv.AppendInt(b, readSignedKey(it.Key(), keyBits), 10)
			keyEnd := len(b)
			b = append(b, '"')
			keyEndQuote := len(b)
			b = append(b, ':')
			if b, err = elemPlan.marshal(b, it.Elem(), mo, depth); err != nil {
				return b, wrapFastMarshalPartial(wrapFastMarshalKey(err, string(b[keyStart:keyEnd])), keyEndQuote)
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
			keyEndQuote := len(b) - 1
			if b, err = elemPlan.marshal(b, e.v, mo, depth); err != nil {
				keyStr := string(e.buf[1 : e.n-2])
				putMapNumEntries(ep)
				return b, wrapFastMarshalPartial(wrapFastMarshalKey(err, keyStr), keyEndQuote)
			}
		}
		putMapNumEntries(ep)
	}
	return append(b, '}'), nil
}

func (p *fastMarshalPlan) marshalMapUintElem(b []byte, ptr unsafe.Pointer, mo *jsonopts.Struct, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return b, errFastFallback
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
			keyStart := len(b)
			b = strconv.AppendUint(b, readUnsignedKey(it.Key(), keyBits), 10)
			keyEnd := len(b)
			b = append(b, '"')
			keyEndQuote := len(b)
			b = append(b, ':')
			if b, err = elemPlan.marshal(b, it.Elem(), mo, depth); err != nil {
				return b, wrapFastMarshalPartial(wrapFastMarshalKey(err, string(b[keyStart:keyEnd])), keyEndQuote)
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
			keyEndQuote := len(b) - 1
			if b, err = elemPlan.marshal(b, e.v, mo, depth); err != nil {
				keyStr := string(e.buf[1 : e.n-2])
				putMapNumEntries(ep)
				return b, wrapFastMarshalPartial(wrapFastMarshalKey(err, keyStr), keyEndQuote)
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
	checkDupInvalidUTF8 := mo.Flags.Get(jsonflags.AllowInvalidUTF8) && !mo.Flags.Get(jsonflags.AllowDuplicateNames) && n > 1
	var seen map[string]struct{}
	b = append(b, '{')
	var err error
	if !mo.Flags.Get(jsonflags.Deterministic) || n <= 1 {
		first := true
		for k, v := range m {
			beforeComma := len(b)
			if !first {
				b = append(b, ',')
			}
			first = false
			keyOff := len(b)
			if b, err = jsonwire.AppendQuoteString(b, k, &mo.Flags); err != nil {
				return b, wrapFastMarshalPartial(newFastSyntacticErr(keyOff, err), beforeComma)
			}
			if checkDupInvalidUTF8 {
				var ferr *fastSemanticError
				if seen, ferr = checkDuplicateMapKey(seen, b[keyOff:], keyOff, beforeComma); ferr != nil {
					return b, ferr
				}
			}
			keyEnd := len(b)
			b = append(b, ':')
			valOff := len(b)
			if b, err = jsonwire.AppendQuoteString(b, v, &mo.Flags); err != nil {
				return b, wrapFastMarshalPartial(wrapFastMarshalKey(newFastSyntacticErr(valOff, err), k), keyEnd)
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
			beforeComma := len(b)
			if idx > 0 {
				b = append(b, ',')
			}
			keyOff := len(b)
			if b, err = jsonwire.AppendQuoteString(b, k, &mo.Flags); err != nil {
				putStrings(names)
				return b, wrapFastMarshalPartial(newFastSyntacticErr(keyOff, err), beforeComma)
			}
			if checkDupInvalidUTF8 {
				var ferr *fastSemanticError
				if seen, ferr = checkDuplicateMapKey(seen, b[keyOff:], keyOff, beforeComma); ferr != nil {
					putStrings(names)
					return b, ferr
				}
			}
			keyEnd := len(b)
			b = append(b, ':')
			valOff := len(b)
			if b, err = jsonwire.AppendQuoteString(b, m[k], &mo.Flags); err != nil {
				putStrings(names)
				return b, wrapFastMarshalPartial(wrapFastMarshalKey(newFastSyntacticErr(valOff, err), k), keyEnd)
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
	checkDupInvalidUTF8 := mo.Flags.Get(jsonflags.AllowInvalidUTF8) && !mo.Flags.Get(jsonflags.AllowDuplicateNames) && n > 1
	var seen map[string]struct{}
	b = append(b, '{')
	var err error
	if !mo.Flags.Get(jsonflags.Deterministic) || n <= 1 {
		first := true
		for k, v := range m {
			beforeComma := len(b)
			if !first {
				b = append(b, ',')
			}
			first = false
			keyOff := len(b)
			if b, err = jsonwire.AppendQuoteString(b, k, &mo.Flags); err != nil {
				return b, wrapFastMarshalPartial(newFastSyntacticErr(keyOff, err), beforeComma)
			}
			if checkDupInvalidUTF8 {
				var ferr *fastSemanticError
				if seen, ferr = checkDuplicateMapKey(seen, b[keyOff:], keyOff, beforeComma); ferr != nil {
					return b, ferr
				}
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
			beforeComma := len(b)
			if idx > 0 {
				b = append(b, ',')
			}
			keyOff := len(b)
			if b, err = jsonwire.AppendQuoteString(b, k, &mo.Flags); err != nil {
				putStrings(names)
				return b, wrapFastMarshalPartial(newFastSyntacticErr(keyOff, err), beforeComma)
			}
			if checkDupInvalidUTF8 {
				var ferr *fastSemanticError
				if seen, ferr = checkDuplicateMapKey(seen, b[keyOff:], keyOff, beforeComma); ferr != nil {
					putStrings(names)
					return b, ferr
				}
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
		return marshalMapAnyCycle(b, m, mo, depth, nil)
	}
	n := len(m)
	if n == 0 {
		if m == nil && mo.Flags.Get(jsonflags.FormatNilMapAsNull) {
			return append(b, "null"...), nil
		}
		return append(b, "{}"...), nil
	}
	checkDupInvalidUTF8 := mo.Flags.Get(jsonflags.AllowInvalidUTF8) && !mo.Flags.Get(jsonflags.AllowDuplicateNames) && n > 1
	var seen map[string]struct{}
	b = append(b, '{')
	var err error
	if !mo.Flags.Get(jsonflags.Deterministic) || n <= 1 {
		first := true
		for k, v := range m {
			beforeComma := len(b)
			if !first {
				b = append(b, ',')
			}
			first = false
			keyOff := len(b)
			if b, err = jsonwire.AppendQuoteString(b, k, &mo.Flags); err != nil {
				return b, wrapFastMarshalPartial(newFastSyntacticErr(keyOff, err), beforeComma)
			}
			if checkDupInvalidUTF8 {
				var ferr *fastSemanticError
				if seen, ferr = checkDuplicateMapKey(seen, b[keyOff:], keyOff, beforeComma); ferr != nil {
					return b, ferr
				}
			}
			keyEnd := len(b)
			b = append(b, ':')
			if b, err = marshalFastAny(b, v, mo, depth+1); err != nil {
				return b, wrapFastMarshalPartial(wrapFastMarshalKey(err, k), keyEnd)
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
			beforeComma := len(b)
			if idx > 0 {
				b = append(b, ',')
			}
			keyOff := len(b)
			if b, err = jsonwire.AppendQuoteString(b, k, &mo.Flags); err != nil {
				putStrings(names)
				return b, wrapFastMarshalPartial(newFastSyntacticErr(keyOff, err), beforeComma)
			}
			if checkDupInvalidUTF8 {
				var ferr *fastSemanticError
				if seen, ferr = checkDuplicateMapKey(seen, b[keyOff:], keyOff, beforeComma); ferr != nil {
					putStrings(names)
					return b, ferr
				}
			}
			keyEnd := len(b)
			b = append(b, ':')
			if b, err = marshalFastAny(b, m[k], mo, depth+1); err != nil {
				putStrings(names)
				return b, wrapFastMarshalPartial(wrapFastMarshalKey(err, k), keyEnd)
			}
		}
		putStrings(names)
	}
	return append(b, '}'), nil
}

//go:noinline
func marshalMapAnyCycle(b []byte, m map[string]any, mo *jsonopts.Struct, depth int, seenPtrs seenPointers) ([]byte, error) {
	if depth >= 10000 {
		return b, newFastSyntacticErr(len(b), errors.New("exceeded max depth"))
	}
	rv := reflect.ValueOf(m)
	if err := visitPointer(&seenPtrs, rv, mo); err != nil {
		return b, &fastSemanticError{
			action:     "marshal",
			goType:     abiTypeOfReflect(rv.Type()),
			offset:     len(b),
			partialEnd: -1,
			err:        err,
			lockedAddr: true,
		}
	}
	defer leavePointer(&seenPtrs, rv)

	n := len(m)
	if n == 0 {
		if m == nil && mo.Flags.Get(jsonflags.FormatNilMapAsNull) {
			return append(b, "null"...), nil
		}
		return append(b, "{}"...), nil
	}
	checkDupInvalidUTF8 := mo.Flags.Get(jsonflags.AllowInvalidUTF8) && !mo.Flags.Get(jsonflags.AllowDuplicateNames) && n > 1
	var seenKeys map[string]struct{}
	b = append(b, '{')
	var err error
	if !mo.Flags.Get(jsonflags.Deterministic) || n <= 1 {
		first := true
		for k, v := range m {
			beforeComma := len(b)
			if !first {
				b = append(b, ',')
			}
			first = false
			keyOff := len(b)
			if b, err = jsonwire.AppendQuoteString(b, k, &mo.Flags); err != nil {
				return b, wrapFastMarshalPartial(newFastSyntacticErr(keyOff, err), beforeComma)
			}
			if checkDupInvalidUTF8 {
				var ferr *fastSemanticError
				if seenKeys, ferr = checkDuplicateMapKey(seenKeys, b[keyOff:], keyOff, beforeComma); ferr != nil {
					return b, ferr
				}
			}
			keyEnd := len(b)
			b = append(b, ':')
			if b, err = marshalAnyCycle(b, v, mo, depth+1, seenPtrs); err != nil {
				return b, wrapFastMarshalPartial(wrapFastMarshalKey(err, k), keyEnd)
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
			beforeComma := len(b)
			if idx > 0 {
				b = append(b, ',')
			}
			keyOff := len(b)
			if b, err = jsonwire.AppendQuoteString(b, k, &mo.Flags); err != nil {
				putStrings(names)
				return b, wrapFastMarshalPartial(newFastSyntacticErr(keyOff, err), beforeComma)
			}
			if checkDupInvalidUTF8 {
				var ferr *fastSemanticError
				if seenKeys, ferr = checkDuplicateMapKey(seenKeys, b[keyOff:], keyOff, beforeComma); ferr != nil {
					putStrings(names)
					return b, ferr
				}
			}
			keyEnd := len(b)
			b = append(b, ':')
			if b, err = marshalAnyCycle(b, m[k], mo, depth+1, seenPtrs); err != nil {
				putStrings(names)
				return b, wrapFastMarshalPartial(wrapFastMarshalKey(err, k), keyEnd)
			}
		}
		putStrings(names)
	}
	return append(b, '}'), nil
}

//go:noinline
func marshalAnyCycle(b []byte, val any, mo *jsonopts.Struct, depth int, seenPtrs seenPointers) ([]byte, error) {
	if depth >= 10000 {
		return b, newFastSyntacticErr(len(b), errors.New("exceeded max depth"))
	}
	switch v := val.(type) {
	case []any:
		rv := reflect.ValueOf(v)
		if err := visitPointer(&seenPtrs, rv, mo); err != nil {
			return b, &fastSemanticError{
				action:     "marshal",
				goType:     abiTypeOfReflect(rv.Type()),
				offset:     len(b),
				partialEnd: -1,
				err:        err,
				lockedAddr: true,
			}
		}
		defer leavePointer(&seenPtrs, rv)
		if len(v) == 0 {
			if v == nil && mo.Flags.Get(jsonflags.FormatNilSliceAsNull) {
				return append(b, "null"...), nil
			}
			return append(b, "[]"...), nil
		}
		b = append(b, '[')
		var err error
		for i := range v {
			partialEnd := len(b)
			if i > 0 {
				b = append(b, ',')
			}
			if b, err = marshalAnyCycle(b, v[i], mo, depth+1, seenPtrs); err != nil {
				return b, wrapFastMarshalPartial(wrapFastMarshalPath(err, strconv.Itoa(i), true), partialEnd)
			}
		}
		return append(b, ']'), nil
	case map[string]any:
		return marshalMapAnyCycle(b, v, mo, depth, seenPtrs)
	}
	rv := reflect.ValueOf(val)
	if rv.IsValid() && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Slice || rv.Kind() == reflect.Map) {
		if err := visitPointer(&seenPtrs, rv, mo); err != nil {
			return b, &fastSemanticError{
				action:     "marshal",
				goType:     abiTypeOfReflect(rv.Type()),
				offset:     len(b),
				partialEnd: -1,
				err:        err,
				lockedAddr: true,
			}
		}
		defer leavePointer(&seenPtrs, rv)
	}
	return appendMarshalAnyFallback(b, val, mo)
}

var (
	float32TypeAbi = abi.TypeFor[float32]()
	float64TypeAbi = abi.TypeFor[float64]()
)

//go:noinline
func marshalFastAnyEface(b []byte, val any, mo *jsonopts.Struct, depth int) ([]byte, error) {
	at, ptr := efacePtr(&val)
	if at == nil {
		return append(b, "null"...), nil
	}
	plan := lookupFastMarshalPlan(at)
	if !plan.canFast {
		return appendMarshalAnyFallback(b, val, mo)
	}
	b, err := plan.marshal(b, ptr, mo, depth+1)
	runtime.KeepAlive(&val)
	if err != nil {
		if at.Kind() != abi.Pointer {
			err = lockFastMarshalAddr(err)
		}
		return b, err
	}
	return b, nil
}

func marshalFastAny(b []byte, val any, mo *jsonopts.Struct, depth int) ([]byte, error) {
	if depth > startDetectingCyclesAfter {
		return marshalAnyCycle(b, val, mo, depth, nil)
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
		off := len(b)
		b, err := jsonwire.AppendQuoteString(b, v, &mo.Flags)
		if err != nil {
			return b, newFastSyntacticErr(off, err)
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
			return b, newFastFloatValErr(float64TypeAbi, val, len(b), v, mo)
		}
		return jsonwire.AppendFloat(b, v, 64), nil
	case float32:
		fv := float64(v)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return b, newFastFloatValErr(float32TypeAbi, val, len(b), fv, mo)
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
		for i := range v {
			partialEnd := len(b)
			if i > 0 {
				b = append(b, ',')
			}
			if b, err = marshalFastAny(b, v[i], mo, depth+1); err != nil {
				return b, wrapFastMarshalPartial(wrapFastMarshalPath(err, strconv.Itoa(i), true), partialEnd)
			}
		}
		return append(b, ']'), nil
	case map[string]any:
		return marshalFastMapStringAny(b, v, mo, depth)
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
			partialEnd := len(b)
			if i > 0 {
				b = append(b, ',')
			}
			off := len(b)
			if b, err = jsonwire.AppendQuoteString(b, s, &mo.Flags); err != nil {
				return b, wrapFastMarshalPartial(wrapFastMarshalPath(newFastSyntacticErr(off, err), strconv.Itoa(i), true), partialEnd)
			}
		}
		return append(b, ']'), nil
	case map[string]string:
		return marshalFastMapStringString(b, v, mo)
	}

	return marshalFastAnyEface(b, val, mo, depth)
}

func fastPartialOutput(b []byte, err error) []byte {
	if se, ok := err.(*fastSemanticError); ok {
		if se.partialBuf != nil {
			return se.partialBuf
		}
		if se.partialEnd >= 0 && se.partialEnd <= len(b) {
			return b[:se.partialEnd]
		}
	}
	return nil
}

// tryFastAppendMarshal attempts to marshal (at, ptr) directly into dst.
// A nil error indicates success. If err == errFastFallback, the caller should
// fall back to standard marshaling using dst[:origLen]. Any other error is a
// finalized fast semantic/syntactic error that must be materialized directly.
func tryFastAppendMarshal(dst []byte, at *abi.Type, ptr unsafe.Pointer, mo *jsonopts.Struct) ([]byte, error) {
	if !canFastMarshalFlags(mo) {
		return dst, errFastFallback
	}
	if at == nil {
		return append(dst, "null"...), nil
	}
	plan := lookupFastMarshalPlan(at)
	if !plan.canFast {
		return dst, errFastFallback
	}
	origLen := len(dst)
	out, err := plan.marshal(dst, ptr, mo, 0)
	if err != nil {
		return dst[:origLen], err
	}
	return out, nil
}

// marshalFastAlloc marshals (plan, ptr) into a freshly allocated byte slice.
// Scalar values bypass fastBufPool using a stack scratch buffer or direct sizing.
func marshalFastAlloc(plan *fastMarshalPlan, ptr unsafe.Pointer, mo *jsonopts.Struct) ([]byte, error) {
	// Dereference leading pointers quickly (bounded to guard against cycles).
	ptrDepth := 0
	for plan.kind == fkPtr && ptrDepth < startDetectingCyclesAfter {
		p := *(*unsafe.Pointer)(ptr)
		if p == nil {
			return []byte("null"), nil
		}
		ptr = p
		plan = plan.elemPlan
		ptrDepth++
	}
	if ptrDepth >= startDetectingCyclesAfter {
		return nil, errFastFallback
	}
	switch plan.kind {
	case fkBool:
		if *(*bool)(ptr) {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case fkString:
		s := *(*string)(ptr)
		if !jsonwire.NeedEscape(unsafe.Slice(unsafe.StringData(s), len(s))) {
			out := make([]byte, len(s)+2)
			out[0] = '"'
			copy(out[1:], s)
			out[len(s)+1] = '"'
			return out, nil
		}
	case fkInt:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], int64(*(*int)(ptr)), 10)), nil
	case fkInt8:
		var scratch [8]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], int64(*(*int8)(ptr)), 10)), nil
	case fkInt16:
		var scratch [8]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], int64(*(*int16)(ptr)), 10)), nil
	case fkInt32:
		var scratch [16]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], int64(*(*int32)(ptr)), 10)), nil
	case fkInt64:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendInt(scratch[:0], *(*int64)(ptr), 10)), nil
	case fkUint:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uint)(ptr)), 10)), nil
	case fkUint8:
		var scratch [8]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uint8)(ptr)), 10)), nil
	case fkUint16:
		var scratch [8]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uint16)(ptr)), 10)), nil
	case fkUint32:
		var scratch [16]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uint32)(ptr)), 10)), nil
	case fkUint64:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], *(*uint64)(ptr), 10)), nil
	case fkUintptr:
		var scratch [32]byte
		return bytes.Clone(strconv.AppendUint(scratch[:0], uint64(*(*uintptr)(ptr)), 10)), nil
	case fkFloat32:
		fv := float64(*(*float32)(ptr))
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			err := error(newFastFloatErr(plan.typ, ptr, 0, fv, mo))
			if ptrDepth > 0 {
				err = wrapFastMarshalIndirect(err)
			}
			return nil, err
		}
		var scratch [32]byte
		return bytes.Clone(jsonwire.AppendFloat(scratch[:0], fv, 32)), nil
	case fkFloat64:
		fv := *(*float64)(ptr)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			err := error(newFastFloatErr(plan.typ, ptr, 0, fv, mo))
			if ptrDepth > 0 {
				err = wrapFastMarshalIndirect(err)
			}
			return nil, err
		}
		var scratch [32]byte
		return bytes.Clone(jsonwire.AppendFloat(scratch[:0], fv, 64)), nil
	case fkByteArray:
		if !mo.Flags.Get(jsonflags.FormatByteArrayAsArray) {
			s := unsafe.Slice((*byte)(ptr), plan.arrayLen)
			encLen := base64.StdEncoding.EncodedLen(len(s))
			out := make([]byte, encLen+2)
			out[0] = '"'
			base64.StdEncoding.Encode(out[1:], s)
			out[encLen+1] = '"'
			return out, nil
		}
	}
	if plan.isFlatSmall {
		est := min(max(plan.minSize+80, 128), 384)
		b, err := plan.marshal(make([]byte, 0, est), ptr, mo, 0)
		if err != nil {
			var partial []byte
			if err != errFastFallback && !mo.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
				if p := fastPartialOutput(b, err); len(p) > 0 {
					partial = bytes.Clone(p)
				}
			}
			if ptrDepth > 0 {
				err = wrapFastMarshalIndirect(err)
			}
			return partial, err
		}
		return b, nil
	}
	fb := getFastBuf()
	b, err := plan.marshal(fb.b, ptr, mo, 0)
	if err != nil {
		var partial []byte
		if err != errFastFallback && !mo.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
			if p := fastPartialOutput(b, err); len(p) > 0 {
				partial = bytes.Clone(p)
			}
		}
		if cap(b) > cap(fb.b) {
			fb.b = b[:0]
		} else {
			fb.b = fb.b[:0]
		}
		fastBufPool.Put(fb)
		if ptrDepth > 0 {
			err = wrapFastMarshalIndirect(err)
		}
		return partial, err
	}
	fb.b = b
	out := bytes.Clone(b)
	putFastBuf(fb)
	return out, nil
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
	hasCustom      bool
	hasCycle       bool
	building       bool
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
			switch p.kind {
			case fkPtr, fkSlice, fkArray:
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
			case fkMapStringElem, fkMapIntElem, fkMapUintElem:
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
			case fkStruct:
				for i := range p.fields {
					f := &p.fields[i]
					if f.plan == nil {
						continue
					}
					if !f.plan.canFast {
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
		if p.building {
			p.hasCycle = true
		}
		return p
	}
	p := &fastUnmarshalPlan{typ: at, rt: rt, building: true}
	visiting[at] = p
	defer func() { p.building = false }()

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
				p.hasCustom = true
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
		case fkArray:
			if rt == slicePoint2F64Type && ep.arrayLen == 2 && ep.elemPlan != nil && ep.elemPlan.kind == fkFloat64 {
				p.kind = fkSlicePoint2F64
			} else {
				p.kind = fkSlice
			}
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
			ep := buildFastUnmarshalPlan(elem, abiTypeOfReflect(elem), visiting)
			if ep.hasCustom {
				return p // map slots aren't addressable for user UnmarshalJSON
			}
			p.elemPlan = ep
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
	return uo.Unmarshalers == nil && !uo.Flags.Get(fastUnmarshalDisallowedFlags) && !uo.Flags.Has(jsonflags.ByteLimit|jsonflags.DepthLimit)
}

type fastDecoder struct {
	data          []byte
	pos           int
	flags         jsonflags.Flags
	strCache      *stringCache
	allowDup      bool
	allowBadUTF8  bool
	legacyMerge   bool
	caseInsens    bool
	caseSensDel   bool
	rejectUnk     bool
	legacyErr     bool
	isComposite   bool
	invokedCustom int
	firstErr      error
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
	if d.pos < len(d.data) && d.data[d.pos] <= ' ' {
		d.skipWSSlow()
	}
}

//go:noinline
func (d *fastDecoder) skipWSSlow() {
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

	case fkSlicePoint2F64:
		return d.unmarshalSlicePoint2F64(ptr)

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

func wrapFastUnmarshalPath(err error, seg string) error {
	if se, ok := err.(*fastSemanticError); ok {
		se.prepend(seg, false)
	}
	return err
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
	err := u.UnmarshalJSON(raw)
	d.invokedCustom++
	if err != nil {
		err = wrapErrUnsupported(err, "UnmarshalJSON method")
		if d.legacyErr && d.isComposite {
			if d.firstErr == nil {
				d.firstErr = internal.TransformUnmarshalError(nil, err)
			}
			return nil
		}
		return &fastSemanticError{
			action:    "unmarshal",
			goType:    p.typ,
			offset:    start,
			synOffset: start,
			err:       err,
			fn:        "UnmarshalJSON",
			collapse:  true,
			jsonKind:  jsontext.Value(raw).Kind(),
		}
	}
	return nil
}

func (d *fastDecoder) skipValueChecked(depth int) error {
	if d.allowDup {
		return d.skipValue(depth)
	}
	if depth >= 10000 {
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
		case len(b) < len("2006-01-02T15:04:05Z"):
			return errFastFallback
		case b[len("2006-01-02T")+1] == ':':
			return errFastFallback
		case b[len("2006-01-02T15:04:05")] == ',':
			return errFastFallback
		case b[len(b)-1] != 'Z':
			if len(b) < len("2006-01-02T15:04:05-07:00") || parseDec2(b[len(b)-len("07:00"):]) >= 24 || parseDec2(b[len(b)-len("00"):]) >= 60 {
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

func (d *fastDecoder) unmarshalSlicePoint2F64(ptr unsafe.Pointer) error {
	if d.legacyMerge && len(*(*[][2]float64)(ptr)) > 0 {
		return errFastFallback
	}
	c := d.data[d.pos]
	if c == 'n' {
		if d.consumeNull() {
			*(*[][2]float64)(ptr) = nil
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
			return errFastFallback
		}
		d.pos++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		v0, err := d.readFloat(64)
		if err != nil {
			return err
		}
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ',' {
			return errFastFallback
		}
		d.pos++
		d.skipWS()
		if d.pos >= len(d.data) {
			return errFastFallback
		}
		v1, err := d.readFloat(64)
		if err != nil {
			return err
		}
		d.skipWS()
		if d.pos >= len(d.data) || d.data[d.pos] != ']' {
			return errFastFallback
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
			if origLen <= sh.Cap && d.invokedCustom == 0 {
				sh.Len = origLen
			}
			return wrapFastUnmarshalPath(err, strconv.Itoa(i))
		}
		i++
		d.skipWS()
		if d.pos >= len(d.data) {
			if origLen <= sh.Cap && d.invokedCustom == 0 {
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
		if origLen <= sh.Cap && d.invokedCustom == 0 {
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
	anyLen := d.flags.Get(jsonflags.UnmarshalArrayFromAnyLength)
	if n == 0 {
		if d.pos < len(d.data) && d.data[d.pos] == ']' {
			d.pos++
			return nil
		}
		if anyLen {
			for {
				if err := d.skipValueChecked(depth); err != nil {
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
		}
		return errFastFallback
	}
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		if !anyLen {
			return errFastFallback
		}
		d.pos++
		reflect.NewAt(p.rt, ptr).Elem().SetZero()
		return nil
	}
	elemPlan := p.elemPlan
	if elemPlan.kind == fkFloat64 && n == 2 && d.pos < len(d.data) {
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
	mustZero := !d.legacyMerge && elemPlan.kind >= fkBytes
	for i := range n {
		if i > 0 {
			d.skipWS()
			if d.pos >= len(d.data) {
				return errFastFallback
			}
			if d.data[d.pos] == ']' {
				if !anyLen {
					return errFastFallback
				}
				d.pos++
				for j := i; j < n; j++ {
					clearBytes(unsafe.Add(ptr, uintptr(j)*elemSize), elemSize, p.elemRT)
				}
				return nil
			}
			if d.data[d.pos] != ',' {
				return errFastFallback
			}
			d.pos++
		}
		elemPtr := unsafe.Add(ptr, uintptr(i)*elemSize)
		if mustZero {
			clearBytes(elemPtr, elemSize, p.elemRT)
		}
		if err := d.unmarshal(elemPlan, elemPtr, depth); err != nil {
			return wrapFastUnmarshalPath(err, strconv.Itoa(i))
		}
	}
	d.skipWS()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		return nil
	}
	if anyLen && d.pos < len(d.data) && d.data[d.pos] == ',' {
		for {
			d.pos++
			if err := d.skipValueChecked(depth); err != nil {
				return err
			}
			d.skipWS()
			if d.pos >= len(d.data) {
				return errFastFallback
			}
			if d.data[d.pos] == ',' {
				continue
			}
			if d.data[d.pos] == ']' {
				d.pos++
				return nil
			}
			return errFastFallback
		}
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
		if _, exists := m[key]; exists && (!d.allowDup || !d.legacyMerge) {
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
			if _, exists := m[key]; exists && (!d.allowDup || !d.legacyMerge) {
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
	var unkKeys []string

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
				if len(unkKeys) >= 8 {
					return errFastFallback
				}
				ks := string(keyBytes)
				for _, uk := range unkKeys {
					if uk == ks {
						return errFastFallback
					}
				}
				unkKeys = append(unkKeys, ks)
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
						return wrapFastUnmarshalPath(err, f.name)
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

//go:noinline
func checkTrailingSyntaxErr(in []byte, pos int, uo *jsonopts.Struct) error {
	uoCopy := *uo
	dec := export.GetBufferedDecoder(in[pos:], &uoCopy)
	err := export.Decoder(dec).CheckEOF()
	export.PutBufferedDecoder(dec)
	if se, ok := err.(*jsontext.SyntacticError); ok {
		seCopy := *se
		seCopy.ByteOffset += int64(pos)
		return &seCopy
	}
	if err != nil {
		return err
	}
	return &jsontext.SyntacticError{
		ByteOffset: int64(pos),
		Err:        jsonwire.NewInvalidCharacterError(in[pos:], "after top-level value"),
	}
}

//go:noinline
func replayFastUnmarshalError(in []byte, rt reflect.Type, ptr unsafe.Pointer, uo *jsonopts.Struct, skipCustom int) error {
	var uoCopy jsonopts.Struct
	if uo != nil {
		uoCopy = *uo
	}
	rem := skipCustom
	uoCopy.Unmarshalers = UnmarshalFromFunc(func(dec *jsontext.Decoder, v Unmarshaler) error {
		if _, isTime := v.(*time.Time); isTime {
			return errors.ErrUnsupported
		}
		if rem > 0 {
			rem--
			_, err := dec.ReadValue()
			return err
		}
		return errors.ErrUnsupported
	})
	var dst any
	if uoCopy.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
		dst = reflect.NewAt(rt, ptr).Interface()
	} else {
		dst = reflect.New(rt).Interface()
	}
	dec := export.GetBufferedDecoder(in, &uoCopy)
	err := unmarshalDecode(dec, dst, &uoCopy, true)
	export.PutBufferedDecoder(dec)
	if err != nil && uoCopy.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics) {
		return internal.TransformUnmarshalError(dst, err)
	}
	return err
}

// tryFastUnmarshal attempts to unmarshal JSON bytes directly into (at, ptr).
// Returns (true, err) if handled (including materialized errors when custom
// Unmarshaler methods ran), or (false, nil) if the standard path should run cleanly.
// To guarantee zero corruption of pre-populated destination values on error,
// composite destinations are only fast-decoded when we can safely restore or
// when pre-validated (or empty/zero).
func tryFastUnmarshal(in []byte, at *abi.Type, ptr unsafe.Pointer, uo *jsonopts.Struct) (bool, error) {
	if !canFastUnmarshalFlags(uo) {
		return false, nil
	}
	plan := lookupFastUnmarshalPlan(at)
	if !plan.canFast {
		return false, nil
	}
	return tryFastUnmarshalCore(in, plan, ptr, uo, false)
}

func tryFastUnmarshalCore(in []byte, plan *fastUnmarshalPlan, ptr unsafe.Pointer, uo *jsonopts.Struct, validated bool) (bool, error) {
	// Fast scalar path: parse + check EOF BEFORE writing to ptr, so any error
	// leaves ptr completely untouched and falls back cleanly.
	switch plan.kind {
	case fkBool, fkString, fkInt, fkInt8, fkInt16, fkInt32, fkInt64,
		fkUint, fkUint8, fkUint16, fkUint32, fkUint64, fkUintptr,
		fkFloat32, fkFloat64:
		if tryFastUnmarshalScalar(in, plan, ptr, uo) {
			return true, nil
		}
		return false, nil
	}

	wasZero := isFastTargetZero(plan, ptr)
	legacyErr := uo.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics)
	if !validated && (!wasZero || (plan.hasCustom && legacyErr)) {
		// Pre-validate syntax for non-zero targets (and all legacy custom targets)
		// so syntax errors never mutate ptr or invoke UnmarshalJSON.
		if !fastCheckValid(in, uo.Flags.Get(jsonflags.AllowInvalidUTF8)) {
			return false, nil
		}
	}

	isComp := plan.kind != fkUnmarshaler
	for cur := plan; cur.kind == fkPtr; {
		cur = cur.elemPlan
		isComp = cur.kind != fkUnmarshaler
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
		legacyErr:    legacyErr,
		isComposite:  isComp,
	}
	err := d.unmarshal(plan, ptr, 0)
	if err == nil {
		d.skipWS()
		if d.pos == len(in) {
			fastStringCachePool.Put(sc)
			return true, d.firstErr
		}
		if d.invokedCustom > 0 {
			fastStringCachePool.Put(sc)
			if d.firstErr != nil {
				return true, d.firstErr
			}
			return true, checkTrailingSyntaxErr(in, d.pos, uo)
		}
	}
	fastStringCachePool.Put(sc)
	if d.invokedCustom > 0 {
		if err != nil && err != errFastFallback {
			outVal := reflect.Zero(reflect.PointerTo(plan.rt)).Interface()
			return true, materializeFastErr(err, outVal, nil, 0, uo)
		}
		if d.firstErr != nil {
			return true, d.firstErr
		}
		return true, replayFastUnmarshalError(in, plan.rt, ptr, uo, d.invokedCustom)
	}
	if err != nil && err != errFastFallback {
		outVal := reflect.Zero(reflect.PointerTo(plan.rt)).Interface()
		return true, materializeFastErr(err, outVal, nil, 0, uo)
	}
	if wasZero {
		// Restore zero state before delegating to standard unmarshalDecode!
		reflect.NewAt(plan.rt, ptr).Elem().SetZero()
	}
	return false, nil
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
	case fkBytes, fkSliceBool, fkSliceString, fkSliceInt, fkSliceInt64, fkSliceUint64, fkSliceFloat64, fkSliceFloat32, fkSlicePoint2F64, fkSlice:
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

func fastCheckValueEnd(data []byte, start int, atEOF bool, allowBadUTF8 bool) (int, bool) {
	d := fastDecoder{data: data, pos: start, allowDup: true, allowBadUTF8: allowBadUTF8}
	if err := d.skipValue(0); err != nil {
		return 0, false
	}
	c0 := data[start]
	if (c0 == '-' || ('0' <= c0 && c0 <= '9')) && d.pos == len(data) && !atEOF {
		return 0, false
	}
	return d.pos, true
}

func tryFastUnmarshalDecode(in *jsontext.Decoder, at *abi.Type, ptr unsafe.Pointer, uo *jsonopts.Struct, last bool) (bool, error) {
	if !canFastUnmarshalFlags(uo) {
		return false, nil
	}
	plan := lookupFastUnmarshalPlan(at)
	if !plan.canFast {
		return false, nil
	}
	xd := export.Decoder(in)
	buf, atEOF, ok := xd.ContiguousValue()
	if !ok {
		return false, nil
	}
	valStart := 0
	for valStart < len(buf) && (buf[valStart] == ' ' || buf[valStart] == '\t' || buf[valStart] == '\r' || buf[valStart] == '\n') {
		valStart++
	}
	if valStart >= len(buf) {
		return false, nil
	}

	allowBadUTF8 := uo.Flags.Get(jsonflags.AllowInvalidUTF8)
	legacyErr := uo.Flags.Get(jsonflags.ReportErrorsWithLegacySemantics)
	isScalar := plan.kind >= fkBool && plan.kind <= fkFloat64
	wasZero := isFastTargetZero(plan, ptr)

	if !isScalar && wasZero && !plan.hasCustom {
		sc := fastStringCachePool.Get()
		d := fastDecoder{
			data:         buf,
			pos:          valStart,
			flags:        uo.Flags,
			strCache:     sc,
			allowDup:     uo.Flags.Get(jsonflags.AllowDuplicateNames),
			allowBadUTF8: allowBadUTF8,
			legacyMerge:  uo.Flags.Get(jsonflags.MergeWithLegacySemantics),
			caseInsens:   uo.Flags.Get(jsonflags.MatchCaseInsensitiveNames),
			caseSensDel:  uo.Flags.Get(jsonflags.MatchCaseSensitiveDelimiter),
			rejectUnk:    uo.Flags.Get(jsonflags.RejectUnknownMembers),
			legacyErr:    legacyErr,
			isComposite:  true,
		}
		err := d.unmarshal(plan, ptr, 0)
		fastStringCachePool.Put(sc)
		if err == nil {
			valEnd := d.pos
			c0 := buf[valStart]
			if !((c0 == '-' || ('0' <= c0 && c0 <= '9')) && valEnd == len(buf) && !atEOF) {
				if last {
					p := valEnd
					for p < len(buf) && (buf[p] == ' ' || buf[p] == '\t' || buf[p] == '\r' || buf[p] == '\n') {
						p++
					}
					if p != len(buf) || !atEOF {
						reflect.NewAt(plan.rt, ptr).Elem().SetZero()
						return false, nil
					}
				}
				xd.ConsumeContiguous(valStart, valEnd)
				return true, nil
			}
		}
		reflect.NewAt(plan.rt, ptr).Elem().SetZero()
		return false, nil
	}

	valEnd, ok := fastCheckValueEnd(buf, valStart, atEOF, allowBadUTF8)
	if !ok {
		return false, nil
	}
	if last {
		p := valEnd
		for p < len(buf) && (buf[p] == ' ' || buf[p] == '\t' || buf[p] == '\r' || buf[p] == '\n') {
			p++
		}
		if p != len(buf) || !atEOF {
			return false, nil
		}
	}

	valSlice := buf[valStart:valEnd]
	handled, err := tryFastUnmarshalCore(valSlice, plan, ptr, uo, true)
	if !handled {
		return false, nil
	}
	if err != nil && !legacyErr && (in.InputOffset() != 0 || valStart != 0) {
		if !plan.hasCustom {
			if wasZero {
				reflect.NewAt(plan.rt, ptr).Elem().SetZero()
			}
			return false, nil
		}
		baseOff := in.InputOffset() + int64(valStart)
		switch e := err.(type) {
		case *SemanticError:
			e.ByteOffset += baseOff
		case *jsontext.SyntacticError:
			e.ByteOffset += baseOff
		}
	}
	xd.ConsumeContiguous(valStart, valEnd)
	return true, err
}

func init() {
	internal.FastUnmarshalDecode = func(decAny, optsAny any, at uintptr, ptr unsafe.Pointer) (bool, error) {
		in, ok := decAny.(*jsontext.Decoder)
		if !ok || in == nil {
			return false, nil
		}
		uo, ok := optsAny.(*jsonopts.Struct)
		if !ok || uo == nil {
			return false, nil
		}
		return tryFastUnmarshalDecode(in, (*abi.Type)(unsafe.Pointer(at)), ptr, uo, false)
	}
}
