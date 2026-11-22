// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server_test

import (
	"net/http"
	"net/http/httptest"
	"net/http/server"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestStaticDirConfinementAndNoBrowse(t *testing.T) {
	outer := t.TempDir()
	pubDir := filepath.Join(outer, "public")
	if err := os.MkdirAll(filepath.Join(pubDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(outer, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("TOP_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pubDir, "hello.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pubDir, "sub", "item.txt"), []byte("sub item"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a symlink inside public pointing outside the root directory.
	if err := os.Symlink(secretFile, filepath.Join(pubDir, "escape.txt")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	s := server.New()
	if err := s.StaticDir("/assets", pubDir, server.StaticConfig{
		CacheControl: "public, max-age=3600",
	}); err != nil {
		t.Fatalf("StaticDir: %v", err)
	}
	defer s.Close()

	// Normal file works with Cache-Control.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/hello.txt", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "hello world" {
		t.Fatalf("hello.txt: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Fatalf("Cache-Control = %q", got)
	}

	// Missing file returns 404 and must not carry Cache-Control.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/does-not-exist.txt", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Cache-Control") != "" {
		t.Fatalf("missing file: status=%d Cache-Control=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}

	// Subdirectory file works.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/sub/item.txt", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "sub item" {
		t.Fatalf("sub/item.txt: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Directory listing is disabled by default -> 404.
	for _, dirPath := range []string{"/assets", "/assets/", "/assets/sub/"} {
		rec = httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, dirPath, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("directory %q should return 404 when Browse=false, got %d (body=%q)", dirPath, rec.Code, rec.Body.String())
		}
	}

	// Symlink escape outside root MUST be blocked by os.OpenRoot!
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/escape.txt", nil))
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "TOP_SECRET") {
		t.Fatalf("symlink escape succeeded! status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Dot-dot traversal attempts must never read secret.txt.
	for _, p := range []string{"/assets/../secret.txt", "/assets/%2e%2e/secret.txt", "/assets/sub/../../secret.txt"} {
		rec = httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "TOP_SECRET") {
			t.Fatalf("traversal %q succeeded! status=%d body=%q", p, rec.Code, rec.Body.String())
		}
	}
}

func TestStaticFSIndexAndBrowse(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":     {Data: []byte("<h1>home</h1>")},
		"docs/a.txt":     {Data: []byte("doc-a")},
		"app/start.html": {Data: []byte("<h1>custom-index</h1>")},
	}

	// Default Browse=false serves index.html, blocks listing of docs/.
	s := server.New()
	s.StaticFS("/site", fsys)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/site/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "<h1>home</h1>" {
		t.Fatalf("/site/: status=%d body=%q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/site", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "<h1>home</h1>" {
		t.Fatalf("/site: status=%d body=%q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/site/docs/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/site/docs/ with Browse=false: status=%d, want 404", rec.Code)
	}

	// Opt-in Browse=true allows directory listing.
	s2 := server.New()
	s2.StaticFS("/browse", fsys, server.StaticConfig{Browse: true})
	rec = httptest.NewRecorder()
	s2.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/browse/docs/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "a.txt") {
		t.Fatalf("/browse/docs/ with Browse=true: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Custom index filename works.
	s3 := server.New()
	s3.StaticFS("/custom", fsys, server.StaticConfig{Index: "start.html"})
	rec = httptest.NewRecorder()
	s3.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/custom/app/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "<h1>custom-index</h1>" {
		t.Fatalf("/custom/app/: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// TestStaticNoBrowseIndexDirectoryBypass verifies that a subdirectory named
// "index.html" (or matching StaticConfig.Index) cannot bypass Browse: false
// and expose a directory listing.
func TestStaticNoBrowseIndexDirectoryBypass(t *testing.T) {
	dir := t.TempDir()
	// Create a directory named "index.html" and a secret sibling file.
	if err := os.MkdirAll(filepath.Join(dir, "sub", "index.html"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "index.html"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "private-listing-target.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := server.New()
	if err := s.StaticDir("/files", dir); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, target := range []string{"/files", "/files/", "/files/sub/", "/files/sub/index.html/"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "private-listing-target.txt") {
			t.Fatalf("Browse=false bypassed for %q: status=%d body=%q", target, rec.Code, rec.Body.String())
		}
	}

	// StaticDir with invalid prefix panics without leaking the opened os.Root.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic for parameterized static prefix")
			}
		}()
		_ = s.StaticDir("/bad/{id}", dir)
	}()
}
