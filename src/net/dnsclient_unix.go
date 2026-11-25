// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// DNS client: see RFC 1035.
// Has to be linked into package net for Dial.

// TODO(rsc):
//	Could potentially handle many outstanding lookups faster.
//	Random UDP source port (net.Dial should do that for us).
//	Random request IDs.

package net

import (
	"context"
	"errors"
	"internal/bytealg"
	"internal/godebug"
	"internal/strconv"
	"internal/stringslite"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	// to be used as a useTCP parameter to exchange
	useTCPOnly  = true
	useUDPOrTCP = false

	// Maximum DNS packet size.
	// Value taken from https://dnsflagday.net/2020/.
	maxDNSPacketSize = 1232
)

var (
	errLameReferral              = errors.New("lame referral")
	errCannotUnmarshalDNSMessage = errors.New("cannot unmarshal DNS message")
	errCannotMarshalDNSMessage   = errors.New("cannot marshal DNS message")
	errServerMisbehaving         = errors.New("server misbehaving")
	errInvalidDNSResponse        = errors.New("invalid DNS response")
	errNoAnswerFromDNSServer     = errors.New("no answer from DNS server")

	// errServerTemporarilyMisbehaving is like errServerMisbehaving, except
	// that when it gets translated to a DNSError, the IsTemporary field
	// gets set to true.
	errServerTemporarilyMisbehaving = &temporaryError{"server misbehaving"}
)

// netedns0 controls whether we send an EDNS0 additional header.
var netedns0 = godebug.New("netedns0")

func newRequest(q dnsmessage.Question, ad bool) (id uint16, udpReq, tcpReq []byte, err error) {
	id = uint16(randInt())
	b := dnsmessage.NewBuilder(make([]byte, 2, 514), dnsmessage.Header{ID: id, RecursionDesired: true, AuthenticData: ad})
	if err := b.StartQuestions(); err != nil {
		return 0, nil, nil, err
	}
	if err := b.Question(q); err != nil {
		return 0, nil, nil, err
	}

	if netedns0.Value() == "0" {
		netedns0.IncNonDefault()
	} else {
		// Accept packets up to maxDNSPacketSize.  RFC 6891.
		if err := b.StartAdditionals(); err != nil {
			return 0, nil, nil, err
		}
		var rh dnsmessage.ResourceHeader
		if err := rh.SetEDNS0(maxDNSPacketSize, dnsmessage.RCodeSuccess, false); err != nil {
			return 0, nil, nil, err
		}
		if err := b.OPTResource(rh, dnsmessage.OPTResource{}); err != nil {
			return 0, nil, nil, err
		}
	}

	tcpReq, err = b.Finish()
	if err != nil {
		return 0, nil, nil, err
	}
	udpReq = tcpReq[2:]
	l := len(tcpReq) - 2
	tcpReq[0] = byte(l >> 8)
	tcpReq[1] = byte(l)
	return id, udpReq, tcpReq, nil
}

func checkResponse(reqID uint16, reqQues dnsmessage.Question, respHdr dnsmessage.Header, respQues dnsmessage.Question) bool {
	if !respHdr.Response {
		return false
	}
	if reqID != respHdr.ID {
		return false
	}
	if reqQues.Type != respQues.Type || reqQues.Class != respQues.Class || !equalASCIIName(reqQues.Name, respQues.Name) {
		return false
	}
	return true
}

func dnsPacketRoundTrip(c Conn, id uint16, query dnsmessage.Question, b []byte) (dnsmessage.Parser, dnsmessage.Header, error) {
	if _, err := c.Write(b); err != nil {
		return dnsmessage.Parser{}, dnsmessage.Header{}, err
	}

	b = make([]byte, maxDNSPacketSize)
	for {
		n, err := c.Read(b)
		if err != nil {
			return dnsmessage.Parser{}, dnsmessage.Header{}, err
		}
		var p dnsmessage.Parser
		// Ignore invalid responses as they may be malicious
		// forgery attempts. Instead continue waiting until
		// timeout. See golang.org/issue/13281.
		h, err := p.Start(b[:n])
		if err != nil {
			continue
		}
		q, err := p.Question()
		if err != nil || !checkResponse(id, query, h, q) {
			continue
		}
		return p, h, nil
	}
}

func dnsStreamRoundTrip(c Conn, id uint16, query dnsmessage.Question, b []byte) (dnsmessage.Parser, dnsmessage.Header, error) {
	if _, err := c.Write(b); err != nil {
		return dnsmessage.Parser{}, dnsmessage.Header{}, err
	}

	b = make([]byte, 1280) // 1280 is a reasonable initial size for IP over Ethernet, see RFC 4035
	if _, err := io.ReadFull(c, b[:2]); err != nil {
		return dnsmessage.Parser{}, dnsmessage.Header{}, err
	}
	l := int(b[0])<<8 | int(b[1])
	if l > len(b) {
		b = make([]byte, l)
	}
	n, err := io.ReadFull(c, b[:l])
	if err != nil {
		return dnsmessage.Parser{}, dnsmessage.Header{}, err
	}
	var p dnsmessage.Parser
	h, err := p.Start(b[:n])
	if err != nil {
		return dnsmessage.Parser{}, dnsmessage.Header{}, errCannotUnmarshalDNSMessage
	}
	q, err := p.Question()
	if err != nil {
		return dnsmessage.Parser{}, dnsmessage.Header{}, errCannotUnmarshalDNSMessage
	}
	if !checkResponse(id, query, h, q) {
		return dnsmessage.Parser{}, dnsmessage.Header{}, errInvalidDNSResponse
	}
	return p, h, nil
}

