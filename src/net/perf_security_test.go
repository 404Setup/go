// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix

package net

import (
	"context"
	"internal/nettrace"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDNSCacheCallerSliceMutationDoesNotPoisonCache(t *testing.T) {
	var hits atomic.Int32
	fake := fakeDNSServer{
		rh: func(_, _ string, q dnsmessage.Message, _ time.Time) (dnsmessage.Message, error) {
			hits.Add(1)
			resp := dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 q.Header.ID,
					Response:           true,
					RecursionAvailable: true,
				},
				Questions: q.Questions,
			}
			switch q.Questions[0].Type {
			case dnsmessage.TypeA:
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  q.Questions[0].Name,
						Type:  dnsmessage.TypeA,
						Class: dnsmessage.ClassINET,
						TTL:   60,
					},
					Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 10}},
				}}
			case dnsmessage.TypeAAAA:
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  q.Questions[0].Name,
						Type:  dnsmessage.TypeAAAA,
						Class: dnsmessage.ClassINET,
						TTL:   60,
					},
					Body: &dnsmessage.AAAAResource{AAAA: [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}},
				}}
			}
			return resp, nil
		},
	}

	r := &Resolver{PreferGo: true, Dial: fake.DialContext}
	ctx := context.Background()

	addrs1, err := r.LookupIPAddr(ctx, "slice-isolation.example.")
	if err != nil {
		t.Fatalf("first LookupIPAddr failed: %v", err)
	}
	if len(addrs1) != 2 {
		t.Fatalf("got %d addrs, want 2", len(addrs1))
	}

	// Mutate both slice contents and try appending beyond slice cap.
	for i := range addrs1 {
		for j := range addrs1[i].IP {
			addrs1[i].IP[j] ^= 0xff
		}
		_ = append(addrs1[i].IP, 0xaa, 0xbb)
	}

	addrs2, err := r.LookupIPAddr(ctx, "slice-isolation.example.")
	if err != nil {
		t.Fatalf("second LookupIPAddr failed: %v", err)
	}
	var gotIPs []string
	for _, a := range addrs2 {
		gotIPs = append(gotIPs, a.IP.String())
	}
	if !slices.Contains(gotIPs, "192.0.2.10") || !slices.Contains(gotIPs, "2001:db8::1") {
		t.Fatalf("cache was poisoned by caller mutation: got %v", gotIPs)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("DNS server hits = %d, want 2 (initial A + AAAA only)", got)
	}

	// Mutate LookupHost slice and verify cached strings remain intact.
	hosts1, err := r.LookupHost(ctx, "slice-isolation.example.")
	if err != nil {
		t.Fatalf("first LookupHost failed: %v", err)
	}
	for i := range hosts1 {
		hosts1[i] = "0.0.0.0"
	}
	hosts2, err := r.LookupHost(ctx, "slice-isolation.example.")
	if err != nil {
		t.Fatalf("second LookupHost failed: %v", err)
	}
	if !slices.Contains(hosts2, "192.0.2.10") || !slices.Contains(hosts2, "2001:db8::1") {
		t.Fatalf("LookupHost cache poisoned: got %v", hosts2)
	}
}

