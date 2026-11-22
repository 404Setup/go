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
