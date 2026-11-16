// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"internal/godebug"
	"net/netip"
	"sync/atomic"
	syncv2 "sync/v2"
	"time"
	_ "unsafe"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	// maxDNSCacheTTL caps positive DNS cache TTLs to avoid retaining
	// stale records for excessively long periods.
	maxDNSCacheTTL = 6 * time.Hour

	// maxDNSNegativeCacheTTL caps negative (NXDOMAIN/NODATA) cache TTLs
	// per RFC 2308 section 5.
	maxDNSNegativeCacheTTL = 5 * time.Minute

	// srcAddrCacheTTL is the TTL for cached RFC 6724 source-address route lookups.
	srcAddrCacheTTL = 5 * time.Second

	// maxDNSCacheEntries bounds the total number of cached DNS entries
	// per cache instance.
	maxDNSCacheEntries = 4096

	// dnsCacheEvictTarget is the target entry count when capacity eviction runs.
	dnsCacheEvictTarget = maxDNSCacheEntries * 3 / 4

	// dnsCacheSweepInterval is the minimum interval between proactive
	// expired-entry sweeps triggered during cache writes.
	dnsCacheSweepInterval = 30 * time.Second
)

// netdnscache controls whether the built-in DNS resolver caches responses
// according to record TTLs. It is enabled by default; setting
// GODEBUG=netdnscache=0 disables caching.
var netdnscache = godebug.New("#netdnscache")

//go:linkname runtimeNano runtime.nanotime
func runtimeNano() int64

// dnsCacheNow returns the current monotonic clock reading in nanoseconds.
// It is a variable so tests can advance time deterministically.
var dnsCacheNow = runtimeNano

type dnsCacheKey struct {
	name  string
	qtype dnsmessage.Type
}

type dnsCacheEntry struct {
	p          dnsmessage.Parser
	server     string
	cfg        *dnsConfig
	ips        []IP
	cname      dnsmessage.Name
	expireAt   int64
	isNotFound bool
}

// dnsCache is a concurrent, lock-free read DNS response cache with TTL-based
// expiration and bounded capacity.
type dnsCache struct {
	entries   syncv2.Map[dnsCacheKey, *dnsCacheEntry]
	lastCfg   atomic.Pointer[dnsConfig]
	size      atomic.Int64
	lastSweep atomic.Int64
	sweeping  atomic.Bool
}

type srcAddrCacheEntry struct {
	src      netip.Addr
	expireAt int64
}

var (
	srcAddrCache     syncv2.Map[netip.Addr, srcAddrCacheEntry]
	srcAddrCacheSize atomic.Int64
)

func lookupSrcAddrCache(dst netip.Addr, now int64) (netip.Addr, bool) {
	entry, ok := srcAddrCache.Load(dst)
	if !ok {
		return netip.Addr{}, false
	}
	if now >= entry.expireAt {
		if srcAddrCache.CompareAndDelete(dst, entry) {
			srcAddrCacheSize.Add(-1)
		}
		return netip.Addr{}, false
	}
	return entry.src, true
}

func storeSrcAddrCache(dst, src netip.Addr, now int64) {
	entry := srcAddrCacheEntry{
		src:      src,
		expireAt: now + int64(srcAddrCacheTTL),
	}
	if _, loaded := srcAddrCache.Swap(dst, entry); !loaded {
		if srcAddrCacheSize.Add(1) > maxDNSCacheEntries {
			srcAddrCache.Clear()
			srcAddrCacheSize.Store(0)
		}
	}
}

func dnsCacheEnabled() bool {
	return netdnscache.Value() != "0"
}

func (r *Resolver) getDNSCache() *dnsCache {
	if r == nil || r.Dial == nil {
		return &DefaultResolver.dnsCache
	}
	return &r.dnsCache
}

func (c *dnsCache) checkConfig(cfg *dnsConfig) {
	if cfg == nil {
		return
	}
	if c.lastCfg.Load() != cfg {
		if prev := c.lastCfg.Swap(cfg); prev != cfg && prev != nil {
			c.clear()
		}
	}
}

func (c *dnsCache) clear() {
	c.entries.Clear()
	c.size.Store(0)
	srcAddrCache.Clear()
	srcAddrCacheSize.Store(0)
}

func (c *dnsCache) getEntry(cfg *dnsConfig, name string, qtype dnsmessage.Type) (*dnsCacheEntry, error, bool) {
	if !dnsCacheEnabled() {
		return nil, nil, false
	}
	c.checkConfig(cfg)
	key := dnsCacheKey{name: lowerASCIIName(name), qtype: qtype}
	entry, ok := c.entries.Load(key)
	if !ok {
		return nil, nil, false
	}
	if cfg != nil && entry.cfg != nil && entry.cfg != cfg {
		return nil, nil, false
	}
	now := dnsCacheNow()
	if now >= entry.expireAt {
		if c.entries.CompareAndDelete(key, entry) {
			c.size.Add(-1)
		}
		return nil, nil, false
	}
	if entry.isNotFound {
		// Return a fresh *DNSError per lookup because callers in lookup()
		// and goLookupIPCNAME() mutate err.(*DNSError).Name in place.
		return entry, newDNSError(errNoSuchHost, name, entry.server), true
	}
	return entry, nil, true
}

