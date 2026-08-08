// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"bytes"
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Make sure "hello world" does not link in all the
// fmt.scanf routines. See issue 6853.
func TestScanfRemoval(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	t.Parallel()

	// Make a directory to work in.
	dir := t.TempDir()

	// Create source.
	src := filepath.Join(dir, "test.go")
	f, err := os.Create(src)
	if err != nil {
		t.Fatalf("could not create source file: %v", err)
	}
	f.Write([]byte(`
package main
import "fmt"
func main() {
	fmt.Println("hello world")
}
`))
	f.Close()

	// Name of destination.
	dst := filepath.Join(dir, "test")

	// Compile source.
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", dst, src)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("could not build target: %v\n%s", err, out)
	}

	// Check destination to see if scanf code was included.
	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", dst)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("could not read target: %v", err)
	}
	if bytes.Contains(out, []byte("scanInt")) {
		t.Fatalf("scanf code not removed from helloworld")
	}
}

// Make sure -S prints assembly code. See issue 14515.
func TestDashS(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	t.Parallel()

	// Make a directory to work in.
	dir := t.TempDir()

	// Create source.
	src := filepath.Join(dir, "test.go")
	f, err := os.Create(src)
	if err != nil {
		t.Fatalf("could not create source file: %v", err)
	}
	f.Write([]byte(`
package main
import "fmt"
func main() {
	fmt.Println("hello world")
}
`))
	f.Close()

	// Compile source.
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags", "-S", "-o", filepath.Join(dir, "test"), src)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("could not build target: %v\n%s", err, out)
	}

	patterns := []string{
		// It is hard to look for actual instructions in an
		// arch-independent way. So we'll just look for
		// pseudo-ops that are arch-independent.
		"\tTEXT\t",
		"\tFUNCDATA\t",
		"\tPCDATA\t",
	}
	outstr := string(out)
	for _, p := range patterns {
		if !strings.Contains(outstr, p) {
			println(outstr)
			panic("can't find pattern " + p)
		}
	}
}

// Verify that build-controlled constants expose dead branches to the compiler
// without changing builds where the value remains a run-time setting.
func TestGOFIPS140DeadCode(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(`
package main

import "crypto/fips140"

//go:noinline
func fipsOnly() {
	println("FIPS-only code")
}

func main() {
	if fips140.Enabled() {
		fipsOnly()
	}
}
`), 0o666); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		gofips140  string
		wantSymbol bool
	}{
		{gofips140: "off", wantSymbol: false},
		{gofips140: "latest", wantSymbol: true},
	} {
		t.Run(tt.gofips140, func(t *testing.T) {
			dst := filepath.Join(dir, "test-"+tt.gofips140)
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", dst, src)
			for _, entry := range cmd.Environ() {
				if !strings.HasPrefix(entry, "GOFIPS140=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "GOFIPS140="+tt.gofips140)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("could not build target: %v\n%s", err, out)
			}

			cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", dst)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("could not read target: %v\n%s", err, out)
			}
			hasSymbol := bytes.Contains(out, []byte(" main.fipsOnly"))
			if hasSymbol != tt.wantSymbol {
				t.Fatalf("fipsOnly symbol present = %v; want %v", hasSymbol, tt.wantSymbol)
			}
		})
	}
}
