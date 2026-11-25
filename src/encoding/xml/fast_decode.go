// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xml

import (
	"bytes"
	"encoding"
	"errors"
	"internal/abi"
	"internal/byteorder"
	"math/bits"
	"reflect"
	"runtime"
	syncv2 "sync/v2"
	"time"
	"unsafe"
)

type xmlDecodeStep struct {
	offset uintptr
	deref  bool
	elemRT reflect.Type
}

type xmlDecodeAttr struct {
	name               Name
	steps              []xmlDecodeStep
	offset             uintptr
	kind               xmlFastKind
	rt                 reflect.Type
	isUnmarshaler      bool
	ptrUnmarshaler     bool
	isTextUnmarshaler  bool
	ptrTextUnmarshaler bool
	isTime             bool
	isSlice            bool
	isPtr              bool
	elemSize           uintptr
	elemKind           xmlFastKind
	elemRT             reflect.Type
	elemUnmarsh        bool
	elemPtrUnmarsh     bool
	elemTextUnmarsh    bool
	elemPtrTextUnm     bool
}

type xmlDecodeElem struct {
	name               Name
	mode               fieldFlags
	parents            []string
	steps              []xmlDecodeStep
	offset             uintptr
	kind               xmlFastKind
	rt                 reflect.Type
	plan               *xmlDecodePlan
	isUnmarshaler      bool
	ptrUnmarshaler     bool
	isTextUnmarshaler  bool
	ptrTextUnmarshaler bool
	isTime             bool
	canFastField       bool
}

type xmlDecodePlan struct {
	typ                *abi.Type
	rt                 reflect.Type
	ptrRT              reflect.Type
	kind               xmlFastKind
	canFast            bool
	building           bool
	buildErr           error
	isUnmarshaler      bool
	ptrUnmarshaler     bool
	isTextUnmarshaler  bool
	ptrTextUnmarshaler bool
	elemSize           uintptr
	elemPlan           *xmlDecodePlan

	// Struct fields
	hasXMLName   bool
	xmlName      string
	xmlSpace     string
	xmlNameSteps []xmlDecodeStep
	xmlNameOff   uintptr
	xmlNameIsN   bool

	attrs      []xmlDecodeAttr
	hasAnyAttr bool
	anyAttrIdx int

	elems       []xmlDecodeElem
	elemByLocal map[string]int // -1/-2 = multiple/ambiguous -> linear scan
	hasParents  bool

	hasCharData bool
	charDataIdx int
	hasComment  bool
	commentIdx  int
	hasInnerXML bool
	innerXMLIdx int
	hasAnyElem  bool
	anyElemIdx  int
}

var xmlDecodePlanCache syncv2.Map[*abi.Type, *xmlDecodePlan]

func lookupXmlDecodePlan(at *abi.Type) *xmlDecodePlan {
	if p, ok := xmlDecodePlanCache.Load(at); ok {
		return p
	}
	visiting := make(map[*abi.Type]*xmlDecodePlan)
	p := buildXmlDecodePlan(xmlAbiTypeToReflect(at), at, visiting)
	finalizeXmlDecodePlans(visiting)
	for kt, kp := range visiting {
		xmlDecodePlanCache.LoadOrStore(kt, kp)
	}
	return p
}

func finalizeXmlDecodePlans(visiting map[*abi.Type]*xmlDecodePlan) {
	for changed := true; changed; {
		changed = false
		for _, p := range visiting {
			switch p.kind {
			case xkPtr, xkSlice:
				if p.elemPlan == nil || !p.elemPlan.canFast {
					if p.canFast {
						p.canFast = false
						changed = true
					}
				}
			}
		}
	}
	for _, p := range visiting {
		p.building = false
	}
}

func resolveDecodeFieldSteps(root reflect.Type, idx []int) (steps []xmlDecodeStep, offset uintptr) {
	t := root
	var cur uintptr
	for i, fieldIdx := range idx {
		if i > 0 && t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Struct {
			steps = append(steps, xmlDecodeStep{offset: cur, deref: true, elemRT: t.Elem()})
			t = t.Elem()
			cur = 0
		}
		sf := t.Field(fieldIdx)
		cur += sf.Offset
		t = sf.Type
	}
	return steps, cur
}

func resolveDecodeFieldPtr(base unsafe.Pointer, steps []xmlDecodeStep, finalOff uintptr) unsafe.Pointer {
	p := base
	for i := range steps {
		s := &steps[i]
		p = unsafe.Add(p, s.offset)
		if s.deref {
			pp := (*unsafe.Pointer)(p)
			if *pp == nil {
				*pp = reflect.New(s.elemRT).UnsafePointer()
			}
			p = *pp
		}
	}
	return unsafe.Add(p, finalOff)
}

