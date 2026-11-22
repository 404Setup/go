// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/server"
	"strings"
	"testing"
)

func TestRouterStaticAndDynamicMatching(t *testing.T) {
	s := server.New()

	s.GET("/", func(c *server.Context) error {
		return c.Text(http.StatusOK, "root")
	})
	s.GET("/users", func(c *server.Context) error {
		return c.Text(http.StatusOK, "users-list")
	})
	s.GET("/users/", func(c *server.Context) error {
		return c.Text(http.StatusOK, "users-slash")
	})
	s.GET("/users/me", func(c *server.Context) error {
		return c.Text(http.StatusOK, "users-me")
	})
	s.GET("/users/me/profile", func(c *server.Context) error {
		return c.Text(http.StatusOK, "users-me-profile")
	})
	s.GET("/users/{id}", func(c *server.Context) error {
		return c.Text(http.StatusOK, "user:"+c.Param("id")+":"+c.RoutePattern())
	})
	s.GET("/users/{id}/settings", func(c *server.Context) error {
		return c.Text(http.StatusOK, "settings:"+c.Param("id"))
	})
	// Different parameter name on non-conflicting branch (unlike httprouter).
	s.GET("/users/{userID}/posts/{postID}", func(c *server.Context) error {
		return c.Text(http.StatusOK, "post:"+c.Param("userID")+":"+c.Param("postID"))
	})
	s.GET("/files/{name}", func(c *server.Context) error {
		return c.Text(http.StatusOK, "file-single:"+c.Param("name"))
	})
	s.GET("/files/{filepath...}", func(c *server.Context) error {
		return c.Text(http.StatusOK, "file-wild:"+c.Param("filepath"))
	})
	// Literal segment containing ':' does not conflict with {param} syntax.
	s.POST("/v1/items:batchGet", func(c *server.Context) error {
		return c.Text(http.StatusOK, "batch-get")
	})
	s.GET("/hello%20world", func(c *server.Context) error {
		return c.Text(http.StatusOK, "escaped-static")
	})

	tests := []struct {
		method string
		target string
		status int
		body   string
	}{
		{http.MethodGet, "/", http.StatusOK, "root"},
		{http.MethodGet, "/users", http.StatusOK, "users-list"},
		{http.MethodGet, "/users/", http.StatusOK, "users-slash"},
		{http.MethodGet, "/users/me", http.StatusOK, "users-me"},
		{http.MethodGet, "/users/me/profile", http.StatusOK, "users-me-profile"},
		// Backtracking: /users/me/settings tries static "me", fails on "settings",
		// then backtracks to /users/{id}/settings with id="me".
		{http.MethodGet, "/users/me/settings", http.StatusOK, "settings:me"},
		{http.MethodGet, "/users/42", http.StatusOK, "user:42:/users/{id}"},
		{http.MethodGet, "/users/42/settings", http.StatusOK, "settings:42"},
		{http.MethodGet, "/users/42/posts/99", http.StatusOK, "post:42:99"},
		// Single param has higher precedence than catch-all wildcard.
		{http.MethodGet, "/files/readme.md", http.StatusOK, "file-single:readme.md"},
		{http.MethodGet, "/files/a/b/c.txt", http.StatusOK, "file-wild:a/b/c.txt"},
		{http.MethodGet, "/files/", http.StatusOK, "file-wild:"},
		{http.MethodGet, "/files", http.StatusNotFound, "not found"},
		// Percent-encoded slash inside single segment stays one parameter value.
		{http.MethodGet, "/users/a%2Fb", http.StatusOK, "user:a/b:/users/{id}"},
		{http.MethodGet, "/files/a%2Fb/c%20d.txt", http.StatusOK, "file-wild:a/b/c d.txt"},
		{http.MethodPost, "/v1/items:batchGet", http.StatusOK, "batch-get"},
		{http.MethodGet, "/hello%20world", http.StatusOK, "escaped-static"},
		// HEAD falls back to GET.
		{http.MethodHead, "/users/42", http.StatusOK, ""},
		{http.MethodHead, "/users/me", http.StatusOK, ""},
		// Unknown route -> 404.
		{http.MethodGet, "/nonexistent", http.StatusNotFound, "not found"},
	}

	for _, tc := range tests {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Errorf("%s %s: status = %d, want %d (body=%q)", tc.method, tc.target, rec.Code, tc.status, rec.Body.String())
			continue
		}
		if tc.method != http.MethodHead && rec.Body.String() != tc.body {
			t.Errorf("%s %s: body = %q, want %q", tc.method, tc.target, rec.Body.String(), tc.body)
		}
	}
}

