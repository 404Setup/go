// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func withMockDNSCacheClock(t *testing.T) *atomic.Int64 {
	t.Helper()
	var now atomic.Int64
	now.Store(int64(1000 * time.Second))
	orig := dnsCacheNow
	dnsCacheNow = func() int64 {
		return now.Load()
	}
	t.Cleanup(func() {
		dnsCacheNow = orig
		DefaultResolver.dnsCache.clear()
	})
	DefaultResolver.dnsCache.clear()
	return &now
}

func TestDNSCacheAllRecordTypes(t *testing.T) {
	defer dnsWaitGroup.Wait()
	clock := withMockDNSCacheClock(t)

	var (
		aQueries     atomic.Int32
		aaaaQueries  atomic.Int32
		cnameQueries atomic.Int32
		srvQueries   atomic.Int32
		mxQueries    atomic.Int32
		nsQueries    atomic.Int32
		txtQueries   atomic.Int32
		ptrQueries   atomic.Int32
		ipOctet      atomic.Uint32
	)
	ipOctet.Store(10)

	fake := fakeDNSServer{
		rh: func(_, _ string, q dnsmessage.Message, _ time.Time) (dnsmessage.Message, error) {
			resp := dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 q.Header.ID,
					Response:           true,
					RCode:              dnsmessage.RCodeSuccess,
					RecursionAvailable: true,
				},
				Questions: q.Questions,
			}
			question := q.Questions[0]
			hdr := dnsmessage.ResourceHeader{
				Name:  question.Name,
				Type:  question.Type,
				Class: dnsmessage.ClassINET,
				TTL:   60,
			}
			switch question.Type {
			case dnsmessage.TypeA:
				aQueries.Add(1)
				octet := byte(ipOctet.Load())
				resp.Answers = []dnsmessage.Resource{{
					Header: hdr,
					Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, octet}},
				}}
			case dnsmessage.TypeAAAA:
				aaaaQueries.Add(1)
				octet := byte(ipOctet.Load())
				resp.Answers = []dnsmessage.Resource{{
					Header: hdr,
					Body:   &dnsmessage.AAAAResource{AAAA: [16]byte{0x20, 0x01, 0x0d, 0xb8, 15: octet}},
				}}
			case dnsmessage.TypeCNAME:
				cnameQueries.Add(1)
				resp.Answers = []dnsmessage.Resource{{
					Header: hdr,
					Body:   &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName("canonical.example.com.")},
				}}
			case dnsmessage.TypeSRV:
				srvQueries.Add(1)
				resp.Answers = []dnsmessage.Resource{{
					Header: hdr,
					Body: &dnsmessage.SRVResource{
						Priority: 10,
						Weight:   20,
						Port:     8443,
						Target:   dnsmessage.MustNewName("target.example.com."),
					},
				}}
			case dnsmessage.TypeMX:
				mxQueries.Add(1)
				resp.Answers = []dnsmessage.Resource{{
					Header: hdr,
					Body: &dnsmessage.MXResource{
						Pref: 10,
						MX:   dnsmessage.MustNewName("mail.example.com."),
					},
				}}
			case dnsmessage.TypeNS:
				nsQueries.Add(1)
				resp.Answers = []dnsmessage.Resource{{
					Header: hdr,
					Body:   &dnsmessage.NSResource{NS: dnsmessage.MustNewName("ns1.example.com.")},
				}}
			case dnsmessage.TypeTXT:
				txtQueries.Add(1)
				resp.Answers = []dnsmessage.Resource{{
					Header: hdr,
					Body:   &dnsmessage.TXTResource{TXT: []string{"v=spf1 ", "include:example.com ~all"}},
				}}
			case dnsmessage.TypePTR:
				ptrQueries.Add(1)
				resp.Answers = []dnsmessage.Resource{{
					Header: hdr,
					Body:   &dnsmessage.PTRResource{PTR: dnsmessage.MustNewName("ptr.example.com.")},
				}}
			}
			return resp, nil
		},
	}

	r := &Resolver{PreferGo: true, Dial: fake.DialContext}
	ctx := context.Background()

	// 1. LookupIPAddr / LookupHost (A + AAAA)
	for i := 0; i < 5; i++ {
		addrs, err := r.LookupIPAddr(ctx, "app.example.com.")
		if err != nil {
			t.Fatalf("LookupIPAddr failed: %v", err)
		}
		if len(addrs) != 2 {
			t.Fatalf("LookupIPAddr got %d addrs, want 2", len(addrs))
		}
	}
	if got := aQueries.Load(); got != 1 {
		t.Fatalf("A queries = %d, want 1", got)
	}
	if got := aaaaQueries.Load(); got != 1 {
		t.Fatalf("AAAA queries = %d, want 1", got)
	}

	// 2. LookupCNAME (reuses cached A/AAAA cname first, and caches explicit CNAME when needed)
	for i := 0; i < 3; i++ {
		cname, err := r.LookupCNAME(ctx, "cname-only.example.com.")
		if err != nil {
			t.Fatalf("LookupCNAME failed: %v", err)
		}
		if cname == "" {
			t.Fatalf("LookupCNAME returned empty cname")
		}
	}
	if got := cnameQueries.Load(); got != 1 {
		t.Fatalf("CNAME queries = %d, want 1", got)
	}

	// 3. LookupSRV
	for i := 0; i < 3; i++ {
		_, srvs, err := r.LookupSRV(ctx, "https", "tcp", "example.com.")
		if err != nil {
			t.Fatalf("LookupSRV failed: %v", err)
		}
		if len(srvs) != 1 || srvs[0].Port != 8443 || srvs[0].Target != "target.example.com." {
			t.Fatalf("LookupSRV unexpected result: %+v", srvs)
		}
	}
	if got := srvQueries.Load(); got != 1 {
		t.Fatalf("SRV queries = %d, want 1", got)
	}

	// 4. LookupMX
	for i := 0; i < 3; i++ {
		mxs, err := r.LookupMX(ctx, "example.com.")
		if err != nil {
			t.Fatalf("LookupMX failed: %v", err)
		}
		if len(mxs) != 1 || mxs[0].Host != "mail.example.com." || mxs[0].Pref != 10 {
			t.Fatalf("LookupMX unexpected result: %+v", mxs)
		}
	}
	if got := mxQueries.Load(); got != 1 {
		t.Fatalf("MX queries = %d, want 1", got)
	}

	// 5. LookupNS
	for i := 0; i < 3; i++ {
		nss, err := r.LookupNS(ctx, "example.com.")
		if err != nil {
			t.Fatalf("LookupNS failed: %v", err)
		}
		if len(nss) != 1 || nss[0].Host != "ns1.example.com." {
			t.Fatalf("LookupNS unexpected result: %+v", nss)
		}
	}
	if got := nsQueries.Load(); got != 1 {
		t.Fatalf("NS queries = %d, want 1", got)
	}

	// 6. LookupTXT
	for i := 0; i < 3; i++ {
		txts, err := r.LookupTXT(ctx, "example.com.")
		if err != nil {
			t.Fatalf("LookupTXT failed: %v", err)
		}
		if len(txts) != 1 || txts[0] != "v=spf1 include:example.com ~all" {
			t.Fatalf("LookupTXT unexpected result: %+v", txts)
		}
	}
	if got := txtQueries.Load(); got != 1 {
		t.Fatalf("TXT queries = %d, want 1", got)
	}

	// 7. LookupAddr (PTR)
	for i := 0; i < 3; i++ {
		ptrs, err := r.LookupAddr(ctx, "192.0.2.1")
		if err != nil {
			t.Fatalf("LookupAddr failed: %v", err)
		}
		if len(ptrs) != 1 || ptrs[0] != "ptr.example.com." {
			t.Fatalf("LookupAddr unexpected result: %+v", ptrs)
		}
	}
	if got := ptrQueries.Load(); got != 1 {
		t.Fatalf("PTR queries = %d, want 1", got)
	}

	// Advance clock by 59s (still within 60s TTL): must still hit cache.
	aBefore := aQueries.Load()
	ipOctet.Store(99)
	clock.Add(int64(59 * time.Second))
	hosts, err := r.LookupHost(ctx, "app.example.com.")
	if err != nil {
		t.Fatalf("LookupHost before TTL expiry failed: %v", err)
	}
	if !slices.Contains(hosts, "192.0.2.10") {
		t.Fatalf("LookupHost before TTL expiry got %v, want 192.0.2.10", hosts)
	}
	if got := aQueries.Load(); got != aBefore {
		t.Fatalf("A queries before TTL expiry = %d, want %d", got, aBefore)
	}

	// Advance clock past 60s TTL: must evict and fetch fresh record (192.0.2.99).
	clock.Add(int64(2 * time.Second))
	hosts, err = r.LookupHost(ctx, "app.example.com.")
	if err != nil {
		t.Fatalf("LookupHost after TTL expiry failed: %v", err)
	}
	if !slices.Contains(hosts, "192.0.2.99") {
		t.Fatalf("LookupHost after TTL expiry got %v, want 192.0.2.99", hosts)
	}
	if got := aQueries.Load(); got != aBefore+1 {
		t.Fatalf("A queries after TTL expiry = %d, want %d", got, aBefore+1)
	}
}

