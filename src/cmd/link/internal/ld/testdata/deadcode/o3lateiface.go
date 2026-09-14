// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

type Late struct{}

func (Late) Close() { closed = true }

type Factory struct{}

func (Factory) Run() { late.(interface{ Close() }).Close() }

var late any = Late{}
var factory interface{ Run() } = Factory{}
var closed bool

func main() {
	factory.Run() // discovers the Close call in a later linker flood
	if !closed {
		panic("late interface call")
	}
	println("ok")
}