func buildXmlDecodePlan(rt reflect.Type, at *abi.Type, visiting map[*abi.Type]*xmlDecodePlan) *xmlDecodePlan {
	if p, ok := visiting[at]; ok {
		return p
	}
	if cached, ok := xmlDecodePlanCache.Load(at); ok {
		return cached
	}

	ptrRT := reflect.PointerTo(rt)
	p := &xmlDecodePlan{
		typ:                at,
		rt:                 rt,
		ptrRT:              ptrRT,
		kind:               classifyXmlKind(rt, at),
		building:           true,
		isUnmarshaler:      rt.Implements(xmlUnmarshalerType),
		ptrUnmarshaler:     ptrRT.Implements(xmlUnmarshalerType),
		isTextUnmarshaler:  rt.Implements(xmlTextUnmarshalerType),
		ptrTextUnmarshaler: ptrRT.Implements(xmlTextUnmarshalerType),
		anyAttrIdx:         -1,
		charDataIdx:        -1,
		commentIdx:         -1,
		innerXMLIdx:        -1,
		anyElemIdx:         -1,
	}
	visiting[at] = p

	if p.kind != xkPtr && (p.isUnmarshaler || p.ptrUnmarshaler || p.isTextUnmarshaler || p.ptrTextUnmarshaler) {
		if at == xmlTimeAbiType {
			p.canFast = true
			return p
		}
		// Still mark canFast for TextUnmarshaler so we avoid Data buffer allocations,
		// and fallback for full Unmarshaler.
		if !p.isUnmarshaler && !p.ptrUnmarshaler {
			p.canFast = true
			return p
		}
		p.canFast = false
		return p
	}

	switch p.kind {
	case xkBool, xkString,
		xkInt, xkInt8, xkInt16, xkInt32, xkInt64,
		xkUint, xkUint8, xkUint16, xkUint32, xkUint64, xkUintptr,
		xkFloat32, xkFloat64,
		xkBytes, xkName, xkTime:
		p.canFast = true
		return p

	case xkPtr:
		ert := rt.Elem()
		// Decode only dereferences a single pointer layer; **T returns an error in read.go.
		if ert.Kind() == reflect.Pointer || ert.Kind() == reflect.Interface {
			p.canFast = false
			return p
		}
		eat := xmlAbiTypeOfReflect(ert)
		p.elemSize = ert.Size()
		p.elemPlan = buildXmlDecodePlan(ert, eat, visiting)
		p.canFast = true
		return p

	case xkSlice:
		ert := rt.Elem()
		if ert.Kind() == reflect.Interface {
			p.canFast = false
			return p
		}
		eat := xmlAbiTypeOfReflect(ert)
		p.elemSize = ert.Size()
		p.elemPlan = buildXmlDecodePlan(ert, eat, visiting)
		p.canFast = true
		return p

	case xkStruct:
		tinfo, err := getTypeInfo(rt)
		if err != nil {
			p.buildErr = err
			p.canFast = false
			return p
		}
		if tinfo.xmlname != nil {
			xn := tinfo.xmlname
			p.hasXMLName = true
			p.xmlName = xn.name
			p.xmlSpace = xn.xmlns
			p.xmlNameSteps, p.xmlNameOff = resolveDecodeFieldSteps(rt, xn.idx)
			p.xmlNameIsN = rt.FieldByIndex(xn.idx).Type == nameType
		}

		for i := range tinfo.fields {
			fi := &tinfo.fields[i]
			sf := rt.FieldByIndex(fi.idx)
			frt := sf.Type
			fat := xmlAbiTypeOfReflect(frt)
			steps, off := resolveDecodeFieldSteps(rt, fi.idx)

			mode := fi.flags & fMode
			switch mode {
			case fAttr:
				fa := buildXmlDecodeAttr(fi, frt, fat, steps, off)
				p.attrs = append(p.attrs, fa)

			case fAny | fAttr:
				if !p.hasAnyAttr {
					p.hasAnyAttr = true
					p.anyAttrIdx = len(p.attrs)
					fa := buildXmlDecodeAttr(fi, frt, fat, steps, off)
					p.attrs = append(p.attrs, fa)
				}

			case fCDATA, fCharData:
				fe := buildXmlDecodeElem(fi, frt, fat, steps, off, visiting)
				if !p.hasCharData {
					p.hasCharData = true
					p.charDataIdx = len(p.elems)
				}
				p.elems = append(p.elems, fe)

			case fComment:
				fe := buildXmlDecodeElem(fi, frt, fat, steps, off, visiting)
				if !p.hasComment {
					p.hasComment = true
					p.commentIdx = len(p.elems)
				}
				p.elems = append(p.elems, fe)

			case fInnerXML:
				fe := buildXmlDecodeElem(fi, frt, fat, steps, off, visiting)
				if !p.hasInnerXML {
					p.hasInnerXML = true
					p.innerXMLIdx = len(p.elems)
				}
				p.elems = append(p.elems, fe)

			case fAny, fAny | fElement:
				fe := buildXmlDecodeElem(fi, frt, fat, steps, off, visiting)
				if !p.hasAnyElem {
					p.hasAnyElem = true
					p.anyElemIdx = len(p.elems)
				}
				p.elems = append(p.elems, fe)

			case fElement:
				fe := buildXmlDecodeElem(fi, frt, fat, steps, off, visiting)
				if len(fe.parents) > 0 {
					p.hasParents = true
				}
				p.elems = append(p.elems, fe)
			}
		}

		if len(p.elems) > 4 {
			m := make(map[string]int, len(p.elems))
			for i := range p.elems {
				e := &p.elems[i]
				if e.mode != fElement {
					continue
				}
				key := e.name.Local
				isParent := len(e.parents) > 0
				if isParent {
					key = e.parents[0]
				}
				if prev, dup := m[key]; dup {
					if !(isParent && prev == -2) {
						m[key] = -1
					}
				} else if isParent {
					m[key] = -2
				} else {
					m[key] = i
				}
			}
			p.elemByLocal = m
		}

		p.canFast = true
		return p

	default:
		p.canFast = false
		return p
	}
}

