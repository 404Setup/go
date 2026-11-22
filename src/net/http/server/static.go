// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
)

// StaticConfig configures static file serving via [Server.StaticDir],
// [Server.StaticFS], [Group.StaticDir], and [Group.StaticFS].
type StaticConfig struct {
	// Browse enables HTML directory listings when a directory does not contain
	// an index.html file. Default is false (returns 404 for directories without
	// index.html).
	Browse bool

	// Index specifies the directory index filename. Default is "index.html".
	Index string

	// CacheControl optionally sets the Cache-Control header on served files.
	CacheControl string
}

// StaticDir serves files from dir under prefix, using [os.OpenRoot] to confine
// file access to dir and prevent path traversal and symlink escape attacks.
func (g *Group) StaticDir(prefix, dir string, cfg ...StaticConfig) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	registered := false
	defer func() {
		if !registered {
			_ = root.Close()
		}
	}()

	var c StaticConfig
	if len(cfg) > 0 {
		c = cfg[0]
	}
	g.StaticFS(prefix, root.FS(), c)

	g.server.mu.Lock()
	g.server.staticRoots = append(g.server.staticRoots, root)
	g.server.mu.Unlock()
	registered = true
	return nil
}

// StaticFS serves files from fsys under prefix.
func (g *Group) StaticFS(prefix string, fsys fs.FS, cfg ...StaticConfig) {
	if fsys == nil {
		panic("server: nil fs.FS")
	}
	var c StaticConfig
	if len(cfg) > 0 {
		c = cfg[0]
	}
	if c.Index == "" {
		c.Index = "index.html"
	}
	if strings.ContainsAny(c.Index, "/\\") || c.Index == "." || c.Index == ".." || hasASCIIControl(c.Index) {
		panic(fmt.Sprintf("server: invalid static index filename %q", c.Index))
	}

	fullPrefix := joinPattern(g.prefix, prefix)
	if strings.ContainsAny(fullPrefix, "{}") {
		panic(fmt.Sprintf("server: static prefix %q must not contain parameters", fullPrefix))
	}
	cleanPref := strings.TrimRight(fullPrefix, "/")

	effectiveFS := fsys
	if !c.Browse {
		effectiveFS = noBrowseFS{fs: fsys, index: c.Index}
	}
	fileServer := http.FileServerFS(effectiveFS)

	var ccSlice []string
	if c.CacheControl != "" {
		ccSlice = []string{c.CacheControl}[:1:1]
	}

	handler := func(ctx *Context) error {
		rel := ctx.Param("filepath")
		// Reject backslashes, NUL bytes, and dot-dot traversal segments defensively.
		if strings.ContainsAny(rel, "\\\x00") || containsDotDot(rel) {
			return NewError(http.StatusNotFound, "not found")
		}

		// Re-root request URL to the relative path for http.FileServerFS.
		r2 := new(http.Request)
		*r2 = *ctx.req
		u2 := new(url.URL)
		*u2 = *ctx.req.URL
		if rel == "" {
			u2.Path = "/"
		} else {
			u2.Path = "/" + rel
		}
		u2.RawPath = ""
		r2.URL = u2

		rw := ctx.ResponseWriter()
		if ccSlice != nil {
			rw = &cacheControlWriter{ResponseWriter: rw, value: ccSlice}
		}

		// When serving a custom index name (other than "index.html"), serve
		// the directory index file explicitly when present.
		if c.Index != "index.html" {
			name := strings.TrimPrefix(path.Clean(u2.Path), "/")
			if name == "" {
				name = "."
			}
			if info, err := fs.Stat(effectiveFS, name); err == nil && info.IsDir() {
				if !strings.HasSuffix(u2.Path, "/") {
					g.server.safeRedirect(ctx.ResponseWriter(), ctx.req, ctx.req.URL.Path+"/")
					return nil
				}
				idxPath := c.Index
				if name != "." {
					idxPath = name + "/" + c.Index
				}
				if idxInfo, err := fs.Stat(effectiveFS, idxPath); err == nil && !idxInfo.IsDir() {
					http.ServeFileFS(rw, r2, effectiveFS, idxPath)
					return nil
				}
				if !c.Browse {
					return NewError(http.StatusNotFound, "not found")
				}
			}
		}

		fileServer.ServeHTTP(rw, r2)
		return nil
	}

	if cleanPref == "" {
		g.GET("/{filepath...}", handler)
		return
	}
	relBase := strings.TrimPrefix(cleanPref, g.prefix)
	g.GET(relBase, func(ctx *Context) error {
		// Serve index file if present as a non-directory, redirect when Browse
		// is enabled, or return 404.
		if idxInfo, err := fs.Stat(effectiveFS, c.Index); err == nil && !idxInfo.IsDir() {
			rw := ctx.ResponseWriter()
			if ccSlice != nil {
				rw = &cacheControlWriter{ResponseWriter: rw, value: ccSlice}
			}
			http.ServeFileFS(rw, ctx.req, effectiveFS, c.Index)
			return nil
		}
		if c.Browse {
			g.server.safeRedirect(ctx.ResponseWriter(), ctx.req, cleanPref+"/")
			return nil
		}
		return NewError(http.StatusNotFound, "not found")
	})
	g.GET(relBase+"/{filepath...}", handler)
}

