// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"fmt"
	"internal/buildcfg"
	"internal/pkgbits"
	"slices"
	"strconv"
	"strings"

	"cmd/compile/internal/ir"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types"
	"cmd/compile/internal/types2"
)

func hasValidToImmutableMethod(t types2.Type) bool {
	obj, _, _ := types2.LookupFieldOrMethod(t, false, nil, "ToImmutable")
	fn, ok := obj.(*types2.Func)
	if !ok {
		return false
	}
	sig, ok := fn.Type().(*types2.Signature)
	if !ok || sig.TypeParams().Len() != 0 || sig.Params().Len() != 0 || sig.Results().Len() != 1 {
		return false
	}
	return types2.Identical(sig.Results().At(0).Type(), t)
}

func isImmutableContainerType(t types2.Type) bool {
	if t == nil {
		return false
	}
	ut := types2.Unalias(t)
	if _, isDefined := ut.(*types2.Named); isDefined {
		// A defined pointer type (type P *list.List) has no methods in Go.
		return false
	}
	if _, isTP := ut.(*types2.TypeParam); isTP {
		if !hasValidToImmutableMethod(t) {
			return false
		}
	}
	ptr, ok := types2.CommonUnderlying(ut).(*types2.Pointer)
	if !ok {
		return false
	}
	named, ok := types2.Unalias(ptr.Elem()).(*types2.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return false
	}
	pkgPath := named.Obj().Pkg().Path()
	name := named.Obj().Name()
	switch pkgPath {
	case "container/list":
		return name == "List"
	case "container/ring":
		return name == "Ring"
	case "sync/v2/container":
		switch name {
		case "LinkedHashMap", "OrderedMap", "TreeMap", "ConcurrentSkipListMap":
			return true
		}
	}
	return false
}

func isImmutableSupportedType(t types2.Type) bool {
	if t == nil {
		return false
	}
	switch types2.CommonUnderlying(t).(type) {
	case *types2.Slice, *types2.Map, *types2.Array:
		return true
	}
	return isImmutableContainerType(t)
}

