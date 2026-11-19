// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
)

// CrossOriginProtection implements protections against [Cross-Site Request
// Forgery (CSRF)] by rejecting non-safe cross-origin browser requests.
//
// Cross-origin requests are currently detected with the [Sec-Fetch-Site]
// header, available in all browsers since 2023, or by comparing the hostname of
// the [Origin] header with the Host header.
//
// The GET, HEAD, and OPTIONS methods are [safe methods] and are always allowed.
// It's important that applications do not perform any state changing actions
// due to requests with safe methods.
//
// Requests without Sec-Fetch-Site or Origin headers are currently assumed to be
// either same-origin or non-browser requests, and are allowed.
//
// The zero value of CrossOriginProtection is valid and has no trusted origins
// or bypass patterns.
//
// [Sec-Fetch-Site]: https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Sec-Fetch-Site
// [Origin]: https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Origin
// [Cross-Site Request Forgery (CSRF)]: https://developer.mozilla.org/en-US/docs/Web/Security/Attacks/CSRF
// [safe methods]: https://developer.mozilla.org/en-US/docs/Glossary/Safe/HTTP
type CrossOriginProtection struct {
	bypass     atomic.Pointer[ServeMux]
	hasTrusted atomic.Bool
	trustedMu  sync.RWMutex
	trusted    map[string]bool
	deny       atomic.Pointer[Handler]
}

// NewCrossOriginProtection returns a new [CrossOriginProtection] value.
func NewCrossOriginProtection() *CrossOriginProtection {
	return &CrossOriginProtection{}
}

// AddTrustedOrigin allows all requests with an [Origin] header
// which exactly matches the given value.
//
// Origin header values are of the form "scheme://host[:port]".
//
// AddTrustedOrigin can be called concurrently with other methods
// or request handling, and applies to future requests.
//
// [Origin]: https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Origin
func (c *CrossOriginProtection) AddTrustedOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("invalid origin %q: %w", origin, err)
	}
	if u.Scheme == "" {
		return fmt.Errorf("invalid origin %q: scheme is required", origin)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid origin %q: host is required", origin)
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid origin %q: userinfo, path, query, and fragment are not allowed", origin)
	}
	c.trustedMu.Lock()
	defer c.trustedMu.Unlock()
	if c.trusted == nil {
		c.trusted = make(map[string]bool)
	}
	c.trusted[origin] = true
	c.hasTrusted.Store(true)
	return nil
}

type noopHandler struct{}

func (noopHandler) ServeHTTP(ResponseWriter, *Request) {}

var sentinelHandler Handler = &noopHandler{}

// AddInsecureBypassPattern permits all requests that match the given pattern.
//
// The pattern syntax and precedence rules are the same as [ServeMux]. Only
// requests that match the pattern directly are permitted. Those that ServeMux
// would redirect to a pattern (e.g. after cleaning the path or adding a
// trailing slash) are not.
//
// AddInsecureBypassPattern panics if the pattern conflicts with one already
// registered, or if the pattern is syntactically invalid (for example, an
// improperly formed wildcard).
//
// AddInsecureBypassPattern can be called concurrently with other methods or
// request handling, and applies to future requests.
func (c *CrossOriginProtection) AddInsecureBypassPattern(pattern string) {
	var bypass *ServeMux

	// Lazily initialize c.bypass
	for {
		bypass = c.bypass.Load()
		if bypass != nil {
			break
		}
		bypass = NewServeMux()
		if c.bypass.CompareAndSwap(nil, bypass) {
			break
		}
	}

	bypass.Handle(pattern, sentinelHandler)
}

// SetDenyHandler sets a handler to invoke when a request is rejected.
// The default error handler responds with a 403 Forbidden status.
//
// SetDenyHandler can be called concurrently with other methods
// or request handling, and applies to future requests.
//
// Check does not call the error handler.
func (c *CrossOriginProtection) SetDenyHandler(h Handler) {
	if h == nil {
		c.deny.Store(nil)
		return
	}
	c.deny.Store(&h)
}

