// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package container provides generic, non-concurrent containers.
//
// The container types in this package are not safe for concurrent mutation.
// Callers must provide synchronization when a container is accessed by
// multiple goroutines and at least one access mutates it.
package container
