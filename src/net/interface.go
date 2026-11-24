// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"errors"
	"internal/strconv"
	"sync"
	"sync/atomic"
	"time"
	_ "unsafe"
)

// BUG(mikio): On JS, methods and functions related to
// Interface are not implemented.

// BUG(mikio): On AIX, DragonFly BSD, NetBSD, OpenBSD, Plan 9 and
// Solaris, the MulticastAddrs method of Interface is not implemented.

var (
	errInvalidInterface         = errors.New("invalid network interface")
	errInvalidInterfaceIndex    = errors.New("invalid network interface index")
	errInvalidInterfaceName     = errors.New("invalid network interface name")
	errNoSuchInterface          = errors.New("no such network interface")
	errNoSuchMulticastInterface = errors.New("no such multicast network interface")
)

// An interfaceAddr is the Addr of an OpError about a network
// interface: its name, or its index when it was asked for by index,
// printed in decimal as an IPv6 zone names an interface either way.
type interfaceAddr struct {
	name  string
	index int
}

func (a interfaceAddr) Network() string { return "ip+net" }

func (a interfaceAddr) String() string {
	if a.name != "" {
		return a.name
	}
	return strconv.Itoa(a.index)
}

// Interface represents a mapping between network interface name
// and index. It also represents network interface facility
// information.
type Interface struct {
	Index        int          // positive integer that starts at one, zero is never used
	MTU          int          // maximum transmission unit
	Name         string       // e.g., "en0", "lo0", "eth0.100"; may be the empty string
	HardwareAddr HardwareAddr // IEEE MAC-48, EUI-48 and EUI-64 form
	Flags        Flags        // e.g., FlagUp, FlagLoopback, FlagMulticast
}

type Flags uint

const (
	FlagUp           Flags = 1 << iota // interface is administratively up
	FlagBroadcast                      // interface supports broadcast access capability
	FlagLoopback                       // interface is a loopback interface
	FlagPointToPoint                   // interface belongs to a point-to-point link
	FlagMulticast                      // interface supports multicast access capability
	FlagRunning                        // interface is in running state
)

var flagNames = []string{
	"up",
	"broadcast",
	"loopback",
	"pointtopoint",
	"multicast",
	"running",
}

func (f Flags) String() string {
	s := ""
	for i, name := range flagNames {
		if f&(1<<uint(i)) != 0 {
			if s != "" {
				s += "|"
			}
			s += name
		}
	}
	if s == "" {
		s = "0"
	}
	return s
}

// Addrs returns a list of unicast interface addresses for a specific
// interface.
func (ifi *Interface) Addrs() ([]Addr, error) {
	if ifi == nil {
		return nil, &OpError{Op: "route", Net: "ip+net", Source: nil, Addr: nil, Err: errInvalidInterface}
	}
	ifat, err := interfaceAddrTable(ifi)
	if err != nil {
		err = &OpError{Op: "route", Net: "ip+net", Source: nil, Addr: interfaceAddr{name: ifi.Name}, Err: err}
	}
	return ifat, err
}

// MulticastAddrs returns a list of multicast, joined group addresses
// for a specific interface.
func (ifi *Interface) MulticastAddrs() ([]Addr, error) {
	if ifi == nil {
		return nil, &OpError{Op: "route", Net: "ip+net", Source: nil, Addr: nil, Err: errInvalidInterface}
	}
	ifat, err := interfaceMulticastAddrTable(ifi)
	if err != nil {
		err = &OpError{Op: "route", Net: "ip+net", Source: nil, Addr: interfaceAddr{name: ifi.Name}, Err: err}
	}
	return ifat, err
}

// Interfaces returns a list of the system's network interfaces.
func Interfaces() ([]Interface, error) {
	ift, err := interfaceTable(0)
	if err != nil {
		return nil, &OpError{Op: "route", Net: "ip+net", Source: nil, Addr: nil, Err: err}
	}
	if len(ift) != 0 {
		zoneCache.update(ift, false)
	}
	return ift, nil
}

// InterfaceAddrs returns a list of the system's unicast interface
// addresses.
//
// The returned list does not identify the associated interface; use
// Interfaces and [Interface.Addrs] for more detail.
func InterfaceAddrs() ([]Addr, error) {
	ifat, err := interfaceAddrTable(nil)
	if err != nil {
		err = &OpError{Op: "route", Net: "ip+net", Source: nil, Addr: nil, Err: err}
	}
	return ifat, err
}