func TestDNSCacheMinTTLAndZeroTTL(t *testing.T) {
	defer dnsWaitGroup.Wait()
	clock := withMockDNSCacheClock(t)

	var queries atomic.Int32
	fake := fakeDNSServer{
		rh: func(_, _ string, q dnsmessage.Message, _ time.Time) (dnsmessage.Message, error) {
			queries.Add(1)
			question := q.Questions[0]
			resp := dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 q.Header.ID,
					Response:           true,
					RCode:              dnsmessage.RCodeSuccess,
					RecursionAvailable: true,
				},
				Questions: q.Questions,
			}
			if question.Type != dnsmessage.TypeA {
				return resp, nil
			}
			switch question.Name.String() {
			case "cname-short.example.com.":
				// CNAME has TTL=5s, A has TTL=300s -> effective TTL must be 5s.
				resp.Answers = []dnsmessage.Resource{
					{
						Header: dnsmessage.ResourceHeader{
							Name:  question.Name,
							Type:  dnsmessage.TypeCNAME,
							Class: dnsmessage.ClassINET,
							TTL:   5,
						},
						Body: &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName("target.example.com.")},
					},
					{
						Header: dnsmessage.ResourceHeader{
							Name:  dnsmessage.MustNewName("target.example.com."),
							Type:  dnsmessage.TypeA,
							Class: dnsmessage.ClassINET,
							TTL:   300,
						},
						Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}},
					},
				}
			case "multi-a.example.com.":
				// Two A records with TTL=120s and TTL=10s -> effective TTL must be 10s.
				resp.Answers = []dnsmessage.Resource{
					{
						Header: dnsmessage.ResourceHeader{
							Name:  question.Name,
							Type:  dnsmessage.TypeA,
							Class: dnsmessage.ClassINET,
							TTL:   120,
						},
						Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}},
					},
					{
						Header: dnsmessage.ResourceHeader{
							Name:  question.Name,
							Type:  dnsmessage.TypeA,
							Class: dnsmessage.ClassINET,
							TTL:   10,
						},
						Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 2}},
					},
				}
			case "zero-ttl.example.com.":
				// TTL=0 must never be cached (RFC 1035 / RFC 2181).
				resp.Answers = []dnsmessage.Resource{
					{
						Header: dnsmessage.ResourceHeader{
							Name:  question.Name,
							Type:  dnsmessage.TypeA,
							Class: dnsmessage.ClassINET,
							TTL:   0,
						},
						Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 3}},
					},
				}
			case "msb-ttl.example.com.":
				// MSB set in TTL (RFC 2181 section 8) must not be cached.
				resp.Answers = []dnsmessage.Resource{
					{
						Header: dnsmessage.ResourceHeader{
							Name:  question.Name,
							Type:  dnsmessage.TypeA,
							Class: dnsmessage.ClassINET,
							TTL:   0x80000010,
						},
						Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 4}},
					},
				}
			}
			return resp, nil
		},
	}

	r := &Resolver{PreferGo: true, Dial: fake.DialContext}
	ctx := context.Background()

	// Test CNAME minimum TTL (5s vs 300s).
	if _, err := r.LookupIP(ctx, "ip4", "cname-short.example.com."); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(4 * time.Second))
	if _, err := r.LookupIP(ctx, "ip4", "cname-short.example.com."); err != nil {
		t.Fatal(err)
	}
	if got := queries.Load(); got != 1 {
		t.Fatalf("cname-short queries at 4s = %d, want 1", got)
	}
	clock.Add(int64(2 * time.Second)) // total 6s > 5s CNAME TTL
	if _, err := r.LookupIP(ctx, "ip4", "cname-short.example.com."); err != nil {
		t.Fatal(err)
	}
	if got := queries.Load(); got != 2 {
		t.Fatalf("cname-short queries at 6s = %d, want 2", got)
	}

	// Test multi-A minimum TTL (10s vs 120s).
	queries.Store(0)
	if _, err := r.LookupIP(ctx, "ip4", "multi-a.example.com."); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(9 * time.Second))
	if _, err := r.LookupIP(ctx, "ip4", "multi-a.example.com."); err != nil {
		t.Fatal(err)
	}
	if got := queries.Load(); got != 1 {
		t.Fatalf("multi-a queries at 9s = %d, want 1", got)
	}
	clock.Add(int64(2 * time.Second)) // total 11s > 10s min A TTL
	if _, err := r.LookupIP(ctx, "ip4", "multi-a.example.com."); err != nil {
		t.Fatal(err)
	}
	if got := queries.Load(); got != 2 {
		t.Fatalf("multi-a queries at 11s = %d, want 2", got)
	}

	// Test TTL=0 is never cached.
	queries.Store(0)
	for i := 0; i < 3; i++ {
		if _, err := r.LookupIP(ctx, "ip4", "zero-ttl.example.com."); err != nil {
			t.Fatal(err)
		}
	}
	if got := queries.Load(); got != 3 {
		t.Fatalf("zero-ttl queries = %d, want 3", got)
	}

	// Test MSB-set TTL is never cached.
	queries.Store(0)
	for i := 0; i < 3; i++ {
		if _, err := r.LookupIP(ctx, "ip4", "msb-ttl.example.com."); err != nil {
			t.Fatal(err)
		}
	}
	if got := queries.Load(); got != 3 {
		t.Fatalf("msb-ttl queries = %d, want 3", got)
	}
}

