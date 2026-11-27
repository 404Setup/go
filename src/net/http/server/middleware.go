// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RequestIDKey is the [Context.Get] key used by [RequestID] middleware.
const RequestIDKey = "server.request_id"

// RecoverConfig configures [RecoverWithConfig].
type RecoverConfig struct {
	// Logger receives structured panic logs when non-nil.
	Logger *slog.Logger

	// OnPanic is an optional hook invoked when a panic is recovered.
	OnPanic func(c *Context, recovered any, stack []byte)
}

// Recover returns middleware that recovers panics in downstream handlers,
// preserving [http.ErrAbortHandler] semantics and returning a generic 500 error
// without leaking panic details to clients.
func Recover() HandlerFunc {
	return RecoverWithConfig(RecoverConfig{})
}

// RecoverWithConfig returns panic recovery middleware configured with cfg.
func RecoverWithConfig(cfg RecoverConfig) HandlerFunc {
	return func(c *Context) (retErr error) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			// Preserve net/http's sentinel panic for aborting connections.
			if r == http.ErrAbortHandler {
				panic(r)
			}
			stack := debug.Stack()
			if cfg.OnPanic != nil {
				cfg.OnPanic(c, r, stack)
			} else {
				logger := cfg.Logger
				if logger == nil && c.server != nil {
					logger = c.server.logger
				}
				if logger != nil {
					logger.ErrorContext(c.Context(), "http handler panic recovered",
						"method", c.Method(),
						"path", c.Path(),
						"panic", fmt.Sprint(r),
						"stack", string(stack),
					)
				}
			}
			retErr = NewError(http.StatusInternalServerError, "internal server error")
		}()
		return c.Next()
	}
}

// LoggerConfig configures [LoggerWithConfig].
type LoggerConfig struct {
	// Logger is the structured logger used to emit request logs.
	// Default is [slog.Default].
	Logger *slog.Logger

	// Skip optionally skips logging for matching requests (for example,
	// health-check endpoints).
	Skip func(*Context) bool
}

// Logger returns structured request logging middleware backed by [log/slog].
// If logger is nil, [slog.Default] is used.
//
// If a downstream handler returns a non-nil error and the response has not yet
// been written, Logger invokes [Context.HandleError] before logging so the
// recorded status code and byte count match what was sent to the client.
func Logger(logger *slog.Logger) HandlerFunc {
	return LoggerWithConfig(LoggerConfig{Logger: logger})
}

// LoggerWithConfig returns structured request logging middleware configured
// with cfg.
func LoggerWithConfig(cfg LoggerConfig) HandlerFunc {
	configuredLogger := cfg.Logger
	return func(c *Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}
		logger := configuredLogger
		if logger == nil {
			if c.server != nil && c.server.logger != nil {
				logger = c.server.logger
			} else {
				logger = slog.Default()
			}
		}
		start := time.Now()
		err := c.Next()
		if err != nil && !c.errHandled {
			c.HandleError(err)
		}
		status := c.StatusCode()
		if status == 0 {
			status = http.StatusOK
		}
		var attrBuf [9]slog.Attr
		attrs := attrBuf[:0]
		attrs = append(attrs,
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.Int("status", status),
			slog.Int64("bytes", c.BytesWritten()),
			slog.Duration("duration", time.Since(start)),
			slog.String("client_ip", c.ClientIP()),
		)
		if pat := c.RoutePattern(); pat != "" {
			attrs = append(attrs, slog.String("route", pat))
		}
		if id, ok := GetValue[string](c, RequestIDKey); ok && id != "" {
			attrs = append(attrs, slog.String("request_id", id))
		}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		}
		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		} else if status >= 400 {
			level = slog.LevelWarn
		}
		logger.LogAttrs(c.Context(), level, "http request", attrs...)
		return err
	}
}

// RequestIDConfig configures [RequestIDWithConfig].
type RequestIDConfig struct {
	// Header is the HTTP header name used to read/write the request ID.
	// Default is "X-Request-ID".
	Header string

	// TrustIncoming allows reusing a validated incoming request ID header from
	// the client/proxy. Default is false (always generate a fresh ID).
	TrustIncoming bool

	// Generator optionally generates custom request IDs. Default generates a
	// 128-bit cryptographically random hex string.
	Generator func() string
}

// RequestID returns middleware that attaches a fresh unique request ID to
// every request via [RequestIDKey] and the X-Request-ID response header.
func RequestID() HandlerFunc {
	return RequestIDWithConfig(RequestIDConfig{})
}