func buildXmlDecodeAttr(fi *fieldInfo, frt reflect.Type, fat *abi.Type, steps []xmlDecodeStep, off uintptr) xmlDecodeAttr {
	ptrFRT := reflect.PointerTo(frt)
	fa := xmlDecodeAttr{
		name:               Name{Space: fi.xmlns, Local: fi.name},
		steps:              steps,
		offset:             off,
		kind:               classifyXmlKind(frt, fat),
		rt:                 frt,
		isUnmarshaler:      frt.Implements(xmlUnmarshalerAttrType),
		ptrUnmarshaler:     ptrFRT.Implements(xmlUnmarshalerAttrType),
		isTextUnmarshaler:  frt.Implements(xmlTextUnmarshalerType),
		ptrTextUnmarshaler: ptrFRT.Implements(xmlTextUnmarshalerType),
		isTime:             fat == xmlTimeAbiType,
	}
	if fa.kind == xkSlice {
		fa.isSlice = true
		ert := frt.Elem()
		eat := xmlAbiTypeOfReflect(ert)
		ptrERT := reflect.PointerTo(ert)
		fa.elemSize = ert.Size()
		fa.elemRT = ert
		fa.elemKind = classifyXmlKind(ert, eat)
		fa.elemUnmarsh = ert.Implements(xmlUnmarshalerAttrType)
		fa.elemPtrUnmarsh = ptrERT.Implements(xmlUnmarshalerAttrType)
		fa.elemTextUnmarsh = ert.Implements(xmlTextUnmarshalerType)
		fa.elemPtrTextUnm = ptrERT.Implements(xmlTextUnmarshalerType)
	} else if fa.kind == xkPtr {
		fa.isPtr = true
		ert := frt.Elem()
		eat := xmlAbiTypeOfReflect(ert)
		ptrERT := reflect.PointerTo(ert)
		fa.elemRT = ert
		fa.elemKind = classifyXmlKind(ert, eat)
		fa.isTime = eat == xmlTimeAbiType
		fa.elemUnmarsh = ert.Implements(xmlUnmarshalerAttrType)
		fa.elemPtrUnmarsh = ptrERT.Implements(xmlUnmarshalerAttrType)
		fa.elemTextUnmarsh = ert.Implements(xmlTextUnmarshalerType)
		fa.elemPtrTextUnm = ptrERT.Implements(xmlTextUnmarshalerType)
	}
	return fa
}

func buildXmlDecodeElem(fi *fieldInfo, frt reflect.Type, fat *abi.Type, steps []xmlDecodeStep, off uintptr, visiting map[*abi.Type]*xmlDecodePlan) xmlDecodeElem {
	ptrFRT := reflect.PointerTo(frt)
	fe := xmlDecodeElem{
		name:               Name{Space: fi.xmlns, Local: fi.name},
		mode:               fi.flags & fMode,
		parents:            fi.parents,
		steps:              steps,
		offset:             off,
		kind:               classifyXmlKind(frt, fat),
		rt:                 frt,
		isUnmarshaler:      frt.Implements(xmlUnmarshalerType),
		ptrUnmarshaler:     ptrFRT.Implements(xmlUnmarshalerType),
		isTextUnmarshaler:  frt.Implements(xmlTextUnmarshalerType),
		ptrTextUnmarshaler: ptrFRT.Implements(xmlTextUnmarshalerType),
		isTime:             fat == xmlTimeAbiType,
	}
	if fe.mode&(fElement|fAny) != 0 {
		fe.plan = buildXmlDecodePlan(frt, fat, visiting)
	}
	return fe
}

func cloneAttrs(attrs []Attr, attrRaw [][]byte) []Attr {
	if len(attrs) == 0 {
		return []Attr{}
	}
	cp := make([]Attr, len(attrs))
	copy(cp, attrs)
	for i := range cp {
		if i < len(attrRaw) && attrRaw[i] != nil {
			cp[i].Value = string(attrRaw[i])
		}
	}
	return cp
}

func materializeAttrVal(a Attr, raw []byte) Attr {
	if raw != nil {
		a.Value = string(raw)
	}
	return a
}

func (d *Decoder) fastUnmarshal(plan *xmlDecodePlan, ptr unsafe.Pointer, start *StartElement) error {
	if d.stkDepth > maxUnmarshalDepth || runtime.GOARCH == "wasm" && d.stkDepth > maxUnmarshalDepthWasm {
		return errUnmarshalDepth
	}
	var startName Name
	var startAttr []Attr
	var startAttrRaw [][]byte
	if start == nil {
		var tok xmlTok
		for {
			if err := d.nextTokenInto(&tok); err != nil {
				return err
			}
			if tok.kind == xtStartElement {
				startName = tok.name
				startAttr = tok.attr
				startAttrRaw = tok.attrRaw
				break
			}
		}
	} else {
		startName = start.Name
		startAttr = start.Attr
	}
	return d.fastUnmarshalStart(plan, ptr, startName, startAttr, startAttrRaw)
}

