// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

type Getter[T any] interface {
	Get() T
	Close()
}
type Generic struct{}

func (Generic) Get() int { return 42 }
func (Generic) Close()   {}

func invoke[T comparable](g Getter[T], want T) {
	if g.Get() != want {
		panic("generic interface")
	}
	g.Close()
}

func main() {
	invoke[int](Generic{}, 42)
	println("ok")
}