func hasHeaderControl(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == 0 || b == '\r' || b == '\n' || (b < 0x20 && b != '\t') || b == 0x7f {
			return true
		}
	}
	return false
}

// RequestIDWithConfig returns request ID middleware configured with cfg.
// It panics if cfg.Header is non-empty and not a valid HTTP header field name.
func RequestIDWithConfig(cfg RequestIDConfig) HandlerFunc {
	header := cfg.Header
	if header == "" {
		header = "X-Request-Id"
	} else {
		if !isValidHTTPMethod(header) {
			panic(fmt.Sprintf("server: invalid RequestID header name %q", header))
		}
		header = http.CanonicalHeaderKey(header)
	}
	gen := cfg.Generator
	if gen == nil {
		gen = generateRequestID
	}
	return func(c *Context) error {
		var id string
		if cfg.TrustIncoming {
			if incoming := c.canonicalReqHeader(header); isValidRequestID(incoming) {
				id = incoming
			}
		}
		if id == "" {
			id = gen()
		}
		c.Set(RequestIDKey, id)
		c.respHeader()[header] = []string{id}
		return c.Next()
	}
}

func generateRequestID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	var dst [32]byte
	hex.Encode(dst[:], raw[:])
	return string(dst[:])
}

func isValidRequestID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		// Allow printable ASCII without whitespace or control chars.
		if b < 0x21 || b > 0x7e {
			return false
		}
	}
	return true
}

var (
	headerValVaryOrigin = []string{"Origin"}[:1:1]
	headerValWildcard   = []string{"*"}[:1:1]
	headerValTrue       = []string{"true"}[:1:1]
	headerValRetryOne   = []string{"1"}[:1:1]
)

// BodyLimit returns middleware that enforces a maximum request body size of
// maxBytes, rejecting larger requests with 413 Request Entity Too Large.
func BodyLimit(maxBytes int64) HandlerFunc {
	if maxBytes <= 0 {
		panic("server: BodyLimit maxBytes must be positive")
	}
	return func(c *Context) error {
		c.SetMaxBodyBytes(maxBytes)
		if c.req.ContentLength > maxBytes {
			c.markRequestTooLarge()
			return WrapError(http.StatusRequestEntityTooLarge, "request body too large", ErrBodyTooLarge)
		}
		c.wrapBodyLimit(maxBytes)
		return c.Next()
	}
}

// Timeout returns middleware that attaches a deadline of duration d to the
// request's [context.Context].
func Timeout(d time.Duration) HandlerFunc {
	if d <= 0 {
		panic("server: Timeout duration must be positive")
	}
	return func(c *Context) error {
		prevReq := c.req
		ctx, cancel := context.WithTimeout(prevReq.Context(), d)
		defer func() {
			cancel()
			c.req = prevReq
		}()
		c.SetContext(ctx)
		return c.Next()
	}
}

// MaxInFlight returns middleware that limits the number of concurrent in-flight
// requests to limit. Excess requests are rejected immediately with 503 Service
// Unavailable. It panics if limit <= 0.
func MaxInFlight(limit int) HandlerFunc {
	if limit <= 0 {
		panic("server: MaxInFlight limit must be positive")
	}
	var active atomic.Int64
	max := int64(limit)
	return func(c *Context) error {
		for {
			cur := active.Load()
			if cur >= max {
				return NewError(http.StatusServiceUnavailable, "service unavailable")
			}
			if active.CompareAndSwap(cur, cur+1) {
				break
			}
		}
		defer active.Add(-1)
		return c.Next()
	}
}

// SecurityHeadersConfig configures [SecurityHeadersWithConfig].
type SecurityHeadersConfig struct {
	// ContentTypeOptions sets X-Content-Type-Options. Default is "nosniff".
	ContentTypeOptions string

	// FrameOptions sets X-Frame-Options. Default is "DENY".
	FrameOptions string

	// ReferrerPolicy sets Referrer-Policy. Default is "strict-origin-when-cross-origin".
	ReferrerPolicy string

	// ContentSecurityPolicy optionally sets Content-Security-Policy.
	ContentSecurityPolicy string

	// StrictTransportSecurity optionally sets Strict-Transport-Security.
	// Only sent when the request arrived over HTTPS (r.TLS != nil or via a
	// configured trusted proxy with X-Forwarded-Proto: https).
	StrictTransportSecurity string

	// PermissionsPolicy optionally sets Permissions-Policy.
	PermissionsPolicy string

	// CrossOriginOpenerPolicy optionally sets Cross-Origin-Opener-Policy.
	CrossOriginOpenerPolicy string
}

