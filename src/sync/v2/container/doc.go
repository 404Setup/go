// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package container provides concurrent containers with ordered iteration.
package container

// noCopy may be added to structs which must not be copied after the first use.
// See https://golang.org/issues/8005#issuecomment-190753527 for details.
type noCopy struct{}

// Lock is a no-op used by -copylocks in go vet.
func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}
