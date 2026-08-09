// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package atomic_test

import (
	"fmt"
	atomicv2 "sync/atomic/v2"
)

func ExampleValue() {
	type config struct {
		name string
	}

	var current atomicv2.Value[*config]
	fmt.Println(current.Load())
	current.Store(&config{name: "production"})
	fmt.Println(current.Load().name)
	// Output:
	// <nil>
	// production
}