func TestRouterManyParamsAndFanout(t *testing.T) {
	s := server.New()

	// > 8 parameters exercises overflow from inline paramBuf to paramExtra.
	s.GET("/{p1}/{p2}/{p3}/{p4}/{p5}/{p6}/{p7}/{p8}/{p9}/{p10}", func(c *server.Context) error {
		var parts []string
		for i := 1; i <= 10; i++ {
			parts = append(parts, c.Param(fmt.Sprintf("p%d", i)))
		}
		return c.Text(http.StatusOK, strings.Join(parts, ","))
	})

	// > 8 static children under the same parent exercises staticSlice -> staticMap promotion.
	for i := range 20 {
		name := fmt.Sprintf("child-%02d", i)
		s.GET("/fanout/"+name+"/{id}", func(c *server.Context) error {
			return c.Text(http.StatusOK, name+":"+c.Param("id"))
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/a/b/c/d/e/f/g/h/i/j", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "a,b,c,d,e,f,g,h,i,j" {
		t.Fatalf("10-param route: status=%d body=%q", rec.Code, rec.Body.String())
	}

	for i := range 20 {
		name := fmt.Sprintf("child-%02d", i)
		req := httptest.NewRequest(http.MethodGet, "/fanout/"+name+"/99", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		want := name + ":99"
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Fatalf("fanout %s: status=%d body=%q, want %q", name, rec.Code, rec.Body.String(), want)
		}
	}
}

func TestRouterMethodNotAllowedAndAllowHeader(t *testing.T) {
	s := server.New()
	s.GET("/items/{id}", func(c *server.Context) error { return c.NoContent() })
	s.DELETE("/items/{id}", func(c *server.Context) error { return c.NoContent() })
	s.Handle("PROPFIND", "/items/{id}", func(c *server.Context) error { return c.NoContent() })

	req := httptest.NewRequest(http.MethodPost, "/items/123", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	wantAllow := "DELETE, GET, HEAD, PROPFIND"
	if got := rec.Header().Get("Allow"); got != wantAllow {
		t.Fatalf("Allow = %q, want %q", got, wantAllow)
	}
}

func TestRouterTrailingSlashRedirect(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.RedirectTrailingSlash = true
	s := server.NewWithConfig(cfg)

	s.GET("/docs/", func(c *server.Context) error { return c.Text(http.StatusOK, "docs") })
	s.POST("/submit", func(c *server.Context) error { return c.Text(http.StatusOK, "submitted") })

	// GET /docs?a=1 -> 301 /docs/?a=1
	req := httptest.NewRequest(http.MethodGet, "/docs?a=1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/docs/?a=1" {
		t.Fatalf("GET /docs: status=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}

	// POST /submit/ -> 307 /submit
	req = httptest.NewRequest(http.MethodPost, "/submit/", nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "/submit" {
		t.Fatalf("POST /submit/: status=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestRouterHeadDynamicBeatsGetStatic(t *testing.T) {
	s := server.New()
	s.GET("/users/me", func(c *server.Context) error {
		c.SetHeader("X-Handler", "get-static")
		return c.NoContent()
	})
	s.HEAD("/users/{id}", func(c *server.Context) error {
		c.SetHeader("X-Handler", "head-param:"+c.Param("id"))
		return c.NoContent()
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/users/me", nil))
	if rec.Code != http.StatusNoContent || rec.Header().Get("X-Handler") != "head-param:me" {
		t.Fatalf("HEAD /users/me: status=%d X-Handler=%q, want head-param:me", rec.Code, rec.Header().Get("X-Handler"))
	}
}

func TestRouterRootCatchAll(t *testing.T) {
	s := server.New()
	s.GET("/{path...}", func(c *server.Context) error {
		return c.Text(http.StatusOK, "catch:"+c.Param("path"))
	})

	for _, tc := range []struct {
		target string
		want   string
	}{
		{"/", "catch:"},
		{"/a", "catch:a"},
		{"/a/b/c", "catch:a/b/c"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.target, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != tc.want {
			t.Errorf("%s: status=%d body=%q, want %q", tc.target, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

func TestRouterRegistrationPanics(t *testing.T) {
	badPatterns := []string{
		"",
		"users",
		"/users//id",
		"/users/../admin",
		"/users/./id",
		"/users/{}",
		"/users/{...}",
		"/users/{123}",
		"/users/{id}/posts/{id}",
		"/users/{a...}/more",
		"/users/{a...}/",
		"/users/pre{id}",
		"/users/{id}post",
		"/users/{a{b}}",
		"/users/\x00",
		"/users/%ZZ",
	}
	for _, pat := range badPatterns {
		t.Run("bad_"+pat, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected panic for invalid pattern %q", pat)
				}
			}()
			s := server.New()
			s.GET(pat, func(c *server.Context) error { return nil })
		})
	}

	// Duplicate/conflicting routes must panic.
	conflicts := [][2]string{
		{"/users", "/users"},
		{"/users/{id}", "/users/{userID}"},
		{"/files/{a...}", "/files/{b...}"},
		{"/users/", "/users/"},
	}
	for _, pair := range conflicts {
		t.Run("conflict_"+pair[0]+"_"+pair[1], func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected panic for conflicting routes %q and %q", pair[0], pair[1])
				}
			}()
			s := server.New()
			s.GET(pair[0], func(c *server.Context) error { return nil })
			s.GET(pair[1], func(c *server.Context) error { return nil })
		})
	}
}