// exchange sends a query on the connection and hopes for a response.
func (r *Resolver) exchange(ctx context.Context, server string, q dnsmessage.Question, timeout time.Duration, useTCP, ad bool) (dnsmessage.Parser, dnsmessage.Header, error) {
	q.Class = dnsmessage.ClassINET
	id, udpReq, tcpReq, err := newRequest(q, ad)
	if err != nil {
		return dnsmessage.Parser{}, dnsmessage.Header{}, errCannotMarshalDNSMessage
	}
	var networks []string
	if useTCP {
		networks = []string{"tcp"}
	} else {
		networks = []string{"udp", "tcp"}
	}
	for _, network := range networks {
		ctx, cancel := context.WithDeadline(ctx, time.Now().Add(timeout))
		defer cancel()

		c, err := r.dial(ctx, network, server)
		if err != nil {
			return dnsmessage.Parser{}, dnsmessage.Header{}, err
		}
		if d, ok := ctx.Deadline(); ok && !d.IsZero() {
			c.SetDeadline(d)
		}
		var p dnsmessage.Parser
		var h dnsmessage.Header
		if _, ok := c.(PacketConn); ok {
			p, h, err = dnsPacketRoundTrip(c, id, q, udpReq)
		} else {
			p, h, err = dnsStreamRoundTrip(c, id, q, tcpReq)
		}
		c.Close()
		if err != nil {
			return dnsmessage.Parser{}, dnsmessage.Header{}, mapErr(err)
		}
		if err := p.SkipQuestion(); err != dnsmessage.ErrSectionDone {
			return dnsmessage.Parser{}, dnsmessage.Header{}, errInvalidDNSResponse
		}
		// RFC 5966 indicates that when a client receives a UDP response with
		// the TC flag set, it should take the TC flag as an indication that it
		// should retry over TCP instead.
		// The case when the TC flag is set in a TCP response is not well specified,
		// so this implements the glibc resolver behavior, returning the existing
		// dns response instead of returning a "errNoAnswerFromDNSServer" error.
		// See go.dev/issue/64896
		if h.Truncated && network == "udp" {
			continue
		}
		return p, h, nil
	}
	return dnsmessage.Parser{}, dnsmessage.Header{}, errNoAnswerFromDNSServer
}

// checkHeader performs basic sanity checks on the header.
func checkHeader(p *dnsmessage.Parser, h dnsmessage.Header) error {
	rcode, hasAdd := extractExtendedRCode(*p, h)

	if rcode == dnsmessage.RCodeNameError {
		return errNoSuchHost
	}

	_, err := p.AnswerHeader()
	if err != nil && err != dnsmessage.ErrSectionDone {
		return errCannotUnmarshalDNSMessage
	}

	// libresolv continues to the next server when it receives
	// an invalid referral response. See golang.org/issue/15434.
	if rcode == dnsmessage.RCodeSuccess && !h.Authoritative && !h.RecursionAvailable && err == dnsmessage.ErrSectionDone && !hasAdd {
		return errLameReferral
	}

	if rcode != dnsmessage.RCodeSuccess && rcode != dnsmessage.RCodeNameError {
		// None of the error codes make sense
		// for the query we sent. If we didn't get
		// a name error and we didn't get success,
		// the server is behaving incorrectly or
		// having temporary trouble.
		if rcode == dnsmessage.RCodeServerFailure {
			return errServerTemporarilyMisbehaving
		}
		return errServerMisbehaving
	}

	return nil
}

func skipToAnswer(p *dnsmessage.Parser, qtype dnsmessage.Type) error {
	for {
		h, err := p.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			return errNoSuchHost
		}
		if err != nil {
			return errCannotUnmarshalDNSMessage
		}
		if h.Type == qtype {
			return nil
		}
		if err := p.SkipAnswer(); err != nil {
			return errCannotUnmarshalDNSMessage
		}
	}
}

// extractExtendedRCode extracts the extended RCode from the OPT resource (EDNS(0))
// If an OPT record is not found, the RCode from the hdr is returned.
// Another return value indicates whether an additional resource was found.
func extractExtendedRCode(p dnsmessage.Parser, hdr dnsmessage.Header) (dnsmessage.RCode, bool) {
	p.SkipAllAnswers()
	p.SkipAllAuthorities()
	hasAdd := false
	for {
		ahdr, err := p.AdditionalHeader()
		if err != nil {
			return hdr.RCode, hasAdd
		}
		hasAdd = true
		if ahdr.Type == dnsmessage.TypeOPT {
			return ahdr.ExtendedRCode(hdr.RCode), hasAdd
		}
		if err := p.SkipAdditional(); err != nil {
			return hdr.RCode, hasAdd
		}
	}
}

// Do a lookup for a single name, which must be rooted
// (otherwise answer will not find the answers).
func (r *Resolver) tryOneName(ctx context.Context, cfg *dnsConfig, name string, qtype dnsmessage.Type) (dnsmessage.Parser, string, error) {
	return r.tryOneNameQuery(ctx, cfg, name, qtype, true)
}