// SecurityHeaders returns middleware that sets standard defensive HTTP
// response headers (X-Content-Type-Options, X-Frame-Options, Referrer-Policy).
func SecurityHeaders() HandlerFunc {
	return SecurityHeadersWithConfig(SecurityHeadersConfig{})
}

// SecurityHeadersWithConfig returns security header middleware configured with cfg.
// It panics if any configured header value contains control characters.
func SecurityHeadersWithConfig(cfg SecurityHeadersConfig) HandlerFunc {
	if cfg.ContentTypeOptions == "" {
		cfg.ContentTypeOptions = "nosniff"
	}
	if cfg.FrameOptions == "" {
		cfg.FrameOptions = "DENY"
	}
	if cfg.ReferrerPolicy == "" {
		cfg.ReferrerPolicy = "strict-origin-when-cross-origin"
	}
	for _, v := range []string{
		cfg.ContentTypeOptions,
		cfg.FrameOptions,
		cfg.ReferrerPolicy,
		cfg.ContentSecurityPolicy,
		cfg.StrictTransportSecurity,
		cfg.PermissionsPolicy,
		cfg.CrossOriginOpenerPolicy,
	} {
		if hasHeaderControl(v) {
			panic("server: SecurityHeaders header value contains control character")
		}
	}
	ctoSlice := []string{cfg.ContentTypeOptions}[:1:1]
	foSlice := []string{cfg.FrameOptions}[:1:1]
	rpSlice := []string{cfg.ReferrerPolicy}[:1:1]
	var cspSlice, ppSlice, coopSlice, hstsSlice []string
	if cfg.ContentSecurityPolicy != "" {
		cspSlice = []string{cfg.ContentSecurityPolicy}[:1:1]
	}
	if cfg.PermissionsPolicy != "" {
		ppSlice = []string{cfg.PermissionsPolicy}[:1:1]
	}
	if cfg.CrossOriginOpenerPolicy != "" {
		coopSlice = []string{cfg.CrossOriginOpenerPolicy}[:1:1]
	}
	if cfg.StrictTransportSecurity != "" {
		hstsSlice = []string{cfg.StrictTransportSecurity}[:1:1]
	}
	return func(c *Context) error {
		h := c.respHeader()
		h["X-Content-Type-Options"] = ctoSlice
		h["X-Frame-Options"] = foSlice
		h["Referrer-Policy"] = rpSlice
		if cspSlice != nil {
			h["Content-Security-Policy"] = cspSlice
		}
		if ppSlice != nil {
			h["Permissions-Policy"] = ppSlice
		}
		if coopSlice != nil {
			h["Cross-Origin-Opener-Policy"] = coopSlice
		}
		if hstsSlice != nil && c.Scheme() == "https" {
			h["Strict-Transport-Security"] = hstsSlice
		}
		return c.Next()
	}
}

// CORSConfig configures [CORS] middleware.
type CORSConfig struct {
	// AllowOrigins is a list of allowed origins (for example,
	// "https://example.com"). Use "*" to allow any origin when
	// AllowCredentials is false.
	AllowOrigins []string

	// AllowOriginFunc optionally validates an origin dynamically.
	// When set, AllowOrigins is ignored.
	AllowOriginFunc func(origin string) bool

	// AllowMethods lists allowed HTTP methods for preflight responses.
	// Default is GET, HEAD, POST, PUT, PATCH, DELETE.
	AllowMethods []string

	// AllowHeaders lists allowed request headers for preflight responses.
	AllowHeaders []string

	// ExposeHeaders lists response headers exposed to browsers.
	ExposeHeaders []string

	// AllowCredentials indicates whether credentials (cookies, authorization
	// headers) are allowed. Cannot be combined with wildcard "*" in AllowOrigins.
	AllowCredentials bool

	// MaxAge sets Access-Control-Max-Age for preflight responses.
	MaxAge time.Duration
}

