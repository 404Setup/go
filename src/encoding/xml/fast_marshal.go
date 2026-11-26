// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xml

import (
	"bytes"
	"encoding"
	"errors"
	"fmt"
	"internal/abi"
	istrconv "internal/strconv"
	"reflect"
	"strconv"
	"strings"
	syncv2 "sync/v2"
	"time"
	"unsafe"
)

var errClosedEncoder = errors.New("use of closed Encoder")

type xmlEface struct {
	typ  *abi.Type
	data unsafe.Pointer
}

//go:nosplit
//go:noinline
func xmlNoescape(p unsafe.Pointer) unsafe.Pointer {
	x := uintptr(p)
	return unsafe.Pointer(x ^ 0)
}

func xmlEfacePtr(a *any) (*abi.Type, unsafe.Pointer) {
	e := (*xmlEface)(xmlNoescape(unsafe.Pointer(a)))
	if e.typ == nil {
		return nil, nil
	}
	if e.typ.IsDirectIface() {
		return e.typ, xmlNoescape(unsafe.Pointer(&e.data))
	}
	return e.typ, e.data
}

func xmlAbiTypeOfReflect(rt reflect.Type) *abi.Type {
	return (*xmlEface)(unsafe.Pointer(&rt)).typData()
}

func (e *xmlEface) typData() *abi.Type {
	return (*abi.Type)(e.data)
}

var xmlReflectTypeIfaceTab = func() unsafe.Pointer {
	rt := reflect.TypeFor[int]()
	return (*[2]unsafe.Pointer)(unsafe.Pointer(&rt))[0]
}()

func xmlAbiTypeToReflect(at *abi.Type) reflect.Type {
	var rt reflect.Type
	p := (*[2]unsafe.Pointer)(unsafe.Pointer(&rt))
	p[0] = xmlReflectTypeIfaceTab
	p[1] = unsafe.Pointer(at)
	return rt
}

type xmlSliceHeader struct {
	data unsafe.Pointer
	len  int
	cap  int
}

type xmlEncoderBuf struct {
	enc Encoder
}

var xmlEncoderPool = syncv2.Pool[*xmlEncoderBuf]{
	New: func() *xmlEncoderBuf {
		eb := &xmlEncoderBuf{}
		eb.enc.p.encoder = &eb.enc
		eb.enc.p.out = make([]byte, 0, 512)
		return eb
	},
}

func getXmlMarshalEncoder() *xmlEncoderBuf {
	eb := xmlEncoderPool.Get()
	p := &eb.enc.p
	p.out = p.out[:0]
	p.seq = 0
	p.indent = ""
	p.prefix = ""
	p.depth = 0
	p.indentedIn = false
	p.putNewline = false
	if len(p.attrNS) > 0 {
		clear(p.attrNS)
	}
	if len(p.attrPrefix) > 0 {
		clear(p.attrPrefix)
	}
	p.prefixes = p.prefixes[:0]
	p.tags = p.tags[:0]
	p.parentStack = p.parentStack[:0]
	p.closed = false
	p.err = nil
	return eb
}

func putXmlMarshalEncoder(eb *xmlEncoderBuf) {
	p := &eb.enc.p
	if cap(p.out) > 64<<10 {
		p.out = make([]byte, 0, 512)
	} else {
		p.out = p.out[:0]
	}
	if cap(p.tags) > 256 {
		p.tags = nil
	} else {
		p.tags = p.tags[:0]
	}
	if cap(p.prefixes) > 256 {
		p.prefixes = nil
	} else {
		p.prefixes = p.prefixes[:0]
	}
	if cap(p.parentStack) > 256 {
		p.parentStack = nil
	} else {
		p.parentStack = p.parentStack[:0]
	}
	xmlEncoderPool.Put(eb)
}

type xmlFastBuf struct {
	b []byte
}

var xmlFastBufPool = syncv2.Pool[*xmlFastBuf]{
	New: func() *xmlFastBuf {
		return &xmlFastBuf{b: make([]byte, 0, 512)}
	},
}

func getXmlFastBuf() *xmlFastBuf {
	fb := xmlFastBufPool.Get()
	fb.b = fb.b[:0]
	return fb
}

func putXmlFastBuf(fb *xmlFastBuf) {
	if cap(fb.b) > 64<<10 {
		fb.b = make([]byte, 0, 512)
	} else {
		fb.b = fb.b[:0]
	}
	xmlFastBufPool.Put(fb)
}

type xmlFastKind uint8

const (
	xkFallback xmlFastKind = iota
	xkBool
	xkString
	xkInt
	xkInt8
	xkInt16
	xkInt32
	xkInt64
	xkUint
	xkUint8
	xkUint16
	xkUint32
	xkUint64
	xkUintptr
	xkFloat32
	xkFloat64
	xkBytes
	xkByteArray
	xkTime
	xkName
	xkAttr
	xkTextMarshaler
	xkMarshaler
	xkPtr
	xkSlice
	xkArray
	xkStruct
	xkInterface
)

var (
	xmlTimeType            = reflect.TypeFor[time.Time]()
	xmlStringType          = reflect.TypeFor[string]()
	xmlBytesType           = reflect.TypeFor[[]byte]()
	xmlTimeAbiType         = xmlAbiTypeOfReflect(xmlTimeType)
	xmlNameAbiType         = xmlAbiTypeOfReflect(nameType)
	xmlAttrAbiType         = xmlAbiTypeOfReflect(attrType)
	xmlMarshalerType       = reflect.TypeFor[Marshaler]()
	xmlMarshalerAttrType   = reflect.TypeFor[MarshalerAttr]()
	xmlTextMarshalerType   = reflect.TypeFor[encoding.TextMarshaler]()
	xmlTextUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
	xmlUnmarshalerType     = reflect.TypeFor[Unmarshaler]()
	xmlUnmarshalerAttrType = reflect.TypeFor[UnmarshalerAttr]()
)

type xmlFieldStep struct {
	offset uintptr
	deref  bool
}

type xmlNameMode uint8

const (
	xmlNameNone xmlNameMode = iota
	xmlNameFixed
	xmlNameDynamic
)

type xmlFastAttr struct {
	name             Name
	escapedPrefix    string // ` local="` when Space == ""
	omitEmpty        bool
	steps            []xmlFieldStep
	offset           uintptr
	kind             xmlFastKind
	rt               reflect.Type
	ptrRT            reflect.Type
	isMarshalerAttr  bool
	ptrMarshalerAttr bool
	isTextMarshaler  bool
	ptrTextMarshaler bool
	isTime           bool
	isSlice          bool
	isPtr            bool
	elemSize         uintptr
	elemKind         xmlFastKind
	elemRT           reflect.Type
	elemPtrRT        reflect.Type
	elemIsTime       bool
	elemTextMarsh    bool
	elemPtrTextMarsh bool
	elemMarshAttr    bool
	elemPtrMarshAttr bool
}

type xmlFastElem struct {
	name             Name
	mode             fieldFlags
	omitEmpty        bool
	parents          []string
	openTag          string // "<local>" or "<local xmlns=...>" when known and static
	closeTag         string // "</local>"
	steps            []xmlFieldStep
	offset           uintptr
	kind             xmlFastKind
	rt               reflect.Type
	ptrRT            reflect.Type
	plan             *xmlMarshalPlan
	elemKind         xmlFastKind
	elemSize         uintptr
	elemRT           reflect.Type
	elemPlan         *xmlMarshalPlan
	isInterface      bool
	isMarshaler      bool
	ptrMarshaler     bool
	isTextMarshaler  bool
	ptrTextMarshaler bool
	isScalarLeaf     bool
	isScalarSlice    bool
	isStructSlice    bool
}