func TestDNSCacheTXTSliceMutationAndNettraceCacheHit(t *testing.T) {
	var hits atomic.Int32
	fake := fakeDNSServer{
		rh: func(_, _ string, q dnsmessage.Message, _ time.Time) (dnsmessage.Message, error) {
			hits.Add(1)
			resp := dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 q.Header.ID,
					Response:           true,
					RecursionAvailable: true,
				},
				Questions: q.Questions,
			}
			switch q.Questions[0].Type {
			case dnsmessage.TypeTXT:
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  q.Questions[0].Name,
						Type:  dnsmessage.TypeTXT,
						Class: dnsmessage.ClassINET,
						TTL:   60,
					},
					Body: &dnsmessage.TXTResource{TXT: []string{"v=spf1 ", "-all"}},
				}}
			case dnsmessage.TypeA:
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  q.Questions[0].Name,
						Type:  dnsmessage.TypeA,
						Class: dnsmessage.ClassINET,
						TTL:   60,
					},
					Body: &dnsmessage.AResource{A: [4]byte{198, 51, 100, 7}},
				}}
			}
			return resp, nil
		},
	}

	r := &Resolver{PreferGo: true, Dial: fake.DialContext}
	ctx := context.Background()

	txts1, err := r.LookupTXT(ctx, "txt-cache.example.")
	if err != nil {
		t.Fatalf("first LookupTXT failed: %v", err)
	}
	if !slices.Equal(txts1, []string{"v=spf1 -all"}) {
		t.Fatalf("LookupTXT = %v, want [v=spf1 -all]", txts1)
	}
	txts1[0] = "poisoned"

	txts2, err := r.LookupTXT(ctx, "txt-cache.example.")
	if err != nil {
		t.Fatalf("second LookupTXT failed: %v", err)
	}
	if !slices.Equal(txts2, []string{"v=spf1 -all"}) {
		t.Fatalf("TXT cache poisoned: got %v", txts2)
	}
	if hits.Load() != 1 {
		t.Fatalf("TXT hits = %d, want 1", hits.Load())
	}

	// Warm ip4 cache and verify nettrace hooks still fire on singleflight-bypass hit.
	if _, err := r.LookupIP(ctx, "ip4", "txt-cache.example."); err != nil {
		t.Fatalf("warm LookupIP failed: %v", err)
	}
	var startCalled, doneCalled bool
	traceCtx := context.WithValue(ctx, nettrace.TraceKey{}, &nettrace.Trace{
		DNSStart: func(name string) { startCalled = name == "txt-cache.example." },
		DNSDone: func(addrs []any, coalesced bool, err error) {
			doneCalled = !coalesced && err == nil && len(addrs) == 1
		},
	})
	ips, err := r.LookupIP(traceCtx, "ip4", "txt-cache.example.")
	if err != nil || len(ips) != 1 || ips[0].String() != "198.51.100.7" {
		t.Fatalf("cached LookupIP = (%v, %v), want [198.51.100.7]", ips, err)
	}
	if !startCalled || !doneCalled {
		t.Fatalf("nettrace hooks not invoked on cache hit: start=%v done=%v", startCalled, doneCalled)
	}
}

func TestDNSCachePutNegativeExpireAtConfigIsolationAndSweepSize(t *testing.T) {
	origNow := dnsCacheNow
	defer func() { dnsCacheNow = origNow }()

	now := int64(1_000_000_000_000)
	dnsCacheNow = func() int64 { return now }

	var c dnsCache
	cfg1 := &dnsConfig{servers: []string{"192.0.2.1:53"}}
	cfg2 := &dnsConfig{servers: []string{"192.0.2.2:53"}}

	// Seed with cfg1 negative entry via putNegativeExpireAt.
	c.putNegativeExpireAt(cfg1, "neg.example.", dnsmessage.TypeA, "192.0.2.1:53", now+int64(10*time.Second))
	if c.lastSweep.Load() != now {
		t.Fatalf("first store did not initialize lastSweep=%d, got %d", now, c.lastSweep.Load())
	}
	// Updating with cfg2 should replace cfg1's entry instead of returning early.
	c.putNegativeExpireAt(cfg2, "neg.example.", dnsmessage.TypeA, "192.0.2.2:53", now+int64(20*time.Second))
	_, err, ok := c.getEntry(cfg2, "neg.example.", dnsmessage.TypeA)
	if !ok || err == nil {
		t.Fatalf("expected negative entry under cfg2, got (%v, %v)", err, ok)
	}
	if dErr, ok := err.(*DNSError); !ok || dErr.Server != "192.0.2.2:53" {
		t.Fatalf("expected server 192.0.2.2:53, got %v", err)
	}

	// Verify maybeSweep adjusts size accurately without clobbering concurrent entries.
	c.clear()
	c.store(dnsCacheKey{name: "old.example.", qtype: dnsmessage.TypeA}, &dnsCacheEntry{expireAt: now + 100}, now)
	c.store(dnsCacheKey{name: "fresh.example.", qtype: dnsmessage.TypeA}, &dnsCacheEntry{expireAt: now + int64(time.Minute)}, now)
	now += int64(31 * time.Second)
	c.maybeSweep(now)
	if got := c.size.Load(); got != 1 {
		t.Fatalf("cache size after sweep = %d, want 1", got)
	}
}

