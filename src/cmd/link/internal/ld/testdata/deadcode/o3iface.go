// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

type transport interface {
	Close() error
	RoundTrip() int
}

// SDK has a colliding Close method, but does not implement transport.
// Its otherwise-unused Close pulls in another service with further calls.
type SDK struct{}

func (SDK) Close() error         { coupledService(); return nil }
func (SDK) RoundTrip(string) int { return 0 }
func (SDK) Used() int            { return 42 }

func coupledService() { new(service).Request() }

type service struct{}

func (*service) Request() { coupledService() }

// One also tests the cheaper method-count rejection.
type One struct{}

func (One) Close() error { coupledService(); return nil }

type Registered struct{}

func (Registered) Close() error   { return nil }
func (Registered) RoundTrip() int { return 7 }

type Promoted struct{ Registered }
type PromotedInterface struct{ transport }
type MethodValue struct{}

func (MethodValue) Close() error { return nil }

var clients = []any{SDK{}, One{}}
var registry = make(map[string]transport)
var callback = MethodValue{}.Close

func init() {
	registry["direct"] = Registered{}
	registry["promoted"] = Promoted{}
	registry["interface"] = PromotedInterface{Registered{}}
}

func invoke(t transport) {
	if t.RoundTrip() != 7 || t.Close() != nil {
		panic("interface call")
	}
}

func main() {
	if clients[0].(SDK).Used() != 42 || callback() != nil {
		panic("direct call")
	}
	for _, t := range registry {
		invoke(t)
	}
	defer invoke(registry["promoted"])
	done := make(chan bool)
	go func() { invoke(registry["direct"]); done <- true }()
	<-done
	println("ok")
}