func TestDNSCacheNegativeWithSOA(t *testing.T) {
	defer dnsWaitGroup.Wait()
	clock := withMockDNSCacheClock(t)

	var nxQueries, noSOAQueries atomic.Int32
	fake := fakeDNSServer{
		rh: func(_, _ string, q dnsmessage.Message, _ time.Time) (dnsmessage.Message, error) {
			question := q.Questions[0]
			if question.Name.String() == "nosoa.example.com." {
				noSOAQueries.Add(1)
				return dnsmessage.Message{
					Header: dnsmessage.Header{
						ID:       q.Header.ID,
						Response: true,
						RCode:    dnsmessage.RCodeNameError,
					},
					Questions: q.Questions,
				}, nil
			}
			nxQueries.Add(1)
			return dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:       q.Header.ID,
					Response: true,
					RCode:    dnsmessage.RCodeNameError,
				},
				Questions: q.Questions,
				Authorities: []dnsmessage.Resource{
					{
						Header: dnsmessage.ResourceHeader{
							Name:  dnsmessage.MustNewName("example.com."),
							Type:  dnsmessage.TypeSOA,
							Class: dnsmessage.ClassINET,
							TTL:   60,
						},
						Body: &dnsmessage.SOAResource{
							NS:     dnsmessage.MustNewName("ns1.example.com."),
							MBox:   dnsmessage.MustNewName("hostmaster.example.com."),
							Serial: 1,
							MinTTL: 15, // min(60, 15) = 15s
						},
					},
				},
			}, nil
		},
	}

	r := &Resolver{PreferGo: true, Dial: fake.DialContext}
	ctx := context.Background()

	// NXDOMAIN with SOA should be cached for min(60, 15) = 15s.
	for i := 0; i < 4; i++ {
		_, err := r.LookupTXT(ctx, "nx.example.com.")
		var dnsErr *DNSError
		if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
			t.Fatalf("expected IsNotFound DNSError, got %v", err)
		}
		// Mutate returned DNSError.Name to verify cached entry is not corrupted.
		dnsErr.Name = "mutated"
	}
	if got := nxQueries.Load(); got != 1 {
		t.Fatalf("nxQueries = %d, want 1", got)
	}

	// Verify subsequent hit still reports the queried name, not "mutated".
	_, err := r.LookupTXT(ctx, "nx.example.com.")
	if dnsErr, ok := err.(*DNSError); !ok || dnsErr.Name != "nx.example.com." {
		t.Fatalf("cached DNSError.Name = %v, want nx.example.com.", err)
	}

	// Advance past 15s negative TTL.
	clock.Add(int64(16 * time.Second))
	_, _ = r.LookupTXT(ctx, "nx.example.com.")
	if got := nxQueries.Load(); got != 2 {
		t.Fatalf("nxQueries after expiry = %d, want 2", got)
	}

	// NXDOMAIN without SOA must not be cached.
	for i := 0; i < 3; i++ {
		_, _ = r.LookupTXT(ctx, "nosoa.example.com.")
	}
	if got := noSOAQueries.Load(); got != 3 {
		t.Fatalf("noSOAQueries = %d, want 3", got)
	}
}