func (d *Decoder) fastUnmarshalStart(plan *xmlDecodePlan, ptr unsafe.Pointer, startName Name, startAttr []Attr, startAttrRaw [][]byte) error {
	if d.stkDepth > maxUnmarshalDepth || runtime.GOARCH == "wasm" && d.stkDepth > maxUnmarshalDepthWasm {
		return errUnmarshalDepth
	}

	if plan.kind == xkPtr {
		pp := (*unsafe.Pointer)(ptr)
		if *pp == nil {
			*pp = reflect.New(plan.elemPlan.rt).UnsafePointer()
		}
		ptr = *pp
		plan = plan.elemPlan
	}

	if plan.buildErr != nil {
		return plan.buildErr
	}

	if plan.isUnmarshaler || plan.ptrUnmarshaler {
		st := StartElement{Name: startName, Attr: cloneAttrs(startAttr, startAttrRaw)}
		pv := reflect.NewAt(plan.rt, ptr)
		if plan.isUnmarshaler {
			if u, ok := reflect.TypeAssert[Unmarshaler](pv.Elem()); ok {
				return d.unmarshalInterface(u, &st)
			}
		}
		if plan.ptrUnmarshaler {
			if u, ok := reflect.TypeAssert[Unmarshaler](pv); ok {
				return d.unmarshalInterface(u, &st)
			}
		}
	}

	if plan.isTextUnmarshaler || plan.ptrTextUnmarshaler {
		return d.fastUnmarshalTextElem(plan, ptr)
	}

	switch plan.kind {
	case xkSlice:
		return d.fastUnmarshalSlice(plan, ptr, startName, startAttr, startAttrRaw)
	case xkBytes:
		return d.fastUnmarshalLeaf(xkBytes, ptr)
	case xkBool, xkString,
		xkInt, xkInt8, xkInt16, xkInt32, xkInt64,
		xkUint, xkUint8, xkUint16, xkUint32, xkUint64, xkUintptr,
		xkFloat32, xkFloat64:
		return d.fastUnmarshalLeaf(plan.kind, ptr)
	case xkName:
		*(*Name)(ptr) = startName
		return d.fastSkip()
	case xkStruct:
		return d.fastUnmarshalStruct(plan, ptr, startName, startAttr, startAttrRaw)
	default:
		st := StartElement{Name: startName, Attr: cloneAttrs(startAttr, startAttrRaw)}
		return d.unmarshal(reflect.NewAt(plan.rt, ptr).Elem(), &st)
	}
}

func (d *Decoder) fastUnmarshalSlice(plan *xmlDecodePlan, ptr unsafe.Pointer, startName Name, startAttr []Attr, startAttrRaw [][]byte) error {
	sh := (*xmlSliceHeader)(ptr)
	n := sh.len
	var elemPtr unsafe.Pointer
	if n < sh.cap {
		sh.len = n + 1
		elemPtr = unsafe.Add(sh.data, uintptr(n)*plan.elemSize)
	} else {
		sv := reflect.NewAt(plan.rt, ptr).Elem()
		sv.Grow(1)
		sv.SetLen(n + 1)
		elemPtr = unsafe.Add((*xmlSliceHeader)(ptr).data, uintptr(n)*plan.elemSize)
	}
	if err := d.fastUnmarshalStart(plan.elemPlan, elemPtr, startName, startAttr, startAttrRaw); err != nil {
		sh.len = n
		return err
	}
	return nil
}

func (d *Decoder) fastUnmarshalTextElem(plan *xmlDecodePlan, ptr unsafe.Pointer) error {
	if plan.typ == xmlTimeAbiType {
		var timeBuf [64]byte
		data, err := d.readLeafText(timeBuf[:0])
		if err != nil {
			return err
		}
		return (*time.Time)(ptr).UnmarshalText(data)
	}
	data, err := d.readLeafText(nil)
	if err != nil {
		return err
	}
	pv := reflect.NewAt(plan.rt, ptr)
	if plan.isTextUnmarshaler {
		if tu, ok := reflect.TypeAssert[encoding.TextUnmarshaler](pv.Elem()); ok {
			return tu.UnmarshalText(data)
		}
	}
	if plan.ptrTextUnmarshaler {
		if tu, ok := reflect.TypeAssert[encoding.TextUnmarshaler](pv); ok {
			return tu.UnmarshalText(data)
		}
	}
	return errors.New("xml: internal error: expected TextUnmarshaler")
}

func (d *Decoder) fastReadSimpleLeafBytes() ([]byte, bool) {
	if d.t != nil || !d.Strict || d.err != nil || d.nextByte >= 0 || d.needClose || d.nextToken != nil || d.saved != nil {
		return nil, false
	}
	stk := d.stk
	if stk == nil || stk.kind != stkStart || stk.name.Space != "" || (stk.next != nil && stk.next.kind == stkNs) {
		return nil, false
	}
	s := d.src
	start := d.pos
	if start >= len(s) {
		return nil, false
	}
	i := start
	line := d.line
	linestart := d.linestart
	if len(s)-i >= 8 {
		if m := xmlCharDataMask(byteorder.LEUint64(s[i:])); m != 0 {
			i += bits.TrailingZeros64(m) >> 3
			c := s[i]
			if c == '<' {
				goto checkClose
			}
			if c == '\n' {
				i++
				line++
				linestart = d.offset + int64(i-d.pos)
			} else if c == '\t' {
				i++
			} else {
				return nil, false
			}
		} else {
			i += 8
		}
		if haveXMLSIMD && len(s)-i >= 32 {
			for len(s)-i >= 32 {
				i, _ = xmlCharDataIndexSIMD(s, i)
				if i < len(s) && s[i] == '\n' {
					i++
					line++
					linestart = d.offset + int64(i-d.pos)
					continue
				}
				break
			}
		}
		for len(s)-i >= 8 {
			if m := xmlCharDataMask(byteorder.LEUint64(s[i:])); m != 0 {
				i += bits.TrailingZeros64(m) >> 3
				c := s[i]
				if c == '\n' {
					i++
					line++
					linestart = d.offset + int64(i-d.pos)
					continue
				}
				if c == '\t' {
					i++
					continue
				}
				break
			}
			i += 8
		}
	}
	for i < len(s) {
		c := s[i]
		if c == '<' || c == '&' || c == ']' || c == '\r' || c >= 0x80 || (c < ' ' && c != '\t' && c != '\n') {
			break
		}
		i++
		if c == '\n' {
			line++
			linestart = d.offset + int64(i-d.pos)
		}
	}
checkClose:
	tag := stk.name.Local
	end := i + 2 + len(tag) + 1
	if end > len(s) || s[i] != '<' || s[i+1] != '/' || s[end-1] != '>' || string(s[i+2:i+2+len(tag)]) != tag {
		return nil, false
	}
	d.stk = stk.next
	d.stkDepth--
	stk.name = Name{}
	stk.next = d.free
	d.free = stk
	d.line = line
	d.linestart = linestart
	d.offset += int64(end - d.pos)
	d.pos = end
	return s[start:i], true
}

