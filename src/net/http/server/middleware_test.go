// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server_test

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/server"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMiddlewareRecoverAndAbortHandler(t *testing.T) {
	var panicked any
	s := server.New()
	s.Use(server.RecoverWithConfig(server.RecoverConfig{
		OnPanic: func(c *server.Context, r any, stack []byte) {
			panicked = r
			if len(stack) == 0 {
				t.Error("expected non-empty stack trace")
			}
		},
	}))

	s.GET("/boom", func(c *server.Context) error {
		panic("sensitive-internal-secret-db-password")
	})
	s.GET("/abort", func(c *server.Context) error {
		panic(http.ErrAbortHandler)
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sensitive-internal-secret") {
		t.Fatalf("panic value leaked to client: %q", rec.Body.String())
	}
	if panicked != "sensitive-internal-secret-db-password" {
		t.Fatalf("OnPanic got %v", panicked)
	}

	// http.ErrAbortHandler MUST be re-panicked, never swallowed.
	defer func() {
		r := recover()
		if r != http.ErrAbortHandler {
			t.Fatalf("expected http.ErrAbortHandler re-panic, got %v", r)
		}
	}()
	rec2 := httptest.NewRecorder()
	s.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/abort", nil))
}

func TestMiddlewareLoggerRecordsAccurateErrorStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	s := server.New()
	s.Use(server.RequestID(), server.Logger(logger))
	s.GET("/users/{id}", func(c *server.Context) error {
		if c.Param("id") == "0" {
			return server.NewError(http.StatusNotFound, "user not found")
		}
		return c.Text(http.StatusOK, "ok")
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/users/0", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal log: %v (raw=%q)", err, buf.String())
	}
	if entry["status"] != float64(http.StatusNotFound) {
		t.Fatalf("logged status = %v, want 404", entry["status"])
	}
	if entry["route"] != "/users/{id}" {
		t.Fatalf("logged route = %v, want /users/{id}", entry["route"])
	}
	if entry["bytes"] != float64(len("user not found")) {
		t.Fatalf("logged bytes = %v, want %d", entry["bytes"], len("user not found"))
	}
	if id, _ := entry["request_id"].(string); len(id) != 32 {
		t.Fatalf("logged request_id = %q, want 32-char hex", id)
	}
}

func TestMiddlewareRequestID(t *testing.T) {
	// Default: ignores incoming X-Request-ID.
	s := server.New()
	s.Use(server.RequestID())
	s.GET("/id", func(c *server.Context) error {
		id, _ := server.GetValue[string](c, server.RequestIDKey)
		return c.Text(http.StatusOK, id)
	})

	req := httptest.NewRequest(http.MethodGet, "/id", nil)
	req.Header.Set("X-Request-ID", "spoofed-id")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Body.String() == "spoofed-id" || len(rec.Body.String()) != 32 {
		t.Fatalf("default RequestID should not trust incoming header, got %q", rec.Body.String())
	}
	if rec.Header().Get("X-Request-ID") != rec.Body.String() {
		t.Fatalf("header mismatch: %q vs %q", rec.Header().Get("X-Request-ID"), rec.Body.String())
	}

	// TrustIncoming: accepts valid ASCII ID, rejects invalid/CRLF/oversized IDs.
	s2 := server.New()
	s2.Use(server.RequestIDWithConfig(server.RequestIDConfig{TrustIncoming: true}))
	s2.GET("/id", func(c *server.Context) error {
		id, _ := server.GetValue[string](c, server.RequestIDKey)
		return c.Text(http.StatusOK, id)
	})

	req = httptest.NewRequest(http.MethodGet, "/id", nil)
	req.Header.Set("X-Request-ID", "trace-abc-123")
	rec = httptest.NewRecorder()
	s2.ServeHTTP(rec, req)
	if rec.Body.String() != "trace-abc-123" {
		t.Fatalf("trusted valid ID got %q", rec.Body.String())
	}

	for _, bad := range []string{"bad id space", "bad\r\nid", strings.Repeat("a", 200)} {
		req := httptest.NewRequest(http.MethodGet, "/id", nil)
		req.Header.Set("X-Request-ID", bad)
		rec := httptest.NewRecorder()
		s2.ServeHTTP(rec, req)
		if rec.Body.String() == bad || len(rec.Body.String()) != 32 {
			t.Fatalf("invalid incoming ID %q should be replaced, got %q", bad, rec.Body.String())
		}
	}
}

