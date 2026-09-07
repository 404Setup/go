// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package container provides generic maps with ordered iteration.
// OrderedMap and ConcurrentSkipListMap support concurrent use. LinkedHashMap
// and TreeMap require external synchronization when used by multiple goroutines
// with at least one writer.
package container

import "reflect"

func mapCheckComparable[V any](operation string) {
	if !reflect.TypeFor[V]().Comparable() {
		panic("called " + operation + " when value is not of comparable type")
	}
}

// noCopy may be added to structs which must not be copied after the first use.
// See https://golang.org/issues/8005#issuecomment-190753527 for details.
type noCopy struct{}

// Lock is a no-op used by -copylocks in go vet.
func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}
