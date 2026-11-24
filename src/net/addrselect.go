// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Minimal RFC 6724 address selection.

package net

import (
	"internal/byteorder"
	"math/bits"
	"net/netip"
	"slices"
)

func sortByRFC6724(addrs []IPAddr) {
	if len(addrs) < 2 {
		return
	}
	if len(addrs) <= 8 {
		var srcsBuf [8]netip.Addr
		srcs := srcsBuf[:len(addrs)]
		fillSrcAddrs(addrs, srcs)
		sortByRFC6724withSrcs(addrs, srcs)
		return
	}
	sortByRFC6724withSrcs(addrs, srcAddrs(addrs))
}

func sortCachedByRFC6724(addrs []cachedAddr) {
	if len(addrs) < 2 {
		return
	}
	var srcsBuf [8]netip.Addr
	var srcs []netip.Addr
	if len(addrs) <= len(srcsBuf) {
		srcs = srcsBuf[:len(addrs)]
	} else {
		srcs = make([]netip.Addr, len(addrs))
	}
	fillCachedSrcAddrs(addrs, srcs)

	var addrInfosBuf [8]byRFC6724CachedInfo
	var addrInfos []byRFC6724CachedInfo
	if len(addrs) <= len(addrInfosBuf) {
		addrInfos = addrInfosBuf[:len(addrs)]
	} else {
		addrInfos = make([]byRFC6724CachedInfo, len(addrs))
	}
	for i, v := range addrs {
		addrInfos[i] = byRFC6724CachedInfo{
			cached:   v,
			addrAttr: ipAttrOf(v.addr),
			src:      srcs[i],
			srcAttr:  ipAttrOf(srcs[i]),
		}
	}
	slices.SortStableFunc(addrInfos, func(a, b byRFC6724CachedInfo) int {
		return compareRFC6724Attrs(a.cached.addr, b.cached.addr, a.addrAttr, b.addrAttr, a.src, b.src, a.srcAttr, b.srcAttr)
	})
	for i := range addrInfos {
		addrs[i] = addrInfos[i].cached
	}
}

func sortByRFC6724withSrcs(addrs []IPAddr, srcs []netip.Addr) {
	if len(addrs) != len(srcs) {
		panic("internal error")
	}
	var addrInfosBuf [8]byRFC6724Info
	var addrInfos []byRFC6724Info
	if len(addrs) <= len(addrInfosBuf) {
		addrInfos = addrInfosBuf[:len(addrs)]
	} else {
		addrInfos = make([]byRFC6724Info, len(addrs))
	}
	for i, v := range addrs {
		addrAttrIP, _ := netip.AddrFromSlice(v.IP)
		addrInfos[i] = byRFC6724Info{
			addr:     addrs[i],
			addrIP:   addrAttrIP,
			addrAttr: ipAttrOf(addrAttrIP),
			src:      srcs[i],
			srcAttr:  ipAttrOf(srcs[i]),
		}
	}
	slices.SortStableFunc(addrInfos, compareByRFC6724)
	for i := range addrInfos {
		addrs[i] = addrInfos[i].addr
	}
}

// srcAddrs tries to UDP-connect to each address to see if it has a
// route. (This doesn't send any packets). The destination port
// number is irrelevant.
func srcAddrs(addrs []IPAddr) []netip.Addr {
	srcs := make([]netip.Addr, len(addrs))
	fillSrcAddrs(addrs, srcs)
	return srcs
}

//go:noinline
func probeSrcAddr(ip IP, zone string) netip.Addr {
	dst := UDPAddr{IP: ip, Port: 53, Zone: zone}
	c, err := DialUDP("udp", nil, &dst)
	if err != nil {
		return netip.Addr{}
	}
	var src netip.Addr
	if local, ok := c.LocalAddr().(*UDPAddr); ok {
		src, _ = netip.AddrFromSlice(local.IP)
	}
	c.Close()
	return src
}

//go:noinline
func probeCachedSrcAddr(addr netip.Addr) netip.Addr {
	var ipBuf [16]byte
	dst := UDPAddr{Port: 53}
	if addr.Is4() {
		a4 := addr.As4()
		copy(ipBuf[:4], a4[:])
		dst.IP = ipBuf[:4]
	} else {
		ipBuf = addr.As16()
		dst.IP = ipBuf[:]
	}
	c, err := DialUDP("udp", nil, &dst)
	if err != nil {
		return netip.Addr{}
	}
	var src netip.Addr
	if local, ok := c.LocalAddr().(*UDPAddr); ok {
		src, _ = netip.AddrFromSlice(local.IP)
	}
	c.Close()
	return src
}