func TestDNSCacheDualStackNODATAAndSearchList(t *testing.T) {
	defer dnsWaitGroup.Wait()
	clock := withMockDNSCacheClock(t)

	conf, err := newResolvConfTest()
	if err != nil {
		t.Fatal(err)
	}
	defer conf.teardown()

	if err := conf.writeAndUpdate([]string{
		"nameserver 192.0.2.1",
		"search corp.example.com",
		"options ndots:2",
	}); err != nil {
		t.Fatal(err)
	}

	var totalQueries atomic.Int32
	fake := fakeDNSServer{
		rh: func(_, _ string, q dnsmessage.Message, _ time.Time) (dnsmessage.Message, error) {
			totalQueries.Add(1)
			question := q.Questions[0]
			// "svc.prod.corp.example.com." returns NXDOMAIN without SOA.
			if question.Name.String() == "svc.prod.corp.example.com." {
				return dnsmessage.Message{
					Header: dnsmessage.Header{
						ID:       q.Header.ID,
						Response: true,
						RCode:    dnsmessage.RCodeNameError,
					},
					Questions: q.Questions,
				}, nil
			}
			// "svc.prod." has an A record (TTL=30s) and NODATA (no SOA) for AAAA.
			resp := dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 q.Header.ID,
					Response:           true,
					RCode:              dnsmessage.RCodeSuccess,
					RecursionAvailable: true,
				},
				Questions: q.Questions,
			}
			if question.Name.String() == "svc.prod." && question.Type == dnsmessage.TypeA {
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  question.Name,
						Type:  dnsmessage.TypeA,
						Class: dnsmessage.ClassINET,
						TTL:   30,
					},
					Body: &dnsmessage.AResource{A: [4]byte{10, 1, 2, 3}},
				}}
			}
			return resp, nil
		},
	}

	r := &Resolver{PreferGo: true, Dial: fake.DialContext}
	ctx := context.Background()

	// First lookup walks "svc.prod.corp.example.com." (A+AAAA) then "svc.prod." (A+AAAA) = 4 queries.
	addrs, err := r.LookupHost(ctx, "svc.prod")
	if err != nil {
		t.Fatalf("first LookupHost failed: %v", err)
	}
	if !slices.Equal(addrs, []string{"10.1.2.3"}) {
		t.Fatalf("got %v, want [10.1.2.3]", addrs)
	}
	if got := totalQueries.Load(); got != 4 {
		t.Fatalf("first lookup queries = %d, want 4", got)
	}

	// Subsequent dual-stack lookups within 30s must hit cache for both search candidate
	// and AAAA NODATA, making 0 network queries.
	for i := 0; i < 5; i++ {
		addrs, err := r.LookupHost(ctx, "svc.prod")
		if err != nil || !slices.Equal(addrs, []string{"10.1.2.3"}) {
			t.Fatalf("cached LookupHost failed: %v, %v", addrs, err)
		}
	}
	if got := totalQueries.Load(); got != 4 {
		t.Fatalf("cached lookup queries = %d, want 4 (0 additional queries)", got)
	}

	// After 31s, all entries expire and are re-queried.
	clock.Add(int64(31 * time.Second))
	if _, err := r.LookupHost(ctx, "svc.prod"); err != nil {
		t.Fatalf("post-expiry LookupHost failed: %v", err)
	}
	if got := totalQueries.Load(); got != 8 {
		t.Fatalf("post-expiry queries = %d, want 8", got)
	}
}