type xmlMarshalPlan struct {
	typ              *abi.Type
	rt               reflect.Type
	ptrRT            reflect.Type
	kind             xmlFastKind
	canFast          bool
	hasCustom        bool
	hasCycle         bool
	building         bool
	buildErr         error
	isMarshaler      bool
	ptrMarshaler     bool
	isTextMarshaler  bool
	ptrTextMarshaler bool
	isNonEmptyIface  bool
	elemSize         uintptr
	arrayLen         int
	elemPlan         *xmlMarshalPlan
	defaultLocal     string
	xmlNameMode      xmlNameMode
	xmlNameSpace     string
	xmlNameLocal     string
	xmlNameEmptyTag  bool
	xmlNameSteps     []xmlFieldStep
	xmlNameOffset    uintptr
	xmlNameIsName    bool
	attrs            []xmlFastAttr
	elems            []xmlFastElem
	hasAttrSpace     bool
	hasParents       bool
}

var xmlMarshalPlanCache syncv2.Map[*abi.Type, *xmlMarshalPlan]

func lookupXmlMarshalPlan(at *abi.Type) *xmlMarshalPlan {
	if p, ok := xmlMarshalPlanCache.Load(at); ok {
		return p
	}
	visiting := make(map[*abi.Type]*xmlMarshalPlan)
	p := buildXmlMarshalPlan(xmlAbiTypeToReflect(at), at, visiting)
	finalizeXmlMarshalPlans(visiting)
	for kt, kp := range visiting {
		xmlMarshalPlanCache.LoadOrStore(kt, kp)
	}
	return p
}

func finalizeXmlMarshalPlans(visiting map[*abi.Type]*xmlMarshalPlan) {
	for changed := true; changed; {
		changed = false
		for _, p := range visiting {
			switch p.kind {
			case xkPtr, xkSlice, xkArray:
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
			case xkStruct:
				for i := range p.elems {
					e := &p.elems[i]
					if e.plan != nil {
						if !e.plan.canFast && p.canFast {
							p.canFast = false
							changed = true
						}
						if e.plan.hasCustom && !p.hasCustom {
							p.hasCustom = true
							changed = true
						}
						if e.plan.hasCycle && !p.hasCycle {
							p.hasCycle = true
							changed = true
						}
					}
				}
			}
		}
	}
	for _, p := range visiting {
		p.building = false
	}
}

func classifyXmlKind(rt reflect.Type, at *abi.Type) xmlFastKind {
	if at == xmlTimeAbiType {
		return xkTime
	}
	if at == xmlNameAbiType {
		return xkName
	}
	if at == xmlAttrAbiType {
		return xkAttr
	}
	switch rt.Kind() {
	case reflect.Bool:
		return xkBool
	case reflect.String:
		return xkString
	case reflect.Int:
		return xkInt
	case reflect.Int8:
		return xkInt8
	case reflect.Int16:
		return xkInt16
	case reflect.Int32:
		return xkInt32
	case reflect.Int64:
		return xkInt64
	case reflect.Uint:
		return xkUint
	case reflect.Uint8:
		return xkUint8
	case reflect.Uint16:
		return xkUint16
	case reflect.Uint32:
		return xkUint32
	case reflect.Uint64:
		return xkUint64
	case reflect.Uintptr:
		return xkUintptr
	case reflect.Float32:
		return xkFloat32
	case reflect.Float64:
		return xkFloat64
	case reflect.Slice:
		if rt.Elem().Kind() == reflect.Uint8 {
			return xkBytes
		}
		return xkSlice
	case reflect.Array:
		if rt.Elem().Kind() == reflect.Uint8 {
			return xkByteArray
		}
		return xkArray
	case reflect.Pointer:
		return xkPtr
	case reflect.Struct:
		return xkStruct
	case reflect.Interface:
		return xkInterface
	default:
		return xkFallback
	}
}

func resolveStructFieldSteps(root reflect.Type, idx []int) (steps []xmlFieldStep, offset uintptr) {
	t := root
	var cur uintptr
	for i, fieldIdx := range idx {
		if i > 0 && t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Struct {
			steps = append(steps, xmlFieldStep{offset: cur, deref: true})
			t = t.Elem()
			cur = 0
		}
		sf := t.Field(fieldIdx)
		cur += sf.Offset
		t = sf.Type
	}
	return steps, cur
}

func buildXmlMarshalPlan(rt reflect.Type, at *abi.Type, visiting map[*abi.Type]*xmlMarshalPlan) *xmlMarshalPlan {
	if p, ok := visiting[at]; ok {
		if p.building {
			p.hasCycle = true
		}
		return p
	}
	if cached, ok := xmlMarshalPlanCache.Load(at); ok {
		return cached
	}

	ptrRT := reflect.PointerTo(rt)
	name := rt.Name()
	if i := strings.IndexByte(name, '['); i >= 0 {
		name = name[:i]
	}

	p := &xmlMarshalPlan{
		typ:              at,
		rt:               rt,
		ptrRT:            ptrRT,
		building:         true,
		defaultLocal:     name,
		isMarshaler:      rt.Implements(xmlMarshalerType),
		ptrMarshaler:     ptrRT.Implements(xmlMarshalerType),
		isTextMarshaler:  rt.Implements(xmlTextMarshalerType),
		ptrTextMarshaler: ptrRT.Implements(xmlTextMarshalerType),
	}
	visiting[at] = p

	k := classifyXmlKind(rt, at)
	p.kind = k

	if p.isMarshaler || p.ptrMarshaler {
		p.hasCustom = true
		if p.isMarshaler && k != xkPtr && k != xkInterface && k != xkSlice && k != xkArray {
			p.canFast = true
			return p
		}
	}

	switch k {
	case xkBool, xkString,
		xkInt, xkInt8, xkInt16, xkInt32, xkInt64,
		xkUint, xkUint8, xkUint16, xkUint32, xkUint64, xkUintptr,
		xkFloat32, xkFloat64,
		xkBytes, xkByteArray, xkTime, xkName:
		p.canFast = true
		return p

	case xkPtr:
		elemRT := rt.Elem()
		p.elemPlan = buildXmlMarshalPlan(elemRT, xmlAbiTypeOfReflect(elemRT), visiting)
		p.canFast = p.elemPlan.canFast
		return p

	case xkSlice:
		elemRT := rt.Elem()
		p.elemSize = elemRT.Size()
		p.elemPlan = buildXmlMarshalPlan(elemRT, xmlAbiTypeOfReflect(elemRT), visiting)
		p.canFast = p.elemPlan.canFast
		return p

	case xkArray:
		elemRT := rt.Elem()
		p.elemSize = elemRT.Size()
		p.arrayLen = rt.Len()
		p.elemPlan = buildXmlMarshalPlan(elemRT, xmlAbiTypeOfReflect(elemRT), visiting)
		p.canFast = p.elemPlan.canFast
		return p

	case xkInterface:
		p.isNonEmptyIface = rt.NumMethod() > 0
		p.hasCustom = true
		p.canFast = true
		return p

	case xkStruct:
		tinfo, err := getTypeInfo(rt)
		if err != nil {
			p.buildErr = err
			p.canFast = true
			return p
		}
		if tinfo.xmlname != nil {
			xn := tinfo.xmlname
			if xn.name != "" {
				p.xmlNameMode = xmlNameFixed
				p.xmlNameSpace = xn.xmlns
				p.xmlNameLocal = xn.name
			} else {
				p.xmlNameMode = xmlNameDynamic
				p.xmlNameEmptyTag = xn.xmlns == "" && xn.name == ""
				steps, off := resolveStructFieldSteps(rt, xn.idx)
				p.xmlNameSteps = steps
				p.xmlNameOffset = off
				p.xmlNameIsName = rt.FieldByIndex(xn.idx).Type == nameType
			}
		}

		for i := range tinfo.fields {
			fi := &tinfo.fields[i]
			sf := rt.FieldByIndex(fi.idx)
			frt := sf.Type
			fat := xmlAbiTypeOfReflect(frt)
			steps, off := resolveStructFieldSteps(rt, fi.idx)

			if fi.flags&fAttr != 0 {
				fa := buildXmlFastAttr(fi, frt, fat, steps, off)
				if fa.name.Space != "" || fa.isMarshalerAttr || fa.ptrMarshalerAttr || fa.kind == xkAttr || (fa.isPtr && (fa.elemKind == xkAttr || fa.elemKind == xkInterface)) || fa.kind == xkInterface || fa.isSlice {
					p.hasAttrSpace = true
				}
				if fa.kind == xkInterface {
					p.hasCustom = true
				}
				p.attrs = append(p.attrs, fa)
				continue
			}

			fe := buildXmlFastElem(fi, frt, fat, steps, off, visiting)
			if len(fe.parents) > 0 {
				p.hasParents = true
			}
			if fe.isInterface || fe.isMarshaler || fe.ptrMarshaler || (fe.plan != nil && fe.plan.hasCustom) {
				p.hasCustom = true
			}
			p.elems = append(p.elems, fe)
		}
		p.canFast = true
		return p

	default:
		p.canFast = false
		return p
	}
}

