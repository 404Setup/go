// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"internal/synctest"
	"runtime"
	"sync"
	"sync/atomic"
)

const (
	h2WorkerPoolShards       = 8
	h2WorkerPoolMaxPerShard  = 32
	h2WorkerRetainedPerShard = 8
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
	handleRequest func(*ResponseWriter, *ServerRequest) error
	wr            FrameWriteRequest
	wd            *writeData
	headerBlock   []byte
	creq          *ClientRequest
	streamf       func(*clientStream)
	fn            func()
}

func (t h2Task) run() {
	switch t.kind {
	case h2TaskRunHandler:
		t.sc.runHandler(t.rw, &t.rw.rws.req, t.handleRequest)
	case h2TaskWriteFrameAsync:
		t.sc.writeFrameAsync(t.wr, t.wd)
	case h2TaskWriteResHeadersAsync:
		t.sc.writeResHeadersAsync(t.wr, t.wr.write.(*writeResHeaders), t.headerBlock)
	case h2TaskWriteEmptyEndStreamAsync:
		t.sc.writeEmptyEndStreamAsync(t.wr, t.wr.StreamID())
	case h2TaskDoRequest:
		(&t.creq.stream).doRequest(t.creq, t.streamf)
	case h2TaskFunc:
		t.fn()
	}
}

type h2Worker struct {
	ch chan h2Task
}

type h2WorkerShard struct {
	mu      sync.Mutex
	ready   []*h2Worker
	gcArmed bool
	_       [32]byte // pad to avoid false sharing across 64-byte cache lines
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
	task.run()
	task = h2Task{}
	if !p.putWorker(w, shardIdx) {
		return
	}
	for task = range w.ch {
		task.run()
		task = h2Task{}
		if !p.putWorker(w, shardIdx) {
			return
		}
	}
}

func (p *h2WorkerPool) putWorker(w *h2Worker, shardIdx uint32) bool {
	s := &p.shards[shardIdx]
	s.mu.Lock()
	if len(s.ready) >= h2WorkerPoolMaxPerShard {
		s.mu.Unlock()
		return false
	}
	s.ready = append(s.ready, w)
	if len(s.ready) > h2WorkerRetainedPerShard && !s.gcArmed {
		s.gcArmed = true
		runtime.AddCleanup(new([16]byte), p.pruneShard, shardIdx)
	}
	s.mu.Unlock()
	return true
}

func (p *h2WorkerPool) pruneShard(shardIdx uint32) {
	s := &p.shards[shardIdx]
	s.mu.Lock()
	s.gcArmed = false
	for len(s.ready) > h2WorkerRetainedPerShard {
		n := len(s.ready) - 1
		w := s.ready[n]
		s.ready[n] = nil
		s.ready = s.ready[:n]
		close(w.ch)
	}
	s.mu.Unlock()
}