func TestSrcAddrCacheConcurrentEviction(t *testing.T) {
	defer clearSrcAddrCache()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			now := dnsCacheNow()
			src := netip.AddrFrom4([4]byte{10, 0, 0, byte(id + 1)})
			for i := 0; i < maxDNSCacheEntries/2; i++ {
				dst := netip.AddrFrom4([4]byte{192, byte(id), byte(i >> 8), byte(i)})
				storeSrcAddrCache(dst, src, now)
				lookupSrcAddrCache(dst, now)
			}
		}(g)
	}
	wg.Wait()
}

func TestIPNetStringAndAddrZoneFormatting(t *testing.T) {
	ip4Net := &IPNet{IP: IPv4(192, 0, 2, 0), Mask: CIDRMask(24, 32)}
	if got := ip4Net.String(); got != "192.0.2.0/24" {
		t.Fatalf("IPv4 IPNet.String() = %q", got)
	}
	ip6Net := &IPNet{IP: ParseIP("2001:db8::"), Mask: CIDRMask(32, 128)}
	if got := ip6Net.String(); got != "2001:db8::/32" {
		t.Fatalf("IPv6 IPNet.String() = %q", got)
	}
	nonCanon := &IPNet{IP: IPv4(198, 51, 100, 0), Mask: IPMask{0xc0, 0x00, 0xff, 0x00}}
	if got := nonCanon.String(); got != "198.51.100.0/c000ff00" {
		t.Fatalf("non-canonical IPNet.String() = %q", got)
	}

	if got := (&TCPAddr{IP: ParseIP("fe80::1"), Port: 443, Zone: "eth0"}).String(); got != "[fe80::1%eth0]:443" {
		t.Fatalf("TCPAddr.String() = %q", got)
	}
	if got := (&TCPAddr{IP: ParseIP("fe80::1"), Port: 443, Zone: "a:b"}).String(); got != "[fe80::1%a:b]:443" {
		t.Fatalf("TCPAddr.String() colon zone = %q", got)
	}
	if got := (&UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 5353}).String(); got != "127.0.0.1:5353" {
		t.Fatalf("UDPAddr.String() IPv4 = %q", got)
	}
	if got := (&UDPAddr{IP: ParseIP("fe80::2"), Port: 5353, Zone: "lo"}).String(); got != "[fe80::2%lo]:5353" {
		t.Fatalf("UDPAddr.String() IPv6 zone = %q", got)
	}
	if got := (&IPAddr{IP: ParseIP("fe80::3"), Zone: "eth1"}).String(); got != "fe80::3%eth1" {
		t.Fatalf("IPAddr.String() = %q", got)
	}
	if got := (HardwareAddr{0x00, 0x00, 0x5e, 0x00, 0x53, 0x01}).String(); got != "00:00:5e:00:53:01" {
		t.Fatalf("HardwareAddr.String() = %q", got)
	}

	for _, tc := range []struct {
		addr, want string
	}{
		{"192.0.2.1", "1.2.0.192.in-addr.arpa."},
		{"::ffff:192.0.2.1", "1.2.0.192.in-addr.arpa."},
		{"2001:db8::1", "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa."},
	} {
		got, err := reverseaddr(tc.addr)
		if err != nil || got != tc.want {
			t.Fatalf("reverseaddr(%q) = (%q, %v), want %q", tc.addr, got, err, tc.want)
		}
	}
}