func TestDNSCacheCaseInsensitiveAndContextCancel(t *testing.T) {
	defer dnsWaitGroup.Wait()
	_ = withMockDNSCacheClock(t)

	var queries atomic.Int32
	fake := fakeDNSServer{
		rh: func(_, _ string, q dnsmessage.Message, _ time.Time) (dnsmessage.Message, error) {
			queries.Add(1)
			return dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 q.Header.ID,
					Response:           true,
					RCode:              dnsmessage.RCodeSuccess,
					RecursionAvailable: true,
				},
				Questions: q.Questions,
				Answers: []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  q.Questions[0].Name,
						Type:  dnsmessage.TypeTXT,
						Class: dnsmessage.ClassINET,
						TTL:   60,
					},
					Body: &dnsmessage.TXTResource{TXT: []string{"hello"}},
				}},
			}, nil
		},
	}

	r := &Resolver{PreferGo: true, Dial: fake.DialContext}
	ctx := context.Background()

	for _, name := range []string{"MixedCase.Example.COM.", "mixedcase.example.com.", "MIXEDCASE.EXAMPLE.COM."} {
		txts, err := r.LookupTXT(ctx, name)
		if err != nil || len(txts) != 1 || txts[0] != "hello" {
			t.Fatalf("LookupTXT(%q) = %v, %v", name, txts, err)
		}
	}
	if got := queries.Load(); got != 1 {
		t.Fatalf("queries across case variations = %d, want 1", got)
	}

	// Canceled context must fail even when answer is in the cache.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.LookupTXT(canceledCtx, "mixedcase.example.com."); err == nil {
		t.Fatal("expected error for canceled context, got nil")
	}
}