func TestMiddlewareBodyLimitAndTimeout(t *testing.T) {
	s := server.New()
	s.POST("/small", server.BodyLimit(10), func(c *server.Context) error {
		b, err := c.BodyBytes()
		if err != nil {
			return err
		}
		return c.Text(http.StatusOK, string(b))
	})
	s.GET("/slow", server.Timeout(20*time.Millisecond), func(c *server.Context) error {
		select {
		case <-c.Context().Done():
			return server.NewError(http.StatusGatewayTimeout, "timeout")
		case <-time.After(2 * time.Second):
			return c.Text(http.StatusOK, "too-late")
		}
	})

	var outerCtxErr error
	s.GET("/fast", func(c *server.Context) error {
		err := c.Next()
		outerCtxErr = c.Context().Err()
		return err
	}, server.Timeout(time.Second), func(c *server.Context) error {
		return c.Text(http.StatusOK, "fast")
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/small", strings.NewReader("12345678901")))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("BodyLimit status = %d, want 413", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/slow", nil))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("Timeout status = %d, want 504", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fast", nil))
	if rec.Code != http.StatusOK || outerCtxErr != nil {
		t.Fatalf("outer middleware saw canceled context after Timeout: code=%d err=%v", rec.Code, outerCtxErr)
	}
}

func TestMiddlewareSecurityHeaders(t *testing.T) {
	s := server.New()
	s.Use(server.SecurityHeadersWithConfig(server.SecurityHeadersConfig{
		StrictTransportSecurity: "max-age=63072000; includeSubDomains",
		ContentSecurityPolicy:   "default-src 'self'",
	}))
	s.GET("/ok", func(c *server.Context) error { return c.NoContent() })

	// Plain HTTP: HSTS must NOT be set.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
		rec.Header().Get("X-Frame-Options") != "DENY" ||
		rec.Header().Get("Referrer-Policy") != "strict-origin-when-cross-origin" ||
		rec.Header().Get("Content-Security-Policy") != "default-src 'self'" ||
		rec.Header().Get("Strict-Transport-Security") != "" {
		t.Fatalf("plain HTTP headers = %v", rec.Header())
	}

	// TLS request: HSTS must be set.
	req := httptest.NewRequest(http.MethodGet, "https://example.com/ok", nil)
	req.TLS = &tls.ConnectionState{}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Header().Get("Strict-Transport-Security") != "max-age=63072000; includeSubDomains" {
		t.Fatalf("HTTPS HSTS missing: %v", rec.Header())
	}
}

func TestMiddlewareCORS(t *testing.T) {
	// Misconfigured wildcard + credentials MUST panic.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic when AllowCredentials is combined with wildcard '*'")
			}
		}()
		server.CORS(server.CORSConfig{
			AllowOrigins:     []string{"*"},
			AllowCredentials: true,
		})
	}()

	s := server.New()
	s.Use(server.CORS(server.CORSConfig{
		AllowOrigins:     []string{"https://app.example.com"},
		AllowMethods:     []string{http.MethodGet, http.MethodPost},
		AllowHeaders:     []string{"Authorization", "Content-Type"},
		ExposeHeaders:    []string{"X-Request-ID"},
		AllowCredentials: true,
		MaxAge:           10 * time.Minute,
	}))
	s.GET("/data", func(c *server.Context) error {
		return c.Text(http.StatusOK, "ok")
	})

	// Allowed preflight
	req := httptest.NewRequest(http.MethodOptions, "/data", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" ||
		rec.Header().Get("Access-Control-Allow-Credentials") != "true" ||
		rec.Header().Get("Access-Control-Max-Age") != "600" {
		t.Fatalf("preflight headers = %v", rec.Header())
	}

	// Disallowed preflight -> 403, no ACAO header
	req = httptest.NewRequest(http.MethodOptions, "/data", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("disallowed preflight: status=%d headers=%v", rec.Code, rec.Header())
	}

	// Disallowed simple GET -> no ACAO header, Vary: Origin still present
	req = httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("disallowed GET: status=%d headers=%v", rec.Code, rec.Header())
	}
	if !strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), "Origin") {
		t.Fatalf("expected Vary: Origin, got %v", rec.Header().Values("Vary"))
	}
}