func fillSrcAddrs(addrs []IPAddr, srcs []netip.Addr) {
	cacheEnabled := dnsCacheEnabled()
	var now int64
	if cacheEnabled {
		now = dnsCacheNow()
	}
	for i := range addrs {
		var dstAddr netip.Addr
		var hasKey bool
		if cacheEnabled {
			if ip, ok := netip.AddrFromSlice(addrs[i].IP); ok {
				dstAddr = ip.Unmap()
				if addrs[i].Zone != "" {
					dstAddr = dstAddr.WithZone(addrs[i].Zone)
				}
				hasKey = true
				if src, ok := lookupSrcAddrCache(dstAddr, now); ok {
					srcs[i] = src
					continue
				}
			}
		}
		srcs[i] = probeSrcAddr(addrs[i].IP, addrs[i].Zone)
		if hasKey {
			storeSrcAddrCache(dstAddr, srcs[i], now)
		}
	}
}

func fillCachedSrcAddrs(addrs []cachedAddr, srcs []netip.Addr) {
	cacheEnabled := dnsCacheEnabled()
	var now int64
	if cacheEnabled {
		now = dnsCacheNow()
	}
	for i := range addrs {
		dstAddr := addrs[i].addr.Unmap()
		if cacheEnabled {
			if src, ok := lookupSrcAddrCache(dstAddr, now); ok {
				srcs[i] = src
				continue
			}
		}
		srcs[i] = probeCachedSrcAddr(addrs[i].addr)
		if cacheEnabled {
			storeSrcAddrCache(dstAddr, srcs[i], now)
		}
	}
}

type ipAttr struct {
	Scope      scope
	Precedence uint8
	Label      uint8
}

func ipAttrOf(ip netip.Addr) ipAttr {
	if !ip.IsValid() {
		return ipAttr{}
	}
	precedence, label := classifyRFC6724Prefix(ip)
	return ipAttr{
		Scope:      classifyScope(ip),
		Precedence: precedence,
		Label:      label,
	}
}

type byRFC6724Info struct {
	addr     IPAddr
	addrIP   netip.Addr
	addrAttr ipAttr
	src      netip.Addr
	srcAttr  ipAttr
}

type byRFC6724CachedInfo struct {
	cached   cachedAddr
	addrAttr ipAttr
	src      netip.Addr
	srcAttr  ipAttr
}

// compareByRFC6724 compares two byRFC6724Info records and returns an integer
// indicating the order. It follows the algorithm and variable names from
// RFC 6724 section 6. Returns -1 if a is preferred, 1 if b is preferred,
// and 0 if they are equal.
func compareByRFC6724(a, b byRFC6724Info) int {
	return compareRFC6724Attrs(a.addrIP, b.addrIP, a.addrAttr, b.addrAttr, a.src, b.src, a.srcAttr, b.srcAttr)
}