func TestDNSCacheConfigReloadAndGODEBUG(t *testing.T) {
	defer dnsWaitGroup.Wait()
	_ = withMockDNSCacheClock(t)

	conf, err := newResolvConfTest()
	if err != nil {
		t.Fatal(err)
	}
	defer conf.teardown()

	if err := conf.writeAndUpdate([]string{"nameserver 192.0.2.1"}); err != nil {
		t.Fatal(err)
	}

	var queries atomic.Int32
	fake := fakeDNSServer{
		rh: func(_, _ string, q dnsmessage.Message, _ time.Time) (dnsmessage.Message, error) {
			queries.Add(1)
			return dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 q.Header.ID,
					Response:           true,
					RCode:              dnsmessage.RCodeSuccess,
					RecursionAvailable: true,
				},
				Questions: q.Questions,
				Answers: []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  q.Questions[0].Name,
						Type:  dnsmessage.TypeTXT,
						Class: dnsmessage.ClassINET,
						TTL:   60,
					},
					Body: &dnsmessage.TXTResource{TXT: []string{"ok"}},
				}},
			}, nil
		},
	}

	r := &Resolver{PreferGo: true, Dial: fake.DialContext}
	ctx := context.Background()

	if _, err := r.LookupTXT(ctx, "reload.example.com."); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LookupTXT(ctx, "reload.example.com."); err != nil {
		t.Fatal(err)
	}
	if got := queries.Load(); got != 1 {
		t.Fatalf("queries before reload = %d, want 1", got)
	}

	// Reloading resolv.conf produces a new *dnsConfig and must invalidate the cache.
	if err := conf.writeAndUpdate([]string{"nameserver 192.0.2.2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LookupTXT(ctx, "reload.example.com."); err != nil {
		t.Fatal(err)
	}
	if got := queries.Load(); got != 2 {
		t.Fatalf("queries after reload = %d, want 2", got)
	}

	// Disabling cache via GODEBUG=netdnscache=0 must bypass cache.
	t.Setenv("GODEBUG", "netdnscache=0")
	for i := 0; i < 3; i++ {
		if _, err := r.LookupTXT(ctx, "reload.example.com."); err != nil {
			t.Fatal(err)
		}
	}
	if got := queries.Load(); got != 5 {
		t.Fatalf("queries with GODEBUG=netdnscache=0 = %d, want 5", got)
	}
}