// CORS returns Cross-Origin Resource Sharing middleware.
//
// Install CORS via [Server.Use] so preflight OPTIONS requests are handled even
// when no explicit OPTIONS route is registered.
//
// It panics if AllowCredentials is true and AllowOrigins contains "*",
// preventing a common CORS security misconfiguration, or if any configured
// header or origin contains control characters.
func CORS(cfg CORSConfig) HandlerFunc {
	allowAll := slices.Contains(cfg.AllowOrigins, "*")
	if cfg.AllowCredentials && allowAll {
		panic("server: CORS AllowCredentials cannot be combined with wildcard '*' origin")
	}
	for _, list := range [][]string{cfg.AllowOrigins, cfg.AllowMethods, cfg.AllowHeaders, cfg.ExposeHeaders} {
		for _, v := range list {
			if hasHeaderControl(v) {
				panic("server: CORS configuration contains control character")
			}
		}
	}

	var allowedSet map[string][]string
	if cfg.AllowOriginFunc == nil && !allowAll && len(cfg.AllowOrigins) > 0 {
		allowedSet = make(map[string][]string, len(cfg.AllowOrigins))
		for _, o := range cfg.AllowOrigins {
			allowedSet[o] = []string{o}[:1:1]
		}
	}

	methods := cfg.AllowMethods
	if len(methods) == 0 {
		methods = []string{
			http.MethodGet,
			http.MethodHead,
			http.MethodPost,
			http.MethodPut,
			http.MethodPatch,
			http.MethodDelete,
		}
	}
	allowMethodsSlice := []string{strings.Join(methods, ", ")}[:1:1]
	allowWildcardHeaders := slices.Contains(cfg.AllowHeaders, "*")
	var allowHeadersSlice []string
	if len(cfg.AllowHeaders) > 0 {
		allowHeadersSlice = []string{strings.Join(cfg.AllowHeaders, ", ")}[:1:1]
	}
	var exposeHeadersSlice []string
	if len(cfg.ExposeHeaders) > 0 {
		exposeHeadersSlice = []string{strings.Join(cfg.ExposeHeaders, ", ")}[:1:1]
	}
	var maxAgeSlice []string
	if cfg.MaxAge > 0 {
		maxAgeSlice = []string{strconv.Itoa(int(cfg.MaxAge / time.Second))}[:1:1]
	}

	return func(c *Context) error {
		h := c.respHeader()
		addVaryToken(h, "Origin")

		originHeaders := c.req.Header["Origin"]
		if len(originHeaders) != 1 {
			return c.Next()
		}
		origin := originHeaders[0]
		if origin == "" || hasHeaderControl(origin) {
			return c.Next()
		}

		isPreflight := c.Method() == http.MethodOptions && c.canonicalReqHeader("Access-Control-Request-Method") != ""
		if isPreflight {
			addVaryToken(h, "Access-Control-Request-Method")
			addVaryToken(h, "Access-Control-Request-Headers")
		}

		var originSlice []string
		if cfg.AllowOriginFunc != nil {
			if (cfg.AllowCredentials && origin == "*") || !cfg.AllowOriginFunc(origin) {
				if isPreflight {
					return c.WriteStatus(http.StatusForbidden)
				}
				return c.Next()
			}
			originSlice = []string{origin}
		} else if allowAll {
			originSlice = headerValWildcard
		} else {
			var ok bool
			originSlice, ok = allowedSet[origin]
			if !ok {
				if isPreflight {
					return c.WriteStatus(http.StatusForbidden)
				}
				return c.Next()
			}
		}

		h["Access-Control-Allow-Origin"] = originSlice
		if cfg.AllowCredentials {
			h["Access-Control-Allow-Credentials"] = headerValTrue
		}
		if exposeHeadersSlice != nil && !isPreflight {
			h["Access-Control-Expose-Headers"] = exposeHeadersSlice
		}

		if isPreflight {
			h["Access-Control-Allow-Methods"] = allowMethodsSlice
			if allowWildcardHeaders && cfg.AllowCredentials {
				if reqHdrs := c.req.Header["Access-Control-Request-Headers"]; len(reqHdrs) > 0 {
					joined := reqHdrs[0]
					if len(reqHdrs) > 1 {
						joined = strings.Join(reqHdrs, ", ")
					}
					if joined != "" && !hasHeaderControl(joined) {
						h["Access-Control-Allow-Headers"] = []string{joined}
					}
				}
			} else if allowHeadersSlice != nil {
				h["Access-Control-Allow-Headers"] = allowHeadersSlice
			}
			if maxAgeSlice != nil {
				h["Access-Control-Max-Age"] = maxAgeSlice
			}
			return c.NoContent()
		}

		return c.Next()
	}
}

// CrossOriginProtection returns middleware that enforces CSRF protection using
// [http.CrossOriginProtection]. If p is nil, a new default instance is created.
func CrossOriginProtection(p *http.CrossOriginProtection) HandlerFunc {
	if p == nil {
		p = http.NewCrossOriginProtection()
	}
	return func(c *Context) error {
		if err := p.Check(c.req); err != nil {
			return WrapError(http.StatusForbidden, "cross-origin request forbidden", err)
		}
		return c.Next()
	}
}

