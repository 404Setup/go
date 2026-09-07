// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package container_test

import (
	"cmp"
	"fmt"
	"sync/v2/container"
)

func ExampleLinkedHashMap() {
	cache := container.NewLinkedHashMap[string, int](true)
	cache.Store("first", 1)
	cache.Store("second", 2)
	cache.Load("first")
	key, value, _ := cache.PollFirst()
	fmt.Println("evicted:", key, value)
	for key, value := range cache.All() {
		fmt.Println(key, value)
	}
	// Output:
	// evicted: second 2
	// first 1
}

func ExampleTreeMap() {
	m := container.NewTreeMap[int, string](cmp.Compare[int])
	m.Store(30, "thirty")
	m.Store(10, "ten")
	m.Store(20, "twenty")
	for key, value := range m.RangeBetween(10, 30) {
		fmt.Println(key, value)
	}
	// Output:
	// 10 ten
	// 20 twenty
}

func ExampleConcurrentSkipListMap() {
	m := container.NewConcurrentSkipListMap[int, string](cmp.Compare[int])
	m.Store(3, "three")
	m.Store(1, "one")
	m.Store(2, "two")
	key, value, ok := m.Ceiling(2)
	fmt.Println(key, value, ok)
	m.Range(func(key int, value string) bool {
		fmt.Println(key, value)
		m.Delete(key)
		return true
	})
	fmt.Println("remaining:", m.Len())
	// Output:
	// 2 two true
	// 1 one
	// 2 two
	// 3 three
	// remaining: 0
}