func (c *dnsCache) get(cfg *dnsConfig, name string, qtype dnsmessage.Type) (dnsmessage.Parser, string, error, bool) {
	entry, err, ok := c.getEntry(cfg, name, qtype)
	if !ok {
		return dnsmessage.Parser{}, "", nil, false
	}
	return entry.p, entry.server, err, true
}

func (c *dnsCache) putPositive(cfg *dnsConfig, name string, qtype dnsmessage.Type, beforeAnswers, answerParser dnsmessage.Parser, server string) {
	if !dnsCacheEnabled() {
		return
	}
	ttl, ok := extractAnswerMinTTL(beforeAnswers, qtype)
	if !ok || ttl <= 0 {
		return
	}
	var ips []IP
	var cname dnsmessage.Name
	if qtype == dnsmessage.TypeA || qtype == dnsmessage.TypeAAAA || qtype == dnsmessage.TypeCNAME {
		ips, cname, ok = extractCachedAddrs(answerParser)
		if !ok {
			return
		}
	}
	c.checkConfig(cfg)
	now := dnsCacheNow()
	entry := &dnsCacheEntry{
		p:        answerParser,
		server:   server,
		cfg:      cfg,
		ips:      ips,
		cname:    cname,
		expireAt: now + int64(ttl),
	}
	c.store(dnsCacheKey{name: lowerASCIIName(name), qtype: qtype}, entry, now)
}

func (c *dnsCache) putNegative(cfg *dnsConfig, name string, qtype dnsmessage.Type, p dnsmessage.Parser, server string) {
	if !dnsCacheEnabled() {
		return
	}
	ttl, ok := extractNegativeTTL(p)
	if !ok || ttl <= 0 {
		return
	}
	c.checkConfig(cfg)
	now := dnsCacheNow()
	entry := &dnsCacheEntry{
		server:     server,
		cfg:        cfg,
		expireAt:   now + int64(ttl),
		isNotFound: true,
	}
	c.store(dnsCacheKey{name: lowerASCIIName(name), qtype: qtype}, entry, now)
}

func (c *dnsCache) putNegativeExpireAt(cfg *dnsConfig, name string, qtype dnsmessage.Type, server string, expireAt int64) {
	if !dnsCacheEnabled() || expireAt <= 0 {
		return
	}
	now := dnsCacheNow()
	if now >= expireAt {
		return
	}
	c.checkConfig(cfg)
	key := dnsCacheKey{name: lowerASCIIName(name), qtype: qtype}
	if existing, ok := c.entries.Load(key); ok && now < existing.expireAt {
		return
	}
	entry := &dnsCacheEntry{
		server:     server,
		cfg:        cfg,
		expireAt:   expireAt,
		isNotFound: true,
	}
	c.store(key, entry, now)
}

// positiveExpireAt returns the minimum expiration timestamp among positive
// cache entries for (name, qtypes). If any qtype in qtypes is not present as
// an unexpired entry in the cache, or no positive entry exists, it returns 0.
func (c *dnsCache) positiveExpireAt(cfg *dnsConfig, name string, qtypes []dnsmessage.Type, hasError [3]bool) (int64, string) {
	if !dnsCacheEnabled() {
		return 0, ""
	}
	now := dnsCacheNow()
	lname := lowerASCIIName(name)
	var minExpire int64
	var server string
	for i, qtype := range qtypes {
		if hasError[i] {
			continue
		}
		entry, ok := c.entries.Load(dnsCacheKey{name: lname, qtype: qtype})
		if !ok || entry.isNotFound || now >= entry.expireAt {
			return 0, ""
		}
		if cfg != nil && entry.cfg != nil && entry.cfg != cfg {
			return 0, ""
		}
		if minExpire == 0 || entry.expireAt < minExpire {
			minExpire = entry.expireAt
			server = entry.server
		}
	}
	return minExpire, server
}

func (c *dnsCache) positiveEntryExpireAt(cfg *dnsConfig, name string, qtype dnsmessage.Type) (int64, string) {
	if !dnsCacheEnabled() {
		return 0, ""
	}
	now := dnsCacheNow()
	entry, ok := c.entries.Load(dnsCacheKey{name: lowerASCIIName(name), qtype: qtype})
	if !ok || entry.isNotFound || now >= entry.expireAt {
		return 0, ""
	}
	if cfg != nil && entry.cfg != nil && entry.cfg != cfg {
		return 0, ""
	}
	return entry.expireAt, entry.server
}

func (c *dnsCache) store(key dnsCacheKey, entry *dnsCacheEntry, now int64) {
	_, loaded := c.entries.Swap(key, entry)
	var count int64
	if !loaded {
		count = c.size.Add(1)
	} else {
		count = c.size.Load()
	}
	if count > maxDNSCacheEntries || (count > 0 && now-c.lastSweep.Load() >= int64(dnsCacheSweepInterval)) {
		c.maybeSweep(now)
	}
}

