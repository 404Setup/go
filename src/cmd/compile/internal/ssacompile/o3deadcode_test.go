// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssacompile

import (
	"testing"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ssa/block"
	"cmd/compile/internal/ssa/ssaop"
	"cmd/compile/internal/types"
)

func TestO3DeadLoop(t *testing.T) {
	saved := base.Flag.O3
	base.Flag.O3 = true
	defer func() { base.Flag.O3 = saved }()
	for _, tt := range []struct {
		name             string
		start, end, step int64
		removed          bool
	}{
		{"branched", 0, 100, 1, true},
		{"reversed-edges", 0, 100, 1, true},
		{"stride", -7, 100, 3, true},
		{"overflow", 120, 127, 3, false},
		{"infinite", 0, 100, 0, false},
		{"inner-cycle", 0, 100, 1, false},
		{"external-use", 0, 100, 1, false},
		{"nil-check", 0, 100, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig(t)
			typ := c.config.Types.Int8
			fun := c.Fun("entry",
				Bloc("entry",
					Valu("mem", ssaop.OpInitMem, types.TypeMem, 0, nil),
					Valu("nil", ssaop.OpConstNil, c.config.Types.BytePtr, 0, nil),
					Valu("start", ssaop.OpConst8, typ, tt.start, nil),
					Valu("end", ssaop.OpConst8, typ, tt.end, nil),
					Valu("step", ssaop.OpConst8, typ, tt.step, nil),
					Goto("header")),
				Bloc("header",
					Valu("i", ssaop.OpPhi, typ, 0, nil, "start", "next"),
					Valu("cmp", ssaop.OpLess8, c.config.Types.Bool, 0, nil, "i", "end"),
					If("cmp", "body", "exit")),
				Bloc("body",
					Valu("choose", ssaop.OpLess8, c.config.Types.Bool, 0, nil, "i", "step"),
					If("choose", "left", "right")),
				Bloc("left", Goto("latch")),
				Bloc("right", Goto("latch")),
				Bloc("latch",
					Valu("next", ssaop.OpAdd8, typ, 0, nil, "i", "step"),
					Goto("header")),
				Bloc("exit", Goto("return")),
				Bloc("return", Exit("mem")))
			switch tt.name {
			case "reversed-edges":
				h := fun.blocks["header"]
				h.SwapSuccessors()
				fun.values["cmp"].Reset(ssaop.OpLeq8)
				fun.values["cmp"].AddArg2(fun.values["end"], fun.values["i"])
			case "inner-cycle":
				b := fun.blocks["left"]
				b.Kind = block.BlockIf
				b.SetControl(fun.values["choose"])
				b.AddEdgeTo(b)
			case "external-use":
				b := fun.blocks["exit"]
				b.Kind = block.BlockIf
				b.SetControl(fun.values["cmp"])
				b.AddEdgeTo(fun.blocks["return"])
			case "nil-check":
				b := fun.blocks["left"]
				b.NewValue2(b.Pos, ssaop.OpNilCheck, c.config.Types.BytePtr, fun.values["nil"], fun.values["mem"])
			}
			checkFunc(fun.f)
			o3Deadcode(fun.f)
			checkFunc(fun.f)
			if got := fun.blocks["header"].Kind == block.BlockPlain; got != tt.removed {
				t.Fatalf("loop removed = %v; want %v\n%s", got, tt.removed, fun.f)
			}
		})
	}
}
