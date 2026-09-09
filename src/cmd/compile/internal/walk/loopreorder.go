// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package walk

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"go/constant"
)

// reorderLoops runs before order lowers array accesses and loop initializers.
func reorderLoops(fn *ir.Func) {
	var edit func(ir.Node) ir.Node
	edit = func(n ir.Node) ir.Node {
		if loop, ok := n.(*ir.ForStmt); ok {
			if replacement := reorderLoop(fn, loop); replacement != nil {
				return replacement
			}
		}
		ir.EditChildren(n, edit)
		return n
	}
	for i, n := range fn.Body {
		fn.Body[i] = edit(n)
	}
}

// countedLoop recognizes for i := lo; i < hi; i++ with a private int index.
func countedLoop(loop *ir.ForStmt) (index *ir.Name, init *ir.AssignStmt, lo, hi int64) {
	if loop.Label != nil || loop.DistinctVars || len(loop.Init()) != 1 {
		return
	}
	init, ok := loop.Init()[0].(*ir.AssignStmt)
	if !ok || !init.Def || !ir.IsConst(init.Y, constant.Int) || len(init.Y.Init()) != 0 {
		return nil, nil, 0, 0
	}
	index, ok = init.X.(*ir.Name)
	if !ok || index.Class != ir.PAUTO || index.Addrtaken() || index.IsClosureVar() || index.Type().Kind() != types.TINT {
		return nil, nil, 0, 0
	}
	// Only the index declaration may accompany its initialization.
	for _, n := range init.Init() {
		if d, ok := n.(*ir.Decl); !ok || d.Op() != ir.ODCL || d.X != index {
			return nil, nil, 0, 0
		}
	}
	cond, ok := loop.Cond.(*ir.BinaryExpr)
	if !ok || cond.Op() != ir.OLT || cond.X != index || len(cond.Init()) != 0 || !ir.IsConst(cond.Y, constant.Int) || len(cond.Y.Init()) != 0 {
		return nil, nil, 0, 0
	}
	post, ok := loop.Post.(*ir.AssignOpStmt)
	if !ok || len(post.Init()) != 0 || post.AsOp != ir.OADD || post.X != index || !ir.IsConst(post.Y, constant.Int) || len(post.Y.Init()) != 0 || ir.Int64Val(post.Y) != 1 {
		return nil, nil, 0, 0
	}
	lo, hi = ir.Int64Val(init.Y), ir.Int64Val(cond.Y)
	// Leave room for tile increments even on 32-bit targets.
	if lo < 0 || hi <= lo || hi > 1<<30 || hi-lo < 2 {
		return nil, nil, 0, 0
	}
	return index, init, lo, hi
}

