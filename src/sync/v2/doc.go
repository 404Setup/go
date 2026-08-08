// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides typed synchronization primitives such as mutual
// exclusion locks, one-time initialization, object pools, and concurrent maps.
// Other than the Once and WaitGroup types, most are intended for use by
// low-level library routines. Higher-level synchronization is better done via
// channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

// noCopy may be added to structs which must not be copied after the first use.
// See https://golang.org/issues/8005#issuecomment-190753527 for details.
type noCopy struct{}

// Lock is a no-op used by -copylocks in go vet.
func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}
