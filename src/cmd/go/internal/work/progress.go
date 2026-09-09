// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package work

import (
	"cmd/go/internal/cfg"
	"fmt"
	"os"
	"strings"
	"time"
)

// Only interactive build/install output may change. In particular, go list,
// test, JSON output, and pipes used by editors must retain their output format.
func buildProgressEnabled(terminal bool) bool {
	return terminal && (cfg.CmdName == "build" || cfg.CmdName == "install") &&
		!cfg.BuildJSON && !cfg.BuildN && !cfg.BuildX && os.Getenv("TERM") != "dumb"
}

// buildProgress shares the shell's print lock so diagnostics cannot interleave
// with the progress line. Counts include cached and skipped actions.
type buildProgress struct {
	sh          *Shell
	start       time.Time
	done, total int
	width, line int
	stop, ended chan struct{}
}

func newBuildProgress(sh *Shell, total, width int) *buildProgress {
	p := &buildProgress{
		sh: sh, start: time.Now(), total: total, width: width,
		stop: make(chan struct{}), ended: make(chan struct{}),
	}
	sh.progress = p
	p.draw()
	go func() {
		defer close(p.ended)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.draw()
			case <-p.stop:
				return
			}
		}
	}()
	return p
}

func (p *buildProgress) draw() {
	p.sh.printLock.Lock()
	defer p.sh.printLock.Unlock()
	filled := 20 * p.done / p.total
	line := fmt.Sprintf("    Building [%s%s] %d/%d %.1fs",
		strings.Repeat("=", filled), strings.Repeat(" ", 20-filled),
		p.done, p.total, time.Since(p.start).Seconds())
	line = line[:min(len(line), p.width)]
	// Overwrite in one write so the terminal never displays a cleared frame.
	p.sh.printer.Printf(nil, "\r%-*s", max(p.line, len(line)), line)
	p.line = len(line)
}

// clear requires the shell's print lock. Spaces and carriage returns also work
// on Windows consoles without enabling ANSI escape processing.
func (p *buildProgress) clear() {
	if p != nil && p.line != 0 {
		p.sh.printer.Printf(nil, "\r%s\r", strings.Repeat(" ", p.line))
		p.line = 0
	}
}

func (p *buildProgress) finish(failed bool) {
	close(p.stop)
	<-p.ended
	p.sh.printLock.Lock()
	defer p.sh.printLock.Unlock()
	p.clear()
	status := "Finished"
	if failed {
		status = "Failed"
	}
	p.sh.printer.Printf(nil, "    %s in %.2fs\n", status, time.Since(p.start).Seconds())
	p.sh.progress = nil
}