func sanitizeAuthRealm(realm string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, realm)
}

// BasicAuthConfig configures [BasicAuthWithConfig].
type BasicAuthConfig struct {
	// Realm is the HTTP Basic authentication realm. Default is "Restricted".
	Realm string

	// Validator validates the username and password for the request.
	// Validators comparing static secrets should use [ConstantTimeEqual].
	// Required (must be non-nil).
	Validator func(c *Context, username, password string) bool

	// Skip optionally bypasses authentication for matching requests.
	Skip func(*Context) bool
}

// BasicAuth returns HTTP Basic Authentication middleware.
// The validator function should use [ConstantTimeEqual] when comparing secrets.
func BasicAuth(realm string, validator func(username, password string) bool) HandlerFunc {
	if validator == nil {
		panic("server: nil BasicAuth validator")
	}
	return BasicAuthWithConfig(BasicAuthConfig{
		Realm: realm,
		Validator: func(_ *Context, username, password string) bool {
			return validator(username, password)
		},
	})
}

// BasicAuthWithConfig returns HTTP Basic Authentication middleware configured
// with cfg. It panics if cfg.Validator is nil.
func BasicAuthWithConfig(cfg BasicAuthConfig) HandlerFunc {
	if cfg.Validator == nil {
		panic("server: nil BasicAuth validator")
	}
	realm := cfg.Realm
	if realm == "" {
		realm = "Restricted"
	}
	safeRealm := sanitizeAuthRealm(realm)
	wwwAuthSlice := []string{`Basic realm="` + safeRealm + `", charset="UTF-8"`}[:1:1]

	return func(c *Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}
		user, pass, ok := c.BasicAuth()
		if !ok || !cfg.Validator(c, user, pass) {
			c.respHeader()["Www-Authenticate"] = wwwAuthSlice
			return NewError(http.StatusUnauthorized, "unauthorized")
		}
		return c.Next()
	}
}

// BearerAuthConfig configures [BearerAuthWithConfig].
type BearerAuthConfig struct {
	// Realm optionally sets the realm parameter in the WWW-Authenticate
	// response header on 401 responses.
	Realm string

	// Validator validates the extracted bearer token for the request.
	// Validators comparing static tokens should use [ConstantTimeEqual].
	// Required (must be non-nil).
	Validator func(c *Context, token string) bool

	// Skip optionally bypasses authentication for matching requests.
	Skip func(*Context) bool
}

// BearerAuth returns HTTP Bearer Token Authentication middleware (RFC 6750).
// It extracts the token using [Context.BearerToken] and passes it to validator.
// When comparing static tokens, validator should use [ConstantTimeEqual].
func BearerAuth(realm string, validator func(c *Context, token string) bool) HandlerFunc {
	return BearerAuthWithConfig(BearerAuthConfig{
		Realm:     realm,
		Validator: validator,
	})
}

// BearerAuthWithConfig returns HTTP Bearer Token Authentication middleware
// configured with cfg. It panics if cfg.Validator is nil.
func BearerAuthWithConfig(cfg BearerAuthConfig) HandlerFunc {
	if cfg.Validator == nil {
		panic("server: nil BearerAuth validator")
	}
	var wwwAuthSlice []string
	if cfg.Realm != "" {
		wwwAuthSlice = []string{`Bearer realm="` + sanitizeAuthRealm(cfg.Realm) + `", charset="UTF-8"`}[:1:1]
	} else {
		wwwAuthSlice = []string{"Bearer"}[:1:1]
	}

	return func(c *Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}
		tok, ok := c.BearerToken()
		if !ok || !cfg.Validator(c, tok) {
			c.respHeader()["Www-Authenticate"] = wwwAuthSlice
			return NewError(http.StatusUnauthorized, "unauthorized")
		}
		return c.Next()
	}
}

// ConstantTimeEqual reports whether strings a and b have identical contents
// without leaking timing information about matching prefixes.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// AdaptHandler converts a standard [http.Handler] into a [HandlerFunc].
// Matched path parameters are copied onto Request via [http.Request.SetPathValue].
// It panics if h is nil.
func AdaptHandler(h http.Handler) HandlerFunc {
	if h == nil {
		panic("server: nil http.Handler")
	}
	return func(c *Context) error {
		if c.entry != nil {
			for i, name := range c.entry.paramNames {
				var val string
				if i < c.nParams {
					if i < maxInlineParams {
						val = c.paramBuf[i]
					} else {
						val = c.paramExtra[i-maxInlineParams]
					}
				}
				c.req.SetPathValue(name, val)
			}
		}
		h.ServeHTTP(c.ResponseWriter(), c.req)
		return nil
	}
}

