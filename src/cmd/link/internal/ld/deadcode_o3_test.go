// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"bytes"
	"cmd/link/internal/loader"
	"internal/testenv"
	"path/filepath"
	"testing"
)

func TestO3MethodDeadcode(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	t.Parallel()
	for _, tt := range []struct {
		name, source, flags string
		keep, drop          []string
	}{
		{"default", "o3iface", "", []string{"main.SDK.Close", "main.One.Close", "main.coupledService"}, nil},
		{"o2", "o3iface", "-o2", []string{"main.SDK.Close", "main.One.Close"}, nil},
		{"disabled", "o3iface", "-o3=false", []string{"main.SDK.Close", "main.One.Close"}, nil},
		{"o3", "o3iface", "-o3", []string{"main.Registered.Close", "main.Registered.RoundTrip", "main.SDK.Used", "main.MethodValue.Close"}, []string{"main.SDK.Close", "main.One.Close", "main.coupledService"}},
		{"generic", "o3generic", "-o3", []string{"main.Generic.Get", "main.Generic.Close"}, nil},
		{"late-interface", "o3lateiface", "-o3", []string{"main.Late.Close"}, nil},
		{"dead-call-default", "o3deadiface", "", []string{"main.Receiver.Unused"}, nil},
		{"dead-call-o3", "o3deadiface", "-o3", []string{"main.Receiver.Active"}, []string{"main.Receiver.Unused"}},
		{"structof", "o3structof", "-o3", []string{"main.Embedded.Get"}, nil},
		{"sql", "o3sql", "-o3", []string{"main.legacyDriver.Open", "main.contextDriver.OpenConnector", "main.connection.Ping", "main.connection.QueryContext"}, nil},
		{"sql-all", "o3sql", "all=-o3", []string{"main.legacyDriver.Open", "main.contextDriver.OpenConnector", "main.connection.Ping", "main.connection.QueryContext"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			exe := filepath.Join(t.TempDir(), "main.exe")
			goTool := testenv.GoToolPath(t)
			source := filepath.Join("testdata", "deadcode", tt.source+".go")
			if out, err := testenv.Command(t, goTool, "build", "-gcflags=-l", "-ldflags="+tt.flags, "-o", exe, source).CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			out, err := testenv.Command(t, goTool, "tool", "nm", exe).CombinedOutput()
			if err != nil {
				t.Fatalf("nm: %v\n%s", err, out)
			}
			for _, name := range tt.keep {
				if !bytes.Contains(out, []byte(" "+name+"\n")) {
					t.Errorf("missing reachable method %s", name)
				}
			}
			for _, name := range tt.drop {
				if bytes.Contains(out, []byte(" "+name+"\n")) {
					t.Errorf("unreachable method %s retained", name)
				}
			}
			if out, err := testenv.Command(t, exe).CombinedOutput(); err != nil || string(out) != "ok\n" {
				t.Fatalf("execution: %v\n%s", err, out)
			}
		})
	}
}

// A bounded proof must always fall back to retention. Duplicate unexported
// names must not make the method-count heuristic reject a valid receiver.
func TestO3MethodProofFallback(t *testing.T) {
	m := methodsig{name: "m", typ: 1}
	n := methodsig{name: "n", typ: 1}
	a := &o3MethodAnalysis{
		interfaces: map[loader.Sym][]methodsig{10: {m, m}, 11: {n}},
		types:      map[loader.Sym]o3MethodSet{20: {methods: map[methodsig]bool{m: true}, count: 2}},
		implements: make(map[o3TypeInterface]bool),
		budget:     10,
	}
	if !a.implementsInterface(20, 10) {
		t.Fatal("duplicate names incorrectly rejected")
	}
	if a.implementsInterface(20, 11) {
		t.Fatal("missing method was not rejected")
	}
	a.budget = 0
	if !a.implementsInterface(21, 10) {
		t.Fatal("exhausted budget must retain unknown receiver")
	}
	if a.implementsInterface(20, 11) {
		t.Fatal("exhausted budget lost an already established proof")
	}
	a.budget = 1
	delete(a.implements, o3TypeInterface{20, 10})
	if !a.implementsInterface(20, 10) {
		t.Fatal("insufficient budget must retain receiver")
	}
}