func (d *Decoder) readLeafText(dst []byte) ([]byte, error) {
	if raw, ok := d.fastReadSimpleLeafBytes(); ok {
		return append(dst, raw...), nil
	}
	depth := 1
	oldNoCopy := d.noCopyText
	d.noCopyText = true
	var tok xmlTok
	for depth > 0 {
		if err := d.nextTokenInto(&tok); err != nil {
			d.noCopyText = oldNoCopy
			return nil, err
		}
		switch tok.kind {
		case xtCharData:
			if depth == 1 && len(tok.data) > 0 {
				dst = append(dst, tok.data...)
			}
		case xtStartElement:
			depth++
		case xtEndElement:
			depth--
		}
	}
	d.noCopyText = oldNoCopy
	return dst, nil
}

func (d *Decoder) fastUnmarshalLeaf(kind xmlFastKind, ptr unsafe.Pointer) error {
	if raw, ok := d.fastReadSimpleLeafBytes(); ok {
		return assignScalarBytes(kind, ptr, raw)
	}
	var stackBuf [256]byte
	data := stackBuf[:0]
	oldNoCopy := d.noCopyText
	d.noCopyText = true
	var tok xmlTok
	for {
		if err := d.nextTokenInto(&tok); err != nil {
			d.noCopyText = oldNoCopy
			return err
		}
		switch tok.kind {
		case xtCharData:
			if len(tok.data) > 0 {
				data = append(data, tok.data...)
			}
		case xtStartElement:
			d.noCopyText = oldNoCopy
			if err := d.fastSkip(); err != nil {
				return err
			}
			d.noCopyText = true
		case xtEndElement:
			d.noCopyText = oldNoCopy
			return assignScalarBytes(kind, ptr, data)
		}
	}
}

func assignScalarBytes(kind xmlFastKind, ptr unsafe.Pointer, src []byte) error {
	switch kind {
	case xkString:
		if len(src) == 0 {
			*(*string)(ptr) = ""
		} else {
			*(*string)(ptr) = string(src)
		}
		return nil
	case xkBytes:
		if len(src) == 0 {
			*(*[]byte)(ptr) = []byte{}
		} else {
			b := make([]byte, len(src))
			copy(b, src)
			*(*[]byte)(ptr) = b
		}
		return nil
	case xkBool:
		if len(src) == 0 {
			*(*bool)(ptr) = false
			return nil
		}
		v, err := parseBoolBytes(src)
		if err != nil {
			return err
		}
		*(*bool)(ptr) = v
		return nil
	case xkInt:
		return assignIntPtr[int](ptr, src, strconvIntSize)
	case xkInt8:
		return assignIntPtr[int8](ptr, src, 8)
	case xkInt16:
		return assignIntPtr[int16](ptr, src, 16)
	case xkInt32:
		return assignIntPtr[int32](ptr, src, 32)
	case xkInt64:
		return assignIntPtr[int64](ptr, src, 64)
	case xkUint:
		return assignUintPtr[uint](ptr, src, strconvIntSize)
	case xkUint8:
		return assignUintPtr[uint8](ptr, src, 8)
	case xkUint16:
		return assignUintPtr[uint16](ptr, src, 16)
	case xkUint32:
		return assignUintPtr[uint32](ptr, src, 32)
	case xkUint64:
		return assignUintPtr[uint64](ptr, src, 64)
	case xkUintptr:
		return assignUintPtr[uintptr](ptr, src, strconvPtrSize)
	case xkFloat32:
		if len(src) == 0 {
			*(*float32)(ptr) = 0
			return nil
		}
		v, err := parseFloatBytes(src, 32)
		if err != nil {
			return err
		}
		*(*float32)(ptr) = float32(v)
		return nil
	case xkFloat64:
		if len(src) == 0 {
			*(*float64)(ptr) = 0
			return nil
		}
		v, err := parseFloatBytes(src, 64)
		if err != nil {
			return err
		}
		*(*float64)(ptr) = v
		return nil
	default:
		return errors.New("xml: internal error: unsupported scalar kind")
	}
}

func assignLeafBytes(kind xmlFastKind, ptr unsafe.Pointer, rt reflect.Type, src []byte, isTime, isTextUnm bool) error {
	if isTime {
		return (*time.Time)(ptr).UnmarshalText(src)
	}
	if isTextUnm {
		pv := reflect.NewAt(rt, ptr)
		if tu, ok := reflect.TypeAssert[encoding.TextUnmarshaler](pv); ok {
			return tu.UnmarshalText(src)
		}
	}
	if kind >= xkBool && kind <= xkBytes {
		return assignScalarBytes(kind, ptr, src)
	}
	return copyValue(reflect.NewAt(rt, ptr).Elem(), src)
}

