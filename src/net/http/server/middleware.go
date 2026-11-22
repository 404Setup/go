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

// Logger returns structured request logging middleware backed by [log/slog].
// If logger is nil, [slog.Default] is used.
//
// If a downstream handler returns a non-nil error and the response has not yet
// been written, Logger invokes [Context.HandleError] before logging so the
// recorded status code and byte count match what was sent to the client.
func Logger(logger *slog.Logger) HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(c *Context) error {
		start := time.Now()
		err := c.Next()
		if err != nil && !c.errHandled {
			c.HandleError(err)
		}
		status := c.StatusCode()
		if status == 0 {
			status = http.StatusOK
		}
		attrs := []slog.Attr{
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.Int("status", status),
			slog.Int64("bytes", c.BytesWritten()),
			slog.Duration("duration", time.Since(start)),
			slog.String("client_ip", c.ClientIP()),
		}
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
		return nil
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
		header = "X-Request-ID"
	}
	gen := cfg.Generator
	if gen == nil {
		gen = generateRequestID
	}
	return func(c *Context) error {
		var id string
		if cfg.TrustIncoming {
			if incoming := c.Header(header); isValidRequestID(incoming) {
				id = incoming
			}
		}
		if id == "" {
			id = gen()
		}
		c.Set(RequestIDKey, id)
		c.SetHeader(header, id)
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

// BodyLimit returns middleware that enforces a maximum request body size of
// maxBytes, rejecting larger requests with 413 Request Entity Too Large.
func BodyLimit(maxBytes int64) HandlerFunc {
	if maxBytes <= 0 {
		panic("server: BodyLimit maxBytes must be positive")
	}
	return func(c *Context) error {
		c.SetMaxBodyBytes(maxBytes)
		if c.req.ContentLength > maxBytes {
			return WrapError(http.StatusRequestEntityTooLarge, "request body too large", ErrBodyTooLarge)
		}
		if c.req.Body != nil {
			c.req.Body = http.MaxBytesReader(c.writer, c.req.Body, maxBytes)
		}
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
	// Only sent when the request arrived over TLS (r.TLS != nil).
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
	return func(c *Context) error {
		h := c.writer.Header()
		h.Set("X-Content-Type-Options", cfg.ContentTypeOptions)
		h.Set("X-Frame-Options", cfg.FrameOptions)
		h.Set("Referrer-Policy", cfg.ReferrerPolicy)
		if cfg.ContentSecurityPolicy != "" {
			h.Set("Content-Security-Policy", cfg.ContentSecurityPolicy)
		}
		if cfg.PermissionsPolicy != "" {
			h.Set("Permissions-Policy", cfg.PermissionsPolicy)
		}
		if cfg.CrossOriginOpenerPolicy != "" {
			h.Set("Cross-Origin-Opener-Policy", cfg.CrossOriginOpenerPolicy)
		}
		if cfg.StrictTransportSecurity != "" && c.req.TLS != nil {
			h.Set("Strict-Transport-Security", cfg.StrictTransportSecurity)
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
// It panics if AllowCredentials is true and AllowOrigins contains "*",
// preventing a common CORS security misconfiguration.
func CORS(cfg CORSConfig) HandlerFunc {
	allowAll := slices.Contains(cfg.AllowOrigins, "*")
	if cfg.AllowCredentials && allowAll {
		panic("server: CORS AllowCredentials cannot be combined with wildcard '*' origin")
	}

	var allowedSet map[string]struct{}
	if cfg.AllowOriginFunc == nil && !allowAll && len(cfg.AllowOrigins) > 0 {
		allowedSet = make(map[string]struct{}, len(cfg.AllowOrigins))
		for _, o := range cfg.AllowOrigins {
			allowedSet[o] = struct{}{}
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
	allowMethodsHeader := strings.Join(methods, ", ")
	allowHeadersHeader := strings.Join(cfg.AllowHeaders, ", ")
	exposeHeadersHeader := strings.Join(cfg.ExposeHeaders, ", ")
	var maxAgeHeader string
	if cfg.MaxAge > 0 {
		maxAgeHeader = strconv.Itoa(int(cfg.MaxAge / time.Second))
	}

	isAllowed := func(origin string) bool {
		if cfg.AllowOriginFunc != nil {
			return cfg.AllowOriginFunc(origin)
		}
		if allowAll {
			return true
		}
		_, ok := allowedSet[origin]
		return ok
	}

	return func(c *Context) error {
		h := c.writer.Header()
		h.Add("Vary", "Origin")

		origin := c.Header("Origin")
		if origin == "" || strings.ContainsAny(origin, "\r\n") {
			return c.Next()
		}

		isPreflight := c.Method() == http.MethodOptions && c.Header("Access-Control-Request-Method") != ""
		if isPreflight {
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
		}

		if !isAllowed(origin) {
			if isPreflight {
				return c.WriteStatus(http.StatusForbidden)
			}
			return c.Next()
		}

		if allowAll && !cfg.AllowCredentials {
			h.Set("Access-Control-Allow-Origin", "*")
		} else {
			h.Set("Access-Control-Allow-Origin", origin)
		}
		if cfg.AllowCredentials {
			h.Set("Access-Control-Allow-Credentials", "true")
		}
		if exposeHeadersHeader != "" && !isPreflight {
			h.Set("Access-Control-Expose-Headers", exposeHeadersHeader)
		}

		if isPreflight {
			h.Set("Access-Control-Allow-Methods", allowMethodsHeader)
			if allowHeadersHeader != "" {
				h.Set("Access-Control-Allow-Headers", allowHeadersHeader)
			}
			if maxAgeHeader != "" {
				h.Set("Access-Control-Max-Age", maxAgeHeader)
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
	wwwAuth := `Basic realm="` + safeRealm + `", charset="UTF-8"`

	return func(c *Context) error {
		user, pass, ok := c.req.BasicAuth()
		if !ok || !validator(user, pass) {
			c.SetHeader("WWW-Authenticate", wwwAuth)
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
		})
		mw(next).ServeHTTP(c.writer, c.req)
		if !called {
			return nil
		}
		return nextErr
	}
}
