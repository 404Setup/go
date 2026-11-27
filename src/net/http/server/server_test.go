// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/server"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestServerGroupsAndMiddlewareOrder(t *testing.T) {
	s := server.New()

	var trace []string
	record := func(name string) server.HandlerFunc {
		return func(c *server.Context) error {
			trace = append(trace, name+":before")
			err := c.Next()
			trace = append(trace, name+":after")
			return err
		}
	}

	s.Use(record("root"))
	api := s.Group("/api", record("api"))
	v1 := api.Group("/v1", record("v1"))
	v1.GET("/ping", record("route"), func(c *server.Context) error {
		trace = append(trace, "handler")
		return c.Text(http.StatusOK, "pong")
	})

	// Sibling group must not inherit v1's middleware.
	v2 := api.Group("/v2")
	v2.GET("/ping", func(c *server.Context) error {
		trace = append(trace, "v2-handler")
		return c.Text(http.StatusOK, "pong2")
	})

	// Empty pattern on group registers the group prefix itself.
	api.GET("", func(c *server.Context) error {
		return c.Text(http.StatusOK, "api-index")
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil))
	if rec.Code != 200 || rec.Body.String() != "pong" {
		t.Fatalf("v1: %d %q", rec.Code, rec.Body.String())
	}
	wantTrace := []string{
		"root:before", "api:before", "v1:before", "route:before",
		"handler",
		"route:after", "v1:after", "api:after", "root:after",
	}
	if !slices.Equal(trace, wantTrace) {
		t.Fatalf("trace = %v, want %v", trace, wantTrace)
	}

	trace = nil
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/ping", nil))
	wantV2 := []string{"root:before", "api:before", "v2-handler", "api:after", "root:after"}
	if !slices.Equal(trace, wantV2) {
		t.Fatalf("v2 trace = %v, want %v", trace, wantV2)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api", nil))
	if rec.Code != 200 || rec.Body.String() != "api-index" {
		t.Fatalf("/api: %d %q", rec.Code, rec.Body.String())
	}

	// Root middleware also wraps 404 responses.
	trace = nil
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("404 status = %d", rec.Code)
	}
	if !slices.Equal(trace, []string{"root:before", "root:after"}) {
		t.Fatalf("404 trace = %v", trace)
	}
}

