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
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
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
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return func(c *Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
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

// RequestIDWithConfig returns request ID middleware configured with cfg.
func RequestIDWithConfig(cfg RequestIDConfig) HandlerFunc {
	header := cfg.Header
	if header == "" {
		header = "X-Request-Id"
	} else {
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
		ctx, cancel := context.WithTimeout(c.Context(), d)
		defer cancel()
		c.SetContext(ctx)
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
// preventing a common CORS security misconfiguration.
func CORS(cfg CORSConfig) HandlerFunc {
	allowAll := slices.Contains(cfg.AllowOrigins, "*")
	if cfg.AllowCredentials && allowAll {
		panic("server: CORS AllowCredentials cannot be combined with wildcard '*' origin")
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
		if origin == "" || hasASCIIControl(origin) {
			return c.Next()
		}

		isPreflight := c.Method() == http.MethodOptions && c.canonicalReqHeader("Access-Control-Request-Method") != ""
		if isPreflight {
			addVaryToken(h, "Access-Control-Request-Method")
			addVaryToken(h, "Access-Control-Request-Headers")
		}

		var originSlice []string
		if cfg.AllowOriginFunc != nil {
			if !cfg.AllowOriginFunc(origin) {
				if isPreflight {
					return c.WriteStatus(http.StatusForbidden)
				}
				return c.Next()
			}
			originSlice = []string{origin}
		} else if allowAll {
			if cfg.AllowCredentials {
				originSlice = []string{origin}
			} else {
				originSlice = headerValWildcard
			}
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
				if reqHdrs := c.canonicalReqHeader("Access-Control-Request-Headers"); reqHdrs != "" && !hasASCIIControl(reqHdrs) {
					h["Access-Control-Allow-Headers"] = []string{reqHdrs}
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

// BasicAuth returns HTTP Basic Authentication middleware.
// The validator function should use [ConstantTimeEqual] when comparing secrets.
func BasicAuth(realm string, validator func(username, password string) bool) HandlerFunc {
	if validator == nil {
		panic("server: nil BasicAuth validator")
	}
	if realm == "" {
		realm = "Restricted"
	}
	// Sanitize realm to prevent header/quote injection.
	safeRealm := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, realm)
	wwwAuthSlice := []string{`Basic realm="` + safeRealm + `", charset="UTF-8"`}[:1:1]

	return func(c *Context) error {
		user, pass, ok := c.req.BasicAuth()
		if !ok || !validator(user, pass) {
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