func TestMiddlewareCrossOriginProtectionBasicAuthAndAdapt(t *testing.T) {
	s := server.New()
	s.Use(server.CrossOriginProtection(nil))

	// Adapt a standard net/http middleware that wraps ResponseWriter.
	wrapMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Adapted", "yes")
			next.ServeHTTP(&headerPrefixWriter{ResponseWriter: w}, r)
		})
	}

	s.POST("/protected",
		server.Adapt(wrapMW),
		server.BasicAuth("Admin \"Zone\"\r\nInject", func(u, p string) bool {
			return server.ConstantTimeEqual(u, "admin") && server.ConstantTimeEqual(p, "s3cr3t")
		}),
		func(c *server.Context) error {
			return c.Text(http.StatusOK, "secret")
		},
	)

	// Cross-origin POST rejected with 403 before BasicAuth.
	req := httptest.NewRequest(http.MethodPost, "/protected", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("CSRF status = %d, want 403", rec.Code)
	}

	// Same-origin POST without credentials -> 401 with sanitized realm.
	req = httptest.NewRequest(http.MethodPost, "/protected", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("BasicAuth missing status = %d, want 401", rec.Code)
	}
	wwwAuth := rec.Header().Get("WWW-Authenticate")
	if strings.ContainsAny(wwwAuth, "\r\n") || !strings.Contains(wwwAuth, `realm="Admin ZoneInject"`) {
		t.Fatalf("WWW-Authenticate not sanitized: %q", wwwAuth)
	}

	// Same-origin POST with valid credentials -> 200 and adapted wrapper worked without recursion!
	req = httptest.NewRequest(http.MethodPost, "/protected", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.SetBasicAuth("admin", "s3cr3t")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Adapted") != "yes" || rec.Body.String() != "[wrapped]secret" {
		t.Fatalf("authorized adapted request: status=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}
}

type headerPrefixWriter struct {
	http.ResponseWriter
	prefixed bool
}

func (w *headerPrefixWriter) Write(p []byte) (int, error) {
	if !w.prefixed {
		w.prefixed = true
		_, _ = io.WriteString(w.ResponseWriter, "[wrapped]")
	}
	return w.ResponseWriter.Write(p)
}

func TestMiddlewareLoggerWithConfigSkipAndErrorPropagation(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	var outerSawErr error
	s := server.New()
	s.Use(func(c *server.Context) error {
		err := c.Next()
		outerSawErr = err
		return err
	})
	s.Use(server.LoggerWithConfig(server.LoggerConfig{
		Logger: logger,
		Skip: func(c *server.Context) bool {
			return c.Path() == "/healthz"
		},
	}))

	s.GET("/healthz", func(c *server.Context) error {
		return c.NoContent()
	})
	s.GET("/fail", func(c *server.Context) error {
		return server.NewError(http.StatusBadRequest, "bad request")
	})

	// Skipped route emits no log.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusNoContent || buf.Len() != 0 {
		t.Fatalf("healthz: code=%d log=%q", rec.Code, buf.String())
	}

	// Error is logged once, handled once, and propagated to outer middleware.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fail", nil))
	if rec.Code != http.StatusBadRequest || rec.Body.String() != "bad request" {
		t.Fatalf("fail: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if outerSawErr == nil {
		t.Fatal("outer middleware did not observe error returned through Logger")
	}
	if buf.Len() == 0 {
		t.Fatal("expected log entry for /fail")
	}
}

func TestMiddlewareAdaptErrorAndTrustedProxyHSTSAndCORSWildcardHeaders(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	s := server.NewWithConfig(cfg)

	s.Use(server.SecurityHeadersWithConfig(server.SecurityHeadersConfig{
		StrictTransportSecurity: "max-age=31536000",
	}))
	s.Use(server.CORS(server.CORSConfig{
		AllowOrigins:     []string{"https://app.example.com"},
		AllowHeaders:     []string{"*"},
		AllowCredentials: true,
	}))

	wrapMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(&headerPrefixWriter{ResponseWriter: w}, r)
		})
	}
	s.GET("/adapted-err", server.Adapt(wrapMW), func(c *server.Context) error {
		return server.NewError(http.StatusTeapot, "teapot")
	})

	// 1. Adapted net/http middleware wraps error responses returned by downstream HandlerFunc.
	req := httptest.NewRequest(http.MethodGet, "/adapted-err", nil)
	req.RemoteAddr = "10.0.0.1:443"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot || rec.Body.String() != "[wrapped]teapot" {
		t.Fatalf("adapted-err: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Fatalf("expected HSTS behind trusted proxy with X-Forwarded-Proto: https, got %v", rec.Header())
	}

	// 2. CORS preflight with AllowCredentials=true and AllowHeaders=["*"] echoes Access-Control-Request-Headers.
	req = httptest.NewRequest(http.MethodOptions, "/adapted-err", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "X-Custom-Auth, Content-Type")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("CORS preflight code=%d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "X-Custom-Auth, Content-Type" {
		t.Fatalf("Access-Control-Allow-Headers = %q", got)
	}
}

