// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"internal/synctest"
	"sync"
	"sync/atomic"
	"time"
)

const (
	h2WorkerPoolShards      = 8
	h2WorkerPoolMaxPerShard = 32
	h2WorkerIdleTimeout     = 2 * time.Second
)

type h2TaskKind uint8

const (
	h2TaskRunHandler h2TaskKind = iota + 1
	h2TaskWriteFrameAsync
	h2TaskWriteResHeadersAsync
	h2TaskWriteEmptyEndStreamAsync
	h2TaskDoRequest
	h2TaskFunc
)

type h2Task struct {
	kind          h2TaskKind
	sc            *serverConn
	rw            *responseWriter
	sreq          *ServerRequest
	handleRequest func(*ResponseWriter, *ServerRequest) error
	wr            FrameWriteRequest
	wd            *writeData
	wrh           *writeResHeaders
	headerBlock   []byte
	cs            *clientStream
	creq          *ClientRequest
	streamf       func(*clientStream)
	fn            func()
}

func (t h2Task) run() {
	switch t.kind {
	case h2TaskRunHandler:
		t.sc.runHandler(t.rw, t.sreq, t.handleRequest)
	case h2TaskWriteFrameAsync:
		t.sc.writeFrameAsync(t.wr, t.wd)
	case h2TaskWriteResHeadersAsync:
		t.sc.writeResHeadersAsync(t.wr, t.wrh, t.headerBlock)
	case h2TaskWriteEmptyEndStreamAsync:
		t.sc.writeEmptyEndStreamAsync(t.wr, t.wr.StreamID())
	case h2TaskDoRequest:
		t.cs.doRequest(t.creq, t.streamf)
	case h2TaskFunc:
		t.fn()
	}
}

type h2Worker struct {
	ch chan h2Task
}

type h2WorkerShard struct {
	mu    sync.Mutex
	ready []*h2Worker
}

type h2WorkerPool struct {
	next   atomic.Uint32
	shards [h2WorkerPoolShards]h2WorkerShard
}

var workerPool h2WorkerPool

func (p *h2WorkerPool) Go(task h2Task) {
	if synctest.IsInBubble() {
		go task.run()
		return
	}
	shardIdx := p.next.Add(1) & (h2WorkerPoolShards - 1)
	s := &p.shards[shardIdx]

	s.mu.Lock()
	n := len(s.ready)
	if n > 0 {
		w := s.ready[n-1]
		s.ready[n-1] = nil
		s.ready = s.ready[:n-1]
		s.mu.Unlock()
		w.ch <- task
		return
	}
	s.mu.Unlock()

	w := &h2Worker{ch: make(chan h2Task, 1)}
	go p.runWorker(w, task, shardIdx)
}

func (p *h2WorkerPool) runWorker(w *h2Worker, task h2Task, shardIdx uint32) {
	timer := time.NewTimer(h2WorkerIdleTimeout)
	for {
		task.run()
		task = h2Task{}

		s := &p.shards[shardIdx]
		s.mu.Lock()
		if len(s.ready) >= h2WorkerPoolMaxPerShard {
			s.mu.Unlock()
			timer.Stop()
			return
		}
		s.ready = append(s.ready, w)
		s.mu.Unlock()

		timer.Reset(h2WorkerIdleTimeout)
		select {
		case task = <-w.ch:
			timer.Stop()
		case <-timer.C:
			if p.removeIdleWorker(s, w) {
				return
			}
			// A caller popped w right before removeIdleWorker locked s.mu.
			task = <-w.ch
		}
	}
}

func (p *h2WorkerPool) removeIdleWorker(s *h2WorkerShard, target *h2Worker) bool {
	s.mu.Lock()
	for i := len(s.ready) - 1; i >= 0; i-- {
		if s.ready[i] == target {
			last := len(s.ready) - 1
			s.ready[i] = s.ready[last]
			s.ready[last] = nil
			s.ready = s.ready[:last]
			s.mu.Unlock()
			return true
		}
	}
	s.mu.Unlock()
	return false
}
