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

// o3Deadcode removes finite loops whose values are used only by the loop
// itself. Ordinary deadcode treats every branch control as live, so it cannot
// collect these cycles of induction variables, conditions, and dead results.
func o3Deadcode(f *ssa.Func) {
	if !base.Flag.O3 {
		return
	}
	budget := 32 * (len(f.Blocks) + f.NumValues())
	for budget > 0 {
		budget -= len(f.Blocks) + f.NumValues()
		order := make([]int32, f.NumBlocks())
		ssa.PostorderWithNumbering(f, order)
		changed := false
		for _, iv := range findIndVar(f) {
			if removeDeadLoop(f, iv, order, &budget) {
				deadcode(f)
				changed = true
				break // Recompute induction variables after removing blocks and phis.
			}
			if budget <= 0 {
				break
			}
		}
		if !changed {
			return
		}
	}
}

func removeDeadLoop(f *ssa.Func, iv indVar, order []int32, budget *int) bool {
	header := iv.ind.Block
	if header.Kind != block.BlockIf || len(header.Preds) != 2 {
		return false
	}
	bodyEdge := 0
	if header.Succs[0].B != iv.entry {
		bodyEdge = 1
		if header.Succs[1].B != iv.entry {
			return false
		}
	}
	// findIndVar can also recognize comparisons below the phi's block. Only
	// remove a loop when the proven comparison is its header's own control.
	control := header.Controls[0]
	if len(control.Args) != 2 || control.Args[0] != iv.ind && control.Args[1] != iv.ind {
		return false
	}

	// Collect a single-entry region. Every path through the body must return
	// to the header; reject exits, nested cycles, and irreducible cycles.
	// Inner dead loops can be removed on an earlier iteration of the pass.
	inside := map[*ssa.Block]bool{header: true}
	blocks := []*ssa.Block{header}
	for i := 0; i < len(blocks); i++ {
		b := blocks[i]
		*budget -= 1 + len(b.Values) + len(b.Preds)
		if *budget < 0 || b.Kind != block.BlockPlain && b.Kind != block.BlockIf {
			return false
		}
		succs := b.Succs
		if b == header {
			succs = succs[bodyEdge : bodyEdge+1]
		}
		for _, e := range succs {
			if e.B == header {
				continue
			}
			if b != header && order[e.B.ID] >= order[b.ID] {
				return false // A cycle which bypasses the proven increment.
			}
			if !inside[e.B] {
				inside[e.B] = true
				blocks = append(blocks, e.B)
			}
		}
	}
	if inside[header.Succs[1-bodyEdge].B] {
		return false
	}
	// Require an invariant limit for the termination proof.
	for _, a := range control.Args {
		if a != iv.ind && inside[a.Block] {
			return false
		}
	}

	// Count internal uses, including phi backedges and branch controls. A
	// value with any other use is an observable loop result, even when that
	// use is several blocks after the loop. Keep all memory operations that
	// produce memory, calls, nil checks, and other explicit side effects.
	uses := make(map[*ssa.Value]int32)
	for _, b := range blocks {
		if b != header {
			for _, e := range b.Preds {
				if !inside[e.B] {
					return false
				}
			}
		}
		for _, v := range b.Values {
			if v.Op.IsCall() || v.Op.HasSideEffects() || ssaop.OpcodeTable[v.Op].NilCheck ||
				v.Type.IsMemory() || v.Type.IsVoid() {
				return false
			}
			*budget -= len(v.Args)
			if *budget < 0 {
				return false
			}
			for _, a := range v.Args {
				if inside[a.Block] {
					uses[a]++
				}
			}
		}
		for _, v := range b.ControlValues() {
			if inside[v.Block] {
				uses[v]++
			}
		}
	}
	for _, b := range blocks {
		for _, v := range b.Values {
			if uses[v] != v.Uses {
				return false
			}
		}
	}

	if f.Pass.Debug > 0 {
		f.Warnl(header.Pos, "Removed dead loop (%d blocks)", len(blocks))
	}
	header.RemoveEdge(bodyEdge)
	header.Kind = block.BlockPlain
	header.Likely = ssa.BranchUnknown
	header.ResetControls()
	f.InvalidateCFG()
	return true
}
