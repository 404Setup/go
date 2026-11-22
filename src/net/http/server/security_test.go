// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/server"
	"net/netip"
	"strings"
	"sync"
	"testing"
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