func TestMiddlewareMaxInFlight(t *testing.T) {
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic for non-positive MaxInFlight limit")
			}
		}()
		server.MaxInFlight(0)
	}()

	s := server.New()
	s.Use(server.Recover(), server.MaxInFlight(2))

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	s.GET("/hold", func(c *server.Context) error {
		started <- struct{}{}
		<-release
		return c.Text(http.StatusOK, "held")
	})
	s.GET("/panic", func(c *server.Context) error {
		panic("boom")
	})
	s.GET("/ok", func(c *server.Context) error {
		return c.Text(http.StatusOK, "ok")
	})

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hold", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("held request status = %d, want 200", rec.Code)
			}
		}()
	}
	<-started
	<-started

	// 3rd concurrent request must immediately receive 503 Service Unavailable.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated request status = %d, want 503", rec.Code)
	}

	close(release)
	wg.Wait()

	// Panicking request must not leak in-flight slots.
	for range 3 {
		rec = httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("panic status = %d, want 500", rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("post-panic request failed: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestMiddlewareBearerAuthAdaptHandlerAndLoggerFallback(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	cfg := server.DefaultConfig()
	cfg.Logger = logger
	s := server.NewWithConfig(cfg)
	// Logger(nil) must fall back to Config.Logger when configured on Server.
	s.Use(server.Logger(nil))

	s.GET("/api/items/{id}",
		server.BearerAuth("API \"Realm\"\r\nBad", func(c *server.Context, tok string) bool {
			if !server.ConstantTimeEqual(tok, "secret-token-123") {
				return false
			}
			c.Set("auth_user", "alice")
			return true
		}),
		func(c *server.Context) error {
			user, _ := server.GetValue[string](c, "auth_user")
			return c.Text(http.StatusOK, user+":"+c.Param("id"))
		},
	)

	// Empty realm sets plain "Bearer" WWW-Authenticate.
	s.GET("/api/plain-bearer",
		server.BearerAuth("", func(c *server.Context, tok string) bool {
			return false
		}),
		func(c *server.Context) error { return c.NoContent() },
	)

	// AdaptHandler inline with middleware and path parameter propagation.
	s.GET("/std-inline/{name}",
		func(c *server.Context) error {
			c.SetHeader("X-Pre", "1")
			return c.Next()
		},
		server.AdaptHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "hello:"+r.PathValue("name"))
		})),
	)

	// Missing token -> 401 + sanitized WWW-Authenticate.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/items/7", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer status = %d, want 401", rec.Code)
	}
	if www := rec.Header().Get("WWW-Authenticate"); strings.ContainsAny(www, "\r\n") || !strings.Contains(www, `Bearer realm="API RealmBad"`) {
		t.Fatalf("WWW-Authenticate not sanitized: %q", www)
	}
	// Logger(nil) logged to Config.Logger!
	if !strings.Contains(logBuf.String(), `"/api/items/7"`) {
		t.Fatalf("Logger(nil) did not use Config.Logger: %q", logBuf.String())
	}

	// Empty realm -> WWW-Authenticate: Bearer.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/plain-bearer", nil))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("plain bearer: code=%d www=%q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}

	// Valid token succeeds and validator context value is visible downstream.
	req := httptest.NewRequest(http.MethodGet, "/api/items/99", nil)
	req.Header.Set("Authorization", "Bearer secret-token-123")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "alice:99" {
		t.Fatalf("valid bearer: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// AdaptHandler works with middleware and PathValue.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/std-inline/world", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("X-Pre") != "1" || rec.Body.String() != "hello:world" {
		t.Fatalf("AdaptHandler: code=%d hdr=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}

	// Nil panics.
	for _, fn := range []func(){
		func() { server.BearerAuth("r", nil) },
		func() { server.AdaptHandler(nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			fn()
		}()
	}
}

func TestMiddlewareAllowHosts(t *testing.T) {
	s := server.New()
	s.Use(server.AllowHosts("example.com", "localhost:8080", "*.api.example.com", "::1"))
	s.GET("/ok", func(c *server.Context) error {
		return c.Text(http.StatusOK, c.Host())
	})

	cases := []struct {
		name   string
		host   string
		extra  string
		status int
	}{
		{"exact", "example.com", "", http.StatusOK},
		{"exact_case_and_any_port", "EXAMPLE.COM:8443", "", http.StatusOK},
		{"port_specific_match", "localhost:8080", "", http.StatusOK},
		{"port_specific_mismatch", "localhost:9090", "", http.StatusForbidden},
		{"port_specific_missing_port", "localhost", "", http.StatusForbidden},
		{"wildcard_single_label", "v1.api.example.com", "", http.StatusOK},
		{"wildcard_multi_label", "eu.v2.api.example.com:443", "", http.StatusOK},
		{"wildcard_apex_rejected", "api.example.com", "", http.StatusForbidden},
		{"wildcard_prefix_attack_rejected", "evilapi.example.com", "", http.StatusForbidden},
		{"ipv6_normalized", "[::1]:8080", "", http.StatusOK},
		{"empty_host", "", "", http.StatusBadRequest},
		{"malformed_slash", "example.com/evil", "", http.StatusBadRequest},
		{"malformed_userinfo", "user@example.com", "", http.StatusBadRequest},
		{"malformed_empty_label", "a..api.example.com", "", http.StatusBadRequest},
		{"duplicate_host_header", "example.com", "evil.com", http.StatusBadRequest},
		{"unlisted_host", "evil.com", "", http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://placeholder/ok", nil)
			req.Host = tc.host
			if tc.extra != "" {
				req.Header.Add("Host", tc.host)
				req.Header.Add("Host", tc.extra)
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("Host=%q extra=%q: status=%d, want %d (body=%q)", tc.host, tc.extra, rec.Code, tc.status, rec.Body.String())
			}
		})
	}

	// Invalid AllowHosts arguments panic at registration.
	for _, fn := range []func(){
		func() { server.AllowHosts() },
		func() { server.AllowHosts("") },
		func() { server.AllowHosts("*") },
		func() { server.AllowHosts("*.") },
		func() { server.AllowHosts("bad host") },
		func() { server.AllowHosts("a..b.com") },
		func() { server.AllowHosts("example.com/path") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected AllowHosts panic")
				}
			}()
			fn()
		}()
	}
}

func TestMiddlewareRateLimitAndConfigValidation(t *testing.T) {
	// 1. Burst and refill + Retry-After + Skip + custom OnLimit.
	s := server.New()
	s.Use(server.RateLimitWithConfig(server.RateLimitConfig{
		Rate:  20, // 1 token per 50ms
		Burst: 2,
		Skip: func(c *server.Context) bool {
			return c.Path() == "/health"
		},
	}))
	s.GET("/api", func(c *server.Context) error { return c.NoContent() })
	s.GET("/health", func(c *server.Context) error { return c.NoContent() })

	for i := range 2 {
		req := httptest.NewRequest(http.MethodGet, "/api", nil)
		req.RemoteAddr = "198.51.100.1:1234"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("burst req %d: status=%d, want 204", i, rec.Code)
		}
	}

	// 3rd immediate request from same IP -> 429 with Retry-After >= 1.
	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.RemoteAddr = "198.51.100.1:1234"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("rate-limited req: status=%d Retry-After=%q", rec.Code, rec.Header().Get("Retry-After"))
	}

	// Different IP has independent bucket.
	req2 := httptest.NewRequest(http.MethodGet, "/api", nil)
	req2.RemoteAddr = "198.51.100.2:1234"
	rec2 := httptest.NewRecorder()
	s.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("other IP status = %d, want 204", rec2.Code)
	}

	// Skipped route bypasses limiter.
	for range 5 {
		reqH := httptest.NewRequest(http.MethodGet, "/health", nil)
		reqH.RemoteAddr = "198.51.100.1:1234"
		recH := httptest.NewRecorder()
		s.ServeHTTP(recH, reqH)
		if recH.Code != http.StatusNoContent {
			t.Fatalf("skipped /health status = %d, want 204", recH.Code)
		}
	}

	// Wait for token refill and retry.
	time.Sleep(70 * time.Millisecond)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("post-refill status = %d, want 204", rec.Code)
	}

	// 2. Custom KeyFunc + OnLimit + MaxKeys eviction + concurrency under -race.
	sConc := server.New()
	sConc.Use(server.RateLimitWithConfig(server.RateLimitConfig{
		Rate:    1000,
		Burst:   5,
		MaxKeys: 128,
		KeyFunc: func(c *server.Context) string { return c.Header("X-Key") },
		OnLimit: func(c *server.Context) error {
			return c.Text(http.StatusTooManyRequests, "custom-limit:"+c.ResponseWriter().Header().Get("Retry-After"))
		},
	}))
	sConc.GET("/k", func(c *server.Context) error { return c.Text(http.StatusOK, "ok") })

	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 50 {
				r := httptest.NewRequest(http.MethodGet, "/k", nil)
				r.Header.Set("X-Key", string(rune('A'+id))+string(rune('0'+i%10)))
				wrec := httptest.NewRecorder()
				sConc.ServeHTTP(wrec, r)
				if wrec.Code != http.StatusOK && wrec.Code != http.StatusTooManyRequests {
					t.Errorf("unexpected status %d", wrec.Code)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// 3. Invalid RateLimit / RequestID / SecurityHeaders / CORS configs panic.
	for _, fn := range []func(){
		func() { server.RateLimit(0, 1) },
		func() { server.RateLimit(-5, 1) },
		func() { server.RateLimit(math.NaN(), 1) },
		func() { server.RateLimit(math.Inf(1), 1) },
		func() { server.RequestIDWithConfig(server.RequestIDConfig{Header: "Bad Header"}) },
		func() { server.RequestIDWithConfig(server.RequestIDConfig{Header: "X-Bad\r\n"}) },
		func() {
			server.SecurityHeadersWithConfig(server.SecurityHeadersConfig{
				ContentSecurityPolicy: "default-src 'self'\r\nX-Bad: 1",
			})
		},
		func() { server.CORS(server.CORSConfig{AllowOrigins: []string{"https://a.com\r\n"}}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected config validation panic")
				}
			}()
			fn()
		}()
	}

	// 4. CORS AllowOriginFunc with AllowCredentials=true must still reject Origin: *.
	sCORS := server.New()
	sCORS.Use(server.CORS(server.CORSConfig{
		AllowOriginFunc:  func(origin string) bool { return true },
		AllowHeaders:     []string{"*"},
		AllowCredentials: true,
	}))
	sCORS.GET("/c", func(c *server.Context) error { return c.NoContent() })

	reqStar := httptest.NewRequest(http.MethodGet, "/c", nil)
	reqStar.Header.Set("Origin", "*")
	recStar := httptest.NewRecorder()
	sCORS.ServeHTTP(recStar, reqStar)
	if recStar.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("CORS AllowOriginFunc with AllowCredentials allowed '*': %v", recStar.Header())
	}

	// Multiple Access-Control-Request-Headers lines echoed safely when AllowHeaders=["*"] and AllowCredentials=true.
	reqPre := httptest.NewRequest(http.MethodOptions, "/c", nil)
	reqPre.Header.Set("Origin", "https://app.example.com")
	reqPre.Header.Set("Access-Control-Request-Method", "POST")
	reqPre.Header.Add("Access-Control-Request-Headers", "X-One")
	reqPre.Header.Add("Access-Control-Request-Headers", "X-Two")
	recPre := httptest.NewRecorder()
	sCORS.ServeHTTP(recPre, reqPre)
	if recPre.Code != http.StatusNoContent || recPre.Header().Get("Access-Control-Allow-Headers") != "X-One, X-Two" {
		t.Fatalf("CORS multi ACRH: code=%d allowHeaders=%q", recPre.Code, recPre.Header().Get("Access-Control-Allow-Headers"))
	}
}

