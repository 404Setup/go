// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/server"
	"strings"
	"testing"
)

type discardResponseWriter struct {
	header http.Header
	code   int
}

func (d *discardResponseWriter) Header() http.Header {
	if d.header == nil {
		d.header = make(http.Header, 4)
	}
	return d.header
}

func (d *discardResponseWriter) WriteHeader(code int) {
	d.code = code
}

func (d *discardResponseWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func (d *discardResponseWriter) WriteString(s string) (int, error) {
	return len(s), nil
}

func buildBenchServer() *server.Server {
	s := server.New()
	for i := range 50 {
		s.GET(fmt.Sprintf("/api/v1/static/resource-%02d", i), func(c *server.Context) error {
			return c.NoContent()
		})
	}
	s.GET("/api/v1/health", func(c *server.Context) error {
		return c.NoContent()
	})
	s.GET("/api/v1/users/{id}", func(c *server.Context) error {
		_ = c.Param("id")
		return c.NoContent()
	})
	s.GET("/api/v1/orgs/{org}/repos/{repo}/issues/{issue}", func(c *server.Context) error {
		_ = c.Param("org")
		_ = c.Param("repo")
		_ = c.Param("issue")
		return c.NoContent()
	})
	s.GET("/files/{filepath...}", func(c *server.Context) error {
		_ = c.Param("filepath")
		return c.NoContent()
	})
	return s
}

func buildBenchServeMux() *http.ServeMux {
	mux := http.NewServeMux()
	for i := range 50 {
		mux.HandleFunc(fmt.Sprintf("GET /api/v1/static/resource-%02d", i), func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = r.PathValue("id")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/orgs/{org}/repos/{repo}/issues/{issue}", func(w http.ResponseWriter, r *http.Request) {
		_ = r.PathValue("org")
		_ = r.PathValue("repo")
		_ = r.PathValue("issue")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /files/{filepath...}", func(w http.ResponseWriter, r *http.Request) {
		_ = r.PathValue("filepath")
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func BenchmarkStaticRoute(b *testing.B) {
	s := buildBenchServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	var w discardResponseWriter

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.ServeHTTP(&w, req)
	}
}

func BenchmarkServeMuxStaticRoute(b *testing.B) {
	mux := buildBenchServeMux()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	var w discardResponseWriter

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		mux.ServeHTTP(&w, req)
	}
}

func BenchmarkParam1Route(b *testing.B) {
	s := buildBenchServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/42", nil)
	var w discardResponseWriter

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.ServeHTTP(&w, req)
	}
}

func BenchmarkServeMuxParam1Route(b *testing.B) {
	mux := buildBenchServeMux()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/42", nil)
	var w discardResponseWriter

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		mux.ServeHTTP(&w, req)
	}
}

func BenchmarkParam3Route(b *testing.B) {
	s := buildBenchServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/orgs/golang/repos/go/issues/12345", nil)
	var w discardResponseWriter

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.ServeHTTP(&w, req)
	}
}

func BenchmarkServeMuxParam3Route(b *testing.B) {
	mux := buildBenchServeMux()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/orgs/golang/repos/go/issues/12345", nil)
	var w discardResponseWriter

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		mux.ServeHTTP(&w, req)
	}
}

func BenchmarkWildcardRoute(b *testing.B) {
	s := buildBenchServer()
	req := httptest.NewRequest(http.MethodGet, "/files/assets/js/app.bundle.js", nil)
	var w discardResponseWriter

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.ServeHTTP(&w, req)
	}
}

func BenchmarkMiddlewareChain5(b *testing.B) {
	s := server.New()
	noop := func(c *server.Context) error {
		c.Set("k1", 1)
		return c.Next()
	}
	s.Use(noop, noop, noop, noop, noop)
	s.GET("/api/v1/users/{id}", func(c *server.Context) error {
		_, _ = c.Get("k1")
		_ = c.Param("id")
		return c.NoContent()
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/42", nil)
	var w discardResponseWriter

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.ServeHTTP(&w, req)
	}
}

func BenchmarkJSONResponse(b *testing.B) {
	type item struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
		OK   bool   `json:"ok"`
	}
	payload := &item{ID: 42, Name: "example", OK: true}
	s := server.New()
	s.GET("/json", func(c *server.Context) error {
		return c.JSON(http.StatusOK, payload)
	})
	req := httptest.NewRequest(http.MethodGet, "/json", nil)
	w := &discardResponseWriter{header: make(http.Header, 4)}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		clear(w.header)
		s.ServeHTTP(w, req)
	}
}

func BenchmarkServeMuxJSONResponse(b *testing.B) {
	type item struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
		OK   bool   `json:"ok"`
	}
	payload := &item{ID: 42, Name: "example", OK: true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json", func(w http.ResponseWriter, r *http.Request) {
		data, err := json.Marshal(payload)
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})
	req := httptest.NewRequest(http.MethodGet, "/json", nil)
	w := &discardResponseWriter{header: make(http.Header, 4)}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		clear(w.header)
		mux.ServeHTTP(w, req)
	}
}

func BenchmarkParallelParamRoute(b *testing.B) {
	s := buildBenchServer()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/users/42", nil)
		var w discardResponseWriter
		for pb.Next() {
			s.ServeHTTP(&w, req)
		}
	})
}

func BenchmarkServeMuxParallelParamRoute(b *testing.B) {
	mux := buildBenchServeMux()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/users/42", nil)
		var w discardResponseWriter
		for pb.Next() {
			mux.ServeHTTP(&w, req)
		}
	})
}

func BenchmarkClientServerHTTP1(b *testing.B) {
	s := server.New()
	s.GET("/users/{id}", func(c *server.Context) error {
		return c.Text(http.StatusOK, c.Param("id"))
	})
	ts := httptest.NewServer(s)
	defer ts.Close()
	client := ts.Client()
	url := ts.URL + "/users/42"

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		resp, err := client.Get(url)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkBindJSON(b *testing.B) {
	type payload struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	s := server.New()
	s.POST("/bind", func(c *server.Context) error {
		var p payload
		if err := c.BindJSON(&p); err != nil {
			return err
		}
		return c.NoContent()
	})
	const raw = `{"name":"alice","age":30}`
	sr := strings.NewReader(raw)
	req := httptest.NewRequest(http.MethodPost, "/bind", io.NopCloser(sr))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(raw))
	var w discardResponseWriter

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		sr.Reset(raw)
		req.Body = io.NopCloser(sr)
		s.ServeHTTP(&w, req)
	}
}