func buildXmlFastAttr(fi *fieldInfo, frt reflect.Type, fat *abi.Type, steps []xmlFieldStep, off uintptr) xmlFastAttr {
	ptrFRT := reflect.PointerTo(frt)
	fa := xmlFastAttr{
		name:             Name{Space: fi.xmlns, Local: fi.name},
		omitEmpty:        fi.flags&fOmitEmpty != 0,
		steps:            steps,
		offset:           off,
		kind:             classifyXmlKind(frt, fat),
		rt:               frt,
		ptrRT:            ptrFRT,
		isMarshalerAttr:  frt.Implements(xmlMarshalerAttrType),
		ptrMarshalerAttr: ptrFRT.Implements(xmlMarshalerAttrType),
		isTextMarshaler:  frt.Implements(xmlTextMarshalerType),
		ptrTextMarshaler: ptrFRT.Implements(xmlTextMarshalerType),
		isTime:           fat == xmlTimeAbiType,
	}
	if fi.xmlns == "" {
		fa.escapedPrefix = " " + fi.name + `="`
	}
	if fa.kind == xkSlice {
		fa.isSlice = true
		ert := frt.Elem()
		eat := xmlAbiTypeOfReflect(ert)
		ptrERT := reflect.PointerTo(ert)
		fa.elemSize = ert.Size()
		fa.elemRT = ert
		fa.elemPtrRT = ptrERT
		fa.elemKind = classifyXmlKind(ert, eat)
		fa.elemIsTime = eat == xmlTimeAbiType
		fa.elemMarshAttr = ert.Implements(xmlMarshalerAttrType)
		fa.elemPtrMarshAttr = ptrERT.Implements(xmlMarshalerAttrType)
		fa.elemTextMarsh = ert.Implements(xmlTextMarshalerType)
		fa.elemPtrTextMarsh = ptrERT.Implements(xmlTextMarshalerType)
	} else if fa.kind == xkPtr {
		fa.isPtr = true
		ert := frt.Elem()
		eat := xmlAbiTypeOfReflect(ert)
		fa.elemRT = ert
		fa.elemKind = classifyXmlKind(ert, eat)
		fa.elemIsTime = eat == xmlTimeAbiType
	}
	return fa
}

func isScalarMarshalKind(k xmlFastKind) bool {
	switch k {
	case xkBool, xkString,
		xkInt, xkInt8, xkInt16, xkInt32, xkInt64,
		xkUint, xkUint8, xkUint16, xkUint32, xkUint64, xkUintptr,
		xkFloat32, xkFloat64, xkBytes:
		return true
	default:
		return false
	}
}

func buildXmlFastElem(fi *fieldInfo, frt reflect.Type, fat *abi.Type, steps []xmlFieldStep, off uintptr, visiting map[*abi.Type]*xmlMarshalPlan) xmlFastElem {
	ptrFRT := reflect.PointerTo(frt)
	fe := xmlFastElem{
		name:             Name{Space: fi.xmlns, Local: fi.name},
		mode:             fi.flags & fMode,
		omitEmpty:        fi.flags&fOmitEmpty != 0,
		parents:          fi.parents,
		steps:            steps,
		offset:           off,
		kind:             classifyXmlKind(frt, fat),
		rt:               frt,
		ptrRT:            ptrFRT,
		isInterface:      frt.Kind() == reflect.Interface,
		isMarshaler:      frt.Implements(xmlMarshalerType),
		ptrMarshaler:     ptrFRT.Implements(xmlMarshalerType),
		isTextMarshaler:  frt.Implements(xmlTextMarshalerType),
		ptrTextMarshaler: ptrFRT.Implements(xmlTextMarshalerType),
	}
	if fe.mode == fElement {
		if fi.xmlns == "" {
			fe.openTag = "<" + fi.name + ">"
		} else {
			var b []byte
			b = append(b, '<')
			b = append(b, fi.name...)
			b = append(b, ` xmlns="`...)
			b = appendEscapeXMLString(b, fi.xmlns, true)
			b = append(b, `">`...)
			fe.openTag = string(b)
		}
		fe.closeTag = "</" + fi.name + ">"
	}
	if !fe.isMarshaler && !fe.ptrMarshaler && !fe.isTextMarshaler && !fe.ptrTextMarshaler && !fe.isInterface {
		if isScalarMarshalKind(fe.kind) {
			fe.isScalarLeaf = true
		}
	}
	if fe.mode != fElement || !fe.isScalarLeaf {
		if fe.mode&(fElement|fAny|fInnerXML) != 0 {
			fe.plan = buildXmlMarshalPlan(frt, fat, visiting)
		}
	}
	if fe.mode == fElement && fe.kind == xkSlice && !fe.isMarshaler && !fe.ptrMarshaler && fe.plan != nil {
		ert := frt.Elem()
		eat := xmlAbiTypeOfReflect(ert)
		ek := classifyXmlKind(ert, eat)
		ptrERT := reflect.PointerTo(ert)
		if !ert.Implements(xmlMarshalerType) && !ptrERT.Implements(xmlMarshalerType) {
			if eat == xmlTimeAbiType {
				fe.isScalarSlice = true
				fe.elemKind = xkTime
				fe.elemSize = ert.Size()
				fe.elemRT = ert
			} else if !ert.Implements(xmlTextMarshalerType) && !ptrERT.Implements(xmlTextMarshalerType) && isScalarMarshalKind(ek) {
				fe.isScalarSlice = true
				fe.elemKind = ek
				fe.elemSize = ert.Size()
				fe.elemRT = ert
			} else if ep := fe.plan.elemPlan; ep != nil && ep.kind == xkStruct && ep.canFast && !ep.building && !ep.isTextMarshaler && !ep.ptrTextMarshaler && ep.xmlNameMode == xmlNameNone && fe.name.Local != "" {
				fe.isStructSlice = true
				fe.elemPlan = ep
				fe.elemSize = fe.plan.elemSize
			}
		}
	}
	return fe
}