func compareRFC6724Attrs(addrA, addrB netip.Addr, aAttr, bAttr ipAttr, SourceDA, SourceDB netip.Addr, sAAttr, sBAttr ipAttr) int {
	attrDA := &aAttr
	attrDB := &bAttr
	attrSourceDA := &sAAttr
	attrSourceDB := &sBAttr

	const preferDA = -1
	const preferDB = 1

	// Rule 1: Avoid unusable destinations.
	// If DB is known to be unreachable or if Source(DB) is undefined, then
	// prefer DA.  Similarly, if DA is known to be unreachable or if
	// Source(DA) is undefined, then prefer DB.
	if !SourceDA.IsValid() && !SourceDB.IsValid() {
		return 0 // "equal"
	}
	if !SourceDB.IsValid() {
		return preferDA
	}
	if !SourceDA.IsValid() {
		return preferDB
	}

	// Rule 2: Prefer matching scope.
	// If Scope(DA) = Scope(Source(DA)) and Scope(DB) <> Scope(Source(DB)),
	// then prefer DA.  Similarly, if Scope(DA) <> Scope(Source(DA)) and
	// Scope(DB) = Scope(Source(DB)), then prefer DB.
	if attrDA.Scope == attrSourceDA.Scope && attrDB.Scope != attrSourceDB.Scope {
		return preferDA
	}
	if attrDA.Scope != attrSourceDA.Scope && attrDB.Scope == attrSourceDB.Scope {
		return preferDB
	}

	// Rule 3: Avoid deprecated addresses.
	// If Source(DA) is deprecated and Source(DB) is not, then prefer DB.
	// Similarly, if Source(DA) is not deprecated and Source(DB) is
	// deprecated, then prefer DA.

	// TODO(bradfitz): implement? low priority for now.

	// Rule 4: Prefer home addresses.
	// If Source(DA) is simultaneously a home address and care-of address
	// and Source(DB) is not, then prefer DA.  Similarly, if Source(DB) is
	// simultaneously a home address and care-of address and Source(DA) is
	// not, then prefer DB.

	// TODO(bradfitz): implement? low priority for now.

	// Rule 5: Prefer matching label.
	// If Label(Source(DA)) = Label(DA) and Label(Source(DB)) <> Label(DB),
	// then prefer DA.  Similarly, if Label(Source(DA)) <> Label(DA) and
	// Label(Source(DB)) = Label(DB), then prefer DB.
	if attrSourceDA.Label == attrDA.Label &&
		attrSourceDB.Label != attrDB.Label {
		return preferDA
	}
	if attrSourceDA.Label != attrDA.Label &&
		attrSourceDB.Label == attrDB.Label {
		return preferDB
	}

	// Rule 6: Prefer higher precedence.
	// If Precedence(DA) > Precedence(DB), then prefer DA.  Similarly, if
	// Precedence(DA) < Precedence(DB), then prefer DB.
	if attrDA.Precedence > attrDB.Precedence {
		return preferDA
	}
	if attrDA.Precedence < attrDB.Precedence {
		return preferDB
	}

	// Rule 7: Prefer native transport.
	// If DA is reached via an encapsulating transition mechanism (e.g.,
	// IPv6 in IPv4) and DB is not, then prefer DB.  Similarly, if DB is
	// reached via encapsulation and DA is not, then prefer DA.

	// TODO(bradfitz): implement? low priority for now.

	// Rule 8: Prefer smaller scope.
	// If Scope(DA) < Scope(DB), then prefer DA.  Similarly, if Scope(DA) >
	// Scope(DB), then prefer DB.
	if attrDA.Scope < attrDB.Scope {
		return preferDA
	}
	if attrDA.Scope > attrDB.Scope {
		return preferDB
	}

	// Rule 9: Use the longest matching prefix.
	// When DA and DB belong to the same address family (both are IPv6 or
	// both are IPv4 [but see below]): If CommonPrefixLen(Source(DA), DA) >
	// CommonPrefixLen(Source(DB), DB), then prefer DA.  Similarly, if
	// CommonPrefixLen(Source(DA), DA) < CommonPrefixLen(Source(DB), DB),
	// then prefer DB.
	//
	// However, applying this rule to IPv4 addresses causes
	// problems (see issues 13283 and 18518), so limit to IPv6.
	if !addrA.Is4() && !addrA.Is4In6() && !addrB.Is4() && !addrB.Is4In6() {
		commonA := commonPrefixLenAddr(SourceDA, addrA)
		commonB := commonPrefixLenAddr(SourceDB, addrB)

		if commonA > commonB {
			return preferDA
		}
		if commonA < commonB {
			return preferDB
		}
	}

	// Rule 10: Otherwise, leave the order unchanged.
	// If DA preceded DB in the original list, prefer DA.
	// Otherwise, prefer DB.
	return 0 // "equal"
}

type policyTableEntry struct {
	Prefix     netip.Prefix
	Precedence uint8
	Label      uint8
}

type policyTable []policyTableEntry

// RFC 6724 section 2.1.
// Items are sorted by the size of their Prefix.Mask.Size,
var rfc6724policyTable = policyTable{
	{
		// "::1/128"
		Prefix:     netip.PrefixFrom(netip.AddrFrom16([16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01}), 128),
		Precedence: 50,
		Label:      0,
	},
	{
		// "::ffff:0:0/96"
		// IPv4-compatible, etc.
		Prefix:     netip.PrefixFrom(netip.AddrFrom16([16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff}), 96),
		Precedence: 35,
		Label:      4,
	},
	{
		// "::/96"
		Prefix:     netip.PrefixFrom(netip.AddrFrom16([16]byte{}), 96),
		Precedence: 1,
		Label:      3,
	},
	{
		// "2001::/32"
		// Teredo
		Prefix:     netip.PrefixFrom(netip.AddrFrom16([16]byte{0x20, 0x01}), 32),
		Precedence: 5,
		Label:      5,
	},
	{
		// "2002::/16"
		// 6to4
		Prefix:     netip.PrefixFrom(netip.AddrFrom16([16]byte{0x20, 0x02}), 16),
		Precedence: 30,
		Label:      2,
	},
	{
		// "3ffe::/16"
		Prefix:     netip.PrefixFrom(netip.AddrFrom16([16]byte{0x3f, 0xfe}), 16),
		Precedence: 1,
		Label:      12,
	},
	{
		// "fec0::/10"
		Prefix:     netip.PrefixFrom(netip.AddrFrom16([16]byte{0xfe, 0xc0}), 10),
		Precedence: 1,
		Label:      11,
	},
	{
		// "fc00::/7"
		Prefix:     netip.PrefixFrom(netip.AddrFrom16([16]byte{0xfc}), 7),
		Precedence: 3,
		Label:      13,
	},
	{
		// "::/0"
		Prefix:     netip.PrefixFrom(netip.AddrFrom16([16]byte{}), 0),
		Precedence: 40,
		Label:      1,
	},
}

