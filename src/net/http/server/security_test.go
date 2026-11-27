// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/server"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// TestSecurityPooledContextIsolation verifies that pooled Context instances
// never leak parameters, values, headers, or query state across concurrent requests.
func TestSecurityPooledContextIsolation(t *testing.T) {
	s := server.New()

	s.GET("/tenant/{tenant}/item/{item}", func(c *server.Context) error {
		// Verify no stale keys exist from previous pooled requests.
		if _, ok := c.Get("secret"); ok {
			return server.NewError(http.StatusInternalServerError, "stale inline value leaked")
		}
		if _, ok := c.Get("overflow5"); ok {
			return server.NewError(http.StatusInternalServerError, "stale map value leaked")
		}
		tenant := c.Param("tenant")
		item := c.Param("item")
		q := c.Query("tok")
		c.Set("secret", tenant+":"+q)
		for i := range 6 {
			c.Set(fmt.Sprintf("overflow%d", i), tenant)
		}
		got, _ := server.GetValue[string](c, "secret")
		if got != tenant+":"+q {
			return server.NewError(http.StatusInternalServerError, "value mismatch")
		}
		return c.Text(http.StatusOK, tenant+"/"+item+"/"+q)
	})

	const workers = 16
	const iters = 100
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range iters {
				tenant := fmt.Sprintf("t%d", id)
				item := fmt.Sprintf("i%d", i)
				tok := fmt.Sprintf("tok-%d-%d", id, i)
				req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/tenant/%s/item/%s?tok=%s", tenant, item, tok), nil)
				rec := httptest.NewRecorder()
				s.ServeHTTP(rec, req)
				want := tenant + "/" + item + "/" + tok
				if rec.Code != http.StatusOK || rec.Body.String() != want {
					t.Errorf("worker %d iter %d: status=%d body=%q want=%q", id, i, rec.Code, rec.Body.String(), want)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

// TestSecurityOpenRedirectPrevention verifies that non-canonical paths such as
// "//evil.com/foo" or "/\evil.com" never produce protocol-relative open redirects.
func TestSecurityOpenRedirectPrevention(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.RedirectTrailingSlash = true
	s := server.NewWithConfig(cfg)
	s.GET("/ok", func(c *server.Context) error { return c.NoContent() })

	cases := []string{
		"//evil.com/steal",
		"///evil.com/steal",
		"/a/..//evil.com/steal",
		"/a/../\\evil.com",
		"//\\evil.com",
	}
	for _, raw := range cases {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		req.URL.Path = raw
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		loc := rec.Header().Get("Location")
		if strings.HasPrefix(loc, "//") || strings.HasPrefix(loc, `/\`) || strings.HasPrefix(loc, "http://evil.com") || strings.HasPrefix(loc, "https://evil.com") {
			t.Fatalf("path %q produced unsafe open redirect Location: %q (status %d)", raw, loc, rec.Code)
		}
	}

	// Context.Redirect must reject control characters and invalid status codes.
	s2 := server.New()
	s2.GET("/crlf", func(c *server.Context) error {
		return c.Redirect(http.StatusFound, "/ok\r\nSet-Cookie: evil=1")
	})
	s2.GET("/badcode", func(c *server.Context) error {
		return c.Redirect(http.StatusOK, "/ok")
	})

	rec := httptest.NewRecorder()
	s2.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/crlf", nil))
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("CRLF redirect not rejected: status=%d headers=%v", rec.Code, rec.Header())
	}

	rec = httptest.NewRecorder()
	s2.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/badcode", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("non-3xx redirect not rejected: status=%d", rec.Code)
	}
}

// TestSecurityPathTraversalAndMiddlewareBypass verifies that path traversal
// tricks cannot bypass group authentication middleware.
func TestSecurityPathTraversalAndMiddlewareBypass(t *testing.T) {
	s := server.New()
	admin := s.Group("/admin", func(c *server.Context) error {
		return server.NewError(http.StatusForbidden, "admin forbidden")
	})
	admin.GET("/secret", func(c *server.Context) error {
		return c.Text(http.StatusOK, "ADMIN_SECRET")
	})
	s.GET("/public", func(c *server.Context) error {
		return c.Text(http.StatusOK, "public")
	})

	attacks := []string{
		"/public/../admin/secret",
		"/admin/./secret",
		"//admin/secret",
		"/admin//secret",
		"/public/%2e%2e/admin/secret",
		"/admin/secret\x00",
	}
	for _, p := range attacks {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		if strings.Contains(p, "%") {
			req = httptest.NewRequest(http.MethodGet, "http://example.com"+p, nil)
		} else {
			req.URL.Path = p
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "ADMIN_SECRET") {
			t.Fatalf("path traversal %q bypassed middleware! status=%d body=%q", p, rec.Code, rec.Body.String())
		}
	}
}

// TestSecurityClientIPTrustedProxies verifies that ClientIP ignores forwarding
// headers by default and walks X-Forwarded-For right-to-left only when the peer
// is a configured trusted proxy.
func TestSecurityClientIPTrustedProxies(t *testing.T) {
	// Default config: no trusted proxies -> XFF and X-Real-IP MUST be ignored.
	sDefault := server.New()
	sDefault.GET("/ip", func(c *server.Context) error {
		return c.Text(http.StatusOK, c.ClientIP())
	})
	req := httptest.NewRequest(http.MethodGet, "/ip", nil)
	req.RemoteAddr = "203.0.113.5:54321"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.Header.Set("X-Real-IP", "10.0.0.1")
	rec := httptest.NewRecorder()
	sDefault.ServeHTTP(rec, req)
	if rec.Body.String() != "203.0.113.5" {
		t.Fatalf("untrusted XFF spoofed ClientIP: got %q, want 203.0.113.5", rec.Body.String())
	}

	// Trusted proxy config: 10.0.0.0/8 and 127.0.0.1/32.
	cfg := server.DefaultConfig()
	cfg.TrustedProxies = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("127.0.0.1/32"),
	}
	sProxy := server.NewWithConfig(cfg)
	sProxy.GET("/ip", func(c *server.Context) error {
		return c.Text(http.StatusOK, c.ClientIP())
	})

	tests := []struct {
		name   string
		remote string
		xff    []string
		xri    string
		want   string
	}{
		{
			name:   "untrusted_peer_ignores_xff",
			remote: "198.51.100.7:1234",
			xff:    []string{"1.2.3.4"},
			want:   "198.51.100.7",
		},
		{
			name:   "right_to_left_stops_at_first_untrusted",
			remote: "10.0.0.1:443",
			// Attacker injected "1.1.1.1", real client is "198.51.100.9", passed through trusted 10.0.0.2 and 10.0.0.1.
			xff:  []string{"1.1.1.1, 198.51.100.9, 10.0.0.2"},
			want: "198.51.100.9",
		},
		{
			name:   "ipv4_mapped_ipv6_peer_matches_ipv4_prefix",
			remote: "[::ffff:10.0.0.5]:8080",
			xff:    []string{"203.0.113.99, ::ffff:10.0.0.2"},
			want:   "203.0.113.99",
		},
		{
			name:   "malformed_xff_falls_back_to_peer",
			remote: "10.0.0.1:443",
			xff:    []string{"not-an-ip"},
			want:   "10.0.0.1",
		},
		{
			name:   "x_real_ip_when_no_xff",
			remote: "127.0.0.1:9999",
			xri:    "203.0.113.42",
			want:   "203.0.113.42",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ip", nil)
			req.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if tc.xri != "" {
				req.Header.Set("X-Real-IP", tc.xri)
			}
			rec := httptest.NewRecorder()
			sProxy.ServeHTTP(rec, req)
			if rec.Body.String() != tc.want {
				t.Fatalf("ClientIP() = %q, want %q", rec.Body.String(), tc.want)
			}
		})
	}
}

// TestSecurityBindJSONDefenses verifies body limit enforcement (413), trailing
// JSON value rejection (400), and non-JSON Content-Type CSRF rejection (415).
func TestSecurityBindJSONDefenses(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.MaxBodyBytes = 64
	s := server.NewWithConfig(cfg)

	var lastErr error
	s.POST("/json", func(c *server.Context) error {
		var dst map[string]any
		err := c.BindJSON(&dst)
		lastErr = err
		if err != nil {
			return err
		}
		return c.NoContent()
	})

	// 1. Non-JSON Content-Type -> 415 (blocks form/text CSRF).
	req := httptest.NewRequest(http.MethodPost, "/json", strings.NewReader(`{"a":1}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType || !errors.Is(lastErr, server.ErrUnsupportedMediaType) {
		t.Fatalf("text/plain: status=%d err=%v, want 415", rec.Code, lastErr)
	}

	// 2. Trailing JSON object smuggling -> 400.
	req = httptest.NewRequest(http.MethodPost, "/json", strings.NewReader(`{"a":1}{"b":2}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !errors.Is(lastErr, server.ErrInvalidJSON) {
		t.Fatalf("trailing JSON: status=%d err=%v, want 400", rec.Code, lastErr)
	}

	// 3. Oversized body (even with unknown ContentLength = -1 / chunked) -> 413.
	req = httptest.NewRequest(http.MethodPost, "/json", strings.NewReader(`{"a":"`+strings.Repeat("x", 100)+`"}`))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || !errors.Is(lastErr, server.ErrBodyTooLarge) {
		t.Fatalf("oversized chunked JSON: status=%d err=%v, want 413", rec.Code, lastErr)
	}
}

// TestSecurityErrorNonLeakage verifies internal errors and wrapped HTTPError.Err
// are never exposed to clients by default or JSON error handlers.
func TestSecurityErrorNonLeakage(t *testing.T) {
	for _, tc := range []struct {
		name string
		eh   server.ErrorHandler
	}{
		{"default", server.DefaultErrorHandler},
		{"json", server.JSONErrorHandler},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := server.DefaultConfig()
			cfg.ErrorHandler = tc.eh
			s := server.NewWithConfig(cfg)

			s.GET("/internal", func(c *server.Context) error {
				return errors.New("pq: password authentication failed for user postgres")
			})
			s.GET("/wrapped", func(c *server.Context) error {
				return server.WrapError(http.StatusBadRequest, "invalid input", errors.New("secret-sql-query"))
			})

			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal", nil))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "postgres") {
				t.Fatalf("internal error leaked: %q", rec.Body.String())
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("missing nosniff header")
			}

			rec = httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/wrapped", nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "secret-sql-query") {
				t.Fatalf("wrapped internal error leaked: %q", rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "invalid input") {
				t.Fatalf("expected public message in %q", rec.Body.String())
			}
		})
	}
}

// TestSecuritySetAttachmentSanitization verifies SetAttachment strips paths,
// quotes, and control characters to prevent Content-Disposition injection.
func TestSecuritySetAttachmentSanitization(t *testing.T) {
	s := server.New()
	s.GET("/dl", func(c *server.Context) error {
		c.SetAttachment("../../evil\";\r\nX-Injected: yes\r\nreport.pdf")
		return c.Text(http.StatusOK, "pdf")
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dl", nil))
	cd := rec.Header().Get("Content-Disposition")
	if strings.ContainsAny(cd, "\r\n") || rec.Header().Get("X-Injected") != "" {
		t.Fatalf("header injection in SetAttachment: cd=%q headers=%v", cd, rec.Header())
	}
	if !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("unexpected Content-Disposition: %q", cd)
	}
}

// TestSecurityConcurrentRegistrationAndServe verifies lock-free snapshot reads
// do not race with concurrent route registration under -race.
func TestSecurityConcurrentRegistrationAndServe(t *testing.T) {
	s := server.New()
	s.GET("/base", func(c *server.Context) error {
		return c.Text(http.StatusOK, "ok")
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 50 {
			s.GET(fmt.Sprintf("/dyn/%d", i), func(c *server.Context) error {
				return c.Text(http.StatusOK, "dyn")
			})
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/base", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("concurrent GET /base status = %d", rec.Code)
				return
			}
			_ = s.Routes()
		}
	}()
	wg.Wait()
}

// TestSecurityClientIPHardening verifies IPv4-mapped IPv6 TrustedProxies
// prefixes, IPv6 zone ID rejection in forwarding headers, and that all-trusted
// X-Forwarded-For chains do not fall through to X-Real-IP.
func TestSecurityClientIPHardening(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.TrustedProxies = []netip.Prefix{
		netip.MustParsePrefix("::ffff:10.0.0.0/104"),
		netip.MustParsePrefix("127.0.0.1/32"),
	}
	s := server.NewWithConfig(cfg)
	s.GET("/ip", func(c *server.Context) error {
		return c.Text(http.StatusOK, c.ClientIP())
	})

	tests := []struct {
		name   string
		remote string
		xff    string
		xri    string
		want   string
	}{
		{
			name:   "ipv4_mapped_ipv6_prefix_matches_ipv4_peer",
			remote: "10.1.2.3:443",
			xff:    "198.51.100.10, 10.9.9.9",
			want:   "198.51.100.10",
		},
		{
			name:   "all_trusted_xff_ignores_spoofed_x_real_ip",
			remote: "10.0.0.1:443",
			xff:    "10.0.0.2, 10.0.0.3",
			xri:    "1.2.3.4",
			want:   "10.0.0.2",
		},
		{
			name:   "xff_ipv6_zone_rejected",
			remote: "10.0.0.1:443",
			xff:    "fe80::1%eth0",
			want:   "10.0.0.1",
		},
		{
			name:   "x_real_ip_ipv6_zone_rejected",
			remote: "127.0.0.1:443",
			xri:    "fe80::1%lo",
			want:   "127.0.0.1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ip", nil)
			req.RemoteAddr = tc.remote
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xri != "" {
				req.Header.Set("X-Real-IP", tc.xri)
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Body.String() != tc.want {
				t.Fatalf("ClientIP() = %q, want %q", rec.Body.String(), tc.want)
			}
		})
	}
}

// TestSecurityMountRawPathAttachmentAndQueryControlChars verifies Mount keeps
// RawPath synchronized on percent-encoded prefixes, SetAttachment strips
// Windows backslashes on all platforms, and safeRedirect rejects control chars
// in RawQuery.
func TestSecurityMountRawPathAttachmentAndQueryControlChars(t *testing.T) {
	s := server.New()
	s.Mount("/legacy", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "path=%s escaped=%s", r.URL.Path, r.URL.EscapedPath())
	}))
	s.GET("/win-dl", func(c *server.Context) error {
		c.SetAttachment(`..\..\Windows\System32\report.pdf`)
		return c.NoContent()
	})
	s.GET("/many-vals", func(c *server.Context) error {
		for i := range 64 {
			c.Set(fmt.Sprintf("k%d", i), i)
		}
		return c.NoContent()
	})

	// Percent-encoded mount prefix must not leak unstripped RawPath.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/leg%61cy/sub/page", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "path=/sub/page escaped=/sub/page" {
		t.Fatalf("escaped Mount: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Windows-style traversal in SetAttachment is stripped on Linux too.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/win-dl", nil))
	cd := rec.Header().Get("Content-Disposition")
	if strings.Contains(cd, "..") || strings.Contains(cd, "System32") || !strings.Contains(cd, "report.pdf") {
		t.Fatalf("SetAttachment did not strip Windows path: %q", cd)
	}

	// Control characters in RawQuery during path-cleaning redirect must be rejected.
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.URL.Path = "/a//b"
	req.URL.RawQuery = "x=1\r\nSet-Cookie: evil=1"
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("CRLF RawQuery redirect not rejected: status=%d headers=%v", rec.Code, rec.Header())
	}

	// Large valuesMap (>32 keys) must not leak into subsequent pooled requests.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/many-vals", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("many-vals status = %d", rec.Code)
	}
}

// TestSecurityDisablePathCleaningEscapedTraversalAndMultiHeaderSmuggling
// verifies that even when DisablePathCleaning is enabled, percent-encoded dot
// or empty segments inside parameters or wildcards are rejected, and multiple
// Content-Type or Origin headers are rejected against header smuggling.
func TestSecurityDisablePathCleaningEscapedTraversalAndMultiHeaderSmuggling(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.DisablePathCleaning = true
	s := server.NewWithConfig(cfg)
	s.Use(server.CORS(server.CORSConfig{
		AllowOrigins: []string{"https://app.example.com"},
	}))

	s.GET("/users/{id}", func(c *server.Context) error {
		return c.Text(http.StatusOK, "user:"+c.Param("id"))
	})
	s.GET("/files/{filepath...}", func(c *server.Context) error {
		return c.Text(http.StatusOK, "file:"+c.Param("filepath"))
	})
	s.POST("/json", func(c *server.Context) error {
		var dst map[string]any
		if err := c.BindJSON(&dst); err != nil {
			return err
		}
		return c.Text(http.StatusOK, c.ContentType())
	})

	// 1. Escaped dot/empty segments must not match {id} or {filepath...} even with DisablePathCleaning.
	for _, target := range []string{
		"/users/..%2Fadmin",
		"/users/a%2F..%2Fb",
		"/users/a%2F.%2Fb",
		"/users/a%2F%2Fb",
		"/users/%2F",
		"/files/%2e%2e/secret",
		"/files/a%2F..%2Fb",
		"/files/a%2F%2Fb",
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("escaped traversal %q should be rejected with 404, got %d (%q)", target, rec.Code, rec.Body.String())
		}
	}

	// 2. Multiple Content-Type headers must be rejected by BindJSON (415) and ContentType() ("").
	req := httptest.NewRequest(http.MethodPost, "/json", strings.NewReader(`{"a":1}`))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("multiple Content-Type headers should return 415, got %d", rec.Code)
	}

	// 3. Multiple Origin headers must not receive Access-Control-Allow-Origin.
	req = httptest.NewRequest(http.MethodGet, "/users/42", nil)
	req.Header.Add("Origin", "https://app.example.com")
	req.Header.Add("Origin", "https://evil.example.com")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("multiple Origin headers granted ACAO=%q", got)
	}
}

// TestSecurityNonSlashPathAndSingleHeaderSmuggling verifies that malformed
// non-slash paths (such as 1-byte or authority-form paths on CONNECT or with
// DisablePathCleaning) never panic or falsely match root parameters, and that
// duplicate single-value headers (Authorization, X-Real-IP, X-Forwarded-Proto,
// X-Request-ID) are rejected.
func TestSecurityNonSlashPathAndSingleHeaderSmuggling(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.DisablePathCleaning = true
	cfg.RedirectTrailingSlash = true
	cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	s := server.NewWithConfig(cfg)
	s.Use(server.RequestIDWithConfig(server.RequestIDConfig{TrustIncoming: true}))

	// Populate both radixEdges (>1 static branches) and a root param route.
	s.GET("/alpha/{id}", func(c *server.Context) error { return c.NoContent() })
	s.GET("/beta/{id}", func(c *server.Context) error { return c.NoContent() })
	s.GET("/{rootParam}", func(c *server.Context) error {
		return c.Text(http.StatusOK, c.Param("rootParam"))
	})
	s.GET("/inspect", func(c *server.Context) error {
		tok, ok := c.BearerToken()
		id, _ := server.GetValue[string](c, server.RequestIDKey)
		return c.Text(http.StatusOK, fmt.Sprintf("ip=%s;scheme=%s;tok=%s:%v;id=%s",
			c.ClientIP(), c.Scheme(), tok, ok, id))
	})
	s.GET("/basic", server.BasicAuth("test", func(u, p string) bool {
		return u == "admin" && p == "pass"
	}), func(c *server.Context) error {
		return c.Text(http.StatusOK, "authed")
	})

	// 1. Non-slash paths ("", "a", "alpha/1") must return 404 without panicking or matching /{rootParam} (405).
	for _, method := range []string{http.MethodConnect, http.MethodGet, http.MethodPost} {
		for _, p := range []string{"", "a", "alpha/1", "example.com:443"} {
			req := httptest.NewRequest(method, "http://example.com/", nil)
			req.URL.Path = p
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s path=%q: status=%d, want 404", method, p, rec.Code)
			}
		}
	}

	// 2. Duplicate Authorization, X-Real-IP, X-Forwarded-Proto, and X-Request-ID headers must be ignored/rejected.
	req := httptest.NewRequest(http.MethodGet, "/inspect", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Add("Authorization", "Bearer tok1")
	req.Header.Add("Authorization", "Bearer tok2")
	req.Header.Add("X-Real-IP", "203.0.113.1")
	req.Header.Add("X-Real-IP", "203.0.113.2")
	req.Header.Add("X-Forwarded-Proto", "https")
	req.Header.Add("X-Forwarded-Proto", "http")
	req.Header.Add("X-Request-ID", "custom-id-1")
	req.Header.Add("X-Request-ID", "custom-id-2")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "ip=10.0.0.1;scheme=http;tok=:false;") || strings.Contains(body, "custom-id") {
		t.Fatalf("duplicate single-value headers were not rejected: %q", body)
	}

	// 3. Duplicate Authorization headers on BasicAuth must return 401.
	req = httptest.NewRequest(http.MethodGet, "/basic", nil)
	req.SetBasicAuth("admin", "pass")
	req.Header.Add("Authorization", "Basic YWRtaW46cGFzcw==")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate BasicAuth Authorization header should return 401, got %d", rec.Code)
	}
}

// TestSecurityNestedTighterBodyLimitAndEncodedPrefixRejection verifies that a
// tighter inner BodyLimit or SetMaxBodyBytes is enforced even when an outer
// BodyLimit middleware already wrapped the request body, that Mount/StaticFS
// reject percent-encoded prefixes, and that custom-index directory redirects
// preserve percent-encoding.
func TestSecurityNestedTighterBodyLimitAndEncodedPrefixRejection(t *testing.T) {
	s := server.New()
	s.Use(server.BodyLimit(4096))

	s.POST("/tight-form", server.BodyLimit(16), func(c *server.Context) error {
		vals, err := c.ParseForm()
		if err != nil {
			return err
		}
		return c.Text(http.StatusOK, vals.Get("k"))
	})
	s.POST("/tight-formvalue", func(c *server.Context) error {
		c.SetMaxBodyBytes(16)
		return c.Text(http.StatusOK, c.FormValue("k"))
	})

	// Chunked (ContentLength = -1) 64-byte form body must be rejected by inner 16-byte BodyLimit.
	req := httptest.NewRequest(http.MethodPost, "/tight-form", strings.NewReader("k="+strings.Repeat("a", 64)))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("nested tighter BodyLimit bypassed on chunked form: status=%d body=%q, want 413", rec.Code, rec.Body.String())
	}

	// Tighter SetMaxBodyBytes in handler must bound FormValue even after outer BodyLimit(4096).
	req = httptest.NewRequest(http.MethodPost, "/tight-formvalue", strings.NewReader("k="+strings.Repeat("b", 64)))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Body.String() != "" {
		t.Fatalf("tighter SetMaxBodyBytes bypassed in FormValue: got %q", rec.Body.String())
	}

	// Percent-encoded Mount and StaticFS prefixes must panic at registration.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic for percent-encoded Mount prefix")
			}
		}()
		s.Mount("/%61pi", http.NotFoundHandler())
	}()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic for percent-encoded StaticFS prefix")
			}
		}()
		s.StaticFS("/%61ssets", fstest.MapFS{})
	}()

	// StaticFS custom index redirect preserves percent-encoded directory names.
	s2 := server.New()
	s2.StaticFS("/docs", fstest.MapFS{
		"my dir/start.html": {Data: []byte("ok")},
	}, server.StaticConfig{Index: "start.html"})
	rec = httptest.NewRecorder()
	s2.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/my%20dir?x=1", nil))
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/docs/my%20dir/?x=1" {
		t.Fatalf("custom index encoded dir redirect: status=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
}

// TestSecurityMaxBytesConnectionCloseAndMaxInt64 verifies that 413 responses
// triggered via markRequestTooLarge or wrapBodyLimit notify net/http to close
// keep-alive connections even through responseWriter and compressResponseWriter,
// and that SetMaxBodyBytes(MaxInt64) does not integer-overflow.
func TestSecurityMaxBytesConnectionCloseAndMaxInt64(t *testing.T) {
	s := server.New()
	s.Use(server.Compress())

	s.POST("/json-small", func(c *server.Context) error {
		c.SetMaxBodyBytes(16)
		var v map[string]any
		if err := c.BindJSON(&v); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, v)
	})
	s.POST("/limit-wrap", server.BodyLimit(16), func(c *server.Context) error {
		_, err := c.BodyBytes()
		return err
	})
	s.POST("/max-int64", func(c *server.Context) error {
		c.SetMaxBodyBytes(int64(1<<63 - 1))
		var v struct {
			OK bool `json:"ok"`
		}
		if err := c.BindJSON(&v); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, v)
	})

	ts := httptest.NewServer(s)
	defer ts.Close()
	client := ts.Client()

	for _, tc := range []struct {
		name string
		path string
		body string
		gzip bool
	}{
		{"ContentLengthTooLarge", "/json-small", `{"data":"` + strings.Repeat("x", 64) + `"}`, false},
		{"ContentLengthTooLargeCompressed", "/json-small", `{"data":"` + strings.Repeat("x", 64) + `"}`, true},
		{"BodyLimitTooLarge", "/limit-wrap", strings.Repeat("y", 64), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, ts.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.gzip {
				req.Header.Set("Accept-Encoding", "gzip")
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413", resp.StatusCode)
			}
			if !resp.Close {
				t.Fatalf("expected Connection: close (resp.Close == true) after 413 with unread body, got resp.Close == false (Header=%v)", resp.Header)
			}
		})
	}

	// Verify SetMaxBodyBytes(math.MaxInt64) does not overflow limit+1 to MinInt64.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/max-int64", strings.NewReader(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("MaxInt64 BindJSON status = %d, want 200", resp.StatusCode)
	}
}

// TestSecurityMissedRouteParamScrub verifies that partially captured parameters
// from failed or backtracked route matches are not visible in NotFound handlers
// or subsequent pooled Context reuse.
func TestSecurityMissedRouteParamScrub(t *testing.T) {
	s := server.New()
	s.NotFound(func(c *server.Context) error {
		if c.Param("p1") != "" || c.Param("p2") != "" || c.Param("p9") != "" {
			return c.Text(http.StatusInternalServerError, fmt.Sprintf("leaked param: p1=%q p2=%q p9=%q", c.Param("p1"), c.Param("p2"), c.Param("p9")))
		}
		return c.Text(http.StatusNotFound, "not found")
	})

	s.GET("/a/{p1}/{p2}/leaf", func(c *server.Context) error {
		return c.Text(http.StatusOK, c.Param("p1")+":"+c.Param("p2"))
	})
	s.GET("/a/{p1}/{p2}/{p3}/{p4}/{p5}/{p6}/{p7}/{p8}/{p9}/deep", func(c *server.Context) error {
		return c.Text(http.StatusOK, c.Param("p9"))
	})

	// Miss after capturing 2 inline params and 9 (>maxInlineParams) params.
	for _, target := range []string{
		"/a/secret1/secret2/wrong",
		"/a/s1/s2/s3/s4/s5/s6/s7/s8/s9/wrong",
		"/a/s1%20x/s2/wrong",
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%q: status=%d body=%q, want 404", target, rec.Code, rec.Body.String())
		}
	}
}

// TestSecurityDownloadFSConfinementAndBearerAuthSmuggling verifies that
// DownloadFS confined with os.Root blocks symlink escapes and traversal without
// leaking Content-Disposition, and that BearerAuth rejects multi-header
// Authorization smuggling and sanitizes realms.
func TestSecurityDownloadFSConfinementAndBearerAuthSmuggling(t *testing.T) {
	outer := t.TempDir()
	pubDir := filepath.Join(outer, "pub")
	if err := os.MkdirAll(pubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outer, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(pubDir, "escape.txt")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	root, err := os.OpenRoot(pubDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	s := server.New()
	s.GET("/dl/{name}", func(c *server.Context) error {
		return c.DownloadFS(root.FS(), c.Param("name"), "out.txt")
	})
	s.GET("/bearer", server.BearerAuth("R\"\r\nX-Bad: 1", func(c *server.Context, tok string) bool {
		return server.ConstantTimeEqual(tok, "good-tok")
	}), func(c *server.Context) error {
		return c.NoContent()
	})

	for _, name := range []string{"escape.txt", "missing.txt"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dl/"+name, nil))
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "TOP_SECRET") || rec.Header().Get("Content-Disposition") != "" {
			t.Fatalf("DownloadFS(%q) leaked data or header: code=%d cd=%q body=%q", name, rec.Code, rec.Header().Get("Content-Disposition"), rec.Body.String())
		}
	}

	// Duplicate Authorization headers must be rejected with 401.
	req := httptest.NewRequest(http.MethodGet, "/bearer", nil)
	req.Header.Add("Authorization", "Bearer good-tok")
	req.Header.Add("Authorization", "Bearer evil-tok")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate Authorization header status=%d, want 401", rec.Code)
	}
	if www := rec.Header().Get("WWW-Authenticate"); strings.ContainsAny(www, "\r\n") {
		t.Fatalf("WWW-Authenticate header injection: %q", www)
	}
}

// TestSecurityDecompressOuterBodyCloseRace verifies that callers wrapping
// Server.ServeHTTP and calling r.Body.Close() after ServeHTTP returns never
// touch a pooled decompressReadCloser reused by another concurrent request.
func TestSecurityDecompressOuterBodyCloseRace(t *testing.T) {
	s := server.New()
	s.Use(server.BodyLimit(4096), server.Timeout(time.Second))
	s.POST("/echo", func(c *server.Context) error {
		b, err := c.BodyBytes()
		if err != nil {
			return err
		}
		return c.Text(http.StatusOK, string(b))
	})

	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		s.ServeHTTP(w, r)
	})

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write([]byte("concurrent-compressed-payload"))
	_ = gw.Close()
	payload := gzBuf.Bytes()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				req := httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(payload))
				req.Header.Set("Content-Encoding", "gzip")
				rec := httptest.NewRecorder()
				outer.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK || rec.Body.String() != "concurrent-compressed-payload" {
					t.Errorf("code=%d body=%q", rec.Code, rec.Body.String())
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestSecurityHostHeaderInjectionAndRateLimitKeyFlood verifies that AllowHosts
// blocks Host header smuggling/injection vectors and RateLimit bounds memory
// via MaxKeys eviction under key-flooding attacks.
func TestSecurityHostHeaderInjectionAndRateLimitKeyFlood(t *testing.T) {
	s := server.New()
	s.Use(server.AllowHosts("app.example.com", "*.tenant.example.com"))
	s.Use(server.RateLimitWithConfig(server.RateLimitConfig{
		Rate:    10,
		Burst:   2,
		MaxKeys: 128,
	}))
	s.GET("/ping", func(c *server.Context) error {
		return c.Text(http.StatusOK, "pong")
	})

	// Host injection vectors must be rejected before reaching handler.
	for _, badHost := range []string{
		"evil.com",
		"tenant.example.com",
		"eviltenant.example.com",
		"app.example.com:evil",
		"app.example.com\r\nX-Injected: 1",
		"app.example.com@evil.com",
		"app.example.com/path",
		"sub..tenant.example.com",
		"[::1",
	} {
		req := httptest.NewRequest(http.MethodGet, "http://placeholder/ping", nil)
		req.Host = badHost
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("Host %q was unexpectedly allowed", badHost)
		}
	}

	// Simulate key-flooding from 500 distinct spoofed/ephemeral IPs with MaxKeys=128.
	for i := range 500 {
		req := httptest.NewRequest(http.MethodGet, "http://app.example.com/ping", nil)
		req.RemoteAddr = fmt.Sprintf("203.0.%d.%d:1234", i/256, i%256)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("distinct IP %d status=%d, want 200", i, rec.Code)
		}
	}

	// Victim IP still enforces its burst limit cleanly after key-flood eviction.
	for i := range 3 {
		req := httptest.NewRequest(http.MethodGet, "http://sub.tenant.example.com/ping", nil)
		req.RemoteAddr = "198.51.100.77:5555"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		want := http.StatusOK
		if i == 2 {
			want = http.StatusTooManyRequests
		}
		if rec.Code != want {
			t.Fatalf("victim IP req %d: status=%d, want %d", i, rec.Code, want)
		}
	}
}

func TestSecurityBodyReaderVaryAndHostHardening(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.MaxBodyBytes = 64
	s := server.NewWithConfig(cfg)
	s.Use(server.AllowHosts("127.0.0.1", "[::1]", "app.example.com"))

	s.POST("/stream-read", func(c *server.Context) error {
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(c.BodyReader()); err != nil {
			return err
		}
		return c.Text(http.StatusOK, buf.String())
	})
	s.GET("/vary", func(c *server.Context) error {
		c.AddVary("Accept", "X-Bad\r\nInject: 1", "Origin, X-Evil")
		return c.NoContent()
	})

	ts := httptest.NewServer(s)
	defer ts.Close()
	client := ts.Client()

	// 1. Live server BodyReader oversized body returns 413 with Connection: close.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/stream-read", strings.NewReader(strings.Repeat("a", 256)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !resp.Close {
		t.Fatalf("oversized BodyReader live: status=%d close=%v", resp.StatusCode, resp.Close)
	}

	// 2. Compressed decompression bomb through BodyReader returns 413 with Connection: close.
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(bytes.Repeat([]byte("B"), 8192))
	_ = gw.Close()
	req, err = http.NewRequest(http.MethodPost, ts.URL+"/stream-read", &gz)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Encoding", "gzip")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !resp.Close {
		t.Fatalf("compressed bomb BodyReader live: status=%d close=%v", resp.StatusCode, resp.Close)
	}

	// 3. AddVary rejects injection vectors.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://app.example.com/vary", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("/vary code=%d", rec.Code)
	}
	if got := strings.Join(rec.Header().Values("Vary"), ","); got != "Accept" || rec.Header().Get("Inject") != "" {
		t.Fatalf("AddVary failed to filter unsafe tokens: vary=%q hdr=%v", got, rec.Header())
	}

	// 4. AllowHosts rejects malformed bracketed IPv6, zone IDs, and out-of-range ports with 400.
	for _, bad := range []string{"[::1%eth0]", "[not-ipv6]", "app.example.com:0", "app.example.com:65536"} {
		r := httptest.NewRequest(http.MethodGet, "http://placeholder/vary", nil)
		r.Host = bad
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("AllowHosts(%q) code=%d, want 400", bad, w.Code)
		}
	}
}
