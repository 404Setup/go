// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

type Receiver struct{}

func (Receiver) Active() int { return 42 }
func (Receiver) Unused()     { panic("unreachable") }

var receiver interface {
	Active() int
	Unused()
} = Receiver{}
var count byte

func main() {
	if receiver.Active() != 42 {
		panic("active method")
	}
	if int(count) < 0 {
		receiver.Unused()
	}
	println("ok")
}