// Adapt converts a standard net/http middleware of the form
// func(http.Handler) http.Handler into a [HandlerFunc].
func Adapt(mw func(http.Handler) http.Handler) HandlerFunc {
	if mw == nil {
		panic("server: nil middleware")
	}
	return func(c *Context) error {
		var nextErr error
		called := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			prevReq := c.req
			prevWriter := c.writer
			c.req = r
			c.writer = w
			defer func() {
				c.req = prevReq
				c.writer = prevWriter
			}()
			nextErr = c.Next()
			if nextErr != nil && !c.errHandled {
				c.HandleError(nextErr)
			}
		})
		mw(next).ServeHTTP(c.ResponseWriter(), c.req)
		if !called {
			return nil
		}
		return nextErr
	}
}

type allowedHostRule struct {
	host     string // exact host or ".suffix" for wildcard subdomains
	port     string // empty means any port is accepted
	wildcard bool
}

type ipv6HostRule struct {
	ip   netip.Addr
	port string
}

// AllowHosts returns middleware that validates the incoming request Host
// against hosts to protect against Host header injection and DNS rebinding.
//
// Each entry may be an exact host ("example.com", "localhost:8080", "[::1]")
// or a wildcard subdomain ("*.example.com", "*.example.com:8443"). Entries
// without an explicit port match any port; entries with a port match only that
// port. Wildcard patterns require at least one subdomain label and do not match
// the apex domain.
//
// Requests with missing, duplicated, or malformed Host headers receive 400 Bad
// Request; valid hosts not in the allowlist receive 403 Forbidden. It panics if
// hosts is empty or any entry is invalid.
func AllowHosts(hosts ...string) HandlerFunc {
	if len(hosts) == 0 {
		panic("server: AllowHosts requires at least one host")
	}
	rules := make([]allowedHostRule, 0, len(hosts))
	var ipv6Rules []ipv6HostRule
	for _, raw := range hosts {
		if raw == "" || raw == "*" {
			panic(fmt.Sprintf("server: invalid AllowHosts entry %q", raw))
		}
		norm := raw
		if strings.IndexByte(norm, '[') < 0 && strings.Count(norm, ":") >= 2 {
			if addr, err := netip.ParseAddr(norm); err == nil && addr.Is6() && addr.Zone() == "" {
				norm = "[" + addr.String() + "]"
			}
		}
		h, p, ok := parseHostAuthority(norm)
		if !ok {
			panic(fmt.Sprintf("server: invalid AllowHosts entry %q", raw))
		}
		if h[0] == '[' {
			addr, err := netip.ParseAddr(h[1 : len(h)-1])
			if err != nil || !addr.Is6() || addr.Zone() != "" {
				panic(fmt.Sprintf("server: invalid AllowHosts entry %q", raw))
			}
			ipv6Rules = append(ipv6Rules, ipv6HostRule{
				ip:   addr,
				port: p,
			})
			continue
		}
		if strings.HasPrefix(h, "*.") {
			suffix := h[1:] // ".example.com"
			if len(suffix) <= 1 || strings.Contains(suffix, "*") || hasEmptyDotLabel(suffix[1:]) {
				panic(fmt.Sprintf("server: invalid AllowHosts wildcard %q", raw))
			}
			rules = append(rules, allowedHostRule{
				host:     suffix,
				port:     p,
				wildcard: true,
			})
			continue
		}
		if strings.Contains(h, "*") || hasEmptyDotLabel(h) {
			panic(fmt.Sprintf("server: invalid AllowHosts entry %q", raw))
		}
		rules = append(rules, allowedHostRule{
			host: h,
			port: p,
		})
	}

	return func(c *Context) error {
		if len(c.req.Header["Host"]) > 1 {
			return NewError(http.StatusBadRequest, "invalid host")
		}
		rawHost := c.req.Host
		reqHost, reqPort, ok := parseHostAuthority(rawHost)
		if !ok {
			return NewError(http.StatusBadRequest, "invalid host")
		}
		if reqHost[0] == '[' {
			if matchIPv6Host(reqHost, reqPort, ipv6Rules) {
				return c.Next()
			}
			return NewError(http.StatusForbidden, "host not allowed")
		}
		if hasEmptyDotLabel(reqHost) {
			return NewError(http.StatusBadRequest, "invalid host")
		}
		for i := range rules {
			r := &rules[i]
			if r.port != "" && r.port != reqPort {
				continue
			}
			if r.wildcard {
				if matchWildcardHost(reqHost, r.host) {
					return c.Next()
				}
			} else if asciiEqualFold(reqHost, r.host) {
				return c.Next()
			}
		}
		return NewError(http.StatusForbidden, "host not allowed")
	}
}

