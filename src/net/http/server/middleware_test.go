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
