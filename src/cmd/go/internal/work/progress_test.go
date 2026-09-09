// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package work

import (
	"bytes"
	"cmd/go/internal/cfg"
	"cmd/go/internal/load"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildProgress(t *testing.T) {
	t.Run("redraw", func(t *testing.T) {
		var writes progressWrites
		p := &buildProgress{
			sh:    NewShell("", &load.TextPrinter{Writer: &writes}),
			start: time.Now(), total: 8, width: 79,
		}
		p.draw()
		p.done = 4
		p.draw()
		if len(writes) != 2 || !strings.HasPrefix(writes[1], "\r    Building [==========") {
			t.Fatalf("redraw must overwrite directly in one write: %q", writes)
		}
		// A shorter line must erase the old suffix in that same write.
		previous := p.line
		p.width = 20
		p.draw()
		if len(writes) != 3 || len(writes[2]) != previous+1 ||
			writes[2][21:] != strings.Repeat(" ", previous-20) {
			t.Fatalf("shorter redraw must pad the old suffix: %q", writes)
		}
	})
	t.Run("output modes", func(t *testing.T) {
		cmd, json, n, x := cfg.CmdName, cfg.BuildJSON, cfg.BuildN, cfg.BuildX
		defer func() { cfg.CmdName, cfg.BuildJSON, cfg.BuildN, cfg.BuildX = cmd, json, n, x }()
		for _, tt := range []struct {
			cmd                  string
			terminal, json, n, x bool
			term                 string
			want                 bool
		}{
			{"build", true, false, false, false, "", true},
			{"install", true, false, false, false, "", true},
			{"build", false, false, false, false, "", false},
			{"build", true, true, false, false, "", false},
			{"build", true, false, true, false, "", false},
			{"build", true, false, false, true, "", false},
			{"build", true, false, false, false, "dumb", false},
			{"list", true, false, false, false, "", false},
			{"test", true, false, false, false, "", false},
			{"run", true, false, false, false, "", false},
			{"vet", true, false, false, false, "", false},
		} {
			cfg.CmdName, cfg.BuildJSON, cfg.BuildN, cfg.BuildX = tt.cmd, tt.json, tt.n, tt.x
			t.Setenv("TERM", tt.term)
			if got := buildProgressEnabled(tt.terminal); got != tt.want {
				t.Errorf("%+v: enabled = %v", tt, got)
			}
		}
	})
	for _, failed := range []bool{false, true} {
		var out bytes.Buffer
		sh := NewShell("", &load.TextPrinter{Writer: &out})
		p := newBuildProgress(sh, 8, 79)
		var wg sync.WaitGroup
		for range p.total {
			wg.Go(func() {
				sh.Printf("diagnostic\n")
				sh.printLock.Lock()
				p.done++
				sh.printLock.Unlock()
				p.draw()
			})
		}
		wg.Wait()
		p.finish(failed)
		sh.Printf("after\n")
		status := "Finished"
		if failed {
			status = "Failed"
		}
		text := out.String()
		if !strings.Contains(text, "[====================] 8/8") ||
			!strings.Contains(text, status+" in ") ||
			!strings.Contains(text, "\rdiagnostic\n") ||
			strings.Count(text, "diagnostic\n") != 8 ||
			!strings.HasSuffix(text, "s\nafter\n") || sh.progress != nil {
			t.Fatalf("unexpected progress output: %q", text)
		}
	}
}

type progressWrites []string

func (w *progressWrites) Write(p []byte) (int, error) {
	*w = append(*w, string(p))
	return len(p), nil
}