func TestDNSCacheEvictionAndCapacityBound(t *testing.T) {
	clock := withMockDNSCacheClock(t)
	var c dnsCache
	cfg := &dnsConfig{servers: defaultNS}

	var p dnsmessage.Parser
	// Populate 100 entries with 10s TTL.
	for i := 0; i < 100; i++ {
		key := dnsCacheKey{name: fmt.Sprintf("host-%d.example.com.", i), qtype: dnsmessage.TypeA}
		c.store(key, &dnsCacheEntry{
			p:        p,
			server:   "127.0.0.1:53",
			cfg:      cfg,
			expireAt: clock.Load() + int64(10*time.Second),
		}, clock.Load())
	}
	if got := c.size.Load(); got != 100 {
		t.Fatalf("size = %d, want 100", got)
	}

	// Advance clock by 35s (> 10s TTL and > 30s sweep interval) and insert 1 new entry.
	clock.Add(int64(35 * time.Second))
	c.store(dnsCacheKey{name: "fresh.example.com.", qtype: dnsmessage.TypeA}, &dnsCacheEntry{
		p:        p,
		server:   "127.0.0.1:53",
		cfg:      cfg,
		expireAt: clock.Load() + int64(60*time.Second),
	}, clock.Load())

	// All 100 expired entries must have been swept, leaving only the 1 fresh entry.
	if got := c.size.Load(); got != 1 {
		t.Fatalf("size after TTL sweep = %d, want 1", got)
	}

	// Now insert maxDNSCacheEntries + 500 unexpired entries to trigger capacity eviction.
	now := clock.Load()
	for i := 0; i < maxDNSCacheEntries+500; i++ {
		key := dnsCacheKey{name: fmt.Sprintf("cap-%d.example.com.", i), qtype: dnsmessage.TypeA}
		c.store(key, &dnsCacheEntry{
			p:        p,
			server:   "127.0.0.1:53",
			cfg:      cfg,
			expireAt: now + int64(300*time.Second),
		}, now)
	}
	if got := c.size.Load(); got > maxDNSCacheEntries {
		t.Fatalf("size = %d, exceeds maxDNSCacheEntries (%d)", got, maxDNSCacheEntries)
	}
}

func TestDNSCacheRealTimeTTL(t *testing.T) {
	defer dnsWaitGroup.Wait()

	var queries atomic.Int32
	fake := fakeDNSServer{
		rh: func(_, _ string, q dnsmessage.Message, _ time.Time) (dnsmessage.Message, error) {
			queries.Add(1)
			return dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 q.Header.ID,
					Response:           true,
					RCode:              dnsmessage.RCodeSuccess,
					RecursionAvailable: true,
				},
				Questions: q.Questions,
				Answers: []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  q.Questions[0].Name,
						Type:  dnsmessage.TypeTXT,
						Class: dnsmessage.ClassINET,
						TTL:   1, // 1 second real-time TTL
					},
					Body: &dnsmessage.TXTResource{TXT: []string{"realtime"}},
				}},
			}, nil
		},
	}

	r := &Resolver{PreferGo: true, Dial: fake.DialContext}
	ctx := context.Background()

	if _, err := r.LookupTXT(ctx, "realtime.example.com."); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LookupTXT(ctx, "realtime.example.com."); err != nil {
		t.Fatal(err)
	}
	if got := queries.Load(); got != 1 {
		t.Fatalf("queries before real-time expiry = %d, want 1", got)
	}

	time.Sleep(1100 * time.Millisecond)
	if _, err := r.LookupTXT(ctx, "realtime.example.com."); err != nil {
		t.Fatal(err)
	}
	if got := queries.Load(); got != 2 {
		t.Fatalf("queries after real-time expiry = %d, want 2", got)
	}
}