// classifyRFC6724Prefix matches ip against rfc6724policyTable in O(1) time.
func classifyRFC6724Prefix(ip netip.Addr) (precedence, label uint8) {
	if ip.Is4() {
		return 35, 4
	}
	a := ip.As16()
	hi := byteorder.BEUint64(a[:8])
	if hi == 0 {
		lo := byteorder.BEUint64(a[8:])
		if lo == 1 {
			return 50, 0
		}
		if lo>>32 == 0xffff {
			return 35, 4
		}
		if lo>>32 == 0 {
			return 1, 3
		}
		return 40, 1
	}
	switch {
	case uint32(hi>>32) == 0x20010000:
		return 5, 5
	case uint16(hi>>48) == 0x2002:
		return 30, 2
	case uint16(hi>>48) == 0x3ffe:
		return 1, 12
	case a[0] == 0xfe && a[1]&0xc0 == 0xc0:
		return 1, 11
	case a[0]&0xfe == 0xfc:
		return 3, 13
	default:
		return 40, 1
	}
}

// Classify returns the policyTableEntry of the entry with the longest
// matching prefix that contains ip.
// The table t must be sorted from largest mask size to smallest.
func (t policyTable) Classify(ip netip.Addr) policyTableEntry {
	// Prefix.Contains() will not match an IPv6 prefix for an IPv4 address.
	if ip.Is4() {
		ip = netip.AddrFrom16(ip.As16())
	}
	for _, ent := range t {
		if ent.Prefix.Contains(ip) {
			return ent
		}
	}
	return policyTableEntry{}
}

// RFC 6724 section 3.1.
type scope uint8

const (
	scopeInterfaceLocal scope = 0x1
	scopeLinkLocal      scope = 0x2
	scopeAdminLocal     scope = 0x4
	scopeSiteLocal      scope = 0x5
	scopeOrgLocal       scope = 0x8
	scopeGlobal         scope = 0xe
)

func classifyScope(ip netip.Addr) scope {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return scopeLinkLocal
	}
	ipv6 := ip.Is6() && !ip.Is4In6()
	ipv6AsBytes := ip.As16()
	if ipv6 && ip.IsMulticast() {
		return scope(ipv6AsBytes[1] & 0xf)
	}
	// Site-local addresses are defined in RFC 3513 section 2.5.6
	// (and deprecated in RFC 3879).
	if ipv6 && ipv6AsBytes[0] == 0xfe && ipv6AsBytes[1]&0xc0 == 0xc0 {
		return scopeSiteLocal
	}
	return scopeGlobal
}

func commonPrefixLenAddr(a, b netip.Addr) int {
	if !a.Is6() || !b.Is6() {
		return 0
	}
	a16 := a.As16()
	b16 := b.As16()
	return bits.LeadingZeros64(byteorder.BEUint64(a16[:8]) ^ byteorder.BEUint64(b16[:8]))
}

// commonPrefixLen reports the length of the longest prefix (looking
// at the most significant, or leftmost, bits) that the
// two addresses have in common, up to the length of a's prefix (i.e.,
// the portion of the address not including the interface ID).
//
// If a or b is an IPv4 address as an IPv6 address, the IPv4 addresses
// are compared (with max common prefix length of 32).
// If a and b are different IP versions, 0 is returned.
//
// See https://tools.ietf.org/html/rfc6724#section-2.2
func commonPrefixLen(a netip.Addr, b IP) int {
	if !a.IsValid() {
		return 0
	}
	if b4 := b.To4(); b4 != nil {
		b = b4
	}
	if a.Is4() {
		if len(b) != 4 {
			return 0
		}
		a4 := a.As4()
		return bits.LeadingZeros32(byteorder.BEUint32(a4[:]) ^ byteorder.BEUint32(b))
	}
	if len(b) != 16 {
		return 0
	}
	a16 := a.As16()
	return bits.LeadingZeros64(byteorder.BEUint64(a16[:8]) ^ byteorder.BEUint64(b[:8]))
}