func containsDotDot(v string) bool {
	for v != "" {
		var seg string
		seg, v, _ = strings.Cut(v, "/")
		if seg == ".." {
			return true
		}
	}
	return false
}

type cacheControlWriter struct {
	http.ResponseWriter
	value   []string
	written bool
}

func (w *cacheControlWriter) WriteHeader(code int) {
	if code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if !w.written {
		w.written = true
		if (code >= 200 && code < 300) || code == http.StatusNotModified {
			h := w.Header()
			if len(h["Cache-Control"]) == 0 {
				h["Cache-Control"] = w.value
			}
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *cacheControlWriter) Write(p []byte) (int, error) {
	if !w.written {
		w.written = true
		h := w.Header()
		if len(h["Cache-Control"]) == 0 {
			h["Cache-Control"] = w.value
		}
	}
	return w.ResponseWriter.Write(p)
}

func (w *cacheControlWriter) WriteString(s string) (int, error) {
	if !w.written {
		w.written = true
		h := w.Header()
		if len(h["Cache-Control"]) == 0 {
			h["Cache-Control"] = w.value
		}
	}
	return io.WriteString(w.ResponseWriter, s)
}

func (w *cacheControlWriter) ReadFrom(r io.Reader) (int64, error) {
	if !w.written {
		w.written = true
		h := w.Header()
		if len(h["Cache-Control"]) == 0 {
			h["Cache-Control"] = w.value
		}
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(writerOnly{w.ResponseWriter}, r)
}

func (w *cacheControlWriter) FlushError() error {
	if !w.written {
		w.written = true
		h := w.Header()
		if len(h["Cache-Control"]) == 0 {
			h["Cache-Control"] = w.value
		}
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *cacheControlWriter) Flush() {
	_ = w.FlushError()
}

func (w *cacheControlWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// noBrowseFS wraps an fs.FS and hides directories that do not contain a
// non-directory index file, preventing directory listing via http.FileServerFS.
type noBrowseFS struct {
	fs    fs.FS
	index string
}

func (n noBrowseFS) Open(name string) (fs.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.IsDir() {
		idx := n.index
		if name != "." && name != "" {
			idx = path.Join(name, n.index)
		}
		idxFile, err := n.fs.Open(idx)
		if err != nil {
			f.Close()
			return nil, fs.ErrNotExist
		}
		idxInfo, statErr := idxFile.Stat()
		idxFile.Close()
		if statErr != nil || idxInfo.IsDir() {
			f.Close()
			return nil, fs.ErrNotExist
		}
	}
	return f, nil
}