const (
	strconvIntSize = 32 << (^uint(0) >> 63)
	strconvPtrSize = 32 << (^uintptr(0) >> 63)
)

func assignIntPtr[T ~int | ~int8 | ~int16 | ~int32 | ~int64](ptr unsafe.Pointer, src []byte, bitSize int) error {
	if len(src) == 0 {
		*(*T)(ptr) = 0
		return nil
	}
	v, err := parseIntBytes(src, bitSize)
	if err != nil {
		return err
	}
	*(*T)(ptr) = T(v)
	return nil
}

func assignUintPtr[T ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr](ptr unsafe.Pointer, src []byte, bitSize int) error {
	if len(src) == 0 {
		*(*T)(ptr) = 0
		return nil
	}
	v, err := parseUintBytes(src, bitSize)
	if err != nil {
		return err
	}
	*(*T)(ptr) = T(v)
	return nil
}

func assignAttrRaw(kind xmlFastKind, ptr unsafe.Pointer, rt reflect.Type, s string, raw []byte) error {
	switch kind {
	case xkString:
		if raw != nil {
			*(*string)(ptr) = string(raw)
		} else {
			*(*string)(ptr) = s
		}
		return nil
	case xkBytes:
		if raw != nil {
			b := make([]byte, len(raw))
			copy(b, raw)
			*(*[]byte)(ptr) = b
		} else if len(s) == 0 {
			*(*[]byte)(ptr) = []byte{}
		} else {
			*(*[]byte)(ptr) = []byte(s)
		}
		return nil
	default:
		if raw != nil {
			return assignLeafBytes(kind, ptr, rt, raw, false, false)
		}
		return assignLeafBytes(kind, ptr, rt, unsafe.Slice(unsafe.StringData(s), len(s)), false, false)
	}
}

func (d *Decoder) fastUnmarshalAttr(fa *xmlDecodeAttr, fptr unsafe.Pointer, a Attr, raw []byte) error {
	valBytes := raw
	if valBytes == nil {
		valBytes = unsafe.Slice(unsafe.StringData(a.Value), len(a.Value))
	}
	if fa.isPtr {
		pp := (*unsafe.Pointer)(fptr)
		if *pp == nil {
			*pp = reflect.New(fa.elemRT).UnsafePointer()
		}
		fptr = *pp
		if fa.isTime {
			return (*time.Time)(fptr).UnmarshalText(valBytes)
		}
		if fa.elemUnmarsh || fa.elemPtrUnmarsh || fa.elemTextUnmarsh || fa.elemPtrTextUnm || fa.elemKind == xkSlice || fa.elemKind == xkPtr {
			return d.unmarshalAttr(reflect.NewAt(fa.elemRT, fptr).Elem(), materializeAttrVal(a, raw))
		}
		if fa.elemKind == xkAttr {
			*(*Attr)(fptr) = materializeAttrVal(a, raw)
			return nil
		}
		return assignAttrRaw(fa.elemKind, fptr, fa.elemRT, a.Value, raw)
	}

	if fa.isUnmarshaler || fa.ptrUnmarshaler || fa.isTextUnmarshaler || fa.ptrTextUnmarshaler {
		if fa.isTime {
			return (*time.Time)(fptr).UnmarshalText(valBytes)
		}
		return d.unmarshalAttr(reflect.NewAt(fa.rt, fptr).Elem(), materializeAttrVal(a, raw))
	}

	if fa.isSlice {
		sh := (*xmlSliceHeader)(fptr)
		n := sh.len
		var elemPtr unsafe.Pointer
		if n < sh.cap {
			sh.len = n + 1
			elemPtr = unsafe.Add(sh.data, uintptr(n)*fa.elemSize)
		} else {
			sv := reflect.NewAt(fa.rt, fptr).Elem()
			sv.Grow(1)
			sv.SetLen(n + 1)
			elemPtr = unsafe.Add((*xmlSliceHeader)(fptr).data, uintptr(n)*fa.elemSize)
		}
		if fa.elemUnmarsh || fa.elemPtrUnmarsh || fa.elemTextUnmarsh || fa.elemPtrTextUnm || fa.elemKind == xkPtr || fa.elemKind == xkSlice {
			if err := d.unmarshalAttr(reflect.NewAt(fa.elemRT, elemPtr).Elem(), materializeAttrVal(a, raw)); err != nil {
				sh.len = n
				return err
			}
			return nil
		}
		if fa.elemKind == xkAttr {
			*(*Attr)(elemPtr) = materializeAttrVal(a, raw)
			return nil
		}
		if err := assignAttrRaw(fa.elemKind, elemPtr, fa.elemRT, a.Value, raw); err != nil {
			sh.len = n
			return err
		}
		return nil
	}

	if fa.kind == xkAttr {
		*(*Attr)(fptr) = materializeAttrVal(a, raw)
		return nil
	}
	return assignAttrRaw(fa.kind, fptr, fa.rt, a.Value, raw)
}

