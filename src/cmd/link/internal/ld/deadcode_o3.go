// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import "cmd/link/internal/loader"

// o3MethodAnalysis refines interface reachability using complete method sets.
// A common name such as Close or Get alone does not make a type implement the
// interface at a call site. In large SDKs, keeping such a method can pull in
// another service, whose calls then keep still more unrelated methods alive.
// All decisions here are proofs of non-implementation, never guesses based on
// package names, call frequency, or the absence of direct calls.
//
// Name-only generic calls and reflection are handled separately by deadcode.
// Dynamic linking does not use this analysis. StructOf can combine an embedded
// type's methods with additional interface methods at run time, so when it is
// reachable we also fall back to matching individual signatures.
type o3MethodAnalysis struct {
	calls      map[methodsig][]loader.Sym
	interfaces map[loader.Sym][]methodsig
	types      map[loader.Sym]o3MethodSet
	implements map[o3TypeInterface]bool
	structOf   loader.Sym
	budget     int
}

type o3MethodSet struct {
	methods map[methodsig]bool
	count   int
}

type o3TypeInterface struct {
	typ, iface loader.Sym
}

func newO3MethodAnalysis(ldr *loader.Loader) *o3MethodAnalysis {
	return &o3MethodAnalysis{
		calls:      make(map[methodsig][]loader.Sym),
		interfaces: make(map[loader.Sym][]methodsig),
		types:      make(map[loader.Sym]o3MethodSet),
		implements: make(map[o3TypeInterface]bool),
		structOf:   ldr.Lookup("reflect.StructOf", abiInternalVer),
		budget:     1 << 20,
	}
}

func (a *o3MethodAnalysis) recordCall(d *deadcodePass, iface loader.Sym, called methodsig) {
	if _, ok := a.interfaces[iface]; !ok {
		p := d.ldr.Data(iface)
		off := commonsize(d.ctxt.Arch) + 4*d.ctxt.Arch.PtrSize
		if decodetypeHasUncommon(d.ctxt.Arch, p) {
			off += uncommonSize(d.ctxt.Arch)
		}
		relocs := d.ldr.Relocs(iface)
		methods := d.decodeMethodSig(d.ldr, d.ctxt.Arch, iface, &relocs, off, 8, int(decodetypeIfaceMethodCount(d.ctxt.Arch, p)))
		// decodeMethodSig returns scratch space reused by the next decode.
		a.interfaces[iface] = append([]methodsig(nil), methods...)
	}
	for _, prev := range a.calls[called] {
		if prev == iface {
			return
		}
	}
	a.calls[called] = append(a.calls[called], iface)
}

func (a *o3MethodAnalysis) recordType(typ loader.Sym, methods []methodsig) {
	if _, ok := a.types[typ]; ok {
		return
	}
	set := o3MethodSet{methods: make(map[methodsig]bool, len(methods)), count: len(methods)}
	for _, m := range methods {
		set.methods[m] = true
	}
	a.types[typ] = set
}

func (a *o3MethodAnalysis) reachable(d *deadcodePass, m methodref) bool {
	if a.structOf != 0 && d.ldr.AttrReachable(a.structOf) {
		return true
	}
	for _, iface := range a.calls[m.m] {
		if a.implementsInterface(m.src, iface) {
			return true
		}
	}
	return false
}

func (a *o3MethodAnalysis) implementsInterface(typ, iface loader.Sym) bool {
	key := o3TypeInterface{typ, iface}
	if result, ok := a.implements[key]; ok {
		return result
	}
	set, ok := a.types[typ]
	methods := a.interfaces[iface]
	if !ok || len(methods) == 0 || a.budget <= 0 {
		// Unknown metadata or an exhausted proof budget can only retain code.
		return true
	}
	// Bound both work and cache size, including cheap method-count failures.
	a.budget--
	result := false
	if set.count >= len(methods) {
		if len(methods) > a.budget {
			return true
		}
		a.budget -= len(methods)
		result = true
		for _, want := range methods {
			if !set.methods[want] {
				result = false
				break
			}
		}
	}
	// Like ordinary DCE, signatures omit the package path of unexported
	// methods. This may retain extra methods, but cannot reject a valid
	// implementation. Use the original count above, not the map size: two
	// unexported methods from different packages may have identical keys.
	a.implements[key] = result
	return result
}