func (r *Resolver) tryOneNameQuery(ctx context.Context, cfg *dnsConfig, name string, qtype dnsmessage.Type, checkCache bool) (dnsmessage.Parser, string, error) {
	_, p, server, err := r.tryOneNameEntry(ctx, cfg, name, qtype, checkCache)
	return p, server, err
}

func (r *Resolver) tryOneNameEntry(ctx context.Context, cfg *dnsConfig, name string, qtype dnsmessage.Type, checkCache bool) (*dnsCacheEntry, dnsmessage.Parser, string, error) {
	var lastErr error
	serverOffset := cfg.serverOffset()
	sLen := uint32(len(cfg.servers))

	n, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, dnsmessage.Parser{}, "", &DNSError{Err: errCannotMarshalDNSMessage.Error(), Name: name}
	}

	if sLen > 0 {
		if err := ctx.Err(); err != nil {
			return nil, dnsmessage.Parser{}, "", newDNSError(mapErr(err), name, cfg.servers[serverOffset%sLen])
		}
		if checkCache {
			if entry, err, ok := r.getDNSCache().getEntry(cfg, name, qtype); ok {
				return entry, entry.p, entry.server, err
			}
		}
	}

	q := dnsmessage.Question{
		Name:  n,
		Type:  qtype,
		Class: dnsmessage.ClassINET,
	}

	cache := r.getDNSCache()
	for i := 0; i < cfg.attempts; i++ {
		for j := uint32(0); j < sLen; j++ {
			server := cfg.servers[(serverOffset+j)%sLen]

			p, h, err := r.exchange(ctx, server, q, cfg.timeout, cfg.useTCP, cfg.trustAD)
			if err != nil {
				dnsErr := newDNSError(err, name, server)
				// Set IsTemporary for socket-level errors. Note that this flag
				// may also be used to indicate a SERVFAIL response.
				if _, ok := err.(*OpError); ok {
					dnsErr.IsTemporary = true
				}
				lastErr = dnsErr
				continue
			}

			if err := checkHeader(&p, h); err != nil {
				if err == errNoSuchHost {
					// The name does not exist, so trying
					// another server won't help.
					cache.putNegative(cfg, name, qtype, p, server)
					return nil, p, server, newDNSError(errNoSuchHost, name, server)
				}
				lastErr = newDNSError(err, name, server)
				continue
			}

			beforeAnswers := p
			if err := skipToAnswer(&p, qtype); err != nil {
				if err == errNoSuchHost {
					// The name does not exist, so trying
					// another server won't help.
					cache.putNegative(cfg, name, qtype, p, server)
					return nil, p, server, newDNSError(errNoSuchHost, name, server)
				}
				lastErr = newDNSError(err, name, server)
				continue
			}

			cache.putPositive(cfg, name, qtype, beforeAnswers, p, server)
			return nil, p, server, nil
		}
	}
	return nil, dnsmessage.Parser{}, "", lastErr
}

// A resolverConfig represents a DNS stub resolver configuration.
type resolverConfig struct {
	initOnce sync.Once // guards init of resolverConfig

	// ch is used as a semaphore that only allows one lookup at a
	// time to recheck resolv.conf.
	ch          chan struct{} // guards lastChecked and modTime
	lastChecked time.Time     // last time resolv.conf was checked

	dnsConfig atomic.Pointer[dnsConfig] // parsed resolv.conf structure used in lookups
}

var resolvConf resolverConfig

func getSystemDNSConfig() *dnsConfig {
	return getSystemDNSConfigNamed("/etc/resolv.conf")
}

func getSystemDNSConfigNamed(path string) *dnsConfig {
	resolvConf.tryUpdate(path)
	return resolvConf.dnsConfig.Load()
}

// init initializes conf and is only called via conf.initOnce.
func (conf *resolverConfig) init() {
	// Set dnsConfig and lastChecked so we don't parse
	// resolv.conf twice the first time.
	conf.dnsConfig.Store(dnsReadConfig("/etc/resolv.conf"))
	conf.lastChecked = time.Now()

	// Prepare ch so that only one update of resolverConfig may
	// run at once.
	conf.ch = make(chan struct{}, 1)
}

// distantFuture is a sentinel time used for tests to signal that
// resolv.conf should not be rechecked.
var distantFuture = time.Date(3000, 1, 2, 3, 4, 5, 6, time.UTC)

// tryUpdate tries to update conf with the named resolv.conf file.
// The name variable only exists for testing. It is otherwise always
// "/etc/resolv.conf".
func (conf *resolverConfig) tryUpdate(name string) {
	conf.initOnce.Do(conf.init)

	dc := conf.dnsConfig.Load()

	// Currently we should never have a config that does not have any
	// available servers to query, since in such cases the servers field
	// is set to [defaultNS], see dnsReadConfig.
	// This assertion main purpose is for testing, such that we never set
	// the mocked dnsConfig in such way.
	if len(dc.servers) == 0 {
		panic("unreachable")
	}

	if dc.noReload {
		return
	}

	// Ensure only one update at a time checks resolv.conf.
	if !conf.tryAcquireSema() {
		return
	}
	defer conf.releaseSema()

	now := time.Now()

	// Only recheck the resolv.conf when:
	// - expired (last re-check was more that 5 seconds ago)
	// - the default nameservers are used (the last parse could not load
	//   resolv.conf, or it did not contain any nameservers)
	// - rechecks are not disabled (only possible in case of testing)
	//
	// Note: We only do one check at a time. Other concurrent requests might
	// still use the previous (outdated) version of the configuration file.
	expired := now.After(conf.lastChecked.Add(5 * time.Second))
	rechecksEnabled := conf.lastChecked != distantFuture // for testing purposes
	recheck := (expired || dc.isDefaultNS()) && rechecksEnabled
	if !recheck {
		return
	}
	conf.lastChecked = now

	switch runtime.GOOS {
	case "windows":
		// There's no file on disk, so don't bother checking
		// and failing.
		//
		// The Windows implementation of dnsReadConfig (called
		// below) ignores the name.
	default:
		var mtime time.Time
		if fi, err := os.Stat(name); err == nil {
			mtime = fi.ModTime()
		}
		if mtime.Equal(conf.dnsConfig.Load().mtime) {
			return
		}
	}

	dnsConf := dnsReadConfig(name)
	conf.dnsConfig.Store(dnsConf)
}