//go:noinline
func matchIPv6Host(reqHost, reqPort string, ipv6Rules []ipv6HostRule) bool {
	if len(ipv6Rules) == 0 {
		return false
	}
	reqIP, err := netip.ParseAddr(reqHost[1 : len(reqHost)-1])
	if err != nil || !reqIP.Is6() || reqIP.Zone() != "" {
		return false
	}
	for i := range ipv6Rules {
		r := &ipv6Rules[i]
		if r.port != "" && r.port != reqPort {
			continue
		}
		if reqIP == r.ip {
			return true
		}
	}
	return false
}

//go:noinline
func parseBracketHostAuthority(s string) (host, port string, ok bool) {
	closeIdx := strings.IndexByte(s, ']')
	if closeIdx <= 1 || strings.IndexByte(s[1:closeIdx], '[') >= 0 {
		return "", "", false
	}
	addr, err := netip.ParseAddr(s[1:closeIdx])
	if err != nil || !addr.Is6() || addr.Zone() != "" {
		return "", "", false
	}
	host = s[:closeIdx+1]
	rem := s[closeIdx+1:]
	if rem == "" {
		return host, "", true
	}
	if rem[0] != ':' || len(rem) == 1 {
		return "", "", false
	}
	port = rem[1:]
	if !isASCIIPort(port) {
		return "", "", false
	}
	return host, port, true
}

func parseHostAuthority(s string) (host, port string, ok bool) {
	if s == "" {
		return "", "", false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b <= 0x20 || b >= 0x7f {
			return "", "", false
		}
		switch b {
		case '/', '\\', '@', '?', '#', ',':
			return "", "", false
		}
	}
	if s[0] == '[' {
		return parseBracketHostAuthority(s)
	}
	if strings.IndexByte(s, '[') >= 0 || strings.IndexByte(s, ']') >= 0 {
		return "", "", false
	}
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return s, "", true
	}
	if colon == 0 {
		return "", "", false
	}
	port = s[colon+1:]
	if strings.IndexByte(port, ':') >= 0 || !isASCIIPort(port) {
		return "", "", false
	}
	return s[:colon], port, true
}

func isASCIIPort(p string) bool {
	if len(p) == 0 || len(p) > 5 {
		return false
	}
	n := 0
	for i := 0; i < len(p); i++ {
		b := p[i]
		if b < '0' || b > '9' {
			return false
		}
		n = n*10 + int(b-'0')
	}
	return n >= 1 && n <= 65535
}

func hasEmptyDotLabel(s string) bool {
	if s == "" {
		return true
	}
	if s[0] == '.' || s[len(s)-1] == '.' {
		return true
	}
	for i := 1; i < len(s); i++ {
		if s[i] == '.' && s[i-1] == '.' {
			return true
		}
	}
	return false
}

func matchWildcardHost(reqHost, suffix string) bool {
	if len(reqHost) <= len(suffix) {
		return false
	}
	split := len(reqHost) - len(suffix)
	if !asciiEqualFold(reqHost[split:], suffix) {
		return false
	}
	return !hasEmptyDotLabel(reqHost[:split])
}

// RateLimitConfig configures [RateLimitWithConfig] token-bucket rate limiting.
type RateLimitConfig struct {
	// Rate is the sustained number of requests allowed per second per key.
	// Must be positive and finite.
	Rate float64

	// Burst is the maximum burst size allowed per key.
	// Default (<= 0) is max(1, int(Rate)).
	Burst int

	// MaxKeys bounds the number of tracked keys in memory to prevent
	// unbounded memory growth from spoofed keys. Default (<= 0) is 65536.
	MaxKeys int

	// KeyFunc extracts the rate-limiting key for each request.
	// Default is (*Context).ClientIP.
	KeyFunc func(*Context) string

	// Skip optionally bypasses rate limiting for matching requests.
	Skip func(*Context) bool

	// OnLimit optionally handles requests that exceed the rate limit after
	// Retry-After has been populated. Default returns 429 Too Many Requests.
	OnLimit func(*Context) error
}