// InterfaceByIndex returns the interface specified by index.
//
// On Solaris, it returns one of the logical network interfaces
// sharing the logical data link; for more precision use
// [InterfaceByName].
func InterfaceByIndex(index int) (*Interface, error) {
	if index <= 0 {
		return nil, &OpError{Op: "route", Net: "ip+net", Source: nil, Addr: interfaceAddr{index: index}, Err: errInvalidInterfaceIndex}
	}
	ift, err := interfaceTable(index)
	if err != nil {
		return nil, &OpError{Op: "route", Net: "ip+net", Source: nil, Addr: interfaceAddr{index: index}, Err: err}
	}
	ifi, err := interfaceByIndex(ift, index)
	if err != nil {
		err = &OpError{Op: "route", Net: "ip+net", Source: nil, Addr: interfaceAddr{index: index}, Err: err}
	}
	return ifi, err
}

func interfaceByIndex(ift []Interface, index int) (*Interface, error) {
	for _, ifi := range ift {
		if index == ifi.Index {
			return &ifi, nil
		}
	}
	return nil, errNoSuchInterface
}

// InterfaceByName returns the interface specified by name.
func InterfaceByName(name string) (*Interface, error) {
	if name == "" {
		return nil, &OpError{Op: "route", Net: "ip+net", Source: nil, Addr: nil, Err: errInvalidInterfaceName}
	}
	ift, err := interfaceTable(0)
	if err != nil {
		return nil, &OpError{Op: "route", Net: "ip+net", Source: nil, Addr: interfaceAddr{name: name}, Err: err}
	}
	if len(ift) != 0 {
		zoneCache.update(ift, false)
	}
	for _, ifi := range ift {
		if name == ifi.Name {
			return &ifi, nil
		}
	}
	return nil, &OpError{Op: "route", Net: "ip+net", Source: nil, Addr: interfaceAddr{name: name}, Err: errNoSuchInterface}
}

// An ipv6ZoneCache represents a cache holding partial network
// interface information. It is used for reducing the cost of IPv6
// addressing scope zone resolution.
//
// Multiple names sharing the index are managed by first-come
// first-served basis for consistency.
type zoneCacheSnapshot struct {
	lastFetched int64 // runtimeNano()
	toIndex     map[string]int
	toName      map[int]string
}

type ipv6ZoneCache struct {
	mu   sync.Mutex
	snap atomic.Pointer[zoneCacheSnapshot]
}

var zoneCache ipv6ZoneCache

const zoneCacheTTL = int64(60 * time.Second)

// update refreshes the network interface information if the cache was last
// updated more than 1 minute ago, or if force is set. It reports whether the
// cache was updated.
func (zc *ipv6ZoneCache) update(ift []Interface, force bool) (updated bool) {
	zc.mu.Lock()
	defer zc.mu.Unlock()
	now := runtimeNano()
	if snap := zc.snap.Load(); !force && snap != nil && now-snap.lastFetched < zoneCacheTTL {
		return false
	}
	if len(ift) == 0 {
		var err error
		if ift, err = interfaceTable(0); err != nil {
			return false
		}
	}
	toIndex := make(map[string]int, len(ift))
	toName := make(map[int]string, len(ift))
	for _, ifi := range ift {
		if ifi.Name != "" {
			toIndex[ifi.Name] = ifi.Index
			if _, ok := toName[ifi.Index]; !ok {
				toName[ifi.Index] = ifi.Name
			}
		}
	}
	zc.snap.Store(&zoneCacheSnapshot{
		lastFetched: now,
		toIndex:     toIndex,
		toName:      toName,
	})
	return true
}

func (zc *ipv6ZoneCache) name(index int) string {
	if index == 0 {
		return ""
	}
	snap := zc.snap.Load()
	updated := false
	if snap == nil || runtimeNano()-snap.lastFetched >= zoneCacheTTL {
		updated = zc.update(nil, false)
		snap = zc.snap.Load()
	}
	if snap != nil {
		if name, ok := snap.toName[index]; ok {
			return name
		}
	}
	if !updated {
		zc.update(nil, true)
		if snap = zc.snap.Load(); snap != nil {
			if name, ok := snap.toName[index]; ok {
				return name
			}
		}
	}
	return strconv.Itoa(index)
}

func (zc *ipv6ZoneCache) index(name string) int {
	if name == "" {
		return 0
	}
	snap := zc.snap.Load()
	updated := false
	if snap == nil || runtimeNano()-snap.lastFetched >= zoneCacheTTL {
		updated = zc.update(nil, false)
		snap = zc.snap.Load()
	}
	if snap != nil {
		if index, ok := snap.toIndex[name]; ok {
			return index
		}
	}
	if !updated {
		zc.update(nil, true)
		if snap = zc.snap.Load(); snap != nil {
			if index, ok := snap.toIndex[name]; ok {
				return index
			}
		}
	}
	index, _, _ := dtoi(name)
	return index
}
