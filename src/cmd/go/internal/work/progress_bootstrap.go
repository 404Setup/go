// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cmd_go_bootstrap

package work

// Terminal detection pulls in net through x/sys/windows, which cmd/dist cannot
// build for go_bootstrap. Progress is only needed in the finished go command.
func (b *Builder) startProgress(all []*Action) *buildProgress {
	return nil
}