func resolveFieldPtr(base unsafe.Pointer, steps []xmlFieldStep, finalOff uintptr) unsafe.Pointer {
	p := base
	for _, s := range steps {
		p = unsafe.Add(p, s.offset)
		if s.deref {
			p = *(*unsafe.Pointer)(p)
			if p == nil {
				return nil
			}
		}
	}
	return unsafe.Add(p, finalOff)
}

func resolveFieldPtrWithAddr(base unsafe.Pointer, steps []xmlFieldStep, finalOff uintptr, canAddr bool) (unsafe.Pointer, bool) {
	p := base
	addr := canAddr
	for _, s := range steps {
		p = unsafe.Add(p, s.offset)
		if s.deref {
			p = *(*unsafe.Pointer)(p)
			if p == nil {
				return nil, false
			}
			addr = true
		}
	}
	return unsafe.Add(p, finalOff), addr
}

func xmlIsEmpty(k xmlFastKind, ptr unsafe.Pointer, rt reflect.Type) bool {
	switch k {
	case xkBool:
		return !*(*bool)(ptr)
	case xkString:
		return len(*(*string)(ptr)) == 0
	case xkInt:
		return *(*int)(ptr) == 0
	case xkInt8:
		return *(*int8)(ptr) == 0
	case xkInt16:
		return *(*int16)(ptr) == 0
	case xkInt32:
		return *(*int32)(ptr) == 0
	case xkInt64:
		return *(*int64)(ptr) == 0
	case xkUint:
		return *(*uint)(ptr) == 0
	case xkUint8:
		return *(*uint8)(ptr) == 0
	case xkUint16:
		return *(*uint16)(ptr) == 0
	case xkUint32:
		return *(*uint32)(ptr) == 0
	case xkUint64:
		return *(*uint64)(ptr) == 0
	case xkUintptr:
		return *(*uintptr)(ptr) == 0
	case xkFloat32:
		return *(*float32)(ptr) == 0
	case xkFloat64:
		return *(*float64)(ptr) == 0
	case xkBytes, xkSlice:
		return (*xmlSliceHeader)(ptr).len == 0
	case xkByteArray, xkArray:
		return rt.Len() == 0
	case xkPtr:
		return *(*unsafe.Pointer)(ptr) == nil
	case xkInterface:
		return (*xmlEface)(ptr).typ == nil
	default:
		return false
	}
}

func appendSimpleXML(out []byte, k xmlFastKind, ptr unsafe.Pointer, rt reflect.Type) ([]byte, error) {
	switch k {
	case xkString:
		return appendEscapeXMLString(out, *(*string)(ptr), true), nil
	case xkInt:
		return istrconv.AppendInt10(out, int64(*(*int)(ptr))), nil
	case xkInt8:
		return istrconv.AppendInt10(out, int64(*(*int8)(ptr))), nil
	case xkInt16:
		return istrconv.AppendInt10(out, int64(*(*int16)(ptr))), nil
	case xkInt32:
		return istrconv.AppendInt10(out, int64(*(*int32)(ptr))), nil
	case xkInt64:
		return istrconv.AppendInt10(out, *(*int64)(ptr)), nil
	case xkUint:
		return istrconv.AppendUint10(out, uint64(*(*uint)(ptr))), nil
	case xkUint8:
		return istrconv.AppendUint10(out, uint64(*(*uint8)(ptr))), nil
	case xkUint16:
		return istrconv.AppendUint10(out, uint64(*(*uint16)(ptr))), nil
	case xkUint32:
		return istrconv.AppendUint10(out, uint64(*(*uint32)(ptr))), nil
	case xkUint64:
		return istrconv.AppendUint10(out, *(*uint64)(ptr)), nil
	case xkUintptr:
		return istrconv.AppendUint10(out, uint64(*(*uintptr)(ptr))), nil
	case xkFloat32:
		return istrconv.AppendFloat(out, float64(*(*float32)(ptr)), 'g', -1, 32), nil
	case xkFloat64:
		return istrconv.AppendFloat(out, *(*float64)(ptr), 'g', -1, 64), nil
	case xkBool:
		if *(*bool)(ptr) {
			return append(out, "true"...), nil
		}
		return append(out, "false"...), nil
	case xkBytes:
		return appendEscapeXMLBytes(out, *(*[]byte)(ptr), true), nil
	case xkByteArray:
		b := unsafe.Slice((*byte)(ptr), rt.Len())
		return appendEscapeXMLBytes(out, b, true), nil
	default:
		return out, &UnsupportedTypeError{rt}
	}
}

func appendSimpleAttrValue(out []byte, k xmlFastKind, ptr unsafe.Pointer, rt reflect.Type) ([]byte, error) {
	return appendSimpleXML(out, k, ptr, rt)
}

func (p *printer) fastMarshalRoot(plan *xmlMarshalPlan, ptr unsafe.Pointer, canAddr bool) error {
	return p.fastMarshalValue(plan, ptr, canAddr, nil, 0)
}