func TestMiddlewareShortCircuitPreventsFallthrough(t *testing.T) {
	s := server.New()
	handlerRan := false

	auth := func(c *server.Context) error {
		if c.Header("Authorization") != "Bearer secret" {
			return server.NewError(http.StatusUnauthorized, "unauthorized")
		}
		return c.Next()
	}
	s.GET("/secret", auth, func(c *server.Context) error {
		handlerRan = true
		return c.Text(http.StatusOK, "top-secret")
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/secret", nil))
	if rec.Code != http.StatusUnauthorized || handlerRan {
		t.Fatalf("short-circuit failed: status=%d handlerRan=%v", rec.Code, handlerRan)
	}

	req := httptest.NewRequest(http.MethodGet, "/secret", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !handlerRan || rec.Body.String() != "top-secret" {
		t.Fatalf("authorized request failed: status=%d handlerRan=%v body=%q", rec.Code, handlerRan, rec.Body.String())
	}
}

func TestServerJSONErrorHandlerAndCustomHandlers(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.ErrorHandler = server.JSONErrorHandler
	s := server.NewWithConfig(cfg)

	s.NotFound(func(c *server.Context) error {
		return server.NewError(http.StatusNotFound, "custom not found")
	})
	s.MethodNotAllowed(func(c *server.Context) error {
		return server.NewError(http.StatusMethodNotAllowed, "custom method not allowed")
	})
	s.GET("/only-get", func(c *server.Context) error {
		return c.NoContent()
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("404 status = %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json unmarshal: %v (raw=%q)", err, rec.Body.String())
	}
	if body["error"] != "custom not found" {
		t.Fatalf("body = %v", body)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/only-get", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("405 status = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "custom method not allowed" {
		t.Fatalf("body = %v", body)
	}

	// Non-error HTTPError status codes (< 400, e.g. 204 or 103) must fall back to 500
	// so error handlers never write a body after 1xx/204/304.
	s.GET("/bad-err-code", func(c *server.Context) error {
		return server.NewError(http.StatusNoContent, "invalid error code")
	})
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/bad-err-code", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("HTTPError(204) status = %d, want 500", rec.Code)
	}

	// Direct http.Flusher and http.Hijacker assertions on c.ResponseWriter() must succeed.
	s.GET("/iface", func(c *server.Context) error {
		rw := c.ResponseWriter()
		flusher, okF := rw.(http.Flusher)
		_, okH := rw.(http.Hijacker)
		if !okF || !okH {
			return server.NewError(http.StatusInternalServerError, "missing Flusher or Hijacker")
		}
		_, _ = io.WriteString(rw, "flushed")
		flusher.Flush()
		return nil
	})
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/iface", nil))
	if rec.Code != http.StatusOK || !rec.Flushed || rec.Body.String() != "flushed" {
		t.Fatalf("/iface: code=%d flushed=%v body=%q", rec.Code, rec.Flushed, rec.Body.String())
	}

	// Use(valid, nil) must panic atomically without appending the first middleware.
	sAtomic := server.New()
	ranPartial := false
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic on nil middleware")
			}
		}()
		sAtomic.Use(func(c *server.Context) error {
			ranPartial = true
			return c.Next()
		}, nil)
	}()
	sAtomic.GET("/check", func(c *server.Context) error { return c.NoContent() })
	rec = httptest.NewRecorder()
	sAtomic.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/check", nil))
	if ranPartial {
		t.Fatal("Use appended partial middleware before panicking on nil")
	}
}

func TestServerHandleHTTPAndMountAndRoutes(t *testing.T) {
	s := server.New()

	s.HandleHTTP(http.MethodGet, "/std/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "std:"+r.PathValue("id"))
	}))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "mounted:"+r.URL.Path)
	})
	s.Mount("/legacy", mux)
	s.Any("/any", func(c *server.Context) error {
		return c.Text(http.StatusOK, c.Method())
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/std/77", nil))
	if rec.Code != 200 || rec.Body.String() != "std:77" {
		t.Fatalf("HandleHTTP: %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/legacy", nil))
	if rec.Code != 200 || rec.Body.String() != "mounted:/" {
		t.Fatalf("Mount root: %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/legacy/sub/page", nil))
	if rec.Code != 200 || rec.Body.String() != "mounted:/sub/page" {
		t.Fatalf("Mount subpath: %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/any", nil))
	if rec.Code != 200 || rec.Body.String() != http.MethodPatch {
		t.Fatalf("Any: %d %q", rec.Code, rec.Body.String())
	}

	if routes := s.Routes(); len(routes) < 10 {
		t.Fatalf("expected >= 10 registered routes, got %d", len(routes))
	}
}

func TestServerServeContextGracefulShutdown(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.ShutdownTimeout = 2 * time.Second
	s := server.NewWithConfig(cfg)

	s.GET("/hello", func(c *server.Context) error {
		return c.Text(http.StatusOK, "world")
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.ServeContext(ctx, ln)
	}()

	resp, err := http.Get("http://" + ln.Addr().String() + "/hello")
	if err != nil {
		cancel()
		t.Fatalf("GET /hello: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "world" {
		cancel()
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeContext returned error on clean shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for ServeContext graceful shutdown")
	}

	// Verify underlying HTTPServer defaults are securely populated.
	hs := s.HTTPServer()
	if hs.ReadHeaderTimeout <= 0 || hs.IdleTimeout <= 0 || hs.MaxHeaderBytes <= 0 {
		t.Fatalf("expected secure default limits on HTTPServer, got ReadHeaderTimeout=%v IdleTimeout=%v MaxHeaderBytes=%d",
			hs.ReadHeaderTimeout, hs.IdleTimeout, hs.MaxHeaderBytes)
	}

	// Calling ServeContext with an already-canceled context must return cleanly without deadlocking.
	s2 := server.New()
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	preCanceled, preCancel := context.WithCancel(context.Background())
	preCancel()
	if err := s2.ServeContext(preCanceled, ln2); err != nil {
		t.Fatalf("ServeContext with pre-canceled context returned error: %v", err)
	}
}

func TestServer1xxInformationalStatusDoesNotFinalizeResponse(t *testing.T) {
	s := server.New()
	s.GET("/early", func(c *server.Context) error {
		c.SetHeader("Link", "</style.css>; rel=preload; as=style")
		c.ResponseWriter().WriteHeader(http.StatusEarlyHints)
		if c.Written() {
			return server.NewError(http.StatusInternalServerError, "103 should not mark response written")
		}
		return c.Text(http.StatusOK, "done")
	})

	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/early")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(b) != "done" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, b)
	}
}

func TestServerHTTPMethodsCoverage(t *testing.T) {
	s := server.New()
	echo := func(c *server.Context) error {
		return c.Text(http.StatusOK, strings.ToLower(c.Method()))
	}
	s.GET("/m", echo)
	s.HEAD("/m", func(c *server.Context) error {
		c.SetHeader("X-Head", "1")
		return c.WriteStatus(http.StatusOK)
	})
	s.POST("/m", echo)
	s.PUT("/m", echo)
	s.PATCH("/m", echo)
	s.DELETE("/m", echo)
	s.OPTIONS("/m", echo)

	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(m, "/m", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s /m: status = %d", m, rec.Code)
		}
		if m == http.MethodHead {
			if rec.Header().Get("X-Head") != "1" {
				t.Errorf("HEAD /m did not invoke explicit HEAD handler")
			}
		} else if rec.Body.String() != strings.ToLower(m) {
			t.Errorf("%s /m: body = %q", m, rec.Body.String())
		}
	}
}

func TestServerServeTLSContextGracefulShutdown(t *testing.T) {
	// Generate an ephemeral self-signed localhost cert/key pair in TempDir.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := server.DefaultConfig()
	cfg.ShutdownTimeout = 2 * time.Second
	s := server.NewWithConfig(cfg)
	s.GET("/tls", func(c *server.Context) error {
		return c.Text(http.StatusOK, "secure")
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.ServeTLSContext(ctx, ln, certFile, keyFile)
	}()

	pool := x509.NewCertPool()
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	pool.AddCert(cert)
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}
	defer client.CloseIdleConnections()

	resp, err := client.Get("https://" + ln.Addr().String() + "/tls")
	if err != nil {
		cancel()
		t.Fatalf("GET /tls: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "secure" {
		cancel()
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeTLSContext error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for ServeTLSContext shutdown")
	}
}

func TestServerAndGroupRouteScopedRegistration(t *testing.T) {
	s := server.New()
	s.Route("/api", func(api *server.Group) {
		api.Use(func(c *server.Context) error {
			c.SetHeader("X-API", "1")
			return c.Next()
		})
		api.GET("/status", func(c *server.Context) error {
			return c.Text(http.StatusOK, "api-ok")
		})
		api.Route("/v1", func(v1 *server.Group) {
			v1.Use(func(c *server.Context) error {
				c.SetHeader("X-V1", "1")
				return c.Next()
			})
			v1.GET("/users/{id}", func(c *server.Context) error {
				return c.Text(http.StatusOK, "user:"+c.Param("id"))
			})
		})
	})
	s.GET("/public", func(c *server.Context) error {
		return c.Text(http.StatusOK, "public")
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/users/9", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "user:9" ||
		rec.Header().Get("X-API") != "1" || rec.Header().Get("X-V1") != "1" {
		t.Fatalf("/api/v1/users/9: code=%d body=%q headers=%v", rec.Code, rec.Body.String(), rec.Header())
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "api-ok" ||
		rec.Header().Get("X-API") != "1" || rec.Header().Get("X-V1") != "" {
		t.Fatalf("/api/status: code=%d body=%q headers=%v", rec.Code, rec.Body.String(), rec.Header())
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/public", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("X-API") != "" {
		t.Fatalf("/public leaked group middleware: code=%d headers=%v", rec.Code, rec.Header())
	}

	// Nil callback must panic.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic for nil Route callback")
			}
		}()
		s.Route("/nil", nil)
	}()
}

func TestServerJSONErrorHandlerClearsStaleContentType(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.ErrorHandler = server.JSONErrorHandler
	s := server.NewWithConfig(cfg)
	s.GET("/xss", func(c *server.Context) error {
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return server.NewError(http.StatusBadRequest, "<b>bad</b>")
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/xss", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want application/json; charset=utf-8", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing nosniff header")
	}
}

func TestServerGroupPrefixValidationAndSentinelErrorHandling(t *testing.T) {
	s := server.New()
	api := s.Group("/api")
	v1 := api.Group("/v1")
	if api.Prefix() != "/api" || v1.Prefix() != "/api/v1" {
		t.Fatalf("Group.Prefix mismatch: api=%q v1=%q", api.Prefix(), v1.Prefix())
	}

	// Bad group prefixes panic fail-fast.
	for _, bad := range []string{"no-slash", "/api/../admin", "/api//v1", "/api/\x00"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected panic for bad Group prefix %q", bad)
				}
			}()
			s.Group(bad)
		}()
	}

	// Sentinel errors returned directly map to appropriate HTTP status codes,
	// and stale representation headers (Content-Disposition, ETag) are cleared.
	s.GET("/sentinel-413", func(c *server.Context) error {
		c.SetAttachment("secret.pdf")
		c.SetHeader("ETag", `"stale"`)
		return server.ErrBodyTooLarge
	})
	s.GET("/sentinel-415", func(c *server.Context) error {
		return server.ErrUnsupportedMediaType
	})
	s.GET("/sentinel-400-json", func(c *server.Context) error {
		return server.ErrInvalidJSON
	})
	s.GET("/sentinel-400-comp", func(c *server.Context) error {
		return server.ErrInvalidCompressedBody
	})

	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/sentinel-413", http.StatusRequestEntityTooLarge},
		{"/sentinel-415", http.StatusUnsupportedMediaType},
		{"/sentinel-400-json", http.StatusBadRequest},
		{"/sentinel-400-comp", http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.status {
			t.Fatalf("%s: status=%d, want %d", tc.path, rec.Code, tc.status)
		}
		if rec.Header().Get("Content-Disposition") != "" || rec.Header().Get("ETag") != "" {
			t.Fatalf("%s: stale representation headers leaked: %v", tc.path, rec.Header())
		}
	}
}

