// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"runtime/pprof"
	"time"
)

func init() {
	register("TimeProf", TimeProf)
}

func TimeProf() {
	f, err := os.CreateTemp("", "timeprof")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	if err := pprof.StartCPUProfile(f); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	t0 := time.Now()
	// Collect enough samples for the time loop to dominate startup and
	// profiler overhead, even when the system is busy.
	for time.Since(t0) < time.Second {
	}

	pprof.StopCPUProfile()

	name := f.Name()
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	fmt.Println(name)
}