func (conf *resolverConfig) tryAcquireSema() bool {
	select {
	case conf.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (conf *resolverConfig) releaseSema() {
	<-conf.ch
}

func (r *Resolver) lookup(ctx context.Context, name string, qtype dnsmessage.Type, conf *dnsConfig) (dnsmessage.Parser, string, error) {
	_, p, server, err := r.lookupEntry(ctx, name, qtype, conf)
	return p, server, err
}

func (r *Resolver) lookupEntry(ctx context.Context, name string, qtype dnsmessage.Type, conf *dnsConfig) (*dnsCacheEntry, dnsmessage.Parser, string, error) {
	if !isDomainName(name) {
		// We used to use "invalid domain name" as the error,
		// but that is a detail of the specific lookup mechanism.
		// Other lookups might allow broader name syntax
		// (for example Multicast DNS allows UTF-8; see RFC 6762).
		// For consistency with libc resolvers, report no such host.
		return nil, dnsmessage.Parser{}, "", newDNSError(errNoSuchHost, name, "")
	}

	if isLocalhostName(name) {
		// RFC 6761, section 6.3, says non-address queries for
		// localhost names should get negative responses without the
		// query being sent to DNS servers. Address queries never
		// reach here; goLookupIPCNAMEOrder answers them itself.
		return nil, dnsmessage.Parser{}, "", newDNSError(errNoSuchHost, name, "")
	}

	if conf == nil {
		conf = getSystemDNSConfig()
	}

	var (
		entry  *dnsCacheEntry
		p      dnsmessage.Parser
		server string
		err    error
	)
	var fqdnBuf [8]string
	fqdns := conf.appendNameList(fqdnBuf[:0], name)
	var prevNotFound [8]bool
	for i, fqdn := range fqdns {
		entry, p, server, err = r.tryOneNameEntry(ctx, conf, fqdn, qtype, true)
		if err == nil {
			if i > 0 {
				cache := r.getDNSCache()
				if expireAt, srv := cache.positiveEntryExpireAt(conf, fqdn, qtype); expireAt > 0 {
					for j := 0; j < i && j < len(prevNotFound); j++ {
						if prevNotFound[j] {
							cache.putNegativeExpireAt(conf, fqdns[j], qtype, srv, expireAt)
						}
					}
				}
			}
			break
		}
		if i < len(prevNotFound) {
			if dnsErr, ok := err.(*DNSError); ok && dnsErr.IsNotFound {
				prevNotFound[i] = true
			}
		}
		if nerr, ok := err.(Error); ok && nerr.Temporary() && r.strictErrors() {
			// If we hit a temporary error with StrictErrors enabled,
			// stop immediately instead of trying more names.
			break
		}
	}
	if err == nil {
		return entry, p, server, nil
	}
	if err, ok := err.(*DNSError); ok {
		// Show original name passed to lookup, not suffixed one.
		// In general we might have tried many suffixes; showing
		// just one is misleading. See also golang.org/issue/6324.
		err.Name = name
	}
	return nil, dnsmessage.Parser{}, "", err
}

// avoidDNS reports whether this is a hostname for which we should not
// use DNS. Currently this includes only .onion, per RFC 7686. See
// golang.org/issue/13705. Does not cover .local names (RFC 6762),
// see golang.org/issue/16739.
func avoidDNS(name string) bool {
	if name == "" {
		return true
	}
	name = stringslite.TrimSuffix(name, ".")
	return stringsHasSuffixFold(name, ".onion")
}

// isLocalhostName reports whether name is "localhost" or a name
// within the ".localhost" domain, which RFC 6761 reserves to mean
// the IP loopback interface.
func isLocalhostName(name string) bool {
	name = stringslite.TrimSuffix(name, ".")
	return stringsEqualFold(name, "localhost") || stringsHasSuffixFold(name, ".localhost")
}

// nameList returns a list of names for sequential DNS queries.
func (conf *dnsConfig) nameList(name string) []string {
	return conf.appendNameList(nil, name)
}

func (conf *dnsConfig) appendNameList(dst []string, name string) []string {
	// Check name length (see isDomainName).
	rooted := len(name) > 0 && name[len(name)-1] == '.'
	if len(name) > 254 || len(name) == 254 && !rooted {
		return nil
	}

	// If name is rooted (trailing dot), try only that name.
	if rooted {
		if avoidDNS(name) {
			return nil
		}
		return append(dst, name)
	}

	hasNdots := bytealg.CountString(name, '.') >= conf.ndots
	name += "."

	if dst == nil {
		dst = make([]string, 0, 1+len(conf.search))
	}
	// If name has enough dots, try unsuffixed first.
	if hasNdots && !avoidDNS(name) {
		dst = append(dst, name)
	}
	// Try suffixes that are not too long (see isDomainName).
	for _, suffix := range conf.search {
		fqdn := name + suffix
		if !avoidDNS(fqdn) && len(fqdn) <= 254 {
			dst = append(dst, fqdn)
		}
	}
	// Try unsuffixed, if not tried first above.
	if !hasNdots && !avoidDNS(name) {
		dst = append(dst, name)
	}
	return dst
}

// hostLookupOrder specifies the order of LookupHost lookup strategies.
// It is basically a simplified representation of nsswitch.conf.
// "files" means /etc/hosts.
type hostLookupOrder int

const (
	// hostLookupCgo means defer to cgo.
	hostLookupCgo      hostLookupOrder = iota
	hostLookupFilesDNS                 // files first
	hostLookupDNSFiles                 // dns first
	hostLookupFiles                    // only files
	hostLookupDNS                      // only DNS
)

var lookupOrderName = map[hostLookupOrder]string{
	hostLookupCgo:      "cgo",
	hostLookupFilesDNS: "files,dns",
	hostLookupDNSFiles: "dns,files",
	hostLookupFiles:    "files",
	hostLookupDNS:      "dns",
}

func (o hostLookupOrder) String() string {
	if s, ok := lookupOrderName[o]; ok {
		return s
	}
	return "hostLookupOrder=" + strconv.Itoa(int(o)) + "??"
}

func (r *Resolver) tryCachedLookupHost(ctx context.Context, name string, conf *dnsConfig) ([]string, bool) {
	if !dnsCacheEnabled() || !isDomainName(name) || isLocalhostName(name) {
		return nil, false
	}
	if conf == nil {
		conf = getSystemDNSConfig()
	}
	if len(conf.servers) == 0 || ctx.Err() != nil {
		return nil, false
	}
	cache := r.getDNSCache()
	var fqdnBuf [8]string
	fqdns := conf.appendNameList(fqdnBuf[:0], name)
	for _, fqdn := range fqdns {
		lname := lowerASCIIName(fqdn)
		eA, errA, okA := cache.getEntryLower(conf, fqdn, lname, dnsmessage.TypeA)
		eAAAA, errAAAA, okAAAA := cache.getEntryLower(conf, fqdn, lname, dnsmessage.TypeAAAA)
		if !okA || !okAAAA {
			return nil, false
		}
		var cachedBuf [8]cachedAddr
		cached := cachedBuf[:0]
		if errA == nil && eA != nil {
			cached = append(cached, eA.addrs...)
		}
		if errAAAA == nil && eAAAA != nil {
			cached = append(cached, eAAAA.addrs...)
		}
		if len(cached) == 0 {
			continue
		}
		if len(cached) == 1 {
			return []string{cached[0].str}, true
		}
		sortCachedByRFC6724(cached)
		out := make([]string, len(cached))
		for i, ca := range cached {
			out[i] = ca.str
		}
		return out, true
	}
	return nil, false
}

func (r *Resolver) tryCachedLookupIPAddr(ctx context.Context, network, host string) ([]IPAddr, bool) {
	if !dnsCacheEnabled() || ctx.Err() != nil {
		return nil, false
	}
	order, conf := systemConf().hostLookupOrder(r, host)
	if order == hostLookupCgo {
		return nil, false
	}
	if order == hostLookupFilesDNS || order == hostLookupFiles {
		if addrs, _ := goLookupIPFiles(host); len(addrs) > 0 {
			return addrs, true
		}
		if order == hostLookupFiles {
			return nil, false
		}
	}
	if !isDomainName(host) || isLocalhostName(host) || network == "CNAME" {
		return nil, false
	}
	if conf == nil {
		conf = getSystemDNSConfig()
	}
	if len(conf.servers) == 0 {
		return nil, false
	}
	var qtypesBuf [2]dnsmessage.Type
	qtypesBuf[0] = dnsmessage.TypeA
	qtypesBuf[1] = dnsmessage.TypeAAAA
	qtypes := qtypesBuf[:2]
	switch ipVersion(network) {
	case '4':
		qtypes = qtypesBuf[:1]
	case '6':
		qtypes = qtypesBuf[1:2]
	}

	cache := r.getDNSCache()
	var fqdnBuf [8]string
	fqdns := conf.appendNameList(fqdnBuf[:0], host)
	for _, fqdn := range fqdns {
		lname := lowerASCIIName(fqdn)
		var entries [2]*dnsCacheEntry
		addrCount := 0
		totalBytes := 0
		for i, qtype := range qtypes {
			entry, err, ok := cache.getEntryLower(conf, fqdn, lname, qtype)
			if !ok {
				return nil, false
			}
			if err == nil && entry != nil {
				entries[i] = entry
				addrCount += len(entry.addrs)
				for _, ca := range entry.addrs {
					if ca.addr.Is4() {
						totalBytes += 4
					} else {
						totalBytes += 16
					}
				}
			}
		}
		if addrCount == 0 {
			continue
		}
		slab := make([]byte, totalBytes)
		addrs := make([]IPAddr, 0, addrCount)
		slabOff := 0
		for i := range qtypes {
			entry := entries[i]
			if entry == nil {
				continue
			}
			for _, ca := range entry.addrs {
				if ca.addr.Is4() {
					a4 := ca.addr.As4()
					copy(slab[slabOff:slabOff+4], a4[:])
					addrs = append(addrs, IPAddr{IP: slab[slabOff : slabOff+4 : slabOff+4]})
					slabOff += 4
				} else {
					a16 := ca.addr.As16()
					copy(slab[slabOff:slabOff+16], a16[:])
					addrs = append(addrs, IPAddr{IP: slab[slabOff : slabOff+16 : slabOff+16]})
					slabOff += 16
				}
			}
		}
		return addrs, true
	}
	return nil, false
}

func (r *Resolver) goLookupHostOrder(ctx context.Context, name string, order hostLookupOrder, conf *dnsConfig) (addrs []string, err error) {
	if order == hostLookupFilesDNS || order == hostLookupFiles {
		// Use entries from /etc/hosts if they match.
		addrs, _ = lookupStaticHost(name)
		if len(addrs) > 0 {
			return
		}

		// Localhost names not in the hosts file fall through to
		// goLookupIPCNAMEOrder, which resolves them itself, even when
		// the order permits no DNS.
		if order == hostLookupFiles && !isLocalhostName(name) {
			return nil, newDNSError(errNoSuchHost, name, "")
		}
		if order == hostLookupFilesDNS {
			order = hostLookupDNS
		}
	}
	if order != hostLookupFiles {
		if addrs, ok := r.tryCachedLookupHost(ctx, name, conf); ok {
			return addrs, nil
		}
	}
	ips, _, err := r.goLookupIPCNAMEOrder(ctx, "ip", name, order, conf)
	if err != nil {
		return
	}
	addrs = make([]string, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, ip.String())
	}
	return
}

// lookup entries from /etc/hosts
func goLookupIPFiles(name string) (addrs []IPAddr, canonical string) {
	addr, canonical := lookupStaticHost(name)
	for _, haddr := range addr {
		haddr, zone := splitHostZone(haddr)
		if ip := ParseIP(haddr); ip != nil {
			addr := IPAddr{IP: ip, Zone: zone}
			addrs = append(addrs, addr)
		}
	}
	return addrs, canonical
}

// goLookupIP is the native Go implementation of LookupIP.
// The libc versions are in cgo_*.go.
func (r *Resolver) goLookupIP(ctx context.Context, network, host string, order hostLookupOrder, conf *dnsConfig) (addrs []IPAddr, err error) {
	addrs, _, err = r.goLookupIPCNAMEOrder(ctx, network, host, order, conf)
	return
}

func (r *Resolver) goLookupIPCNAMEOrder(ctx context.Context, network, name string, order hostLookupOrder, conf *dnsConfig) (addrs []IPAddr, cname dnsmessage.Name, err error) {
	addrs, cname, err = r.goLookupIPCNAME(ctx, network, name, order, conf)
	sortByRFC6724(addrs)
	return addrs, cname, err
}

func (r *Resolver) goLookupIPCNAME(ctx context.Context, network, name string, order hostLookupOrder, conf *dnsConfig) (addrs []IPAddr, cname dnsmessage.Name, err error) {
	if order == hostLookupFilesDNS || order == hostLookupFiles {
		var canonical string
		addrs, canonical = goLookupIPFiles(name)

		if len(addrs) > 0 {
			var err error
			cname, err = dnsmessage.NewName(canonical)
			if err != nil {
				return nil, dnsmessage.Name{}, err
			}
			return addrs, cname, nil
		}

		if order == hostLookupFiles && !isLocalhostName(name) {
			return nil, dnsmessage.Name{}, newDNSError(errNoSuchHost, name, "")
		}
	}

	if !isDomainName(name) {
		// See comment in func lookup above about use of errNoSuchHost.
		return nil, dnsmessage.Name{}, newDNSError(errNoSuchHost, name, "")
	}

	if isLocalhostName(name) {
		// Per RFC 6761, localhost names mean loopback and must never
		// reach DNS (or be expanded with search domains on the way).
		// The hosts file, checked above, can still override.
		// See go.dev/issue/57757 and go.dev/issue/32017.
		var addrs []IPAddr
		if ipVersion(network) != '4' {
			addrs = append(addrs, IPAddr{IP: IPv6loopback})
		}
		if ipVersion(network) != '6' {
			addrs = append(addrs, IPAddr{IP: IPv4(127, 0, 0, 1)})
		}
		sortByRFC6724(addrs)
		cname, err := dnsmessage.NewName(absDomainName(name))
		if err != nil {
			return nil, dnsmessage.Name{}, err
		}
		return addrs, cname, nil
	}
	type result struct {
		p      dnsmessage.Parser
		server string
		error  error
		entry  *dnsCacheEntry
	}
	type indexedResult struct {
		idx int
		res result
	}

	if conf == nil {
		conf = getSystemDNSConfig()
	}

	var qtypesBuf [3]dnsmessage.Type
	qtypesBuf[0] = dnsmessage.TypeA
	qtypesBuf[1] = dnsmessage.TypeAAAA
	qtypes := qtypesBuf[:2]
	if network == "CNAME" {
		qtypesBuf[2] = dnsmessage.TypeCNAME
		qtypes = qtypesBuf[:3]
	}
	switch ipVersion(network) {
	case '4':
		qtypes = qtypesBuf[:1]
	case '6':
		qtypes = qtypesBuf[1:2]
	}

	cache := r.getDNSCache()
	canUseCache := len(conf.servers) > 0 && ctx.Err() == nil
	var lane chan indexedResult
	var lastErr error
	var fqdnBuf [8]string
	fqdns := conf.appendNameList(fqdnBuf[:0], name)
	var prevAllNotFound [8]bool
	for fqdnIdx, fqdn := range fqdns {
		var results [3]result
		var hit [3]bool
		misses := len(qtypes)
		if canUseCache {
			lname := lowerASCIIName(fqdn)
			for i, qtype := range qtypes {
				if entry, err, ok := cache.getEntryLower(conf, fqdn, lname, qtype); ok {
					results[i] = result{entry.p, entry.server, err, entry}
					hit[i] = true
					misses--
				}
			}
		}
		if misses > 0 {
			if conf.singleRequest || misses == 1 {
				for i, qtype := range qtypes {
					if !hit[i] {
						dnsWaitGroup.Add(1)
						p, server, err := r.tryOneNameQuery(ctx, conf, fqdn, qtype, false)
						dnsWaitGroup.Done()
						results[i] = result{p: p, server: server, error: err}
					}
				}
			} else {
				if lane == nil {
					lane = make(chan indexedResult, len(qtypes))
				}
				for i, qtype := range qtypes {
					if !hit[i] {
						dnsWaitGroup.Add(1)
						go func(idx int, qtype dnsmessage.Type) {
							p, server, err := r.tryOneNameQuery(ctx, conf, fqdn, qtype, false)
							lane <- indexedResult{idx: idx, res: result{p: p, server: server, error: err}}
							dnsWaitGroup.Done()
						}(i, qtype)
					}
				}
				for range misses {
					ir := <-lane
					results[ir.idx] = ir.res
				}
			}
		}

		hitStrictError := false
		fqdnAllNotFound := true
		var hasError [3]bool
		var slab []byte
		slabOff := 0
		for i := range qtypes {
			result := results[i]
			if result.error != nil {
				hasError[i] = true
				if dnsErr, ok := result.error.(*DNSError); !ok || !dnsErr.IsNotFound {
					fqdnAllNotFound = false
				}
				if nerr, ok := result.error.(Error); ok && nerr.Temporary() && r.strictErrors() {
					// This error will abort the nameList loop.
					hitStrictError = true
					lastErr = result.error
				} else if lastErr == nil || fqdn == name+"." {
					// Prefer error for original name.
					lastErr = result.error
				}
				continue
			}
			fqdnAllNotFound = false
			if result.entry != nil {
				if len(result.entry.addrs) > 0 && slab == nil {
					addrCount := 0
					totalBytes := 0
					for j := i; j < len(qtypes); j++ {
						if results[j].error == nil && results[j].entry != nil {
							addrCount += len(results[j].entry.addrs)
							for _, ca := range results[j].entry.addrs {
								if ca.addr.Is4() {
									totalBytes += 4
								} else {
									totalBytes += 16
								}
							}
						}
					}
					if totalBytes > 0 {
						slab = make([]byte, totalBytes)
					}
					if addrs == nil && addrCount > 0 {
						addrs = make([]IPAddr, 0, addrCount)
					}
				}
				for _, ca := range result.entry.addrs {
					if ca.addr.Is4() {
						a4 := ca.addr.As4()
						copy(slab[slabOff:slabOff+4], a4[:])
						addrs = append(addrs, IPAddr{IP: slab[slabOff : slabOff+4 : slabOff+4]})
						slabOff += 4
					} else {
						a16 := ca.addr.As16()
						copy(slab[slabOff:slabOff+16], a16[:])
						addrs = append(addrs, IPAddr{IP: slab[slabOff : slabOff+16 : slabOff+16]})
						slabOff += 16
					}
				}
				if cname.Length == 0 && result.entry.cname.Length != 0 {
					cname = result.entry.cname
				}
				continue
			}

			// Presotto says it's okay to assume that servers listed in
			// /etc/resolv.conf are recursive resolvers.
			//
			// We asked for recursion, so it should have included all the
			// answers we need in this one packet.
			//
			// Further, RFC 1034 section 4.3.1 says that "the recursive
			// response to a query will be... The answer to the query,
			// possibly preface by one or more CNAME RRs that specify
			// aliases encountered on the way to an answer."
			//
			// Therefore, we should be able to assume that we can ignore
			// CNAMEs and that the A and AAAA records we requested are
			// for the canonical name.

		loop:
			for {
				h, err := result.p.AnswerHeader()
				if err != nil && err != dnsmessage.ErrSectionDone {
					lastErr = &DNSError{
						Err:    errCannotUnmarshalDNSMessage.Error(),
						Name:   name,
						Server: result.server,
					}
				}
				if err != nil {
					break
				}
				switch h.Type {
				case dnsmessage.TypeA:
					a, err := result.p.AResource()
					if err != nil {
						lastErr = &DNSError{
							Err:    errCannotUnmarshalDNSMessage.Error(),
							Name:   name,
							Server: result.server,
						}
						break loop
					}
					addrs = append(addrs, IPAddr{IP: IP(a.A[:])})
					if cname.Length == 0 && h.Name.Length != 0 {
						cname = h.Name
					}

				case dnsmessage.TypeAAAA:
					aaaa, err := result.p.AAAAResource()
					if err != nil {
						lastErr = &DNSError{
							Err:    errCannotUnmarshalDNSMessage.Error(),
							Name:   name,
							Server: result.server,
						}
						break loop
					}
					addrs = append(addrs, IPAddr{IP: IP(aaaa.AAAA[:])})
					if cname.Length == 0 && h.Name.Length != 0 {
						cname = h.Name
					}

				case dnsmessage.TypeCNAME:
					c, err := result.p.CNAMEResource()
					if err != nil {
						lastErr = &DNSError{
							Err:    errCannotUnmarshalDNSMessage.Error(),
							Name:   name,
							Server: result.server,
						}
						break loop
					}
					if cname.Length == 0 && c.CNAME.Length > 0 {
						cname = c.CNAME
					}

				default:
					if err := result.p.SkipAnswer(); err != nil {
						lastErr = &DNSError{
							Err:    errCannotUnmarshalDNSMessage.Error(),
							Name:   name,
							Server: result.server,
						}
						break loop
					}
					continue
				}
			}
		}
		if hitStrictError {
			// If either family hit an error with StrictErrors enabled,
			// discard all addresses. This ensures that network flakiness
			// cannot turn a dualstack hostname IPv4/IPv6-only.
			addrs = nil
			break
		}
		if len(addrs) > 0 || network == "CNAME" && cname.Length > 0 {
			if expireAt, srv := cache.positiveExpireAt(conf, fqdn, qtypes, hasError); expireAt > 0 {
				for i, qtype := range qtypes {
					if hasError[i] {
						if dnsErr, ok := results[i].error.(*DNSError); ok && dnsErr.IsNotFound {
							cache.putNegativeExpireAt(conf, fqdn, qtype, srv, expireAt)
						}
					}
				}
				for j := 0; j < fqdnIdx && j < len(prevAllNotFound); j++ {
					if prevAllNotFound[j] {
						for _, qtype := range qtypes {
							cache.putNegativeExpireAt(conf, fqdns[j], qtype, srv, expireAt)
						}
					}
				}
			}
			break
		}
		if fqdnIdx < len(prevAllNotFound) {
			prevAllNotFound[fqdnIdx] = fqdnAllNotFound
		}
	}
	if lastErr, ok := lastErr.(*DNSError); ok {
		// Show original name passed to lookup, not suffixed one.
		// In general we might have tried many suffixes; showing
		// just one is misleading. See also golang.org/issue/6324.
		lastErr.Name = name
	}
	if len(addrs) == 0 && !(network == "CNAME" && cname.Length > 0) {
		if order == hostLookupDNSFiles {
			var canonical string
			addrs, canonical = goLookupIPFiles(name)
			if len(addrs) > 0 {
				var err error
				cname, err = dnsmessage.NewName(canonical)
				if err != nil {
					return nil, dnsmessage.Name{}, err
				}
				return addrs, cname, nil
			}
		}
		if lastErr != nil {
			return nil, dnsmessage.Name{}, lastErr
		}
	}
	return addrs, cname, nil
}

// goLookupCNAME is the native Go (non-cgo) implementation of LookupCNAME.
func (r *Resolver) goLookupCNAME(ctx context.Context, host string, order hostLookupOrder, conf *dnsConfig) (string, error) {
	_, cname, err := r.goLookupIPCNAMEOrder(ctx, "CNAME", host, order, conf)
	return cname.String(), err
}

// goLookupPTR is the native Go implementation of LookupAddr.
func (r *Resolver) goLookupPTR(ctx context.Context, addr string, order hostLookupOrder, conf *dnsConfig) ([]string, error) {
	if order == hostLookupFiles || order == hostLookupFilesDNS {
		names := lookupStaticAddr(addr)
		if len(names) > 0 {
			return names, nil
		}

		if order == hostLookupFiles {
			return nil, newDNSError(errNoSuchHost, addr, "")
		}
	}

	arpa, err := reverseaddr(addr)
	if err != nil {
		return nil, err
	}
	p, server, err := r.lookup(ctx, arpa, dnsmessage.TypePTR, conf)
	if err != nil {
		if dnsErr, ok := errors.AsType[*DNSError](err); ok && dnsErr.IsNotFound {
			if order == hostLookupDNSFiles {
				names := lookupStaticAddr(addr)
				if len(names) > 0 {
					return names, nil
				}
			}
		}
		return nil, err
	}
	var ptrs []string
	for {
		h, err := p.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return nil, &DNSError{
				Err:    errCannotUnmarshalDNSMessage.Error(),
				Name:   addr,
				Server: server,
			}
		}
		if h.Type != dnsmessage.TypePTR {
			err := p.SkipAnswer()
			if err != nil {
				return nil, &DNSError{
					Err:    errCannotUnmarshalDNSMessage.Error(),
					Name:   addr,
					Server: server,
				}
			}
			continue
		}
		ptr, err := p.PTRResource()
		if err != nil {
			return nil, &DNSError{
				Err:    errCannotUnmarshalDNSMessage.Error(),
				Name:   addr,
				Server: server,
			}
		}
		ptrs = append(ptrs, ptr.PTR.String())

	}

	return ptrs, nil
}