func TestServerMatchAtomicityErrorNilGuardsAndContextChecks(t *testing.T) {
	s := server.New()
	s.POST("/conflict", func(c *server.Context) error { return c.NoContent() })

	// 1. Match panicking on a conflicting or invalid second method must leave routes unchanged.
	for _, methods := range [][]string{
		{http.MethodGet, http.MethodPost}, // POST conflicts with existing route
		{http.MethodGet, http.MethodGet},  // duplicate method in Match
		{http.MethodGet, "BAD METHOD"},    // invalid HTTP method
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected Match(%v) panic", methods)
				}
			}()
			s.Match(methods, "/conflict", func(c *server.Context) error { return c.NoContent() })
		}()
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/conflict", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("Match(%v) partially registered GET /conflict: status=%d, want 405", methods, rec.Code)
		}
	}

	// 2. Typed-nil *HTTPError and stale Content-Range are handled safely without panicking.
	s.GET("/typed-nil-err", func(c *server.Context) error {
		c.SetHeader("Content-Range", "bytes 0-10/100")
		var he *server.HTTPError
		return he
	})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/typed-nil-err", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("typed-nil *HTTPError status = %d, want 500", rec.Code)
	}
	if cr := rec.Header().Get("Content-Range"); cr != "" {
		t.Fatalf("stale Content-Range leaked on error: %q", cr)
	}

	// DefaultErrorHandler / JSONErrorHandler with nil context or nil error must not panic.
	server.DefaultErrorHandler(nil, server.NewError(http.StatusBadRequest, "bad"))
	server.JSONErrorHandler(nil, server.NewError(http.StatusBadRequest, "bad"))

	// 3. Context lifecycle methods panic cleanly on nil context.
	for _, fn := range []func(){
		func() { _ = s.ListenAndServeContext(nil, "127.0.0.1:0") },
		func() { _ = s.ListenAndServeTLSContext(nil, "127.0.0.1:0", "c", "k") },
		func() { _ = s.ServeContext(nil, nil) },
		func() { _ = s.ServeTLSContext(nil, nil, "c", "k") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic on nil context")
				}
			}()
			fn()
		}()
	}
}
