// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package reflectdata

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/internal/obj"
)

// deferredMethodUses keeps O3 method reachability on the call until code
// generation. In particular, an unreachable dynamic lookup must not disable
// method pruning for the whole program. Go/defer calls may be invoked by the
// runtime without executing an SSA call (open-coded defers), so retain their
// markers eagerly. Calls inside their wrappers can still be analyzed normally.
func deferredMethodUses(n *ir.CallExpr) *ir.MethodUses {
	if !base.Flag.O3 || base.Flag.N != 0 || n.GoDefer {
		return nil
	}
	if n.MethodUses == nil {
		n.MethodUses = new(ir.MethodUses)
	}
	return n.MethodUses
}

// MarkMethodUse records a named reflection lookup or interface method call.
func MarkMethodUse(n *ir.CallExpr, from *obj.LSym, r obj.Reloc) {
	if uses := deferredMethodUses(n); uses != nil {
		uses.Relocs = append(uses.Relocs, r)
	} else {
		from.AddRel(base.Ctxt, r)
	}
}

// MarkReflectMethod records a reflective lookup with an unknown method name.
func MarkReflectMethod(n *ir.CallExpr, from *obj.LSym) {
	if uses := deferredMethodUses(n); uses != nil {
		uses.Reflect = true
	} else {
		from.Set(obj.AttrReflectMethod, true)
	}
}
