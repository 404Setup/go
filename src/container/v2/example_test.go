// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container_test

import (
	"cmp"
	"container/v2"
	"fmt"
)

func ExampleMap() {
	m := container.NewOrderedMap[string, int]()
	m.Store("third", 3)
	m.Store("first", 1)
	m.Store("second", 2)

	for key, value := range m.All() {
		fmt.Println(key, value)
	}
	// Output:
	// first 1
	// second 2
	// third 3
}

func ExampleSet() {
	s := container.NewOrderedSet[int]()
	s.Add(3)
	s.Add(1)
	s.Add(2)
	s.Add(1)

	for value := range s.All() {
		fmt.Println(value)
	}
	// Output:
	// 1
	// 2
	// 3
}

func ExamplePriorityQueue() {
	q := container.NewPriorityQueue(cmp.Compare[int], 30, 10, 20)
	for q.Len() != 0 {
		value, _ := q.Pop()
		fmt.Println(value)
	}
	// Output:
	// 10
	// 20
	// 30
}
