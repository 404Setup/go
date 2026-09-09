// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestFastMathEnvironment(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("requires hardware flush mode")
	}
	for _, tt := range []struct {
		name, ldflags, gcflags string
		flush                  bool
	}{
		{"default", "", "", false},
		{"enabled", "-fmth", "", true},
		{"disabled", "-fmth=false", "", false},
		{"last-wins", "-fmth -fmth=false", "", false},
		{"no-opt", "-fmth", "-N", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testFastMathEnvironment(t, tt.ldflags, tt.gcflags, tt.flush, false)
		})
	}
}

func TestFastMathCgo(t *testing.T) {
	testenv.MustHaveCGO(t)
	if runtime.GOARCH != "amd64" {
		t.Skip("MXCSR callback test")
	}
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			testFastMathEnvironment(t, "-fmth="+strconv.FormatBool(enabled), "", enabled, true)
		})
	}
}

func testFastMathEnvironment(t *testing.T, ldflags, gcflags string, flush, cgo bool) {
	t.Helper()
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	dst := filepath.Join(dir, "env.exe")
	args := []string{"build", "-ldflags=" + ldflags, "-gcflags=" + gcflags, "-o", dst}
	files := map[string]string{"main.go": fastMathEnvironmentSource}
	if cgo {
		files["cgo.go"] = fastMathCgoSource
	}
	for name, source := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(source), 0o666); err != nil {
			t.Fatal(err)
		}
		args = append(args, path)
	}
	if out, err := testenv.Command(t, testenv.GoToolPath(t), args...).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if out, err := testenv.Command(t, dst, strconv.FormatBool(flush)).CombinedOutput(); err != nil || string(out) != "ok\n" {
		t.Fatalf("execution: %v\n%s", err, out)
	}
}

const fastMathEnvironmentSource = `package main

import (
	"math"
	"os"
	"runtime"
)

var wantFlush = os.Args[1] == "true"
var initialized = checkFP()
var foreignCheck func() bool

//go:noinline
func mul64(x, y float64) float64 { return x*y }
//go:noinline
func mul32(x, y float32) float32 { return x*y }

func checkFP() bool {
	return (math.Float64bits(mul64(math.Float64frombits(1), 0x1p52)) == 0) == wantFlush &&
		(math.Float64bits(mul64(math.Float64frombits(0x0010000000000000), 0.5)) == 0) == wantFlush &&
		(math.Float32bits(mul32(math.Float32frombits(1), 0x1p23)) == 0) == wantFlush &&
		(math.Float32bits(mul32(math.Float32frombits(0x00800000), 0.5)) == 0) == wantFlush
}

func main() {
	if !initialized || !checkFP() { panic("initial floating-point environment") }
	runtime.GOMAXPROCS(4)
	ready, done := make(chan bool, 16), make(chan bool, 16)
	release := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			ready <- checkFP()
			<-release
			runtime.Gosched()
			done <- checkFP()
		}()
	}
	for i := 0; i < 16; i++ { if !<-ready { panic("new thread floating-point environment") } }
	close(release)
	for i := 0; i < 16; i++ { if !<-done { panic("resumed thread floating-point environment") } }
	if foreignCheck != nil && !foreignCheck() { panic("foreign floating-point environment") }
	println("ok")
}
`

const fastMathCgoSource = `package main

/*
#cgo !windows CFLAGS: -pthread
#cgo !windows LDFLAGS: -pthread
#include <xmmintrin.h>
#include <stdint.h>
#ifdef _WIN32
#include <windows.h>
#else
#include <pthread.h>
#endif

extern int goCheck(void);

static int callback_check(void) {
	unsigned old = _mm_getcsr();
	unsigned host = (old & ~0x8040u) ^ 0x2000u;
	_mm_setcsr(host);
	int ok = goCheck();
	unsigned after = _mm_getcsr();
	_mm_setcsr(old);
	return ok && (after & 0xffc0u) == (host & 0xffc0u);
}

#ifdef _WIN32
static DWORD WINAPI worker(void *p) { return callback_check(); }
static int foreign_thread(void) {
	HANDLE t = CreateThread(0, 0, worker, 0, 0, 0);
	if (!t) return 0;
	WaitForSingleObject(t, INFINITE);
	DWORD result = 0;
	GetExitCodeThread(t, &result);
	CloseHandle(t);
	return result;
}
#else
static void *worker(void *p) { return (void *)(uintptr_t)callback_check(); }
static int foreign_thread(void) {
	pthread_t t;
	void *result;
	if (pthread_create(&t, 0, worker, 0)) return 0;
	if (pthread_join(t, &result)) return 0;
	return (int)(uintptr_t)result;
}
#endif

static void clear_flush(void) { _mm_setcsr(_mm_getcsr() & ~0x8040u); }
*/
import "C"

//export goCheck
func goCheck() C.int {
	if checkFP() { return 1 }
	return 0
}

func init() {
	foreignCheck = func() bool {
		if C.callback_check() == 0 || C.foreign_thread() == 0 { return false }
		C.clear_flush()
		return checkFP()
	}
}
`