func (d *Decoder) fastUnmarshalStruct(plan *xmlDecodePlan, ptr unsafe.Pointer, startName Name, startAttr []Attr, startAttrRaw [][]byte) error {
	if plan.hasXMLName {
		if plan.xmlName != "" && plan.xmlName != startName.Local {
			return UnmarshalError("expected element type <" + plan.xmlName + "> but have <" + startName.Local + ">")
		}
		if plan.xmlSpace != "" && plan.xmlSpace != startName.Space {
			e := "expected element <" + plan.xmlName + "> in name space " + plan.xmlSpace + " but have "
			if startName.Space == "" {
				e += "no name space"
			} else {
				e += startName.Space
			}
			return UnmarshalError(e)
		}
		fvPtr := resolveDecodeFieldPtr(ptr, plan.xmlNameSteps, plan.xmlNameOff)
		if plan.xmlNameIsN {
			*(*Name)(fvPtr) = startName
		}
	}

	if len(startAttr) > 0 && len(plan.attrs) > 0 {
		for ai, a := range startAttr {
			var raw []byte
			if ai < len(startAttrRaw) {
				raw = startAttrRaw[ai]
			}
			handled := false
			for i := range plan.attrs {
				fa := &plan.attrs[i]
				if i == plan.anyAttrIdx {
					continue
				}
				if a.Name.Local == fa.name.Local && (fa.name.Space == "" || fa.name.Space == a.Name.Space) {
					fptr := resolveDecodeFieldPtr(ptr, fa.steps, fa.offset)
					if err := d.fastUnmarshalAttr(fa, fptr, a, raw); err != nil {
						return err
					}
					handled = true
				}
			}
			if !handled && plan.anyAttrIdx >= 0 {
				fa := &plan.attrs[plan.anyAttrIdx]
				fptr := resolveDecodeFieldPtr(ptr, fa.steps, fa.offset)
				if err := d.fastUnmarshalAttr(fa, fptr, a, raw); err != nil {
					return err
				}
			}
		}
	}

	var (
		saveDataPtr  unsafe.Pointer
		saveDataElem *xmlDecodeElem
		saveCommPtr  unsafe.Pointer
		saveCommKind xmlFastKind
		saveXMLPtr   unsafe.Pointer
		saveXMLKind  xmlFastKind
		saveXMLIndex int
		saveXMLData  []byte
		saveAnyPtr   unsafe.Pointer
		saveAnyElem  *xmlDecodeElem
		data         []byte
		comment      []byte
	)

	if plan.hasCharData {
		fe := &plan.elems[plan.charDataIdx]
		saveDataElem = fe
		saveDataPtr = resolveDecodeFieldPtr(ptr, fe.steps, fe.offset)
	}
	if plan.hasComment {
		fe := &plan.elems[plan.commentIdx]
		saveCommKind = fe.kind
		saveCommPtr = resolveDecodeFieldPtr(ptr, fe.steps, fe.offset)
	}
	if plan.hasAnyElem {
		fe := &plan.elems[plan.anyElemIdx]
		saveAnyElem = fe
		saveAnyPtr = resolveDecodeFieldPtr(ptr, fe.steps, fe.offset)
	}
	if plan.hasInnerXML {
		fe := &plan.elems[plan.innerXMLIdx]
		saveXMLKind = fe.kind
		saveXMLPtr = resolveDecodeFieldPtr(ptr, fe.steps, fe.offset)
		if d.saved == nil {
			saveXMLIndex = 0
			d.saved = new(bytes.Buffer)
		} else {
			saveXMLIndex = d.savedOffset()
		}
	}

	noCopyStruct := saveDataPtr == nil && saveCommPtr == nil && saveXMLPtr == nil
	var tok xmlTok

Loop:
	for {
		var savedOffset int
		if saveXMLPtr != nil {
			savedOffset = d.savedOffset()
		}
		oldNoCopy := d.noCopyText
		if noCopyStruct {
			d.noCopyText = true
		}
		err := d.nextTokenInto(&tok)
		d.noCopyText = oldNoCopy
		if err != nil {
			return err
		}
		switch tok.kind {
		case xtStartElement:
			var consumed bool
			if plan.elemByLocal != nil {
				idx, ok := plan.elemByLocal[tok.name.Local]
				if ok {
					if idx >= 0 {
						fe := &plan.elems[idx]
						if fe.name.Space == "" || fe.name.Space == tok.name.Space {
							fptr := resolveDecodeFieldPtr(ptr, fe.steps, fe.offset)
							consumed = true
							err = d.fastUnmarshalElem(fe, fptr, tok.name, tok.attr, tok.attrRaw)
						}
					} else if idx == -2 || plan.hasParents {
						consumed, err = d.fastUnmarshalPath(plan, ptr, nil, tok.name, tok.attr, tok.attrRaw)
					} else {
						consumed, err = d.fastMatchFlatElement(plan, ptr, tok.name, tok.attr, tok.attrRaw)
					}
				}
			} else if !plan.hasParents {
				consumed, err = d.fastMatchFlatElement(plan, ptr, tok.name, tok.attr, tok.attrRaw)
			} else {
				consumed, err = d.fastUnmarshalPath(plan, ptr, nil, tok.name, tok.attr, tok.attrRaw)
			}
			if err != nil {
				return err
			}
			if !consumed {
				if saveAnyPtr != nil {
					if saveAnyElem.plan != nil && saveAnyElem.plan.canFast {
						if err := d.fastUnmarshalStart(saveAnyElem.plan, saveAnyPtr, tok.name, tok.attr, tok.attrRaw); err != nil {
							return err
						}
					} else {
						st := StartElement{Name: tok.name, Attr: cloneAttrs(tok.attr, tok.attrRaw)}
						if err := d.unmarshal(reflect.NewAt(saveAnyElem.rt, saveAnyPtr).Elem(), &st); err != nil {
							return err
						}
					}
				} else if err := d.fastSkip(); err != nil {
					return err
				}
			}

		case xtEndElement:
			if saveXMLPtr != nil {
				saveXMLData = d.saved.Bytes()[saveXMLIndex:savedOffset]
				if saveXMLIndex == 0 {
					d.saved = nil
				}
			}
			break Loop

		case xtCharData:
			if saveDataPtr != nil && len(tok.data) > 0 {
				data = append(data, tok.data...)
			}

		case xtComment:
			if saveCommPtr != nil && len(tok.data) > 0 {
				comment = append(comment, tok.data...)
			}
		}
	}

	if saveDataPtr != nil {
		if saveDataElem.isTime {
			if err := (*time.Time)(saveDataPtr).UnmarshalText(data); err != nil {
				return err
			}
		} else if saveDataElem.isTextUnmarshaler || saveDataElem.ptrTextUnmarshaler {
			pv := reflect.NewAt(saveDataElem.rt, saveDataPtr)
			var handled bool
			if saveDataElem.isTextUnmarshaler {
				if tu, ok := reflect.TypeAssert[encoding.TextUnmarshaler](pv.Elem()); ok {
					if err := tu.UnmarshalText(data); err != nil {
						return err
					}
					handled = true
				}
			}
			if !handled && saveDataElem.ptrTextUnmarshaler {
				if tu, ok := reflect.TypeAssert[encoding.TextUnmarshaler](pv); ok {
					if err := tu.UnmarshalText(data); err != nil {
						return err
					}
					handled = true
				}
			}
			if !handled {
				if err := assignLeafBytes(saveDataElem.kind, saveDataPtr, saveDataElem.rt, data, false, false); err != nil {
					return err
				}
			}
		} else {
			if err := assignLeafBytes(saveDataElem.kind, saveDataPtr, saveDataElem.rt, data, false, false); err != nil {
				return err
			}
		}
	}

	if saveCommPtr != nil {
		switch saveCommKind {
		case xkString:
			*(*string)(saveCommPtr) = string(comment)
		case xkBytes:
			*(*[]byte)(saveCommPtr) = comment
		}
	}

	if saveXMLPtr != nil {
		switch saveXMLKind {
		case xkString:
			*(*string)(saveXMLPtr) = string(saveXMLData)
		case xkBytes:
			*(*[]byte)(saveXMLPtr) = saveXMLData
		}
	}

	return nil
}

