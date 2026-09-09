// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssacompile

import (
	"testing"

	"cmd/compile/internal/ssa/ssaop"
	"cmd/compile/internal/types"
)

func TestNonNegativeIndVars(t *testing.T) {
	for _, tt := range []struct {
		name             string
		start, end, step int64
		want             bool
	}{
		{"nested", 0, 100, 1, true},
		{"negative-input", -1, 100, 1, false},
		{"unknown-input", 0, 100, 1, false},
		{"safe-stride", 0, 100, 3, true},
		{"overflow", 0, 127, 3, false},
		{"unchecked-update", 0, 100, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig(t)
			i8, boolean := c.config.Types.Int8, c.config.Types.Bool
			fun := c.Fun("entry",
				Bloc("entry",
					Valu("mem", ssaop.OpInitMem, types.TypeMem, 0, nil),
					Valu("start", ssaop.OpConst8, i8, tt.start, nil),
					Valu("end", ssaop.OpConst8, i8, tt.end, nil),
					Valu("step", ssaop.OpConst8, i8, tt.step, nil),
					Goto("outer")),
				Bloc("outer",
					Valu("x", ssaop.OpPhi, i8, 0, nil, "start", "result"),
					Valu("outercmp", ssaop.OpLess8, boolean, 0, nil, "x", "end"),
					If("outercmp", "preheader", "exit")),
				Bloc("preheader", Goto("inner")),
				Bloc("inner",
					Valu("i", ssaop.OpPhi, i8, 0, nil, "x", "next"),
					Valu("cmp", ssaop.OpLess8, boolean, 0, nil, "i", "end"),
					If("cmp", "body", "after")),
				Bloc("body",
					Valu("next", ssaop.OpAdd8, i8, 0, nil, "i", "step"),
					Goto("inner")),
				Bloc("after",
					Valu("result", ssaop.OpCopy, i8, 0, nil, "i"),
					Goto("outer")),
				Bloc("exit", Exit("mem")))
			if tt.name == "unknown-input" {
				fun.values["start"].Reset(ssaop.OpArg)
			}
			if tt.name == "unchecked-update" {
				v := fun.values["result"]
				v.Reset(ssaop.OpAdd8)
				v.AddArg2(fun.values["i"], fun.values["step"])
			}
			checkFunc(fun.f)
			got := nonNegativeIndVars(fun.f, findIndVar(fun.f))
			for _, name := range []string{"x", "i", "next", "result"} {
				found := false
				for _, v := range got {
					found = found || v == fun.values[name]
				}
				if found != tt.want {
					t.Errorf("%s non-negative = %v; want %v", name, found, tt.want)
				}
			}
		})
	}
}