// isSimpleOriginHostPort reports whether s is a non-empty ASCII hostname or
// IPv4 address with an optional numeric port, containing no characters that
// url.Parse could interpret as userinfo, IPv6 brackets, path, query, fragment,
// or percent-encoding. For any s where this returns true, url.Parse("https://"+s)
// is guaranteed to succeed with u.Host == s.
func isSimpleOriginHostPort(s string) bool {
	if len(s) == 0 {
		return false
	}
	i := 0
	for ; i < len(s); i++ {
		b := s[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '.' || b == '-' {
			continue
		}
		if b == ':' {
			i++
			for ; i < len(s); i++ {
				if s[i] < '0' || s[i] > '9' {
					return false
				}
			}
			return true
		}
		return false
	}
	return true
}

func originMatchesHost(origin, host string) bool {
	var rest string
	if len(origin) > len("https://") && origin[:len("https://")] == "https://" {
		rest = origin[len("https://"):]
	} else if len(origin) > len("http://") && origin[:len("http://")] == "http://" {
		rest = origin[len("http://"):]
	}
	if rest != "" && isSimpleOriginHostPort(rest) {
		return rest == host
	}
	o, err := url.Parse(origin)
	return err == nil && o.Host == host
}

// Check applies cross-origin checks to a request.
// It returns an error if the request should be rejected.
func (c *CrossOriginProtection) Check(req *Request) error {
	switch req.Method {
	case "GET", "HEAD", "OPTIONS":
		// Safe methods are always allowed.
		return nil
	}

	switch req.Header.get("Sec-Fetch-Site") {
	case "":
		// No Sec-Fetch-Site header is present.
		// Fallthrough to check the Origin header.
	case "same-origin", "none":
		return nil
	default:
		if c.isRequestExempt(req, "", false) {
			return nil
		}
		return errCrossOriginRequest
	}

	origin := req.Header.get("Origin")
	if origin == "" {
		// Neither Sec-Fetch-Site nor Origin headers are present.
		// Either the request is same-origin or not a browser request.
		return nil
	}

	if originMatchesHost(origin, req.Host) {
		// The Origin header matches the Host header. Note that the Host header
		// doesn't include the scheme, so we don't know if this might be an
		// HTTP→HTTPS cross-origin request. We fail open, since all modern
		// browsers support Sec-Fetch-Site since 2023, and running an older
		// browser makes a clear security trade-off already. Sites can mitigate
		// this with HTTP Strict Transport Security (HSTS).
		return nil
	}

	if c.isRequestExempt(req, origin, true) {
		return nil
	}
	return errCrossOriginRequestFromOldBrowser
}

var (
	errCrossOriginRequest               = errors.New("cross-origin request detected from Sec-Fetch-Site header")
	errCrossOriginRequestFromOldBrowser = errors.New("cross-origin request detected, and/or browser is out of date: " +
		"Sec-Fetch-Site is missing, and Origin does not match Host")
)

// isRequestExempt checks the bypasses which require taking a lock, and should
// be deferred until the last moment.
func (c *CrossOriginProtection) isRequestExempt(req *Request, origin string, haveOrigin bool) bool {
	if bypass := c.bypass.Load(); bypass != nil {
		if h, _ := bypass.Handler(req); h == sentinelHandler {
			// The request matches a bypass pattern.
			return true
		}
	}

	if !c.hasTrusted.Load() {
		return false
	}
	if !haveOrigin {
		origin = req.Header.get("Origin")
	}
	if origin == "" {
		return false
	}
	c.trustedMu.RLock()
	ok := c.trusted[origin]
	c.trustedMu.RUnlock()
	// The request matches a trusted origin.
	return ok
}

// Handler returns a handler that applies cross-origin checks
// before invoking the handler h.
//
// If a request fails cross-origin checks, the request is rejected
// with a 403 Forbidden status or handled with the handler passed
// to [CrossOriginProtection.SetDenyHandler].
func (c *CrossOriginProtection) Handler(h Handler) Handler {
	return HandlerFunc(func(w ResponseWriter, r *Request) {
		if err := c.Check(r); err != nil {
			if deny := c.deny.Load(); deny != nil {
				(*deny).ServeHTTP(w, r)
				return
			}
			Error(w, err.Error(), StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}