func parseCanonicalIndex(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

func (pw *pkgWriter) collectImmutableFunc(decl *syntax.FuncDecl, obj *types2.Func) {
	p, ok := decl.Pragma.(*pragmas)
	if !ok || p == nil || p.Immutable == nil {
		return
	}
	pos := p.Immutable.Pos
	if decl.Recv == nil && decl.Name != nil && decl.Name.Value == "init" {
		pw.errorf(pos, "cannot use //go:immutable on init function")
		return
	}

	sig := obj.Type().(*types2.Signature)
	fnName := obj.Name()
	args := p.Immutable.Args

	var params, results []int
	if len(args) == 0 {
		nparams := sig.Params().Len()
		for i := range nparams {
			if sig.Variadic() && i == nparams-1 {
				continue
			}
			if isImmutableSupportedType(sig.Params().At(i).Type()) {
				params = append(params, i)
			}
		}
		if len(params) == 0 {
			pw.errorf(pos, "//go:immutable function %s has no supported collection or container parameters", fnName)
			return
		}
	} else {
		seenParams := make(map[int]bool)
		seenResults := make(map[int]bool)
		for _, tok := range args {
			switch {
			case strings.HasPrefix(tok, "args"):
				idx, ok := parseCanonicalIndex(tok[len("args"):])
				if !ok {
					pw.errorf(pos, "invalid //go:immutable argument %q", tok)
					continue
				}
				if idx >= sig.Params().Len() {
					pw.errorf(pos, "//go:immutable parameter args%d out of range for %s (have %d parameters)", idx, fnName, sig.Params().Len())
					continue
				}
				if seenParams[idx] {
					pw.errorf(pos, "duplicate //go:immutable parameter args%d", idx)
					continue
				}
				pt := sig.Params().At(idx).Type()
				if !isImmutableSupportedType(pt) {
					pw.errorf(pos, "//go:immutable parameter args%d of %s has unsupported type %v", idx, fnName, pt)
					continue
				}
				seenParams[idx] = true
				params = append(params, idx)

			case tok == "r":
				if sig.Results().Len() == 0 {
					pw.errorf(pos, "//go:immutable result r out of range for %s (have 0 results)", fnName)
					continue
				}
				if sig.Results().Len() > 1 {
					pw.errorf(pos, "//go:immutable shorthand r requires 1 result (have %d results)", sig.Results().Len())
					continue
				}
				if seenResults[0] {
					pw.errorf(pos, "duplicate //go:immutable result r0")
					continue
				}
				rt := sig.Results().At(0).Type()
				if !isImmutableSupportedType(rt) {
					pw.errorf(pos, "//go:immutable result r0 of %s has unsupported type %v", fnName, rt)
					continue
				}
				seenResults[0] = true
				results = append(results, 0)

			case strings.HasPrefix(tok, "r"):
				idx, ok := parseCanonicalIndex(tok[1:])
				if !ok {
					pw.errorf(pos, "invalid //go:immutable argument %q", tok)
					continue
				}
				if idx >= sig.Results().Len() {
					pw.errorf(pos, "//go:immutable result r%d out of range for %s (have %d results)", idx, fnName, sig.Results().Len())
					continue
				}
				if seenResults[idx] {
					pw.errorf(pos, "duplicate //go:immutable result r%d", idx)
					continue
				}
				rt := sig.Results().At(idx).Type()
				if !isImmutableSupportedType(rt) {
					pw.errorf(pos, "//go:immutable result r%d of %s has unsupported type %v", idx, fnName, rt)
					continue
				}
				seenResults[idx] = true
				results = append(results, idx)

			default:
				pw.errorf(pos, "invalid //go:immutable argument %q", tok)
			}
		}
		if decl.Body == nil && len(results) > 0 {
			pw.errorf(pos, "cannot use //go:immutable result constraint on function %s without body", fnName)
			results = nil
		}
		slices.Sort(params)
		slices.Sort(results)
	}

	if len(params) == 0 && len(results) == 0 {
		return
	}

	info := &ir.ImmutableFuncInfo{
		Params:  params,
		Results: results,
	}
	pw.immutableFuncs[obj] = info

	for _, idx := range params {
		pv := sig.Params().At(idx)
		if pv.Name() != "" && pv.Name() != "_" {
			pw.immutableVars[pv] = true
			pw.immutableParamVars[pv] = true
		}
	}
	for _, idx := range results {
		rv := sig.Results().At(idx)
		if rv.Name() != "" && rv.Name() != "_" {
			pw.immutableResultVars[rv] = true
		}
	}
}

func (pw *pkgWriter) isImmutableVar(obj *types2.Var) bool {
	if obj == nil {
		return false
	}
	if pw.immutableVars[obj] {
		return true
	}
	if obj.Pkg() != nil && obj.Pkg() != pw.curpkg {
		if res, cached := pw.importedImmutable[obj]; cached {
			return res
		}
		res := isImportedImmutable(obj)
		if pw.importedImmutable == nil {
			pw.importedImmutable = make(map[*types2.Var]bool)
		}
		pw.importedImmutable[obj] = res
		return res
	}
	return false
}

func isImportedImmutable(obj *types2.Var) bool {
	if obj.Pkg() == nil {
		return false
	}
	sym := types.NewPkg(obj.Pkg().Path(), "").Lookup(obj.Name())
	if sym.Def == nil {
		if pri, ok := objReader[sym]; ok {
			_, _ = pri.pr.objIdxMayFail(pri.idx, nil, nil, false)
		}
	}
	if name, ok := sym.Def.(*ir.Name); ok {
		return name.Pragma()&ir.Immutable != 0
	}
	return false
}

func resolveExportedObjReader(pkgPath, symName string) (pkgReaderIndex, pkgbits.CodeObj, bool) {
	sym := types.NewPkg(pkgPath, "").Lookup(symName)
	for range 8 {
		pri, ok := objReader[sym]
		if !ok {
			return pkgReaderIndex{}, 0, false
		}
		rname := pri.pr.newReader(pkgbits.SectionName, pri.idx, pkgbits.SyncObject1)
		_, sym2 := rname.qualifiedIdent()
		tag := pkgbits.CodeObj(rname.Code(pkgbits.SyncCodeObj))
		if tag != pkgbits.ObjStub {
			return pri, tag, true
		}
		sym = sym2
	}
	return pkgReaderIndex{}, 0, false
}

func readImmutableFuncExtHeader(rext *reader) *ir.ImmutableFuncInfo {
	rext.Sync(pkgbits.SyncFuncExt)
	pragma := rext.pragmaFlag()
	if pragma&ir.Immutable == 0 {
		return nil
	}
	nparams := rext.Len()
	params := make([]int, nparams)
	for i := range params {
		params[i] = rext.Len()
	}
	nresults := rext.Len()
	results := make([]int, nresults)
	for i := range results {
		results[i] = rext.Len()
	}
	return &ir.ImmutableFuncInfo{
		Params:  params,
		Results: results,
	}
}

func skipExportedFuncExtRest(rext *reader, numRecvParams int) {
	rext.Sync(pkgbits.SyncLinkname)
	if rext.Int64() < 0 {
		_ = rext.String()
		_ = rext.Bool()
	}
	if buildcfg.GOARCH == "wasm" || buildcfg.GOARCH == "wasm32" {
		_ = rext.String()
		_ = rext.String()
		_ = rext.String()
	}
	if rext.Bool() {
		_ = rext.Uint64()
		for range numRecvParams {
			_ = rext.String()
		}
		if rext.Bool() {
			cost := rext.Len()
			_ = rext.Bool()
			if cost&inlinePropertiesFlag != 0 || buildcfg.Experiment.NewInliner {
				_ = rext.String()
			}
		}
	} else {
		_ = rext.Reloc(pkgbits.SectionBody)
	}
	rext.Sync(pkgbits.SyncEOF)
}

func (pw *pkgWriter) immutableFuncInfo(fn *types2.Func) *ir.ImmutableFuncInfo {
	if fn == nil {
		return nil
	}
	orig := fn.Origin()
	if orig.Pkg() == nil {
		return nil
	}
	if orig.Pkg() == pw.curpkg {
		return pw.immutableFuncs[orig]
	}
	if pw.importedImmutableFuncs == nil {
		pw.importedImmutableFuncs = make(map[*types2.Func]*ir.ImmutableFuncInfo)
	}
	if info, cached := pw.importedImmutableFuncs[orig]; cached {
		return info
	}

	sig := orig.Type().(*types2.Signature)
	if sig.Recv() == nil || isGenericMethod(sig) {
		symName := orig.Name()
		if isGenericMethod(sig) {
			recv := sig.Recv().Type()
			fstr := "%s.%s"
			if _, ok := types2.Unalias(recv).(*types2.Pointer); ok {
				fstr = "(*%s).%s"
			}
			symName = fmt.Sprintf(fstr, types2.Unalias(deref2(recv)).(*types2.Named).Obj().Name(), symName)
		}
		sym := types.NewPkg(orig.Pkg().Path(), "").Lookup(symName)
		if sym.Def != nil {
			if name, ok := sym.Def.(*ir.Name); ok && name.Func != nil {
				info := name.Func.ImmutableInfo()
				pw.importedImmutableFuncs[orig] = info
				return info
			}
		}
		pri, tag, ok := resolveExportedObjReader(orig.Pkg().Path(), symName)
		if !ok || tag != pkgbits.ObjFunc {
			pw.importedImmutableFuncs[orig] = nil
			return nil
		}
		rext := pri.pr.newReader(pkgbits.SectionObjExt, pri.idx, pkgbits.SyncObject1)
		info := readImmutableFuncExtHeader(rext)
		pw.importedImmutableFuncs[orig] = info
		return info
	}

	if types2.IsInterface(sig.Recv().Type()) {
		pw.importedImmutableFuncs[orig] = nil
		return nil
	}
	named, ok := types2.Unalias(deref2(sig.Recv().Type())).(*types2.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		pw.importedImmutableFuncs[orig] = nil
		return nil
	}
	namedOrig := named.Origin()
	typeObj := namedOrig.Obj()
	pri, tag, ok := resolveExportedObjReader(typeObj.Pkg().Path(), typeObj.Name())
	if !ok || tag != pkgbits.ObjType {
		pw.importedImmutableFuncs[orig] = nil
		return nil
	}

	rext := pri.pr.newReader(pkgbits.SectionObjExt, pri.idx, pkgbits.SyncObject1)
	rext.Sync(pkgbits.SyncTypeExt)
	_ = rext.pragmaFlag()
	_ = rext.Int64()
	_ = rext.Int64()

	var nonGenericMethods []*types2.Func
	for i := range namedOrig.NumMethods() {
		m := namedOrig.Method(i)
		if !isGenericMethod(m.Type()) {
			nonGenericMethods = append(nonGenericMethods, m)
		}
	}
	for i, m := range nonGenericMethods {
		msig := m.Type().(*types2.Signature)
		mInfo := readImmutableFuncExtHeader(rext)
		pw.importedImmutableFuncs[m] = mInfo
		if i+1 < len(nonGenericMethods) {
			skipExportedFuncExtRest(rext, 1+msig.Params().Len())
		}
	}
	return pw.importedImmutableFuncs[orig]
}

func isStdToImmutableFunc(fn *types2.Func, resType types2.Type) bool {
	if fn == nil {
		return false
	}
	orig := fn.Origin()
	if orig.Pkg() == nil || orig.Name() != "ToImmutable" {
		return false
	}
	switch orig.Pkg().Path() {
	case "slices", "maps", "container/list", "container/ring":
		return true
	case "sync/v2/container":
		sig, ok := orig.Type().(*types2.Signature)
		if ok && sig.Recv() != nil {
			return true
		}
		return isImmutableContainerType(resType)
	}
	return false
}

func isMutatingContainerMethod(fn *types2.Func) bool {
	if fn == nil {
		return false
	}
	orig := fn.Origin()
	if orig.Pkg() == nil {
		return false
	}
	sig, ok := orig.Type().(*types2.Signature)
	if !ok || sig.Recv() == nil {
		return false
	}
	named, ok := types2.Unalias(deref2(sig.Recv().Type())).(*types2.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return false
	}
	pkgPath := named.Obj().Pkg().Path()
	typeName := named.Obj().Name()
	mName := orig.Name()

	switch pkgPath {
	case "container/list":
		if typeName == "List" {
			switch mName {
			case "Len", "Front", "Back", "IsImmutable", "ToImmutable":
				return false
			default:
				return true
			}
		}
	case "container/ring":
		if typeName == "Ring" {
			switch mName {
			case "Next", "Prev", "Move", "Len", "Do", "IsImmutable", "ToImmutable":
				return false
			default:
				return true
			}
		}
	case "sync/v2/container":
		switch typeName {
		case "LinkedHashMap":
			switch mName {
			case "Len", "Peek", "Load", "First", "Last", "All", "Backward", "Range", "IsImmutable", "ToImmutable":
				return false
			default:
				return true
			}
		case "OrderedMap":
			switch mName {
			case "Load", "All", "Range", "IsImmutable", "ToImmutable":
				return false
			default:
				return true
			}
		case "TreeMap", "ConcurrentSkipListMap":
			switch mName {
			case "Len", "Load", "First", "Last", "Floor", "Lower", "Ceiling", "Higher", "All", "Backward", "RangeBetween", "Range", "IsImmutable", "ToImmutable":
				return false
			default:
				return true
			}
		}
	}
	return false
}

func isRingLinkMethod(fn *types2.Func) bool {
	if fn == nil {
		return false
	}
	orig := fn.Origin()
	if orig.Pkg() == nil || orig.Pkg().Path() != "container/ring" || orig.Name() != "Link" {
		return false
	}
	sig, ok := orig.Type().(*types2.Signature)
	return ok && sig.Recv() != nil
}

func isRingNavMethod(fn *types2.Func) bool {
	if fn == nil {
		return false
	}
	orig := fn.Origin()
	if orig.Pkg() == nil || orig.Pkg().Path() != "container/ring" {
		return false
	}
	switch orig.Name() {
	case "Next", "Prev", "Move":
		sig, ok := orig.Type().(*types2.Signature)
		return ok && sig.Recv() != nil
	}
	return false
}

func isListOrRingElemNavMethod(fn *types2.Func) bool {
	if isRingNavMethod(fn) {
		return true
	}
	if fn == nil {
		return false
	}
	orig := fn.Origin()
	if orig.Pkg() == nil || orig.Pkg().Path() != "container/list" {
		return false
	}
	switch orig.Name() {
	case "Front", "Back", "Next", "Prev":
		sig, ok := orig.Type().(*types2.Signature)
		return ok && sig.Recv() != nil
	}
	return false
}

func (pw *pkgWriter) checkImmutable(noders []*noder) {
	calledNames := make(map[*syntax.Name]bool)
	for _, p := range noders {
		v := &immutableChecker{
			pw:          pw,
			atPkgLevel:  true,
			calledNames: calledNames,
		}
		syntax.Walk(p.file, v)
	}
}

type immutableChecker struct {
	pw          *pkgWriter
	inInit      bool
	atPkgLevel  bool
	curFunc     *types2.Func
	curFuncInfo *ir.ImmutableFuncInfo
	inClosure   bool
	calledNames map[*syntax.Name]bool
}

func resolveCallTarget(pw *pkgWriter, fun syntax.Expr) (fn *types2.Func, targetIdent *syntax.Name, isMethodExpr bool) {
	cur := syntax.Unparen(fun)
	if tv, ok := pw.maybeTypeAndValue(cur); ok && tv.IsType() {
		return nil, nil, false
	}
	if idx, ok := cur.(*syntax.IndexExpr); ok {
		args := syntax.UnpackListExpr(idx.Index)
		isTypeArgs := len(args) > 1
		if len(args) == 1 {
			if tv, ok := pw.maybeTypeAndValue(args[0]); ok && tv.IsType() {
				isTypeArgs = true
			}
		}
		if isTypeArgs {
			cur = syntax.Unparen(idx.X)
		}
	}
	switch e := cur.(type) {
	case *syntax.Name:
		if f, ok := pw.info.Uses[e].(*types2.Func); ok {
			return f, e, false
		}
	case *syntax.SelectorExpr:
		if sel := pw.info.Selections[e]; sel != nil {
			if f, ok := sel.Obj().(*types2.Func); ok {
				return f, e.Sel, sel.Kind() == types2.MethodExpr
			}
			return nil, nil, false
		}
		if isPkgQual(pw.info, e) {
			if f, ok := pw.info.Uses[e.Sel].(*types2.Func); ok {
				return f, e.Sel, false
			}
		}
	}
	return nil, nil, false
}

func (c *immutableChecker) Visit(n syntax.Node) syntax.Visitor {
	if n == nil {
		return nil
	}
	pw := c.pw

	switch n := n.(type) {
	case *syntax.FuncDecl:
		var fn *types2.Func
		if n.Name != nil {
			fn, _ = pw.info.Defs[n.Name].(*types2.Func)
		}
		copy := *c
		copy.atPkgLevel = false
		copy.inInit = n.Recv == nil && n.Name != nil && n.Name.Value == "init"
		copy.curFunc = fn
		copy.curFuncInfo = pw.immutableFuncs[fn]
		copy.inClosure = false
		return &copy

	case *syntax.FuncLit:
		// Closures can escape and run after initialization completes, and
		// have their own return statements independent of curFunc.
		copy := *c
		copy.atPkgLevel = false
		copy.inInit = false
		copy.inClosure = true
		return &copy

	case *syntax.Name:
		if inst, ok := pw.info.Instances[n]; ok {
			c.checkInstanceConstraints(n, inst)
		}
		if !c.calledNames[n] {
			if fn, ok := pw.info.Uses[n].(*types2.Func); ok {
				if info := pw.immutableFuncInfo(fn); info != nil && len(info.Params) > 0 {
					pw.errorf(n, "cannot use //go:immutable function %s as function value", fn.Name())
				}
			}
		}

	case *syntax.AssignStmt:
		isOwnDef := pw.immutableDefStmts[n]
		lhsList := syntax.UnpackListExpr(n.Lhs)
		rhsList := syntax.UnpackListExpr(n.Rhs)
		if n.Lhs != nil {
			for i, lhs := range lhsList {
				if isOwnDef {
					if name, ok := syntax.Unparen(lhs).(*syntax.Name); ok && pw.info.Defs[name] != nil {
						continue
					}
				}
				if n.Op == syntax.Def {
					// Short var decl redeclaring an existing variable in the same scope.
					if name, ok := syntax.Unparen(lhs).(*syntax.Name); ok {
						if obj, ok := pw.info.Uses[name].(*types2.Var); ok {
							if pw.isImmutableVar(obj) {
								if !(c.inInit && obj.Parent() == pw.curpkg.Scope()) {
									pw.errorf(lhs, "cannot assign to //go:immutable variable %s", obj.Name())
								}
							} else if pw.immutableResultVars[obj] {
								if !c.checkAssignedResultRHS(rhsList, i, len(lhsList)) {
									pw.errorf(lhs, "cannot assign non-immutable value to //go:immutable result %s", obj.Name())
								}
							}
						}
					}
					continue
				}

				if n.Op == 0 {
					if name, ok := syntax.Unparen(lhs).(*syntax.Name); ok {
						if obj, ok := pw.info.Uses[name].(*types2.Var); ok && pw.immutableResultVars[obj] {
							if !c.checkAssignedResultRHS(rhsList, i, len(lhsList)) {
								pw.errorf(lhs, "cannot assign non-immutable value to //go:immutable result %s", obj.Name())
							}
							continue
						}
					}
				}

				c.checkMutationExpr(lhs, "assign to")
			}
		}

	case *syntax.ReturnStmt:
		if !c.inClosure && c.curFuncInfo != nil && len(c.curFuncInfo.Results) > 0 {
			c.checkReturnStmt(n)
		}

	case *syntax.ForStmt:
		if rc, ok := n.Init.(*syntax.RangeClause); ok && rc.Lhs != nil && !rc.Def {
			for _, lhs := range syntax.UnpackListExpr(rc.Lhs) {
				c.checkMutationExpr(lhs, "assign to")
			}
		}

	case *syntax.CommClause:
		// If Comm is an AssignStmt like `case out = <-ch:`, AssignStmt visitor handles it.

	case *syntax.Operation:
		if n.Op == syntax.And && n.Y == nil {
			c.checkMutationExpr(n.X, "take address of")
		}

	case *syntax.SelectorExpr:
		if sel := pw.info.Selections[n]; sel != nil && sel.Kind() == types2.MethodVal {
			if fn, ok := sel.Obj().(*types2.Func); ok {
				if isMutatingContainerMethod(fn) {
					if obj := pw.unwrapImmutableContainer(n.X); obj != nil {
						if !(c.inInit && obj.Parent() == pw.curpkg.Scope()) {
							pw.errorf(n, "cannot call mutating method %s on //go:immutable variable %s", fn.Name(), obj.Name())
						}
						break
					}
				}
				if recv := fn.Signature().Recv(); recv != nil {
					if _, recvIsPtr := types2.CommonUnderlying(recv.Type()).(*types2.Pointer); recvIsPtr {
						if xt := pw.info.TypeOf(n.X); xt != nil {
							if _, xIsPtr := types2.CommonUnderlying(xt).(*types2.Pointer); !xIsPtr && !selectionTraversesPtr(xt, sel.Index()) {
								c.checkMutationExpr(n.X, "call pointer-receiver method on")
							}
						}
					}
				}
			}
		}

	case *syntax.SliceExpr:
		if obj := pw.unwrapDirectCollection(n.X); obj != nil {
			if _, isArr := types2.CommonUnderlying(obj.Type()).(*types2.Array); isArr {
				if !(c.inInit && obj.Parent() == pw.curpkg.Scope()) {
					if pw.immutableResultVars[obj] {
						pw.errorf(n, "cannot slice //go:immutable result %s", obj.Name())
					} else {
						pw.errorf(n, "cannot slice //go:immutable array %s", obj.Name())
					}
				}
				break
			}
		}
		if xt := pw.info.TypeOf(n.X); xt != nil {
			if _, isArr := types2.CommonUnderlying(xt).(*types2.Array); isArr {
				c.checkMutationExpr(n.X, "slice")
			}
		}

	case *syntax.CallExpr:
		c.checkCallExpr(n)
	}

	return c
}

func (c *immutableChecker) checkInstanceConstraints(id *syntax.Name, inst types2.Instance) {
	pw := c.pw
	origObj := pw.info.Uses[id]
	if origObj == nil || inst.TypeArgs == nil {
		return
	}
	var tparams *types2.TypeParamList
	switch o := origObj.(type) {
	case *types2.Func:
		tparams = o.Origin().Type().(*types2.Signature).TypeParams()
	case *types2.TypeName:
		switch t := o.Type().(type) {
		case *types2.Named:
			tparams = t.Origin().TypeParams()
		case *types2.Alias:
			tparams = t.Origin().TypeParams()
		}
	}
	if tparams == nil {
		return
	}
	for k := 0; k < tparams.Len() && k < inst.TypeArgs.Len(); k++ {
		constraint := tparams.At(k).Constraint()
		iface, ok := types2.CommonUnderlying(constraint).(*types2.Interface)
		if !ok || iface.NumMethods() == 0 {
			continue
		}
		targ := inst.TypeArgs.At(k)
		if targ == nil || types2.IsInterface(targ) {
			continue
		}
		for mi := range iface.NumMethods() {
			im := iface.Method(mi)
			mobj, _, _ := types2.LookupFieldOrMethod(targ, false, im.Pkg(), im.Name())
			if mfn, ok := mobj.(*types2.Func); ok {
				if minfo := pw.immutableFuncInfo(mfn); minfo != nil && len(minfo.Params) > 0 {
					qf := types2.RelativeTo(pw.curpkg)
					pw.errorf(id, "%s does not satisfy %s (%s method has //go:immutable parameter constraints)",
						types2.TypeString(targ, qf), types2.TypeString(constraint, qf), im.Name())
					break
				}
			}
		}
	}
}

func (c *immutableChecker) checkCallExpr(n *syntax.CallExpr) {
	pw := c.pw
	funExpr := syntax.Unparen(n.Fun)

	// Check type conversions such as (*[N]T)(immSlice) and Interface(concreteWithImmutableMethod).
	if tv, ok := pw.maybeTypeAndValue(funExpr); ok && tv.IsType() && len(n.ArgList) == 1 {
		if ptr, isPtr := types2.CommonUnderlying(tv.Type).(*types2.Pointer); isPtr {
			if _, isArr := types2.CommonUnderlying(ptr.Elem()).(*types2.Array); isArr {
				if obj := pw.unwrapDirectCollection(n.ArgList[0]); obj != nil {
					if _, isSlice := types2.CommonUnderlying(obj.Type()).(*types2.Slice); isSlice {
						if !(c.inInit && obj.Parent() == pw.curpkg.Scope()) {
							pw.errorf(n, "cannot convert //go:immutable slice %s to array pointer", obj.Name())
						}
					}
				}
			}
		}
		return
	}

	if fun, ok := funExpr.(*syntax.Name); ok {
		if b, ok := pw.info.Uses[fun].(*types2.Builtin); ok && len(n.ArgList) > 0 {
			switch b.Name() {
			case "delete":
				if obj := pw.unwrapDirectCollection(n.ArgList[0]); obj != nil {
					if !(c.inInit && obj.Parent() == pw.curpkg.Scope()) {
						pw.errorf(n.ArgList[0], "cannot delete from //go:immutable map %s", obj.Name())
					}
				}
			case "clear":
				if obj := pw.unwrapDirectCollection(n.ArgList[0]); obj != nil {
					if !(c.inInit && obj.Parent() == pw.curpkg.Scope()) {
						pw.errorf(n.ArgList[0], "cannot clear //go:immutable variable %s", obj.Name())
					}
				}
			case "copy":
				if obj := pw.unwrapDirectCollection(n.ArgList[0]); obj != nil {
					if !(c.inInit && obj.Parent() == pw.curpkg.Scope()) {
						pw.errorf(n.ArgList[0], "cannot copy into //go:immutable slice %s", obj.Name())
					}
				}
			case "append":
				if len(n.ArgList) > 1 {
					dst := syntax.Unparen(n.ArgList[0])
					if se, isSlice := dst.(*syntax.SliceExpr); isSlice && !pw.isFullClippedSliceExpr(se) {
						if obj := pw.unwrapDirectCollection(se); obj != nil {
							if !(c.inInit && obj.Parent() == pw.curpkg.Scope()) {
								pw.errorf(n.ArgList[0], "cannot append into unclipped subslice of //go:immutable slice %s", obj.Name())
							}
						}
					} else if name, isName := dst.(*syntax.Name); isName {
						if obj, ok := pw.info.Uses[name].(*types2.Var); ok && (pw.immutableParamVars[obj] || pw.immutableResultVars[obj]) {
							pw.errorf(n.ArgList[0], "cannot append directly to //go:immutable slice %s without clipping capacity", obj.Name())
						}
					}
				}
			}
			return
		}
	}

	fn, targetIdent, isMethodExpr := resolveCallTarget(pw, n.Fun)
	if fn == nil {
		return
	}
	if targetIdent != nil {
		c.calledNames[targetIdent] = true
	}

	if isMethodExpr && len(n.ArgList) > 0 && isMutatingContainerMethod(fn) {
		if obj := pw.unwrapImmutableContainer(n.ArgList[0]); obj != nil {
			if !(c.inInit && obj.Parent() == pw.curpkg.Scope()) {
				pw.errorf(n.ArgList[0], "cannot call mutating method %s on //go:immutable variable %s", fn.Name(), obj.Name())
			}
		}
	}

	if isRingLinkMethod(fn) {
		argIdx := 0
		if isMethodExpr {
			argIdx = 1
		}
		if argIdx < len(n.ArgList) {
			if obj := pw.unwrapImmutableContainer(n.ArgList[argIdx]); obj != nil {
				if !(c.inInit && obj.Parent() == pw.curpkg.Scope()) {
					pw.errorf(n.ArgList[argIdx], "cannot link //go:immutable variable %s", obj.Name())
				}
			}
		}
	}

	info := pw.immutableFuncInfo(fn)
	if info == nil || len(info.Params) == 0 {
		return
	}

	sig := fn.Type().(*types2.Signature)
	if tv, ok := pw.maybeTypeAndValue(n.Fun); ok {
		if callSig, ok := tv.Type.(*types2.Signature); ok {
			sig = callSig
		}
	}

	// Tuple-forwarding call f(g()).
	if len(n.ArgList) == 1 && !n.HasDots {
		if tv, ok := pw.maybeTypeAndValue(n.ArgList[0]); ok {
			if tuple, isTuple := tv.Type.(*types2.Tuple); isTuple {
				offset := 0
				paramCount := sig.Params().Len()
				if isMethodExpr {
					offset = 1
					if sig.Recv() == nil && paramCount > 0 {
						paramCount--
					}
				}
				for _, pIdx := range info.Params {
					if sig.Variadic() && pIdx == paramCount-1 {
						if tuple.Len() > pIdx+offset {
							pw.errorf(n.ArgList[0], "cannot pass mutable variadic arguments as //go:immutable parameter args%d in call to %s", pIdx, fn.Name())
						}
						continue
					}
					if !c.isImmutableTupleElem(n.ArgList[0], pIdx+offset) {
						pw.errorf(n.ArgList[0], "cannot pass non-immutable value as //go:immutable parameter args%d in call to %s", pIdx, fn.Name())
					}
				}
				return
			}
		}
	}

	args := n.ArgList
	paramCount := sig.Params().Len()
	if isMethodExpr {
		if len(args) == 0 {
			return
		}
		args = args[1:]
		// Note: when tv.Type came from MethodExpr, sig.Params() includes receiver at index 0.
		if sig.Recv() == nil && paramCount > 0 {
			paramCount--
		}
	}

	for _, pIdx := range info.Params {
		if sig.Variadic() && pIdx == paramCount-1 {
			if pIdx == len(args) {
				// Empty variadic pack evaluates to nil slice.
				continue
			}
			if n.HasDots && pIdx == len(args)-1 {
				if !c.checkImmutableArgExpr(args[pIdx], pIdx, fn.Name()) {
					continue
				}
				continue
			}
			pw.errorf(n, "cannot pass mutable variadic arguments as //go:immutable parameter args%d in call to %s", pIdx, fn.Name())
			continue
		}
		if pIdx < len(args) {
			c.checkImmutableArgExpr(args[pIdx], pIdx, fn.Name())
		}
	}
}

func (c *immutableChecker) checkImmutableArgExpr(arg syntax.Expr, pIdx int, fnName string) bool {
	ok, uninitGlobal := c.evalImmutableExpr(arg)
	if ok {
		return true
	}
	if uninitGlobal != nil {
		c.pw.errorf(arg, "cannot pass package-level //go:immutable variable %s before package initialization completes", uninitGlobal.Name())
		return false
	}
	c.pw.errorf(arg, "cannot pass non-immutable argument as //go:immutable parameter args%d in call to %s", pIdx, fnName)
	return false
}

func (c *immutableChecker) checkReturnStmt(n *syntax.ReturnStmt) {
	pw := c.pw
	results := syntax.UnpackListExpr(n.Results)
	if len(results) == 0 {
		// Bare return: named result variables are guarded at assignment sites.
		return
	}

	sig := c.curFunc.Type().(*types2.Signature)
	if len(results) == 1 && sig.Results().Len() > 1 {
		// Multi-value return forwarding: return g()
		for _, rIdx := range c.curFuncInfo.Results {
			if !c.isImmutableTupleElem(results[0], rIdx) {
				pw.errorf(results[0], "cannot return non-immutable value for //go:immutable result r%d of %s", rIdx, c.curFunc.Name())
			}
		}
		return
	}

	for _, rIdx := range c.curFuncInfo.Results {
		if rIdx >= len(results) {
			continue
		}
		ok, uninitGlobal := c.evalImmutableExpr(results[rIdx])
		if !ok {
			if uninitGlobal != nil {
				pw.errorf(results[rIdx], "cannot return package-level //go:immutable variable %s before package initialization completes", uninitGlobal.Name())
			} else {
				pw.errorf(results[rIdx], "cannot return non-immutable value for //go:immutable result r%d of %s", rIdx, c.curFunc.Name())
			}
		}
	}
}

func (c *immutableChecker) checkAssignedResultRHS(rhsList []syntax.Expr, lhsIdx, lhsCount int) bool {
	if len(rhsList) == 0 {
		return false
	}
	if len(rhsList) == 1 && lhsCount > 1 {
		return c.isImmutableTupleElem(rhsList[0], lhsIdx)
	}
	if lhsIdx >= len(rhsList) {
		return false
	}
	ok, _ := c.evalImmutableExpr(rhsList[lhsIdx])
	return ok
}

func (c *immutableChecker) isImmutableTupleElem(expr syntax.Expr, idx int) bool {
	call, ok := syntax.Unparen(expr).(*syntax.CallExpr)
	if !ok {
		return false
	}
	fn, _, _ := resolveCallTarget(c.pw, call.Fun)
	if fn == nil {
		return false
	}
	info := c.pw.immutableFuncInfo(fn)
	return info != nil && slices.Contains(info.Results, idx)
}

func (c *immutableChecker) isNilValueExpr(expr syntax.Expr) bool {
	e := syntax.Unparen(expr)
	if tv, ok := c.pw.maybeTypeAndValue(e); ok && tv.IsNil() {
		return true
	}
	if name, ok := e.(*syntax.Name); ok {
		if _, isNil := c.pw.info.Uses[name].(*types2.Nil); isNil {
			return true
		}
	}
	return false
}

// evalImmutableExpr reports whether expr is statically guaranteed to evaluate
// to an immutable value. If expr references a same-package global collection
// variable during package initialization (before runtime freezing runs), it
// returns (false, globalVar).
func (c *immutableChecker) evalImmutableExpr(expr syntax.Expr) (bool, *types2.Var) {
	pw := c.pw
	e := syntax.Unparen(expr)
	if e == nil {
		return false, nil
	}
	if c.isNilValueExpr(e) {
		return true, nil
	}

	switch n := e.(type) {
	case *syntax.Name:
		if obj, ok := pw.info.Uses[n].(*types2.Var); ok {
			if pw.immutableResultVars[obj] {
				return true, nil
			}
			if pw.isImmutableVar(obj) {
				if (c.inInit || c.atPkgLevel) && obj.Parent() == pw.curpkg.Scope() {
					if _, isArr := types2.CommonUnderlying(obj.Type()).(*types2.Array); !isArr {
						return false, obj
					}
				}
				return true, nil
			}
		}
		return false, nil

	case *syntax.SelectorExpr:
		if pw.info.Selections[n] == nil && isPkgQual(pw.info, n) {
			if obj, ok := pw.info.Uses[n.Sel].(*types2.Var); ok && pw.isImmutableVar(obj) {
				return true, nil
			}
		}
		return false, nil

	case *syntax.SliceExpr:
		xt := pw.info.TypeOf(n.X)
		if xt == nil {
			return false, nil
		}
		if _, isSlice := types2.CommonUnderlying(xt).(*types2.Slice); !isSlice {
			return false, nil
		}
		return c.evalImmutableExpr(n.X)

	case *syntax.CallExpr:
		funExpr := syntax.Unparen(n.Fun)
		// Type conversion T(arg): allow T(nil) on nilable supported types, or
		// underlying-preserving conversions such as NamedSlice(immSlice).
		if tv, ok := pw.maybeTypeAndValue(funExpr); ok && tv.IsType() && len(n.ArgList) == 1 {
			dstType := tv.Type
			if !isImmutableSupportedType(dstType) {
				return false, nil
			}
			arg := n.ArgList[0]
			if c.isNilValueExpr(arg) {
				if _, isArr := types2.CommonUnderlying(dstType).(*types2.Array); !isArr {
					return true, nil
				}
				return false, nil
			}
			srcType := pw.info.TypeOf(arg)
			if srcType == nil || !sameImmutableCarrierKind(dstType, srcType) {
				return false, nil
			}
			return c.evalImmutableExpr(arg)
		}

		fn, _, isMethodExpr := resolveCallTarget(pw, n.Fun)
		if fn == nil {
			return false, nil
		}
		if isStdToImmutableFunc(fn, pw.info.TypeOf(n)) {
			return true, nil
		}
		if isRingNavMethod(fn) {
			if isMethodExpr {
				if len(n.ArgList) > 0 {
					return c.evalImmutableExpr(n.ArgList[0])
				}
				return false, nil
			}
			if sel, ok := funExpr.(*syntax.SelectorExpr); ok {
				return c.evalImmutableExpr(sel.X)
			}
		}
		if info := pw.immutableFuncInfo(fn); info != nil && slices.Contains(info.Results, 0) {
			return true, nil
		}
		return false, nil
	}

	return false, nil
}

func (pw *pkgWriter) isFullClippedSliceExpr(se *syntax.SliceExpr) bool {
	if !se.Full || se.Index[1] == nil || se.Index[2] == nil {
		return false
	}
	hi := syntax.Unparen(se.Index[1])
	mx := syntax.Unparen(se.Index[2])
	if tv1, ok1 := pw.maybeTypeAndValue(hi); ok1 && tv1.Value != nil {
		if tv2, ok2 := pw.maybeTypeAndValue(mx); ok2 && tv2.Value != nil {
			return tv1.Value.ExactString() == tv2.Value.ExactString()
		}
	}
	if n1, ok1 := hi.(*syntax.Name); ok1 {
		if n2, ok2 := mx.(*syntax.Name); ok2 {
			return pw.info.Uses[n1] != nil && pw.info.Uses[n1] == pw.info.Uses[n2]
		}
	}
	return false
}

func sameImmutableCarrierKind(dst, src types2.Type) bool {
	du := types2.CommonUnderlying(dst)
	su := types2.CommonUnderlying(src)
	switch du.(type) {
	case *types2.Slice:
		_, ok := su.(*types2.Slice)
		return ok
	case *types2.Map:
		_, ok := su.(*types2.Map)
		return ok
	case *types2.Array:
		_, ok := su.(*types2.Array)
		return ok
	}
	return isImmutableContainerType(dst) && isImmutableContainerType(src) && types2.Identical(du, su)
}

func selectionTraversesPtr(xt types2.Type, indices []int) bool {
	if len(indices) <= 1 || xt == nil {
		return false
	}
	cur := types2.CommonUnderlying(xt)
	if _, isPtr := cur.(*types2.Pointer); isPtr {
		return true
	}
	for _, idx := range indices[:len(indices)-1] {
		st, ok := cur.(*types2.Struct)
		if !ok || idx < 0 || idx >= st.NumFields() {
			return false
		}
		ft := types2.CommonUnderlying(st.Field(idx).Type())
		if _, isPtr := ft.(*types2.Pointer); isPtr {
			return true
		}
		cur = ft
	}
	return false
}

func (c *immutableChecker) checkMutationExpr(expr syntax.Expr, verb string) {
	obj, ok := c.pw.immutableRootVar(expr)
	if !ok {
		return
	}
	// Package-level //go:immutable vars may be populated directly inside init().
	if c.inInit && obj.Parent() == c.pw.curpkg.Scope() {
		return
	}
	if c.pw.immutableResultVars[obj] {
		c.pw.errorf(expr, "cannot %s //go:immutable result %s", verb, obj.Name())
		return
	}
	c.pw.errorf(expr, "cannot %s //go:immutable variable %s", verb, obj.Name())
}

func (pw *pkgWriter) isTrackedImmutableObj(obj *types2.Var) bool {
	return obj != nil && (pw.isImmutableVar(obj) || pw.immutableResultVars[obj])
}

// unwrapImmutableContainer returns the immutable container variable if e is
// directly that variable or a Ring/List element navigation chain from it.
func (pw *pkgWriter) unwrapImmutableContainer(e syntax.Expr) *types2.Var {
	for {
		switch n := syntax.Unparen(e).(type) {
		case *syntax.Name:
			if obj, ok := pw.info.Uses[n].(*types2.Var); ok && pw.isTrackedImmutableObj(obj) && isImmutableContainerType(obj.Type()) {
				return obj
			}
			return nil
		case *syntax.SelectorExpr:
			if pw.info.Selections[n] == nil && isPkgQual(pw.info, n) {
				if obj, ok := pw.info.Uses[n.Sel].(*types2.Var); ok && pw.isTrackedImmutableObj(obj) && isImmutableContainerType(obj.Type()) {
					return obj
				}
			}
			return nil
		case *syntax.CallExpr:
			fn, _, isMethodExpr := resolveCallTarget(pw, n.Fun)
			if !isListOrRingElemNavMethod(fn) {
				return nil
			}
			if isMethodExpr {
				if len(n.ArgList) == 0 {
					return nil
				}
				e = n.ArgList[0]
				continue
			}
			if sel, ok := syntax.Unparen(n.Fun).(*syntax.SelectorExpr); ok {
				e = sel.X
				continue
			}
			return nil
		default:
			return nil
		}
	}
}

// unwrapDirectCollection returns the immutable variable if e is directly v or
// a sub-slice expression on v.
func (pw *pkgWriter) unwrapDirectCollection(e syntax.Expr) *types2.Var {
	for {
		switch n := syntax.Unparen(e).(type) {
		case *syntax.Name:
			if obj, ok := pw.info.Uses[n].(*types2.Var); ok && pw.isTrackedImmutableObj(obj) {
				return obj
			}
			return nil
		case *syntax.SelectorExpr:
			if pw.info.Selections[n] == nil {
				if obj, ok := pw.info.Uses[n.Sel].(*types2.Var); ok && pw.isTrackedImmutableObj(obj) {
					return obj
				}
			}
			return nil
		case *syntax.SliceExpr:
			e = n.X
		default:
			return nil
		}
	}
}

// immutableRootVar reports whether assigning to or taking the address of e
// mutates an immutable variable itself or its inline backing storage.
func (pw *pkgWriter) immutableRootVar(e syntax.Expr) (*types2.Var, bool) {
	cur := syntax.Unparen(e)
	for {
		switch n := cur.(type) {
		case *syntax.ParenExpr:
			cur = n.X
		case *syntax.Name:
			if obj, ok := pw.info.Uses[n].(*types2.Var); ok && pw.isTrackedImmutableObj(obj) {
				return obj, true
			}
			return nil, false
		case *syntax.Operation:
			if n.Op == syntax.Mul && n.Y == nil {
				if obj := pw.unwrapImmutableContainer(n.X); obj != nil {
					return obj, true
				}
			}
			return nil, false
		case *syntax.SelectorExpr:
			sel := pw.info.Selections[n]
			if sel == nil {
				if obj, ok := pw.info.Uses[n.Sel].(*types2.Var); ok && pw.isTrackedImmutableObj(obj) {
					return obj, true
				}
				return nil, false
			}
			if obj := pw.unwrapImmutableContainer(n.X); obj != nil && len(sel.Index()) == 1 {
				return obj, true
			}
			xt := pw.info.TypeOf(n.X)
			if xt == nil {
				return nil, false
			}
			if _, isPtr := types2.CommonUnderlying(xt).(*types2.Pointer); isPtr {
				return nil, false
			}
			if selectionTraversesPtr(xt, sel.Index()) {
				return nil, false
			}
			cur = n.X
		case *syntax.IndexExpr:
			if obj := pw.unwrapDirectCollection(n.X); obj != nil {
				return obj, true
			}
			xt := pw.info.TypeOf(n.X)
			if xt == nil {
				return nil, false
			}
			if _, isArr := types2.CommonUnderlying(xt).(*types2.Array); isArr {
				cur = n.X
				continue
			}
			return nil, false
		case *syntax.SliceExpr:
			if obj := pw.unwrapDirectCollection(n.X); obj != nil {
				return obj, true
			}
			return nil, false
		default:
			return nil, false
		}
	}
}