func TestRFC6724ClassifyMatchesPolicyTable(t *testing.T) {
	samples := []string{
		"::",
		"::1",
		"::ffff:192.0.2.1",
		"::192.0.2.1",
		"192.0.2.1",
		"2001::1",
		"2001:db8::1",
		"2002::1",
		"3ffe::1",
		"fec0::1",
		"fc00::1",
		"fd12:3456::1",
		"fe80::1",
		"2606:4700:4700::1111",
	}
	for _, s := range samples {
		ip := netip.MustParseAddr(s)
		want := rfc6724policyTable.Classify(ip)
		gotP, gotL := classifyRFC6724Prefix(ip)
		if gotP != want.Precedence || gotL != want.Label {
			t.Fatalf("classifyRFC6724Prefix(%s) = (%d, %d), want (%d, %d)", s, gotP, gotL, want.Precedence, want.Label)
		}
	}
}

func TestZoneCacheConcurrent(t *testing.T) {
	ift, err := Interfaces()
	if err != nil || len(ift) == 0 {
		t.Skip("no interfaces available")
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				ifi := ift[i%len(ift)]
				_ = zoneCache.name(ifi.Index)
				_ = zoneCache.index(ifi.Name)
				if id == 0 && i%50 == 0 {
					zoneCache.update(ift, true)
				}
			}
		}(g)
	}
	wg.Wait()
}

func BenchmarkDNSLookupIPAddrCached(b *testing.B) {
	fake := fakeDNSServer{
		rh: func(_, _ string, q dnsmessage.Message, _ time.Time) (dnsmessage.Message, error) {
			resp := dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 q.Header.ID,
					Response:           true,
					RecursionAvailable: true,
				},
				Questions: q.Questions,
			}
			switch q.Questions[0].Type {
			case dnsmessage.TypeA:
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 300},
					Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}},
				}}
			case dnsmessage.TypeAAAA:
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET, TTL: 300},
					Body:   &dnsmessage.AAAAResource{AAAA: [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}},
				}}
			}
			return resp, nil
		},
	}
	r := &Resolver{PreferGo: true, Dial: fake.DialContext}
	ctx := context.Background()
	if _, err := r.LookupIPAddr(ctx, "bench-ipaddr.example."); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.LookupIPAddr(ctx, "bench-ipaddr.example."); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIPNetString(b *testing.B) {
	v4 := &IPNet{IP: IPv4(192, 0, 2, 0), Mask: CIDRMask(24, 32)}
	v6 := &IPNet{IP: ParseIP("2001:db8::"), Mask: CIDRMask(48, 128)}
	b.Run("IPv4", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = v4.String()
		}
	})
	b.Run("IPv6", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = v6.String()
		}
	})
}

func BenchmarkAddrStrings(b *testing.B) {
	tcpZone := &TCPAddr{IP: ParseIP("fe80::1"), Port: 443, Zone: "eth0"}
	udpV4 := &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 5353}
	udpZone := &UDPAddr{IP: ParseIP("fe80::1"), Port: 5353, Zone: "eth0"}
	b.Run("TCPZone", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = tcpZone.String()
		}
	})
	b.Run("UDPv4", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = udpV4.String()
		}
	})
	b.Run("UDPZone", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = udpZone.String()
		}
	})
}

func BenchmarkReverseAddr(b *testing.B) {
	b.Run("IPv4", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := reverseaddr("192.0.2.1"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("IPv6", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := reverseaddr("2001:db8::1"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkZoneCacheName(b *testing.B) {
	ift, err := Interfaces()
	if err != nil || len(ift) == 0 {
		b.Skip("no interfaces available")
	}
	idx := ift[0].Index
	_ = zoneCache.name(idx)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = zoneCache.name(idx)
		}
	})
}
