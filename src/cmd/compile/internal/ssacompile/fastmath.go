// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssacompile

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/ssa/ssaop"
	"math"
)

// fastmath permits reassociation, reciprocals, contraction, and identities that
// assume finite operands/results and ignore signed zero, like LLVM's fast-math
// flags. Math library substitution is handled by ssagen; the linker's -fmth
// flag separately enables the runtime's per-thread flush mode.
func fastmath(f *ssa.Func) {
	if !base.Flag.Fmth {
		return
	}
	// Share a reciprocal only when it replaces multiple divisions in a block.
	// A single variable division should not grow into a division and a multiply.
	shared := make(map[*ssa.Value]bool)
	for _, b := range f.Blocks {
		counts := make(map[*ssa.Value]int)
		for _, v := range b.Values {
			if v.Op == ssaop.OpDiv32F || v.Op == ssaop.OpDiv64F {
				counts[v.Args[1]]++
			}
		}
		for _, v := range b.Values {
			if (v.Op == ssaop.OpDiv32F || v.Op == ssaop.OpDiv64F) && counts[v.Args[1]] > 1 {
				shared[v] = true
			}
		}
	}
	applyRewrite(f, func(*ssa.Block) bool { return false }, func(v *ssa.Value) bool {
		return rewriteFastMath(v, shared[v])
	}, ssa.RemoveDeadValues)
	cse(f)
	opt(f)
}

func rewriteFastMath(v *ssa.Value, sharedDivisor bool) bool {
	// Explicit rounding normally blocks FMA contraction. Fast math permits it.
	if v.Op == ssaop.OpRound32F || v.Op == ssaop.OpRound64F {
		v.CopyOf(v.Args[0])
		return true
	}
	switch v.Op {
	case ssaop.OpAdd32F, ssaop.OpAdd64F, ssaop.OpSub32F, ssaop.OpSub64F,
		ssaop.OpMul32F, ssaop.OpMul64F, ssaop.OpDiv32F, ssaop.OpDiv64F,
		ssaop.OpEq32F, ssaop.OpEq64F, ssaop.OpLeq32F, ssaop.OpLeq64F,
		ssaop.OpNeq32F, ssaop.OpNeq64F, ssaop.OpLess32F, ssaop.OpLess64F:
	default:
		return false
	}
	x, y := v.Args[0], v.Args[1]
	constOp, addOp, subOp, mulOp, divOp := ssaop.OpConst64F, ssaop.OpAdd64F, ssaop.OpSub64F, ssaop.OpMul64F, ssaop.OpDiv64F
	if x.Type.Size() == 4 {
		constOp, addOp, subOp, mulOp, divOp = ssaop.OpConst32F, ssaop.OpAdd32F, ssaop.OpSub32F, ssaop.OpMul32F, ssaop.OpDiv32F
	}
	isConst := func(a *ssa.Value, c float64) bool { return a.Op == constOp && a.AuxFloat() == c }
	constant := func(c float64) bool {
		v.Reset(constOp)
		v.AuxInt = ssa.Float64ToAuxInt(c)
		return true
	}
	switch v.Op {
	case ssaop.OpAdd32F, ssaop.OpAdd64F, ssaop.OpMul32F, ssaop.OpMul64F:
		// Keep constants on the right, so reassociation has one direction.
		if x.Op == constOp && y.Op != constOp {
			v.SetArg(0, y)
			v.SetArg(1, x)
			return true
		}
		identity := 0.0
		if v.Op == mulOp {
			identity = 1
			if isConst(y, 0) {
				return constant(0)
			}
			// (x / y) * y -> x, in either operand order.
			if x.Op == divOp && x.Args[1] == y {
				v.CopyOf(x.Args[0])
				return true
			}
			if y.Op == divOp && y.Args[1] == x {
				v.CopyOf(y.Args[0])
				return true
			}
			// sqrt(x) * sqrt(x) -> x, allowing approximate intrinsic results.
			if x == y && (x.Op == ssaop.OpSqrt || x.Op == ssaop.OpSqrt32) {
				v.CopyOf(x.Args[0])
				return true
			}
		}
		if isConst(y, identity) {
			v.CopyOf(x)
			return true
		}
		// (x - y) + y, including the commuted form.
		if v.Op == addOp {
			if x.Op == subOp && x.Args[1] == y {
				v.CopyOf(x.Args[0])
				return true
			}
			if y.Op == subOp && y.Args[1] == x {
				v.CopyOf(y.Args[0])
				return true
			}
		}
		// (x op c1) op c2 -> x op (c1 op c2), exposing identities.
		if x.Op == v.Op && x.Uses == 1 && x.Args[1].Op == constOp && y.Op == constOp {
			c := x.Args[1].AuxFloat() + y.AuxFloat()
			if v.Op == mulOp {
				c = x.Args[1].AuxFloat() * y.AuxFloat()
			}
			if constOp == ssaop.OpConst32F {
				c = float64(float32(c))
			}
			// Do not introduce an overflowing or underflowing multiplier.
			if !math.IsInf(c, 0) && !math.IsNaN(c) && (v.Op == addOp || c != 0) {
				k := v.Block.NewValue0I(v.Pos, constOp, v.Type, ssa.Float64ToAuxInt(c))
				v.SetArg(0, x.Args[0])
				v.SetArg(1, k)
				return true
			}
		}
	case ssaop.OpSub32F, ssaop.OpSub64F:
		if x == y {
			return constant(0)
		}
		if isConst(y, 0) {
			v.CopyOf(x)
			return true
		}
		if x.Op == addOp {
			for i, a := range x.Args {
				if a == y {
					v.CopyOf(x.Args[1-i])
					return true
				}
			}
		}
	case ssaop.OpDiv32F, ssaop.OpDiv64F:
		if x == y {
			return constant(1)
		}
		if isConst(x, 0) {
			return constant(0)
		}
		if isConst(y, 1) {
			v.CopyOf(x)
			return true
		}
		if y.Op == constOp {
			c := 1 / y.AuxFloat()
			if constOp == ssaop.OpConst32F {
				c = float64(float32(c))
			}
			if c != 0 && !math.IsInf(c, 0) && !math.IsNaN(c) {
				k := v.Block.NewValue0I(v.Pos, constOp, v.Type, ssa.Float64ToAuxInt(c))
				v.Reset(mulOp)
				v.AddArg2(x, k)
				return true
			}
		} else if sharedDivisor && !isConst(x, 1) {
			one := v.Block.NewValue0I(v.Pos, constOp, v.Type, ssa.Float64ToAuxInt(1))
			reciprocal := v.Block.NewValue2(v.Pos, divOp, v.Type, one, y)
			v.Reset(mulOp)
			v.AddArg2(x, reciprocal)
			return true
		}
	case ssaop.OpEq32F, ssaop.OpEq64F, ssaop.OpLeq32F, ssaop.OpLeq64F,
		ssaop.OpNeq32F, ssaop.OpNeq64F, ssaop.OpLess32F, ssaop.OpLess64F:
		if x == y {
			value := v.Op == ssaop.OpEq32F || v.Op == ssaop.OpEq64F || v.Op == ssaop.OpLeq32F || v.Op == ssaop.OpLeq64F
			v.Reset(ssaop.OpConstBool)
			v.AuxInt = ssa.BoolToAuxInt(value)
			return true
		}
	}
	return false
}