func (c *dnsCache) maybeSweep(now int64) {
	if !c.sweeping.CompareAndSwap(false, true) {
		return
	}
	defer c.sweeping.Store(false)
	c.lastSweep.Store(now)

	var live, evicted int64
	for k, v := range c.entries.All() {
		if now >= v.expireAt {
			if c.entries.CompareAndDelete(k, v) {
				evicted++
			}
		} else {
			live++
		}
	}

	if live > maxDNSCacheEntries {
		for k, v := range c.entries.All() {
			if live <= dnsCacheEvictTarget {
				break
			}
			if c.entries.CompareAndDelete(k, v) {
				live--
				evicted++
			}
		}
	}

	c.size.Store(live)
	if evicted > 0 {
		c.entries.Shrink()
	}
}

// extractAnswerMinTTL scans the Answer section of p and returns the minimum
// TTL across all records matching qtype or TypeCNAME.
// Per RFC 1035 section 3.2.1 and RFC 2181 section 8, if any relevant record
// has TTL == 0 or its MSB set, the response must not be cached.
func extractAnswerMinTTL(p dnsmessage.Parser, qtype dnsmessage.Type) (time.Duration, bool) {
	var minTTL uint32
	found := false
	for {
		h, err := p.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return 0, false
		}
		if h.Type == qtype || h.Type == dnsmessage.TypeCNAME {
			if h.TTL == 0 || h.TTL&0x80000000 != 0 {
				return 0, false
			}
			if !found || h.TTL < minTTL {
				minTTL = h.TTL
				found = true
			}
		}
		if err := p.SkipAnswer(); err != nil {
			return 0, false
		}
	}
	if !found {
		return 0, false
	}
	ttl := time.Duration(minTTL) * time.Second
	if ttl > maxDNSCacheTTL {
		ttl = maxDNSCacheTTL
	}
	return ttl, true
}

// extractCachedAddrs pre-parses A, AAAA, and CNAME records from p so that
// cache hits in goLookupIPCNAME avoid repeating wire-format unmarshaling and
// per-IP slice allocations.
func extractCachedAddrs(p dnsmessage.Parser) (ips []IP, cname dnsmessage.Name, ok bool) {
	for {
		h, err := p.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			return ips, cname, true
		}
		if err != nil {
			return nil, dnsmessage.Name{}, false
		}
		switch h.Type {
		case dnsmessage.TypeA:
			a, err := p.AResource()
			if err != nil {
				return nil, dnsmessage.Name{}, false
			}
			ips = append(ips, IP(a.A[:]))
			if cname.Length == 0 && h.Name.Length != 0 {
				cname = h.Name
			}
		case dnsmessage.TypeAAAA:
			aaaa, err := p.AAAAResource()
			if err != nil {
				return nil, dnsmessage.Name{}, false
			}
			ips = append(ips, IP(aaaa.AAAA[:]))
			if cname.Length == 0 && h.Name.Length != 0 {
				cname = h.Name
			}
		case dnsmessage.TypeCNAME:
			c, err := p.CNAMEResource()
			if err != nil {
				return nil, dnsmessage.Name{}, false
			}
			if cname.Length == 0 && c.CNAME.Length > 0 {
				cname = c.CNAME
			}
		default:
			if err := p.SkipAnswer(); err != nil {
				return nil, dnsmessage.Name{}, false
			}
		}
	}
}

// extractNegativeTTL scans the Authority section of p for a SOA record and
// returns min(SOA header TTL, SOA MinTTL) per RFC 2308 section 5.
func extractNegativeTTL(p dnsmessage.Parser) (time.Duration, bool) {
	if err := p.SkipAllAnswers(); err != nil {
		return 0, false
	}
	var minTTL uint32
	found := false
	for {
		h, err := p.AuthorityHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return 0, false
		}
		if h.Type == dnsmessage.TypeSOA {
			soa, err := p.SOAResource()
			if err != nil {
				return 0, false
			}
			ttl := min(h.TTL, soa.MinTTL)
			if ttl == 0 || ttl&0x80000000 != 0 {
				return 0, false
			}
			if !found || ttl < minTTL {
				minTTL = ttl
				found = true
			}
			continue
		}
		if err := p.SkipAuthority(); err != nil {
			return 0, false
		}
	}
	if !found {
		return 0, false
	}
	ttl := time.Duration(minTTL) * time.Second
	if ttl > maxDNSNegativeCacheTTL {
		ttl = maxDNSNegativeCacheTTL
	}
	return ttl, true
}

// lowerASCIIName returns s with all ASCII uppercase letters converted to lowercase.
// If s contains no ASCII uppercase letters, it returns s without allocation.
func lowerASCIIName(s string) string {
	for i := 0; i < len(s); i++ {
		if 'A' <= s[i] && s[i] <= 'Z' {
			b := make([]byte, len(s))
			copy(b, s[:i])
			for ; i < len(s); i++ {
				b[i] = lowerASCII(s[i])
			}
			return string(b)
		}
	}
	return s
}