func (d *Decoder) fastMatchFlatElement(plan *xmlDecodePlan, ptr unsafe.Pointer, startName Name, startAttr []Attr, startAttrRaw [][]byte) (bool, error) {
	for i := range plan.elems {
		fe := &plan.elems[i]
		if fe.mode != fElement || fe.name.Local != startName.Local {
			continue
		}
		if fe.name.Space != "" && fe.name.Space != startName.Space {
			continue
		}
		fptr := resolveDecodeFieldPtr(ptr, fe.steps, fe.offset)
		return true, d.fastUnmarshalElem(fe, fptr, startName, startAttr, startAttrRaw)
	}
	return false, nil
}

func (d *Decoder) fastUnmarshalPath(plan *xmlDecodePlan, ptr unsafe.Pointer, parents []string, startName Name, startAttr []Attr, startAttrRaw [][]byte) (bool, error) {
	recurse := false
Loop:
	for i := range plan.elems {
		fe := &plan.elems[i]
		if fe.mode != fElement || len(fe.parents) < len(parents) || (fe.name.Space != "" && fe.name.Space != startName.Space) {
			continue
		}
		for j := range parents {
			if parents[j] != fe.parents[j] {
				continue Loop
			}
		}
		if len(fe.parents) == len(parents) && fe.name.Local == startName.Local {
			fptr := resolveDecodeFieldPtr(ptr, fe.steps, fe.offset)
			return true, d.fastUnmarshalElem(fe, fptr, startName, startAttr, startAttrRaw)
		}
		if len(fe.parents) > len(parents) && fe.parents[len(parents)] == startName.Local {
			recurse = true
			parents = fe.parents[:len(parents)+1]
			break
		}
	}
	if !recurse {
		return false, nil
	}
	var tok xmlTok
	for {
		oldNoCopy := d.noCopyText
		d.noCopyText = true
		err := d.nextTokenInto(&tok)
		d.noCopyText = oldNoCopy
		if err != nil {
			return true, err
		}
		switch tok.kind {
		case xtStartElement:
			consumed2, err := d.fastUnmarshalPath(plan, ptr, parents, tok.name, tok.attr, tok.attrRaw)
			if err != nil {
				return true, err
			}
			if !consumed2 {
				if err := d.fastSkip(); err != nil {
					return true, err
				}
			}
		case xtEndElement:
			return true, nil
		}
	}
}

func (d *Decoder) fastUnmarshalElem(fe *xmlDecodeElem, fptr unsafe.Pointer, startName Name, startAttr []Attr, startAttrRaw [][]byte) error {
	if fe.plan != nil && fe.plan.canFast {
		return d.fastUnmarshalStart(fe.plan, fptr, startName, startAttr, startAttrRaw)
	}
	st := StartElement{Name: startName, Attr: cloneAttrs(startAttr, startAttrRaw)}
	return d.unmarshal(reflect.NewAt(fe.rt, fptr).Elem(), &st)
}