// RateLimit returns per-client-IP token-bucket rate limiting middleware with
// sustained requestsPerSecond and burst capacity.
// It panics if requestsPerSecond is not positive and finite.
func RateLimit(requestsPerSecond float64, burst int) HandlerFunc {
	return RateLimitWithConfig(RateLimitConfig{
		Rate:  requestsPerSecond,
		Burst: burst,
	})
}

const (
	rateLimitShards      = 64
	defaultRateLimitKeys = 65536
	rateLimitEvictSample = 16
)

type rateBucket struct {
	tokens  float64
	lastSec float64
}

type rateLimitShard struct {
	mu      sync.Mutex
	buckets map[string]rateBucket
	_       [64]byte
}

// RateLimitWithConfig returns token-bucket rate limiting middleware configured
// with cfg. It panics if cfg.Rate is not positive and finite.
func RateLimitWithConfig(cfg RateLimitConfig) HandlerFunc {
	if math.IsNaN(cfg.Rate) || math.IsInf(cfg.Rate, 0) || cfg.Rate <= 0 {
		panic("server: RateLimit Rate must be positive and finite")
	}
	burst := cfg.Burst
	if burst <= 0 {
		if cfg.Rate >= float64(math.MaxInt) {
			burst = math.MaxInt
		} else {
			burst = int(cfg.Rate)
		}
		if burst < 1 {
			burst = 1
		}
	}
	maxKeys := cfg.MaxKeys
	if maxKeys <= 0 {
		maxKeys = defaultRateLimitKeys
	}
	perShard := (maxKeys + rateLimitShards - 1) / rateLimitShards
	if perShard < 1 {
		perShard = 1
	}

	shards := make([]rateLimitShard, rateLimitShards)
	for i := range shards {
		shards[i].buckets = make(map[string]rateBucket)
	}

	keyFn := cfg.KeyFunc
	if keyFn == nil {
		keyFn = (*Context).ClientIP
	}
	rate := cfg.Rate
	floatBurst := float64(burst)
	epoch := time.Now()

	return func(c *Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}
		key := keyFn(c)
		sh := &shards[fnv64aString(key)&(rateLimitShards-1)]
		now := time.Since(epoch).Seconds()

		sh.mu.Lock()
		b, ok := sh.buckets[key]
		if !ok {
			if len(sh.buckets) >= perShard {
				evictRateBucket(sh.buckets, now, rate, floatBurst)
			}
			sh.buckets[key] = rateBucket{
				tokens:  floatBurst - 1,
				lastSec: now,
			}
			sh.mu.Unlock()
			return c.Next()
		}

		elapsed := now - b.lastSec
		if elapsed < 0 {
			elapsed = 0
		}
		tokens := b.tokens + elapsed*rate
		if tokens > floatBurst {
			tokens = floatBurst
		}
		if tokens < 1.0 {
			b.tokens = tokens
			b.lastSec = now
			sh.buckets[key] = b
			sh.mu.Unlock()

			waitSec := (1.0 - tokens) / rate
			ceilSec := math.Ceil(waitSec)
			var retryAfter int
			if math.IsNaN(ceilSec) || ceilSec < 1 {
				retryAfter = 1
			} else if math.IsInf(ceilSec, 1) || ceilSec >= float64(math.MaxInt) {
				retryAfter = math.MaxInt
			} else {
				retryAfter = int(ceilSec)
			}
			if retryAfter == 1 {
				c.respHeader()["Retry-After"] = headerValRetryOne
			} else {
				c.respHeader()["Retry-After"] = []string{strconv.Itoa(retryAfter)}
			}
			if cfg.OnLimit != nil {
				return cfg.OnLimit(c)
			}
			return NewError(http.StatusTooManyRequests, "too many requests")
		}

		b.tokens = tokens - 1.0
		b.lastSec = now
		sh.buckets[key] = b
		sh.mu.Unlock()
		return c.Next()
	}
}

func evictRateBucket(buckets map[string]rateBucket, now, rate, floatBurst float64) {
	var oldestKey string
	var oldestTime float64
	hasOldest := false
	checked := 0
	for k, b := range buckets {
		elapsed := now - b.lastSec
		if elapsed < 0 {
			elapsed = 0
		}
		if b.tokens+elapsed*rate >= floatBurst {
			delete(buckets, k)
			return
		}
		if !hasOldest || b.lastSec < oldestTime {
			oldestKey = k
			oldestTime = b.lastSec
			hasOldest = true
		}
		checked++
		if checked >= rateLimitEvictSample {
			break
		}
	}
	if hasOldest {
		delete(buckets, oldestKey)
	}
}

func fnv64aString(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	var h uint64 = offset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}
