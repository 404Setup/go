// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "reflect"

type Embedded struct{ X, Y int }

func (Embedded) Get() int { return 42 }

type Extra interface{ Other() }
type combined interface {
	Get() int
	Other()
}

// StructOf may add methods to Embedded's method set. Embedded does not
// implement combined, but its Get implementation must still be retained.
func main() {
	typ := reflect.StructOf([]reflect.StructField{
		{Name: "Embedded", Type: reflect.TypeOf(Embedded{}), Anonymous: true},
		{Name: "Extra", Type: reflect.TypeFor[Extra](), Anonymous: true},
	})
	x := reflect.New(typ).Elem().Interface().(combined)
	if x.Get() != 42 {
		panic("generated method set")
	}
	println("ok")
}
