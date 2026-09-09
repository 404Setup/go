// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !cmd_go_bootstrap

package work

import (
	"os"

	"golang.org/x/term"
)

func (b *Builder) startProgress(all []*Action) *buildProgress {
	if b.IsCmdList || !buildProgressEnabled(term.IsTerminal(int(os.Stderr.Fd()))) {
		return nil
	}
	total := 0
	for _, a := range all {
		if a.Actor != nil {
			total++
		}
	}
	if total == 0 {
		return nil
	}
	width, _, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || width < 2 {
		width = 80
	}
	return newBuildProgress(b.BackgroundShell(), total, width-1)
}