func TestMiddlewareAuthConfigsAllowHostsIPv6AndRateLimitOverflow(t *testing.T) {
	// 1. BasicAuthWithConfig and BearerAuthWithConfig with Context + Skip.
	s := server.New()
	s.GET("/basic", server.BasicAuthWithConfig(server.BasicAuthConfig{
		Realm: `Ops "\r\nZone`,
		Skip: func(c *server.Context) bool {
			return c.Query("skip") == "1"
		},
		Validator: func(c *server.Context, u, p string) bool {
			if server.ConstantTimeEqual(u, "ops") && server.ConstantTimeEqual(p, "pw") {
				c.Set("user", u)
				return true
			}
			return false
		},
	}), func(c *server.Context) error {
		u, _ := server.GetValue[string](c, "user")
		return c.Text(http.StatusOK, "ok:"+u)
	})

	s.GET("/bearer", server.BearerAuthWithConfig(server.BearerAuthConfig{
		Realm: "API",
		Skip: func(c *server.Context) bool {
			return c.Query("public") == "1"
		},
		Validator: func(c *server.Context, tok string) bool {
			if server.ConstantTimeEqual(tok, "tok-1") {
				c.Set("sub", "svc")
				return true
			}
			return false
		},
	}), func(c *server.Context) error {
		sub, _ := server.GetValue[string](c, "sub")
		return c.Text(http.StatusOK, "ok:"+sub)
	})

	// Skipped requests succeed without credentials.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/basic?skip=1", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok:" {
		t.Fatalf("basic skip: code=%d body=%q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/bearer?public=1", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok:" {
		t.Fatalf("bearer skip: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// Authenticated requests propagate context values from Validator.
	req := httptest.NewRequest(http.MethodGet, "/basic", nil)
	req.SetBasicAuth("ops", "pw")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok:ops" {
		t.Fatalf("basic authed: code=%d body=%q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/bearer", nil)
	req.Header.Set("Authorization", "Bearer tok-1")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok:svc" {
		t.Fatalf("bearer authed: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// Nil validators panic.
	for _, fn := range []func(){
		func() { server.BasicAuthWithConfig(server.BasicAuthConfig{}) },
		func() { server.BearerAuthWithConfig(server.BearerAuthConfig{}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic for nil validator")
				}
			}()
			fn()
		}()
	}

	// 2. AllowHosts IPv6 canonicalization, bracket validation, and port range checks.
	sHosts := server.New()
	sHosts.Use(server.AllowHosts("2001:0db8::1", "[::1]:8443"))
	sHosts.GET("/h", func(c *server.Context) error { return c.NoContent() })

	for _, tc := range []struct {
		host   string
		status int
	}{
		{"[2001:db8::1]", http.StatusNoContent},
		{"[2001:0db8:0:0::1]:9000", http.StatusNoContent},
		{"[0:0:0:0:0:0:0:1]:8443", http.StatusNoContent},
		{"[::1]:8080", http.StatusForbidden},
		{"[2001:db8::2]", http.StatusForbidden},
		{"[not-an-ip]", http.StatusBadRequest},
		{"[::1%eth0]:8443", http.StatusBadRequest},
		{"example.com:0", http.StatusBadRequest},
		{"example.com:99999", http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://placeholder/h", nil)
		req.Host = tc.host
		rec := httptest.NewRecorder()
		sHosts.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("AllowHosts(%q): status=%d, want %d", tc.host, rec.Code, tc.status)
		}
	}

	for _, bad := range []string{"[not-an-ip]", "[::1%eth0]", "example.com:0", "example.com:65536"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected AllowHosts(%q) panic", bad)
				}
			}()
			server.AllowHosts(bad)
		}()
	}

	// 3. RateLimitWithConfig with tiny rate must not overflow Retry-After to 1.
	sRL := server.New()
	sRL.Use(server.RateLimitWithConfig(server.RateLimitConfig{
		Rate:  1e-308,
		Burst: 1,
	}))
	sRL.GET("/rl", func(c *server.Context) error { return c.NoContent() })

	rec = httptest.NewRecorder()
	sRL.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rl", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("first tiny-rate req status=%d, want 204", rec.Code)
	}
	rec = httptest.NewRecorder()
	sRL.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rl", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second tiny-rate req status=%d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" || ra == "1" || strings.HasPrefix(ra, "-") {
		t.Fatalf("tiny-rate Retry-After overflowed: %q", ra)
	}
}
