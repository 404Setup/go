// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssacompile

import (
	"testing"

	"cmd/compile/internal/ssa/block"
	"cmd/compile/internal/ssa/ssaop"
	"cmd/compile/internal/types"
)

func TestLoopUnroll(t *testing.T) {
	for _, tt := range []struct {
		name             string
		start, end, step int64
		factor           int
		full             bool
	}{
		{"full", 0, 8, 1, 8, true},
		{"stride", -7, 8, 3, 5, true},
		{"partial4", 0, 100, 1, 4, false},
		{"partial2", 0, 34, 1, 2, false},
		{"remainder", 0, 33, 1, 1, false},
		{"single", 0, 1, 1, 1, false},
		{"overflow", 120, 127, 3, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig(t)
			typ := c.config.Types.Int8
			fun := c.Fun("entry",
				Bloc("entry",
					Valu("mem", ssaop.OpInitMem, types.TypeMem, 0, nil),
					Valu("start", ssaop.OpConst8, typ, tt.start, nil),
					Valu("end", ssaop.OpConst8, typ, tt.end, nil),
					Valu("step", ssaop.OpConst8, typ, tt.step, nil),
					Goto("header")),
				Bloc("header",
					Valu("i", ssaop.OpPhi, typ, 0, nil, "start", "next"),
					Valu("cmp", ssaop.OpLess8, c.config.Types.Bool, 0, nil, "i", "end"),
					If("cmp", "body", "exit")),
				Bloc("body",
					Valu("next", ssaop.OpAdd8, typ, 0, nil, "i", "step"),
					Goto("header")),
				Bloc("exit", Exit("mem")))
			checkFunc(fun.f)
			changed := false
			for _, iv := range findIndVar(fun.f) {
				changed = unrollLoop(fun.f, iv) || changed
			}
			if changed != (tt.factor > 1) {
				t.Fatalf("unrolled = %t; want factor %d", changed, tt.factor)
			}
			// Count clones before deadcode discards the unused induction values.
			adds := 0
			for _, b := range fun.f.Blocks {
				if tt.full && b == fun.blocks["body"] {
					continue // The original body is now unreachable.
				}
				for _, v := range b.Values {
					if v.Op == ssaop.OpAdd8 {
						adds++
					}
				}
			}
			if adds != tt.factor {
				t.Fatalf("body copies = %d; want %d", adds, tt.factor)
			}
			deadcode(fun.f)
			checkFunc(fun.f)
			if got := fun.blocks["header"].Kind == block.BlockPlain; got != tt.full {
				t.Fatalf("loop removed = %t; want %t", got, tt.full)
			}
		})
	}
}
