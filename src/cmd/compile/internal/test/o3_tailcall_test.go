// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"bytes"
	"internal/testenv"
	"os"
	"path/filepath"
	"testing"
)

func TestO3TailReceiver(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(o3TailSource), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, level := range []string{"-o2", "-o3", "-o3 -N"} {
		t.Run(level, func(t *testing.T) {
			dst := filepath.Join(dir, "main.exe")
			flags := level + " -d=ssa/check/on,tailcall=1"
			out, err := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+flags, "-o", dst, src).CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, out)
			}
			for _, name := range []string{"command.Apply wrapper", "dict.Lookup wrapper", "channel.Count wrapper"} {
				if got, want := bytes.Contains(out, []byte(name)), level == "-o3"; got != want {
					t.Errorf("%s tail call = %v; want %v\n%s", name, got, want, out)
				}
			}
			if out, err := testenv.Command(t, dst).CombinedOutput(); err != nil || string(out) != "ok\n" {
				t.Fatalf("incorrect execution: %v\n%s", err, out)
			}
		})
	}
	for _, arch := range []string{"386", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			cmd := testenv.Command(t, testenv.GoToolPath(t), "tool", "compile", "-o3", "-d=ssa/check/on", "-o", filepath.Join(dir, "cross.o"), src)
			cmd.Env = append(cmd.Environ(), "GOARCH="+arch)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("compile failed: %v\n%s", err, out)
			}
		})
	}
}

const o3TailSource = `package main

type command func(int) int
type wrapper struct { pad int; command }
type dict map[int]int
type mapWrapper struct { pad int; dict }
type channel chan int
type chanWrapper struct { pad int; channel }

//go:noinline
func (c command) Apply(a,b,d,e,f,g,h,i,j,k,l,m int) [12]int {
	r := [12]int{a,b,d,e,f,g,h,i,j,k,l,m}
	if c != nil { r[0] = c(a) }
	return r
}

//go:noinline
func (d dict) Lookup(k int) int { return d[k] }

//go:noinline
func (c channel) Count() int { return len(c) }

var recovered int

//go:noinline
func (c command) Recover() { if recover() != nil { recovered++ } }

var apply = (*wrapper).Apply
var lookup = (*mapWrapper).Lookup
var count = (*chanWrapper).Count
var recoverCall = (*wrapper).Recover

func mustPanic(f func()) {
	defer func() { if recover() == nil { panic("missing nil receiver panic") } }()
	f()
}

func recoverThroughWrapper() {
	w := &wrapper{}
	defer recoverCall(w)
	panic("recovered through tail call")
}

func main() {
	w := &wrapper{pad: 99, command: func(x int) int { return x + 10 }}
	want := [12]int{11,2,3,4,5,6,7,8,9,10,11,12}
	if apply(w,1,2,3,4,5,6,7,8,9,10,11,12) != want { panic("stack args or results") }
	methodValue := w.Apply
	if methodValue(1,2,3,4,5,6,7,8,9,10,11,12) != want { panic("method value") }
	w.command = nil
	want[0] = 1
	if apply(w,1,2,3,4,5,6,7,8,9,10,11,12) != want { panic("nil function receiver") }
	if lookup(&mapWrapper{dict: dict{1: 42}},1) != 42 || lookup(&mapWrapper{},1) != 0 { panic("map receiver") }
	c := make(channel,2)
	c <- 42
	if count(&chanWrapper{channel: c}) != 1 || count(&chanWrapper{}) != 0 { panic("channel receiver") }
	mustPanic(func() { apply(nil,1,2,3,4,5,6,7,8,9,10,11,12) })
	mustPanic(func() { lookup(nil,1) })
	mustPanic(func() { count(nil) })
	recoverThroughWrapper()
	if recovered != 1 { panic("recover through wrapper") }
	println("ok")
}
`