func (p *printer) fastMarshalValue(plan *xmlMarshalPlan, ptr unsafe.Pointer, canAddr bool, fe *xmlFastElem, depth int) error {
	if depth > 1000 {
		return errors.New("xml: nested too deeply")
	}

	for {
		switch plan.kind {
		case xkPtr:
			ptr = *(*unsafe.Pointer)(ptr)
			if ptr == nil {
				return nil
			}
			plan = plan.elemPlan
			canAddr = true
			depth++
			if depth > 1000 {
				return errors.New("xml: nested too deeply")
			}
			continue

		case xkInterface:
			var elemTyp *abi.Type
			var elemData unsafe.Pointer
			if plan.isNonEmptyIface {
				ni := (*abi.NonEmptyInterface)(ptr)
				if ni.ITab == nil {
					return nil
				}
				elemTyp = ni.ITab.Type
				elemData = ni.Data
			} else {
				e := (*xmlEface)(ptr)
				if e.typ == nil {
					return nil
				}
				elemTyp = e.typ
				elemData = e.data
			}
			if elemTyp.IsDirectIface() {
				ptr = unsafe.Pointer(&elemData)
				canAddr = false
			} else {
				ptr = elemData
				canAddr = false
			}
			plan = lookupXmlMarshalPlan(elemTyp)
			depth++
			if depth > 1000 {
				return errors.New("xml: nested too deeply")
			}
			continue
		}
		break
	}

	if plan.buildErr != nil {
		return plan.buildErr
	}
	if !plan.canFast {
		return &UnsupportedTypeError{plan.rt}
	}

	// Check Marshaler.
	if plan.isMarshaler {
		v := reflect.NewAt(plan.rt, ptr).Elem().Interface().(Marshaler)
		return p.marshalInterface(v, fastDefaultStart(plan.rt, fe))
	}
	if canAddr && plan.ptrMarshaler {
		v := reflect.NewAt(plan.rt, ptr).Interface().(Marshaler)
		return p.marshalInterface(v, fastDefaultStart(plan.rt, fe))
	}

	// Check TextMarshaler.
	if plan.kind == xkTime {
		var start StartElement
		if fe != nil && fe.name.Local != "" {
			start.Name = fe.name
		} else {
			start.Name.Local = plan.defaultLocal
		}
		if err := p.writeStartFast(&start, false); err != nil {
			return err
		}
		var err error
		p.out, err = (*time.Time)(ptr).AppendText(p.out)
		if err != nil {
			return err
		}
		return p.writeEndFast(start.Name, false)
	}
	if plan.isTextMarshaler {
		v := reflect.NewAt(plan.rt, ptr).Elem().Interface().(encoding.TextMarshaler)
		return p.marshalTextInterface(v, fastDefaultStart(plan.rt, fe))
	}
	if canAddr && plan.ptrTextMarshaler {
		v := reflect.NewAt(plan.rt, ptr).Interface().(encoding.TextMarshaler)
		return p.marshalTextInterface(v, fastDefaultStart(plan.rt, fe))
	}

	// Slices and arrays (non-uint8).
	if plan.kind == xkSlice {
		sh := (*xmlSliceHeader)(ptr)
		for i := 0; i < sh.len; i++ {
			ep := unsafe.Add(sh.data, uintptr(i)*plan.elemSize)
			if err := p.fastMarshalValue(plan.elemPlan, ep, true, fe, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if plan.kind == xkArray {
		for i := 0; i < plan.arrayLen; i++ {
			ep := unsafe.Add(ptr, uintptr(i)*plan.elemSize)
			if err := p.fastMarshalValue(plan.elemPlan, ep, canAddr, fe, depth+1); err != nil {
				return err
			}
		}
		return nil
	}

	// Determine start tag name.
	var startName Name
	if plan.kind == xkStruct {
		switch plan.xmlNameMode {
		case xmlNameFixed:
			startName.Space = plan.xmlNameSpace
			startName.Local = plan.xmlNameLocal
		case xmlNameDynamic:
			xptr := resolveFieldPtr(ptr, plan.xmlNameSteps, plan.xmlNameOffset)
			if xptr != nil && plan.xmlNameIsName {
				v := *(*Name)(xptr)
				if v.Local != "" {
					startName = v
				}
			}
		}
	}
	if startName.Local == "" && fe != nil {
		startName = fe.name
	}
	if startName.Local == "" {
		if plan.defaultLocal == "" {
			return &UnsupportedTypeError{plan.rt}
		}
		startName.Local = plan.defaultLocal
	}

	if plan.kind == xkName {
		if err := p.writeStartNameFast(startName, false); err != nil {
			return err
		}
		return p.writeEndFast(startName, false)
	}

	if plan.kind != xkStruct {
		if err := p.writeStartNameFast(startName, false); err != nil {
			return err
		}
		var err error
		p.out, err = appendSimpleXML(p.out, plan.kind, ptr, plan.rt)
		if err != nil {
			return err
		}
		return p.writeEndFast(startName, false)
	}

	// Struct marshaling.
	if err := p.writeStructStartFast(plan, ptr, canAddr, startName); err != nil {
		return err
	}

	if err := p.fastMarshalStructBody(plan, ptr, canAddr, depth); err != nil {
		return err
	}

	return p.writeEndFast(startName, plan.hasAttrSpace)
}

func fastDefaultStart(rt reflect.Type, fe *xmlFastElem) StartElement {
	var start StartElement
	if fe != nil && fe.name.Local != "" {
		start.Name = fe.name
	} else if rt.Name() != "" {
		start.Name.Local = rt.Name()
	} else if rt.Kind() == reflect.Pointer {
		start.Name.Local = rt.Elem().Name()
	}
	return start
}

func (p *printer) writeStartFast(start *StartElement, withPrefixes bool) error {
	if start.Name.Local == "" {
		return fmt.Errorf("xml: start tag with no name")
	}
	p.tags = append(p.tags, start.Name)
	if withPrefixes {
		p.markPrefix()
	}
	if len(p.prefix) != 0 || len(p.indent) != 0 {
		p.writeIndent(1)
	}
	p.out = append(p.out, '<')
	p.out = append(p.out, start.Name.Local...)
	if start.Name.Space != "" {
		p.out = append(p.out, ` xmlns="`...)
		p.out = appendEscapeXMLString(p.out, start.Name.Space, true)
		p.out = append(p.out, '"')
	}
	for _, attr := range start.Attr {
		name := attr.Name
		if name.Local == "" {
			continue
		}
		p.out = append(p.out, ' ')
		if name.Space != "" {
			prefix := p.createAttrPrefix(name.Space)
			p.out = append(p.out, prefix...)
			p.out = append(p.out, ':')
		}
		p.out = append(p.out, name.Local...)
		p.out = append(p.out, `="`...)
		p.out = appendEscapeXMLString(p.out, attr.Value, true)
		p.out = append(p.out, '"')
	}
	p.out = append(p.out, '>')
	return nil
}

func (p *printer) writeStartNameFast(name Name, withPrefixes bool) error {
	if name.Local == "" {
		return fmt.Errorf("xml: start tag with no name")
	}
	p.tags = append(p.tags, name)
	if withPrefixes {
		p.markPrefix()
	}
	if len(p.prefix) != 0 || len(p.indent) != 0 {
		p.writeIndent(1)
	}
	p.out = append(p.out, '<')
	p.out = append(p.out, name.Local...)
	if name.Space != "" {
		p.out = append(p.out, ` xmlns="`...)
		p.out = appendEscapeXMLString(p.out, name.Space, true)
		p.out = append(p.out, '"')
	}
	p.out = append(p.out, '>')
	return nil
}

func (p *printer) writeEndFast(name Name, withPrefixes bool) error {
	if name.Local == "" {
		return fmt.Errorf("xml: end tag with no name")
	}
	if len(p.tags) == 0 || p.tags[len(p.tags)-1].Local == "" {
		return fmt.Errorf("xml: end tag </%s> without start tag", name.Local)
	}
	if top := p.tags[len(p.tags)-1]; top != name {
		if top.Local != name.Local {
			return fmt.Errorf("xml: end tag </%s> does not match start tag <%s>", name.Local, top.Local)
		}
		return fmt.Errorf("xml: end tag </%s> in namespace %s does not match start tag <%s> in namespace %s", name.Local, name.Space, top.Local, top.Space)
	}
	p.tags = p.tags[:len(p.tags)-1]

	if len(p.prefix) != 0 || len(p.indent) != 0 {
		p.writeIndent(-1)
	}
	p.out = append(p.out, '<', '/')
	p.out = append(p.out, name.Local...)
	p.out = append(p.out, '>')
	if withPrefixes {
		p.popPrefix()
	}
	return nil
}

func (p *printer) writeStructStartFast(plan *xmlMarshalPlan, ptr unsafe.Pointer, canAddr bool, startName Name) error {
	if startName.Local == "" {
		return fmt.Errorf("xml: start tag with no name")
	}
	overrideEmptyNS := plan.xmlNameMode == xmlNameDynamic &&
		startName.Space == "" &&
		plan.xmlNameEmptyTag &&
		len(p.tags) != 0 &&
		p.tags[len(p.tags)-1].Space != ""

	p.tags = append(p.tags, startName)
	if plan.hasAttrSpace {
		p.markPrefix()
	}
	if len(p.prefix) != 0 || len(p.indent) != 0 {
		p.writeIndent(1)
	}
	p.out = append(p.out, '<')
	p.out = append(p.out, startName.Local...)
	if startName.Space != "" {
		p.out = append(p.out, ` xmlns="`...)
		p.out = appendEscapeXMLString(p.out, startName.Space, true)
		p.out = append(p.out, '"')
	}

	for i := range plan.attrs {
		fa := &plan.attrs[i]
		fptr, fAddr := resolveFieldPtrWithAddr(ptr, fa.steps, fa.offset, canAddr)
		if fptr == nil {
			continue
		}
		if fa.omitEmpty && xmlIsEmpty(fa.kind, fptr, fa.rt) {
			continue
		}
		if fa.kind == xkInterface && (*xmlEface)(fptr).typ == nil {
			continue
		}
		if err := p.fastWriteAttr(fa, fptr, fAddr); err != nil {
			return err
		}
	}

	if overrideEmptyNS {
		p.out = append(p.out, ` xmlns=""`...)
	}
	p.out = append(p.out, '>')
	return nil
}

func (p *printer) fastWriteAttr(fa *xmlFastAttr, fptr unsafe.Pointer, canAddr bool) error {
	if fa.isMarshalerAttr {
		v := reflect.NewAt(fa.rt, fptr).Elem().Interface().(MarshalerAttr)
		attr, err := v.MarshalXMLAttr(fa.name)
		if err != nil {
			return err
		}
		if attr.Name.Local != "" {
			p.appendAttr(attr)
		}
		return nil
	}
	if canAddr && fa.ptrMarshalerAttr {
		v := reflect.NewAt(fa.rt, fptr).Interface().(MarshalerAttr)
		attr, err := v.MarshalXMLAttr(fa.name)
		if err != nil {
			return err
		}
		if attr.Name.Local != "" {
			p.appendAttr(attr)
		}
		return nil
	}
	if fa.isTime {
		if fa.escapedPrefix != "" {
			p.out = append(p.out, fa.escapedPrefix...)
		} else {
			p.out = append(p.out, ' ')
			if fa.name.Space != "" {
				prefix := p.createAttrPrefix(fa.name.Space)
				p.out = append(p.out, prefix...)
				p.out = append(p.out, ':')
			}
			p.out = append(p.out, fa.name.Local...)
			p.out = append(p.out, `="`...)
		}
		var err error
		p.out, err = (*time.Time)(fptr).AppendText(p.out)
		if err != nil {
			return err
		}
		p.out = append(p.out, '"')
		return nil
	}
	if fa.isTextMarshaler && !(fa.isPtr && fa.elemIsTime) {
		v := reflect.NewAt(fa.rt, fptr).Elem().Interface().(encoding.TextMarshaler)
		tb, err := v.MarshalText()
		if err != nil {
			return err
		}
		p.appendAttrBytes(fa, tb)
		return nil
	}
	if canAddr && fa.ptrTextMarshaler {
		v := reflect.NewAt(fa.rt, fptr).Interface().(encoding.TextMarshaler)
		tb, err := v.MarshalText()
		if err != nil {
			return err
		}
		p.appendAttrBytes(fa, tb)
		return nil
	}

	k := fa.kind
	rt := fa.rt
	if k == xkPtr {
		fptr = *(*unsafe.Pointer)(fptr)
		if fptr == nil {
			return nil
		}
		k = fa.elemKind
		rt = fa.elemRT
		if fa.elemIsTime {
			var scratch [64]byte
			tb, err := (*time.Time)(fptr).AppendText(scratch[:0])
			if err != nil {
				return err
			}
			p.appendAttrBytes(fa, tb)
			return nil
		}
	} else if k == xkInterface {
		rv := reflect.NewAt(fa.rt, fptr).Elem()
		var dummy StartElement
		if err := p.marshalAttr(&dummy, fa.name, rv); err != nil {
			return err
		}
		for _, a := range dummy.Attr {
			p.appendAttr(a)
		}
		return nil
	}

	if k == xkSlice {
		sh := (*xmlSliceHeader)(fptr)
		for i := 0; i < sh.len; i++ {
			ep := unsafe.Add(sh.data, uintptr(i)*fa.elemSize)
			if err := p.fastWriteSliceAttrElem(fa, ep); err != nil {
				return err
			}
		}
		return nil
	}

	if k == xkAttr {
		attr := *(*Attr)(fptr)
		if attr.Name.Local != "" {
			p.appendAttr(attr)
		}
		return nil
	}

	if fa.escapedPrefix != "" {
		p.out = append(p.out, fa.escapedPrefix...)
	} else {
		p.out = append(p.out, ' ')
		if fa.name.Space != "" {
			prefix := p.createAttrPrefix(fa.name.Space)
			p.out = append(p.out, prefix...)
			p.out = append(p.out, ':')
		}
		p.out = append(p.out, fa.name.Local...)
		p.out = append(p.out, `="`...)
	}
	var err error
	p.out, err = appendSimpleAttrValue(p.out, k, fptr, rt)
	if err != nil {
		return err
	}
	p.out = append(p.out, '"')
	return nil
}

func (p *printer) fastWriteSliceAttrElem(fa *xmlFastAttr, ep unsafe.Pointer) error {
	if fa.elemMarshAttr {
		v := reflect.NewAt(fa.elemRT, ep).Elem().Interface().(MarshalerAttr)
		attr, err := v.MarshalXMLAttr(fa.name)
		if err != nil {
			return err
		}
		if attr.Name.Local != "" {
			p.appendAttr(attr)
		}
		return nil
	}
	if fa.elemPtrMarshAttr {
		v := reflect.NewAt(fa.elemRT, ep).Interface().(MarshalerAttr)
		attr, err := v.MarshalXMLAttr(fa.name)
		if err != nil {
			return err
		}
		if attr.Name.Local != "" {
			p.appendAttr(attr)
		}
		return nil
	}
	if fa.elemIsTime {
		if fa.escapedPrefix != "" {
			p.out = append(p.out, fa.escapedPrefix...)
		} else {
			p.out = append(p.out, ' ')
			if fa.name.Space != "" {
				prefix := p.createAttrPrefix(fa.name.Space)
				p.out = append(p.out, prefix...)
				p.out = append(p.out, ':')
			}
			p.out = append(p.out, fa.name.Local...)
			p.out = append(p.out, `="`...)
		}
		var err error
		p.out, err = (*time.Time)(ep).AppendText(p.out)
		if err != nil {
			return err
		}
		p.out = append(p.out, '"')
		return nil
	}
	if fa.elemTextMarsh {
		v := reflect.NewAt(fa.elemRT, ep).Elem().Interface().(encoding.TextMarshaler)
		tb, err := v.MarshalText()
		if err != nil {
			return err
		}
		p.appendAttrBytes(fa, tb)
		return nil
	}
	if fa.elemPtrTextMarsh {
		v := reflect.NewAt(fa.elemRT, ep).Interface().(encoding.TextMarshaler)
		tb, err := v.MarshalText()
		if err != nil {
			return err
		}
		p.appendAttrBytes(fa, tb)
		return nil
	}
	if fa.elemKind == xkAttr {
		attr := *(*Attr)(ep)
		if attr.Name.Local != "" {
			p.appendAttr(attr)
		}
		return nil
	}
	if fa.elemKind == xkInterface || fa.elemKind == xkPtr || fa.elemKind == xkSlice {
		rv := reflect.NewAt(fa.elemRT, ep).Elem()
		var dummy StartElement
		if err := p.marshalAttr(&dummy, fa.name, rv); err != nil {
			return err
		}
		for _, a := range dummy.Attr {
			p.appendAttr(a)
		}
		return nil
	}
	if fa.escapedPrefix != "" {
		p.out = append(p.out, fa.escapedPrefix...)
	} else {
		p.out = append(p.out, ' ')
		if fa.name.Space != "" {
			prefix := p.createAttrPrefix(fa.name.Space)
			p.out = append(p.out, prefix...)
			p.out = append(p.out, ':')
		}
		p.out = append(p.out, fa.name.Local...)
		p.out = append(p.out, `="`...)
	}
	var err error
	p.out, err = appendSimpleAttrValue(p.out, fa.elemKind, ep, fa.elemRT)
	if err != nil {
		return err
	}
	p.out = append(p.out, '"')
	return nil
}

func (p *printer) appendAttr(attr Attr) {
	if attr.Name.Local == "" {
		return
	}
	p.out = append(p.out, ' ')
	if attr.Name.Space != "" {
		prefix := p.createAttrPrefix(attr.Name.Space)
		p.out = append(p.out, prefix...)
		p.out = append(p.out, ':')
	}
	p.out = append(p.out, attr.Name.Local...)
	p.out = append(p.out, `="`...)
	p.out = appendEscapeXMLString(p.out, attr.Value, true)
	p.out = append(p.out, '"')
}

func (p *printer) appendAttrBytes(fa *xmlFastAttr, b []byte) {
	if fa.escapedPrefix != "" {
		p.out = append(p.out, fa.escapedPrefix...)
	} else {
		p.out = append(p.out, ' ')
		if fa.name.Space != "" {
			prefix := p.createAttrPrefix(fa.name.Space)
			p.out = append(p.out, prefix...)
			p.out = append(p.out, ':')
		}
		p.out = append(p.out, fa.name.Local...)
		p.out = append(p.out, `="`...)
	}
	p.out = appendEscapeXMLBytes(p.out, b, true)
	p.out = append(p.out, '"')
}

func (p *printer) trimParents(base int, parents []string) error {
	stack := p.parentStack[base:]
	split := 0
	for ; split < len(parents) && split < len(stack); split++ {
		if parents[split] != stack[split] {
			break
		}
	}
	for i := len(stack) - 1; i >= split; i-- {
		if err := p.writeEndFast(Name{Local: stack[i]}, false); err != nil {
			return err
		}
	}
	p.parentStack = p.parentStack[:base+split]
	return nil
}

func (p *printer) pushParents(parents []string) error {
	for i := 0; i < len(parents); i++ {
		if err := p.writeStartNameFast(Name{Local: parents[i]}, false); err != nil {
			return err
		}
	}
	p.parentStack = append(p.parentStack, parents...)
	return nil
}

func (p *printer) fastMarshalStructBody(plan *xmlMarshalPlan, ptr unsafe.Pointer, canAddr bool, depth int) error {
	parentBase := len(p.parentStack)
	hasIndent := len(p.prefix) != 0 || len(p.indent) != 0

	for i := range plan.elems {
		fe := &plan.elems[i]
		fptr, fAddr := resolveFieldPtrWithAddr(ptr, fe.steps, fe.offset, canAddr)
		if fptr == nil {
			continue
		}

		switch fe.mode {
		case fCDATA, fCharData:
			if plan.hasParents && len(p.parentStack) > parentBase {
				if err := p.trimParents(parentBase, fe.parents); err != nil {
					return err
				}
			}
			isCDATA := fe.mode == fCDATA
			if fe.kind == xkTime {
				if !isCDATA {
					var err error
					p.out, err = (*time.Time)(fptr).AppendText(p.out)
					if err != nil {
						return err
					}
					continue
				}
				var scratch [64]byte
				tb, err := (*time.Time)(fptr).AppendText(scratch[:0])
				if err != nil {
					return err
				}
				p.appendCDATA(tb)
				continue
			}
			if fe.isTextMarshaler {
				v := reflect.NewAt(fe.rt, fptr).Elem().Interface().(encoding.TextMarshaler)
				tb, err := v.MarshalText()
				if err != nil {
					return err
				}
				if isCDATA {
					p.appendCDATA(tb)
				} else {
					p.out = appendEscapeXMLBytes(p.out, tb, true)
				}
				continue
			}
			if fAddr && fe.ptrTextMarshaler {
				v := reflect.NewAt(fe.rt, fptr).Interface().(encoding.TextMarshaler)
				tb, err := v.MarshalText()
				if err != nil {
					return err
				}
				if isCDATA {
					p.appendCDATA(tb)
				} else {
					p.out = appendEscapeXMLBytes(p.out, tb, true)
				}
				continue
			}
			if fe.kind == xkString {
				str := *(*string)(fptr)
				if isCDATA {
					p.appendCDATA(unsafe.Slice(unsafe.StringData(str), len(str)))
				} else {
					p.out = appendEscapeXMLString(p.out, str, true)
				}
				continue
			}
			if fe.kind == xkBytes {
				b := *(*[]byte)(fptr)
				if isCDATA {
					p.appendCDATA(b)
				} else {
					p.out = appendEscapeXMLBytes(p.out, b, true)
				}
				continue
			}
			if fe.kind != xkPtr && fe.kind != xkInterface && fe.isScalarLeaf {
				var scratch [64]byte
				sb, err := appendSimpleXML(scratch[:0], fe.kind, fptr, fe.rt)
				if err == nil {
					if isCDATA {
						p.appendCDATA(sb)
					} else {
						p.out = append(p.out, sb...)
					}
					continue
				}
			}
			// Fall back via reflect for pointer/interface chardata/cdata.
			rv := indirect(reflect.NewAt(fe.rt, fptr).Elem())
			if !rv.IsValid() {
				continue
			}
			var scratch [64]byte
			var b []byte
			switch rv.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				b = istrconv.AppendInt10(scratch[:0], rv.Int())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				b = istrconv.AppendUint10(scratch[:0], rv.Uint())
			case reflect.Float32, reflect.Float64:
				b = istrconv.AppendFloat(scratch[:0], rv.Float(), 'g', -1, rv.Type().Bits())
			case reflect.Bool:
				b = strconv.AppendBool(scratch[:0], rv.Bool())
			case reflect.String:
				sv := rv.String()
				b = unsafe.Slice(unsafe.StringData(sv), len(sv))
			case reflect.Slice:
				if elem, ok := reflect.TypeAssert[[]byte](rv); ok {
					b = elem
				} else {
					continue
				}
			default:
				continue
			}
			if isCDATA {
				p.appendCDATA(b)
			} else {
				p.out = appendEscapeXMLBytes(p.out, b, true)
			}
			continue

		case fComment:
			if plan.hasParents && len(p.parentStack) > parentBase {
				if err := p.trimParents(parentBase, fe.parents); err != nil {
					return err
				}
			}
			if fe.kind == xkString {
				str := *(*string)(fptr)
				if len(str) == 0 {
					continue
				}
				if strings.Contains(str, "--") {
					return fmt.Errorf(`xml: comments must not contain "--"`)
				}
				if hasIndent {
					p.writeIndent(0)
				}
				p.out = append(p.out, "<!--"...)
				p.out = append(p.out, str...)
				if str[len(str)-1] == '-' {
					p.out = append(p.out, ' ')
				}
				p.out = append(p.out, "-->"...)
				continue
			}
			if fe.kind == xkBytes {
				b := *(*[]byte)(fptr)
				if len(b) == 0 {
					continue
				}
				if bytes.Contains(b, ddBytes) {
					return fmt.Errorf(`xml: comments must not contain "--"`)
				}
				if hasIndent {
					p.writeIndent(0)
				}
				p.out = append(p.out, "<!--"...)
				p.out = append(p.out, b...)
				if b[len(b)-1] == '-' {
					p.out = append(p.out, ' ')
				}
				p.out = append(p.out, "-->"...)
				continue
			}
			rv := indirect(reflect.NewAt(fe.rt, fptr).Elem())
			k := rv.Kind()
			if !(k == reflect.String || k == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8) {
				return fmt.Errorf("xml: bad type for comment field of %s", plan.rt)
			}
			if rv.Len() == 0 {
				continue
			}
			if hasIndent {
				p.writeIndent(0)
			}
			p.out = append(p.out, "<!--"...)
			var dashLast bool
			if k == reflect.String {
				str := rv.String()
				if strings.Contains(str, "--") {
					return fmt.Errorf(`xml: comments must not contain "--"`)
				}
				dashLast = str[len(str)-1] == '-'
				p.out = append(p.out, str...)
			} else {
				b := rv.Bytes()
				if bytes.Contains(b, ddBytes) {
					return fmt.Errorf(`xml: comments must not contain "--"`)
				}
				dashLast = b[len(b)-1] == '-'
				p.out = append(p.out, b...)
			}
			if dashLast {
				p.out = append(p.out, ' ')
			}
			p.out = append(p.out, "-->"...)
			continue

		case fInnerXML:
			if fe.rt == xmlStringType {
				p.out = append(p.out, *(*string)(fptr)...)
				continue
			}
			if fe.rt == xmlBytesType {
				p.out = append(p.out, *(*[]byte)(fptr)...)
				continue
			}
			rv := indirect(reflect.NewAt(fe.rt, fptr).Elem())
			switch raw := rv.Interface().(type) {
			case []byte:
				p.out = append(p.out, raw...)
				continue
			case string:
				p.out = append(p.out, raw...)
				continue
			}

		case fElement, fElement | fAny:
			if plan.hasParents {
				if len(p.parentStack) > parentBase {
					if err := p.trimParents(parentBase, fe.parents); err != nil {
						return err
					}
				}
				if cur := len(p.parentStack) - parentBase; len(fe.parents) > cur {
					if (fe.kind != xkPtr && fe.kind != xkInterface) || !xmlIsEmpty(fe.kind, fptr, fe.rt) {
						if err := p.pushParents(fe.parents[cur:]); err != nil {
							return err
						}
					}
				}
			}
		}

		if fe.omitEmpty && xmlIsEmpty(fe.kind, fptr, fe.rt) {
			continue
		}

		if fe.kind == xkTime && fe.mode == fElement && !fe.isMarshaler && !fe.ptrMarshaler {
			if !hasIndent {
				p.out = append(p.out, fe.openTag...)
				var err error
				p.out, err = (*time.Time)(fptr).AppendText(p.out)
				if err != nil {
					return err
				}
				p.out = append(p.out, fe.closeTag...)
				continue
			}
			p.writeIndent(1)
			p.out = append(p.out, fe.openTag...)
			var err error
			p.out, err = (*time.Time)(fptr).AppendText(p.out)
			if err != nil {
				return err
			}
			p.writeIndent(-1)
			p.out = append(p.out, fe.closeTag...)
			continue
		}

		if fe.isScalarLeaf && fe.mode == fElement {
			if !hasIndent {
				p.out = append(p.out, fe.openTag...)
				var err error
				p.out, err = appendSimpleXML(p.out, fe.kind, fptr, fe.rt)
				if err != nil {
					return err
				}
				p.out = append(p.out, fe.closeTag...)
				continue
			}
			p.writeIndent(1)
			p.out = append(p.out, fe.openTag...)
			var err error
			p.out, err = appendSimpleXML(p.out, fe.kind, fptr, fe.rt)
			if err != nil {
				return err
			}
			p.writeIndent(-1)
			p.out = append(p.out, fe.closeTag...)
			continue
		}

		if fe.isScalarSlice {
			sh := (*xmlSliceHeader)(fptr)
			if !hasIndent {
				for j := 0; j < sh.len; j++ {
					ep := unsafe.Add(sh.data, uintptr(j)*fe.elemSize)
					p.out = append(p.out, fe.openTag...)
					var err error
					if fe.elemKind == xkTime {
						p.out, err = (*time.Time)(ep).AppendText(p.out)
					} else {
						p.out, err = appendSimpleXML(p.out, fe.elemKind, ep, fe.elemRT)
					}
					if err != nil {
						return err
					}
					p.out = append(p.out, fe.closeTag...)
				}
				continue
			}
			for j := 0; j < sh.len; j++ {
				ep := unsafe.Add(sh.data, uintptr(j)*fe.elemSize)
				p.writeIndent(1)
				p.out = append(p.out, fe.openTag...)
				var err error
				if fe.elemKind == xkTime {
					p.out, err = (*time.Time)(ep).AppendText(p.out)
				} else {
					p.out, err = appendSimpleXML(p.out, fe.elemKind, ep, fe.elemRT)
				}
				if err != nil {
					return err
				}
				p.writeIndent(-1)
				p.out = append(p.out, fe.closeTag...)
			}
			continue
		}

		if fe.isStructSlice {
			if depth+2 > 1000 {
				return errors.New("xml: nested too deeply")
			}
			sh := (*xmlSliceHeader)(fptr)
			epPlan := fe.elemPlan
			for j := 0; j < sh.len; j++ {
				ep := unsafe.Add(sh.data, uintptr(j)*fe.elemSize)
				if err := p.writeStructStartFast(epPlan, ep, true, fe.name); err != nil {
					return err
				}
				if err := p.fastMarshalStructBody(epPlan, ep, true, depth+2); err != nil {
					return err
				}
				if err := p.writeEndFast(fe.name, epPlan.hasAttrSpace); err != nil {
					return err
				}
			}
			continue
		}

		if err := p.fastMarshalValue(fe.plan, fptr, fAddr, fe, depth+1); err != nil {
			return err
		}
	}

	if plan.hasParents {
		if err := p.trimParents(parentBase, nil); err != nil {
			return err
		}
	}
	return p.cachedWriteError()
}

func (p *printer) appendCDATA(s []byte) {
	if len(s) == 0 {
		return
	}
	p.out = append(p.out, "<![CDATA["...)
	for {
		before, after, ok := bytes.Cut(s, cdataEnd)
		if !ok {
			break
		}
		p.out = append(p.out, before...)
		p.out = append(p.out, "]]]]><![CDATA[>"...)
		s = after
	}
	p.out = append(p.out, s...)
	p.out = append(p.out, "]]>"...)
}