func reorderLoop(fn *ir.Func, outer *ir.ForStmt) ir.Node {
	if len(outer.Body) != 1 {
		return nil
	}
	inner, ok := outer.Body[0].(*ir.ForStmt)
	if !ok {
		return nil
	}
	i, iinit, ilo, ihi := countedLoop(outer)
	j, jinit, jlo, jhi := countedLoop(inner)
	if i == nil || j == nil || i == j {
		return nil
	}

	orientations := make(map[*ir.Name]int)
	row, column := false, false
	access := func(n ir.Node) bool {
		x, ok := n.(*ir.IndexExpr)
		if !ok || x.Op() != ir.OINDEX || len(x.Init()) != 0 {
			return false
		}
		y, ok := x.X.(*ir.IndexExpr)
		if !ok || y.Op() != ir.OINDEX || len(y.Init()) != 0 {
			return false
		}
		a, ok := y.X.(*ir.Name)
		if !ok || a.Addrtaken() || a.IsClosureVar() ||
			(a.Class != ir.PAUTO && a.Class != ir.PPARAM && a.Class != ir.PPARAMOUT) ||
			!a.Type().IsArray() || !a.Type().Elem().IsArray() ||
			(!x.Type().IsInteger() && !x.Type().IsFloat()) {
			return false
		}
		orientation, first, second := 0, int64(0), int64(0)
		switch {
		case y.Index == i && x.Index == j:
			orientation, first, second = 1, ihi, jhi
		case y.Index == j && x.Index == i:
			orientation, first, second = 2, jhi, ihi
		default:
			return false
		}
		if first > a.Type().NumElem() || second > a.Type().Elem().NumElem() {
			return false // Reordering a bounds panic could change visible results.
		}
		if previous := orientations[a]; previous != 0 && previous != orientation {
			return false
		}
		orientations[a] = orientation
		row = row || orientation == 1
		column = column || orientation == 2
		return true
	}
	var pure func(ir.Node) bool
	pure = func(n ir.Node) bool {
		if n == nil || len(n.Init()) != 0 {
			return false
		}
		switch n.Op() {
		case ir.OLITERAL:
			return n.Type().IsInteger() || n.Type().IsFloat()
		case ir.ONAME:
			return n == i || n == j
		case ir.OINDEX:
			return access(n)
		case ir.OADD, ir.OSUB, ir.OMUL, ir.OAND, ir.OOR, ir.OXOR, ir.OANDNOT:
			x := n.(*ir.BinaryExpr)
			return pure(x.X) && pure(x.Y)
		case ir.ONEG, ir.OPLUS, ir.OBITNOT:
			return pure(n.(*ir.UnaryExpr).X)
		case ir.OCONV, ir.OCONVNOP:
			x := n.(*ir.ConvExpr)
			return (x.Type().IsInteger() || x.Type().IsFloat()) && pure(x.X)
		}
		return false
	}
	for _, n := range inner.Body {
		if len(n.Init()) != 0 {
			return nil
		}
		switch n := n.(type) {
		case *ir.AssignStmt:
			if !access(n.X) || !pure(n.Y) {
				return nil
			}
		case *ir.AssignOpStmt:
			switch n.AsOp {
			case ir.OADD, ir.OSUB, ir.OMUL, ir.OAND, ir.OOR, ir.OXOR, ir.OANDNOT:
			default:
				return nil
			}
			if !access(n.X) || !pure(n.Y) {
				return nil
			}
		default:
			return nil
		}
	}
	if !column {
		return nil
	}
	if !row {
		outer.Body, inner.Body = inner.Body, ir.Nodes{outer}
		if base.Debug.LoopReorder != 0 {
			base.WarnfAt(outer.Pos(), "Interchanged loops")
		}
		return inner
	}
	const tileSize = 32
	if ihi-ilo < 2*tileSize || jhi-jlo < 2*tileSize {
		return nil
	}
	// Keep both original bounds for partial tiles. min is computed once per
	// tile, and tile+tileSize cannot overflow by countedLoop's bound check.
	tile := func(loop *ir.ForStmt, index *ir.Name, init *ir.AssignStmt, lo, hi int64) *ir.ForStmt {
		pos := loop.Pos()
		start := typecheck.TempAt(pos, fn, index.Type())
		end := typecheck.TempAt(pos, fn, index.Type())
		add := ir.NewBinaryExpr(pos, ir.OADD, start, ir.NewInt(pos, tileSize))
		limit := ir.NewCallExpr(pos, ir.OMIN, nil, []ir.Node{add, loop.Cond.(*ir.BinaryExpr).Y})
		endInit := typecheck.Stmt(ir.NewAssignStmt(pos, end, limit))
		init.Y = start
		loop.Cond.(*ir.BinaryExpr).Y = end
		tiled := ir.NewForStmt(pos,
			ir.NewAssignStmt(pos, start, ir.NewInt(pos, lo)),
			ir.NewBinaryExpr(pos, ir.OLT, start, ir.NewInt(pos, hi)),
			ir.NewAssignOpStmt(pos, ir.OADD, start, ir.NewInt(pos, tileSize)),
			[]ir.Node{endInit}, false)
		return tiled
	}
	itile := tile(outer, i, iinit, ilo, ihi)
	jtile := tile(inner, j, jinit, jlo, jhi)
	itile.Body = append(itile.Body, jtile)
	jtile.Body = append(jtile.Body, outer)
	if base.Debug.LoopReorder != 0 {
		base.WarnfAt(outer.Pos(), "Tiled loops (32x32)")
	}
	return typecheck.Stmt(itile)
}
