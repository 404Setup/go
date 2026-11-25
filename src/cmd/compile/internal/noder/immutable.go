// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types"
	"cmd/compile/internal/types2"
)

func isImmutableSupportedType(t types2.Type) bool {
	if t == nil {
		return false
	}
	switch types2.Unalias(t).Underlying().(type) {
	case *types2.Slice, *types2.Map, *types2.Array:
		return true
	}
	return false
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

func (pw *pkgWriter) checkImmutable(noders []*noder) {
	for _, p := range noders {
		v := &immutableChecker{
			pw: pw,
		}
		syntax.Walk(p.file, v)
	}
}

type immutableChecker struct {
	pw     *pkgWriter
	inInit bool
}

func (c *immutableChecker) Visit(n syntax.Node) syntax.Visitor {
	if n == nil {
		return nil
	}
	pw := c.pw

	switch n := n.(type) {
	case *syntax.FuncDecl:
		copy := *c
		copy.inInit = n.Recv == nil && n.Name != nil && n.Name.Value == "init"
		return &copy

	case *syntax.FuncLit:
		// Closures can escape and run after initialization completes.
		copy := *c
		copy.inInit = false
		return &copy

	case *syntax.AssignStmt:
		isOwnDef := pw.immutableDefStmts[n]
		if n.Lhs != nil {
			for _, lhs := range syntax.UnpackListExpr(n.Lhs) {
				if isOwnDef {
					if name, ok := syntax.Unparen(lhs).(*syntax.Name); ok && pw.info.Defs[name] != nil {
						continue
					}
				}
				if n.Op == syntax.Def {
					// Short var decl redeclaring an existing variable in the same scope.
					if name, ok := syntax.Unparen(lhs).(*syntax.Name); ok {
						if obj, ok := pw.info.Uses[name].(*types2.Var); ok && pw.isImmutableVar(obj) {
							if !(c.inInit && obj.Parent() == pw.curpkg.Scope()) {
								pw.errorf(lhs, "cannot assign to //go:immutable variable %s", obj.Name())
							}
						}
					}
					continue
				}
				c.checkMutationExpr(lhs, "assign to")
			}
		}

	case *syntax.ForStmt:
		if rc, ok := n.Init.(*syntax.RangeClause); ok && rc.Lhs != nil && !rc.Def {
			for _, lhs := range syntax.UnpackListExpr(rc.Lhs) {
				c.checkMutationExpr(lhs, "assign to")
			}
		}

	case *syntax.Operation:
		if n.Op == syntax.And && n.Y == nil {
			c.checkMutationExpr(n.X, "take address of")
		}

	case *syntax.SelectorExpr:
		if sel := pw.info.Selections[n]; sel != nil && sel.Kind() == types2.MethodVal {
			if fn, ok := sel.Obj().(*types2.Func); ok {
				if recv := fn.Signature().Recv(); recv != nil {
					if _, recvIsPtr := types2.Unalias(recv.Type()).Underlying().(*types2.Pointer); recvIsPtr {
						if xt := pw.info.TypeOf(n.X); xt != nil {
							if _, xIsPtr := types2.Unalias(xt).Underlying().(*types2.Pointer); !xIsPtr {
								c.checkMutationExpr(n.X, "call pointer-receiver method on")
							}
						}
					}
				}
			}
		}

	case *syntax.SliceExpr:
		if obj := pw.unwrapDirectCollection(n.X); obj != nil {
			if _, isArr := types2.Unalias(obj.Type()).Underlying().(*types2.Array); isArr {
				if !(c.inInit && obj.Parent() == pw.curpkg.Scope()) {
					pw.errorf(n, "cannot slice //go:immutable array %s", obj.Name())
				}
			}
		}

	case *syntax.CallExpr:
		if fun, ok := syntax.Unparen(n.Fun).(*syntax.Name); ok {
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
				}
			}
		}
	}

	return c
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
	c.pw.errorf(expr, "cannot %s //go:immutable variable %s", verb, obj.Name())
}

// unwrapDirectCollection returns the immutable variable if e is directly v or
// a sub-slice expression on v.
func (pw *pkgWriter) unwrapDirectCollection(e syntax.Expr) *types2.Var {
	for {
		switch n := syntax.Unparen(e).(type) {
		case *syntax.Name:
			if obj, ok := pw.info.Uses[n].(*types2.Var); ok && pw.isImmutableVar(obj) {
				return obj
			}
			return nil
		case *syntax.SelectorExpr:
			if pw.info.Selections[n] == nil {
				if obj, ok := pw.info.Uses[n.Sel].(*types2.Var); ok && pw.isImmutableVar(obj) {
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
			if obj, ok := pw.info.Uses[n].(*types2.Var); ok && pw.isImmutableVar(obj) {
				return obj, true
			}
			return nil, false
		case *syntax.SelectorExpr:
			if pw.info.Selections[n] == nil {
				if obj, ok := pw.info.Uses[n.Sel].(*types2.Var); ok && pw.isImmutableVar(obj) {
					return obj, true
				}
				return nil, false
			}
			xt := pw.info.TypeOf(n.X)
			if xt == nil {
				return nil, false
			}
			if _, isPtr := types2.Unalias(xt).Underlying().(*types2.Pointer); isPtr {
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
			if _, isArr := types2.Unalias(xt).Underlying().(*types2.Array); isArr {
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