func TestDNSCacheConcurrentRace(t *testing.T) {
	defer dnsWaitGroup.Wait()
	clock := withMockDNSCacheClock(t)

	fake := fakeDNSServer{
		rh: func(_, _ string, q dnsmessage.Message, _ time.Time) (dnsmessage.Message, error) {
			question := q.Questions[0]
			resp := dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 q.Header.ID,
					Response:           true,
					RCode:              dnsmessage.RCodeSuccess,
					RecursionAvailable: true,
				},
				Questions: q.Questions,
			}
			switch question.Type {
			case dnsmessage.TypeA:
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  question.Name,
						Type:  dnsmessage.TypeA,
						Class: dnsmessage.ClassINET,
						TTL:   2,
					},
					Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}},
				}}
			case dnsmessage.TypeAAAA:
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  question.Name,
						Type:  dnsmessage.TypeAAAA,
						Class: dnsmessage.ClassINET,
						TTL:   2,
					},
					Body: &dnsmessage.AAAAResource{AAAA: [16]byte{0x20, 0x01, 0x0d, 0xb8, 15: 1}},
				}}
			}
			return resp, nil
		},
	}

	r := &Resolver{PreferGo: true, Dial: fake.DialContext}
	ctx := context.Background()

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				host := fmt.Sprintf("host-%d.example.com.", i%8)
				addrs, err := r.LookupHost(ctx, host)
				if err != nil || len(addrs) == 0 {
					t.Errorf("LookupHost(%q) = %v, %v", host, addrs, err)
					return
				}
				if i%20 == 0 {
					clock.Add(int64(time.Second))
				}
			}
		}(g)
	}
	wg.Wait()
}

func newBenchmarkResolver(ttl uint32) *Resolver {
	fake := fakeDNSServer{
		rh: func(_, _ string, q dnsmessage.Message, _ time.Time) (dnsmessage.Message, error) {
			question := q.Questions[0]
			resp := dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 q.Header.ID,
					Response:           true,
					RCode:              dnsmessage.RCodeSuccess,
					RecursionAvailable: true,
				},
				Questions: q.Questions,
			}
			switch question.Type {
			case dnsmessage.TypeA:
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  question.Name,
						Type:  dnsmessage.TypeA,
						Class: dnsmessage.ClassINET,
						TTL:   ttl,
					},
					Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}},
				}}
			case dnsmessage.TypeAAAA:
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  question.Name,
						Type:  dnsmessage.TypeAAAA,
						Class: dnsmessage.ClassINET,
						TTL:   ttl,
					},
					Body: &dnsmessage.AAAAResource{AAAA: [16]byte{0x20, 0x01, 0x0d, 0xb8, 15: 1}},
				}}
			case dnsmessage.TypeTXT:
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name:  question.Name,
						Type:  dnsmessage.TypeTXT,
						Class: dnsmessage.ClassINET,
						TTL:   ttl,
					},
					Body: &dnsmessage.TXTResource{TXT: []string{"v=spf1 include:example.com ~all"}},
				}}
			}
			return resp, nil
		},
	}
	return &Resolver{PreferGo: true, Dial: fake.DialContext}
}

func BenchmarkDNSLookupHostUncached(b *testing.B) {
	b.Setenv("GODEBUG", "netdnscache=0")
	r := newBenchmarkResolver(300)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.LookupHost(ctx, "bench.example.com."); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDNSLookupHostCached(b *testing.B) {
	r := newBenchmarkResolver(300)
	ctx := context.Background()
	if _, err := r.LookupHost(ctx, "bench.example.com."); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.LookupHost(ctx, "bench.example.com."); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDNSLookupHostCachedParallel(b *testing.B) {
	r := newBenchmarkResolver(300)
	ctx := context.Background()
	if _, err := r.LookupHost(ctx, "bench.example.com."); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := r.LookupHost(ctx, "bench.example.com."); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkDNSLookupTXTUncached(b *testing.B) {
	b.Setenv("GODEBUG", "netdnscache=0")
	r := newBenchmarkResolver(300)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.LookupTXT(ctx, "bench.example.com."); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDNSLookupTXTCached(b *testing.B) {
	r := newBenchmarkResolver(300)
	ctx := context.Background()
	if _, err := r.LookupTXT(ctx, "bench.example.com."); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.LookupTXT(ctx, "bench.example.com."); err != nil {
			b.Fatal(err)
		}
	}
}
