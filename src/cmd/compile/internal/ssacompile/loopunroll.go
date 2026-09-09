// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssacompile

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/ssa/block"
	"cmd/compile/internal/ssa/ssaop"
)

// o2LoopUnroll removes loop control overhead and exposes constant indices to opt.
func o2LoopUnroll(f *ssa.Func) {
	if !base.Flag.O2 {
		return
	}
	changed := false
	for _, iv := range findIndVar(f) {
		if unrollLoop(f, iv) {
			changed = true
		}
	}
	if changed {
		deadcode(f)
	}
}

func unrollLoop(f *ssa.Func, iv indVar) bool {
	header, body := iv.ind.Block, iv.entry
	// ponytail: only straight-line, constant-trip loops; add remainder loops
	// and branching loop bodies when benchmarks justify their code size cost.
	if header.Kind != block.BlockIf || len(header.Preds) != 2 ||
		len(body.Preds) != 1 || body.Preds[0].B != header {
		return false
	}
	if header.Controls[0].Uses != 1 {
		return false // The comparison must not also feed a cloned body value.
	}
	var phis []*ssa.Value
	for _, v := range header.Values {
		if v.Op == ssaop.OpPhi {
			phis = append(phis, v)
		} else if v != header.Controls[0] {
			// Header computations would need to run between the cloned bodies.
			return false
		}
	}
	// Nil checks and eliminated bounds checks can leave a chain of plain
	// blocks. Treat that chain as one body, without copying any branches.
	var values []*ssa.Value
	latch := body
	for {
		if latch.Kind != block.BlockPlain || len(latch.Preds) != 1 {
			return false
		}
		values = append(values, latch.Values...)
		if len(values) > 32 {
			return false
		}
		for _, v := range latch.Values {
			if v.Op.IsCall() || v.Op.HasSideEffects() {
				return false
			}
		}
		if latch.Succs[0].B == header {
			break
		}
		latch = latch.Succs[0].B
	}
	if !iv.min.IsGenericIntConst() || !iv.max.IsGenericIntConst() {
		return false
	}
	lo, hi := iv.min.AuxInt, iv.max.AuxInt
	if iv.flags&indVarMinExc != 0 {
		if lo == maxSignedValue(iv.ind.Type) {
			return false
		}
		lo++
	}
	if iv.flags&indVarMaxInc == 0 {
		if hi == minSignedValue(iv.ind.Type) {
			return false
		}
		hi--
	}
	if lo > hi {
		return false
	}
	// findIndVar already proved that the final increment cannot wrap.
	trips := diff(hi, lo)/uint64(iv.step) + 1
	factor := uint64(4)
	full := trips <= 16
	if full {
		factor = trips
	} else {
		for factor > 1 && trips%factor != 0 {
			factor /= 2
		}
	}
	if factor < 2 || uint64(len(values))*factor > 128 {
		return false
	}

	back := latch.Succs[0].I
	entry := 1 - back
	replacements := make(map[*ssa.Value]*ssa.Value, len(values)+len(phis))
	lookup := func(v *ssa.Value) *ssa.Value {
		if w := replacements[v]; w != nil {
			return w
		}
		return v
	}
	target := latch
	first := uint64(1) // Keep the original first iteration for partial unrolling.
	if full {
		target = f.NewBlock(block.BlockPlain)
		target.Pos = body.Pos
		pred := header.Preds[entry]
		pred.B.Succs[pred.I] = ssa.Edge{B: target, I: 0}
		target.Preds = append(target.Preds, pred)
		target.Succs = append(target.Succs, ssa.Edge{B: header, I: entry})
		header.Preds[entry] = ssa.Edge{B: target, I: 0}
		for _, phi := range phis {
			replacements[phi] = phi.Args[entry]
		}
		first = 0
	}
	next := make([]*ssa.Value, len(phis))
	for i := first; i < factor; i++ {
		if i != 0 {
			// Resolve all backedge arguments before advancing any phi, including
			// mutually dependent phis such as a, b = b, a.
			for j, phi := range phis {
				next[j] = lookup(phi.Args[back])
			}
			for j, phi := range phis {
				replacements[phi] = next[j]
			}
		}
		// SSA values are not scheduled yet. Allocate every clone before wiring
		// its arguments, so this also works with randomized value ordering.
		for _, v := range values {
			w := target.NewValue0(v.Pos, v.Op, v.Type)
			w.Aux, w.AuxInt = v.Aux, v.AuxInt
			replacements[v] = w
		}
		for _, v := range values {
			for _, arg := range v.Args {
				replacements[v].AddArg(lookup(arg))
			}
		}
	}
	for j, phi := range phis {
		next[j] = lookup(phi.Args[back])
	}
	for j, phi := range phis {
		if full {
			phi.SetArg(entry, next[j])
		} else {
			phi.SetArg(back, next[j])
		}
	}
	if full {
		// The expanded block computes the final phi inputs. Keep the original
		// exit edge, including any exit phis, and let deadcode remove the loop.
		if header.Succs[0].B == body {
			header.SwapSuccessors()
		}
		header.Reset(block.BlockFirst)
		header.Likely = ssa.BranchUnknown
		f.InvalidateCFG()
	}
	if f.Pass.Debug > 0 {
		f.Warnl(header.Pos, "Unrolled loop %d times (full=%t)", factor, full)
	}
	return true
}
