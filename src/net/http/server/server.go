// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultIdleTimeout       = 120 * time.Second
	defaultShutdownTimeout   = 15 * time.Second
	defaultMaxHeaderBytes    = 1 << 20 // 1 MiB
	defaultMaxBodyBytes      = 4 << 20 // 4 MiB
)

// HandlerFunc handles an HTTP request within a [*Context].
// Any non-nil error returned is handled by the server's [ErrorHandler].
type HandlerFunc func(*Context) error

// Config configures a [*Server].
//
// Zero values for security-sensitive fields (ReadHeaderTimeout, IdleTimeout,
// ShutdownTimeout, MaxHeaderBytes, MaxBodyBytes) use safe defaults from
// [DefaultConfig]. Set ReadHeaderTimeout, IdleTimeout, or MaxBodyBytes to a
// negative value to explicitly disable that limit.
type Config struct {
	// ReadHeaderTimeout is the amount of time allowed to read request headers.
	// Default is 10s. Set to < 0 to disable.
	ReadHeaderTimeout time.Duration

	// ReadTimeout is the maximum duration for reading the entire request,
	// including the body. Default is 0 (no timeout).
	ReadTimeout time.Duration

	// WriteTimeout is the maximum duration before timing out writes of the
	// response. Default is 0 (no timeout).
	WriteTimeout time.Duration

	// IdleTimeout is the maximum amount of time to wait for the next request
	// when keep-alives are enabled. Default is 120s. Set to < 0 to disable.
	IdleTimeout time.Duration

	// ShutdownTimeout is the graceful shutdown timeout used by
	// [Server.ListenAndServeContext] and [Server.ServeContext]. Default is 15s.
	ShutdownTimeout time.Duration

	// MaxHeaderBytes controls the maximum number of bytes the server will read
	// parsing request headers. Default is 1 MiB.
	MaxHeaderBytes int

	// MaxBodyBytes is the default maximum request body size enforced by
	// [Context.BindJSON], [Context.BindJSONStrict], [Context.ParseForm], and
	// [Context.BodyBytes]. Default is 4 MiB. Set to < 0 to disable.
	MaxBodyBytes int64

	// TrustedProxies lists IP prefixes whose X-Forwarded-For and X-Real-IP
	// headers are trusted by [Context.ClientIP]. When empty (the default),
	// forwarding headers are ignored and only Request.RemoteAddr is used.
	TrustedProxies []netip.Prefix

	// ErrorHandler handles non-nil errors returned by handlers and middleware.
	// Default is [DefaultErrorHandler].
	ErrorHandler ErrorHandler

	// Logger is an optional structured logger used for internal server errors.
	Logger *slog.Logger

	// TLSConfig optionally provides a TLS configuration for ServeTLS and
	// ListenAndServeTLS.
	TLSConfig *tls.Config

	// BaseContext optionally specifies a function that returns the base context
	// for incoming requests on the underlying [http.Server].
	BaseContext func(net.Listener) context.Context

	// RedirectTrailingSlash redirects requests whose path differs from a
	// registered route only by a trailing slash (301 for GET/HEAD, 307 for
	// other methods). Default is false.
	RedirectTrailingSlash bool

	// DisablePathCleaning disables automatic canonicalization of paths
	// containing "..", ".", or repeated slashes. Default is false.
	DisablePathCleaning bool
}

// DefaultConfig returns a [Config] populated with secure defaults.
func DefaultConfig() Config {
	return Config{
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		IdleTimeout:       defaultIdleTimeout,
		ShutdownTimeout:   defaultShutdownTimeout,
		MaxHeaderBytes:    defaultMaxHeaderBytes,
		MaxBodyBytes:      defaultMaxBodyBytes,
		ErrorHandler:      DefaultErrorHandler,
	}
}

// Server is an HTTP router, middleware runner, and lifecycle wrapper around
// [http.Server]. It implements [http.Handler].
type Server struct {
	cfg            Config
	maxBodyBytes   int64
	errorHandler   ErrorHandler
	logger         *slog.Logger
	trustedProxies []netip.Prefix

	mu             sync.Mutex
	root           *Group
	notFound       HandlerFunc
	methodNotAllow HandlerFunc
	snap           atomic.Pointer[routerSnapshot]
	httpSrv        *http.Server
	staticRoots    []*staticRootCloser
}

type staticRootCloser interface {
	Close() error
}

// New creates a new [*Server] with [DefaultConfig].
func New() *Server {
	return NewWithConfig(DefaultConfig())
}

// NewWithConfig creates a new [*Server] using cfg, filling zero values with
// secure defaults.
func NewWithConfig(cfg Config) *Server {
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = defaultReadHeaderTimeout
	} else if cfg.ReadHeaderTimeout < 0 {
		cfg.ReadHeaderTimeout = 0
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	} else if cfg.IdleTimeout < 0 {
		cfg.IdleTimeout = 0
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}
	if cfg.MaxHeaderBytes <= 0 {
		cfg.MaxHeaderBytes = defaultMaxHeaderBytes
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.ErrorHandler == nil {
		cfg.ErrorHandler = DefaultErrorHandler
	}

	var proxies []netip.Prefix
	if len(cfg.TrustedProxies) > 0 {
		proxies = make([]netip.Prefix, len(cfg.TrustedProxies))
		for i, p := range cfg.TrustedProxies {
			proxies[i] = netip.PrefixFrom(p.Addr().Unmap(), p.Bits())
		}
	}

	s := &Server{
		cfg:            cfg,
		maxBodyBytes:   cfg.MaxBodyBytes,
		errorHandler:   cfg.ErrorHandler,
		logger:         cfg.Logger,
		trustedProxies: proxies,
		notFound:       defaultNotFoundHandler,
		methodNotAllow: defaultMethodNotAllowedHandler,
	}
	s.root = &Group{server: s}

	initial := &routerSnapshot{
		notFoundChain:    []HandlerFunc{s.notFound},
		methodNotAllowed: []HandlerFunc{s.methodNotAllow},
	}
	s.snap.Store(initial)
	return s
}

func defaultNotFoundHandler(c *Context) error {
	return NewError(http.StatusNotFound, "not found")
}

func defaultMethodNotAllowedHandler(c *Context) error {
	return NewError(http.StatusMethodNotAllowed, "method not allowed")
}

func (s *Server) isTrustedProxy(ip netip.Addr) bool {
	for _, p := range s.trustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Use appends middleware to the server's root middleware chain.
// Root middleware runs on subsequently registered routes, subsequently created
// groups, and 404/405 responses.
func (s *Server) Use(middlewares ...HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, mw := range middlewares {
		if mw == nil {
			panic("server: nil middleware")
		}
		s.root.middlewares = append(s.root.middlewares, mw)
	}
	s.rebuildFallbackChainsLocked()
}

// NotFound sets a custom handler for unmatched routes (404).
func (s *Server) NotFound(h HandlerFunc) {
	if h == nil {
		panic("server: nil NotFound handler")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notFound = h
	s.rebuildFallbackChainsLocked()
}

// MethodNotAllowed sets a custom handler for matched paths with disallowed
// HTTP methods (405). The Allow response header is populated before h runs.
func (s *Server) MethodNotAllowed(h HandlerFunc) {
	if h == nil {
		panic("server: nil MethodNotAllowed handler")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methodNotAllow = h
	s.rebuildFallbackChainsLocked()
}

func (s *Server) rebuildFallbackChainsLocked() {
	next := s.snap.Load().clone()
	next.notFoundChain = buildChain(s.root.middlewares, []HandlerFunc{s.notFound})
	next.methodNotAllowed = buildChain(s.root.middlewares, []HandlerFunc{s.methodNotAllow})
	s.snap.Store(next)
}

func buildChain(prefix []HandlerFunc, suffix []HandlerFunc) []HandlerFunc {
	chain := make([]HandlerFunc, 0, len(prefix)+len(suffix))
	chain = append(chain, prefix...)
	chain = append(chain, suffix...)
	return chain
}

// Group creates a route group sharing a URL path prefix and middleware.
func (s *Server) Group(prefix string, middlewares ...HandlerFunc) *Group {
	return s.root.Group(prefix, middlewares...)
}

// Handle registers handlers for method and pattern.
// If method is "", the route matches any HTTP method as a fallback after
// method-specific routes.
func (s *Server) Handle(method, pattern string, handlers ...HandlerFunc) {
	s.root.Handle(method, pattern, handlers...)
}

// GET registers handlers for HTTP GET requests matching pattern.
func (s *Server) GET(pattern string, handlers ...HandlerFunc) {
	s.root.GET(pattern, handlers...)
}

// HEAD registers handlers for HTTP HEAD requests matching pattern.
func (s *Server) HEAD(pattern string, handlers ...HandlerFunc) {
	s.root.HEAD(pattern, handlers...)
}

// POST registers handlers for HTTP POST requests matching pattern.
func (s *Server) POST(pattern string, handlers ...HandlerFunc) {
	s.root.POST(pattern, handlers...)
}

// PUT registers handlers for HTTP PUT requests matching pattern.
func (s *Server) PUT(pattern string, handlers ...HandlerFunc) {
	s.root.PUT(pattern, handlers...)
}

// PATCH registers handlers for HTTP PATCH requests matching pattern.
func (s *Server) PATCH(pattern string, handlers ...HandlerFunc) {
	s.root.PATCH(pattern, handlers...)
}

// DELETE registers handlers for HTTP DELETE requests matching pattern.
func (s *Server) DELETE(pattern string, handlers ...HandlerFunc) {
	s.root.DELETE(pattern, handlers...)
}

// OPTIONS registers handlers for HTTP OPTIONS requests matching pattern.
func (s *Server) OPTIONS(pattern string, handlers ...HandlerFunc) {
	s.root.OPTIONS(pattern, handlers...)
}

// Any registers handlers for all standard HTTP methods (GET, HEAD, POST, PUT,
// PATCH, DELETE, OPTIONS).
func (s *Server) Any(pattern string, handlers ...HandlerFunc) {
	s.root.Any(pattern, handlers...)
}

// HandleHTTP registers a standard [http.Handler] for method and pattern.
// Matched path parameters are copied onto Request via [http.Request.SetPathValue].
func (s *Server) HandleHTTP(method, pattern string, h http.Handler) {
	s.root.HandleHTTP(method, pattern, h)
}

// Mount attaches a standard [http.Handler] at prefix, stripping prefix from
// the request URL path before forwarding.
func (s *Server) Mount(prefix string, h http.Handler) {
	s.root.Mount(prefix, h)
}

// StaticDir serves files from dir under prefix, confined using [os.OpenRoot]
// to prevent symlink and path traversal escapes. Directory listing is disabled
// by default.
func (s *Server) StaticDir(prefix, dir string, cfg ...StaticConfig) error {
	return s.root.StaticDir(prefix, dir, cfg...)
}

// StaticFS serves files from fsys under prefix. Directory listing is disabled
// by default.
func (s *Server) StaticFS(prefix string, fsys fs.FS, cfg ...StaticConfig) {
	s.root.StaticFS(prefix, fsys, cfg...)
}

// Routes returns a copy of all registered routes.
func (s *Server) Routes() []RouteInfo {
	return slices.Clone(s.snap.Load().routes)
}

// Group represents a collection of routes sharing a path prefix and middleware.
type Group struct {
	server      *Server
	prefix      string
	middlewares []HandlerFunc
}

// Use appends middleware to the group for subsequently registered routes and
// child groups.
func (g *Group) Use(middlewares ...HandlerFunc) {
	g.server.mu.Lock()
	defer g.server.mu.Unlock()
	for _, mw := range middlewares {
		if mw == nil {
			panic("server: nil middleware")
		}
		g.middlewares = append(g.middlewares, mw)
	}
	if g == g.server.root {
		g.server.rebuildFallbackChainsLocked()
	}
}

// Group creates a nested subgroup inheriting this group's prefix and current
// middleware slice.
func (g *Group) Group(prefix string, middlewares ...HandlerFunc) *Group {
	g.server.mu.Lock()
	defer g.server.mu.Unlock()
	if prefix != "" && prefix[0] != '/' {
		panic(fmt.Sprintf("server: group prefix %q must start with '/'", prefix))
	}
	for _, mw := range middlewares {
		if mw == nil {
			panic("server: nil middleware")
		}
	}
	fullPrefix := joinPrefix(g.prefix, prefix)
	mws := make([]HandlerFunc, 0, len(g.middlewares)+len(middlewares))
	mws = append(mws, g.middlewares...)
	mws = append(mws, middlewares...)
	return &Group{
		server:      g.server,
		prefix:      fullPrefix,
		middlewares: mws,
	}
}

// Handle registers handlers on the group for method and pattern.
func (g *Group) Handle(method, pattern string, handlers ...HandlerFunc) {
	if method != "" && !isValidHTTPMethod(method) {
		panic(fmt.Sprintf("server: invalid HTTP method %q", method))
	}
	if len(handlers) == 0 {
		panic("server: at least one handler is required")
	}
	for _, h := range handlers {
		if h == nil {
			panic("server: nil handler")
		}
	}

	fullPattern := joinPattern(g.prefix, pattern)
	parsed, err := parsePattern(fullPattern)
	if err != nil {
		panic("server: " + err.Error())
	}

	g.server.mu.Lock()
	defer g.server.mu.Unlock()

	chain := buildChain(g.middlewares, handlers)
	entry := &routeEntry{
		method:     method,
		pattern:    fullPattern,
		paramNames: parsed.paramNames,
		chain:      chain,
	}

	next := g.server.snap.Load().clone()
	mr := next.getOrCreateMethodRouter(method)
	if err := mr.add(parsed, entry); err != nil {
		panic("server: " + err.Error())
	}
	displayMethod := method
	if displayMethod == "" {
		displayMethod = "*"
	}
	next.routes = append(next.routes, RouteInfo{
		Method:  displayMethod,
		Pattern: fullPattern,
	})
	g.server.snap.Store(next)
}

// GET registers handlers for HTTP GET requests matching pattern.
func (g *Group) GET(pattern string, handlers ...HandlerFunc) {
	g.Handle(http.MethodGet, pattern, handlers...)
}

// HEAD registers handlers for HTTP HEAD requests matching pattern.
func (g *Group) HEAD(pattern string, handlers ...HandlerFunc) {
	g.Handle(http.MethodHead, pattern, handlers...)
}

// POST registers handlers for HTTP POST requests matching pattern.
func (g *Group) POST(pattern string, handlers ...HandlerFunc) {
	g.Handle(http.MethodPost, pattern, handlers...)
}

// PUT registers handlers for HTTP PUT requests matching pattern.
func (g *Group) PUT(pattern string, handlers ...HandlerFunc) {
	g.Handle(http.MethodPut, pattern, handlers...)
}

// PATCH registers handlers for HTTP PATCH requests matching pattern.
func (g *Group) PATCH(pattern string, handlers ...HandlerFunc) {
	g.Handle(http.MethodPatch, pattern, handlers...)
}

// DELETE registers handlers for HTTP DELETE requests matching pattern.
func (g *Group) DELETE(pattern string, handlers ...HandlerFunc) {
	g.Handle(http.MethodDelete, pattern, handlers...)
}

// OPTIONS registers handlers for HTTP OPTIONS requests matching pattern.
func (g *Group) OPTIONS(pattern string, handlers ...HandlerFunc) {
	g.Handle(http.MethodOptions, pattern, handlers...)
}

var anyMethods = []string{
	http.MethodGet,
	http.MethodHead,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
	http.MethodOptions,
}

// Any registers handlers for GET, HEAD, POST, PUT, PATCH, DELETE, and OPTIONS.
func (g *Group) Any(pattern string, handlers ...HandlerFunc) {
	for _, m := range anyMethods {
		g.Handle(m, pattern, handlers...)
	}
}

// HandleHTTP registers a standard [http.Handler] for method and pattern.
func (g *Group) HandleHTTP(method, pattern string, h http.Handler) {
	if h == nil {
		panic("server: nil http.Handler")
	}
	g.Handle(method, pattern, func(c *Context) error {
		for i, name := range c.paramNames {
			var val string
			if len(c.paramExtra) > 0 {
				if i < len(c.paramExtra) {
					val = c.paramExtra[i]
				}
			} else if i < c.nParams {
				val = c.paramBuf[i]
			}
			c.req.SetPathValue(name, val)
		}
		h.ServeHTTP(c.ResponseWriter(), c.req)
		return nil
	})
}

// Mount attaches a standard [http.Handler] at prefix, stripping prefix from
// the request URL path before forwarding.
func (g *Group) Mount(prefix string, h http.Handler) {
	if h == nil {
		panic("server: nil http.Handler")
	}
	fullPrefix := joinPattern(g.prefix, prefix)
	if strings.ContainsAny(fullPrefix, "{}") {
		panic(fmt.Sprintf("server: mount prefix %q must not contain parameters", fullPrefix))
	}
	cleanPref := strings.TrimRight(fullPrefix, "/")

	handler := func(c *Context) error {
		r2 := new(http.Request)
		*r2 = *c.req
		u2 := new(url.URL)
		*u2 = *c.req.URL
		if cleanPref != "" {
			p := strings.TrimPrefix(u2.Path, cleanPref)
			if p == "" {
				p = "/"
			}
			u2.Path = p
			if u2.RawPath != "" {
				rp := strings.TrimPrefix(u2.RawPath, cleanPref)
				if rp == "" {
					rp = "/"
				}
				u2.RawPath = rp
			}
		}
		r2.URL = u2
		h.ServeHTTP(c.ResponseWriter(), r2)
		return nil
	}

	if cleanPref == "" {
		g.Handle("", "/{rest...}", handler)
		return
	}
	// Strip g.prefix because g.Handle will re-prepend g.prefix.
	relBase := strings.TrimPrefix(cleanPref, g.prefix)
	g.Handle("", relBase, handler)
	g.Handle("", relBase+"/{rest...}", handler)
}

func joinPrefix(parent, child string) string {
	parent = strings.TrimRight(parent, "/")
	child = strings.TrimRight(child, "/")
	if child == "" {
		return parent
	}
	return parent + child
}

func joinPattern(prefix, pattern string) string {
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return pattern
	}
	if pattern == "" {
		return prefix
	}
	if pattern[0] != '/' {
		return prefix + "/" + pattern
	}
	return prefix + pattern
}

// ServeHTTP implements [http.Handler].
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqPath := r.URL.Path
	rawPath := r.URL.RawPath
	snap := s.snap.Load()

	// Fast path for exact static routes: every key in staticTable was validated
	// by parsePattern to be clean and NUL-free, so an exact match with empty
	// RawPath guarantees reqPath needs neither NUL scanning nor cleaning.
	if rawPath == "" {
		if entry := snap.matchStatic(r.Method, reqPath); entry != nil {
			c := acquireContext(s, w, r)
			c.routePattern = entry.pattern
			var err error
			if len(entry.chain) == 1 {
				err = entry.chain[0](c)
			} else {
				c.handlers = entry.chain
				err = c.Next()
			}
			if err != nil {
				c.HandleError(err)
			}
			releaseContext(c)
			return
		}
	}

	// Single-pass NUL + canonical path check on reqPath.
	clean, hasNul := checkPath(reqPath)
	if hasNul {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if !s.cfg.DisablePathCleaning && r.Method != http.MethodConnect && !clean {
		s.safeRedirect(w, r, cleanPath(reqPath))
		return
	}

	matchPath := reqPath
	escaped := false
	if rawPath != "" {
		rawClean, rawNul := checkPath(rawPath)
		if rawNul {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		if !s.cfg.DisablePathCleaning && r.Method != http.MethodConnect && !rawClean {
			s.safeRedirect(w, r, cleanPath(reqPath))
			return
		}
		matchPath = rawPath
		escaped = true
	}

	c := acquireContext(s, w, r)
	pc := paramCapture{
		buf:   &c.paramBuf,
		extra: &c.paramExtra,
	}

	var entry *routeEntry
	if !escaped {
		entry = snap.matchAfterStatic(r.Method, matchPath, &pc)
	} else {
		entry = snap.match(r.Method, matchPath, true, &pc)
	}
	if entry != nil {
		c.nParams = pc.n
		c.paramNames = entry.paramNames
		c.routePattern = entry.pattern
		var err error
		if len(entry.chain) == 1 {
			err = entry.chain[0](c)
		} else {
			c.handlers = entry.chain
			err = c.Next()
		}
		if err != nil {
			c.HandleError(err)
		}
		releaseContext(c)
		return
	}

	// Optional trailing-slash redirect (safe against open redirects).
	if s.cfg.RedirectTrailingSlash && matchPath != "/" && r.Method != http.MethodConnect {
		var alt string
		if matchPath[len(matchPath)-1] == '/' {
			alt = matchPath[:len(matchPath)-1]
		} else {
			alt = matchPath + "/"
		}
		if snap.match(r.Method, alt, escaped, nil) != nil {
			releaseContext(c)
			target := reqPath
			if target[len(target)-1] == '/' {
				target = target[:len(target)-1]
			} else {
				target = target + "/"
			}
			s.safeRedirect(w, r, target)
			return
		}
	}

	// Check if other HTTP methods match this path (405 Method Not Allowed).
	var allowBuf [numStdMethods]string
	allowed := snap.allowedMethods(matchPath, escaped, allowBuf[:0])
	if len(allowed) > 0 {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		c.handlers = snap.methodNotAllowed
		if err := c.Next(); err != nil {
			c.HandleError(err)
		}
		releaseContext(c)
		return
	}

	// 404 Not Found.
	c.handlers = snap.notFoundChain
	if err := c.Next(); err != nil {
		c.HandleError(err)
	}
	releaseContext(c)
}

func (s *Server) safeRedirect(w http.ResponseWriter, r *http.Request, targetPath string) {
	// Ensure redirect target is strictly a relative path with a single leading
	// slash and no backslash or control character (prevents open redirects like
	// "//evil.com" or "/\evil.com").
	if len(targetPath) == 0 || targetPath[0] != '/' {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if len(targetPath) >= 2 && (targetPath[1] == '/' || targetPath[1] == '\\') {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	for i := 0; i < len(targetPath); i++ {
		if targetPath[i] < 0x20 || targetPath[i] == 0x7f {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
	}
	target := targetPath
	if r.URL.RawQuery != "" {
		target = targetPath + "?" + r.URL.RawQuery
	}
	code := http.StatusTemporaryRedirect
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		code = http.StatusMovedPermanently
	}
	http.Redirect(w, r, target, code)
}

// checkPath scans p in a single pass and reports whether p is canonical
// (starts with '/' with no "//", "/./", "/../", trailing "/.", or trailing
// "/..") and whether p contains a NUL byte.
func checkPath(p string) (clean bool, hasNul bool) {
	n := len(p)
	if n == 0 || p[0] != '/' {
		return false, strings.IndexByte(p, 0) >= 0
	}
	clean = true
	for i := 0; i < n; i++ {
		b := p[i]
		if b == 0 {
			return false, true
		}
		if b != '/' {
			continue
		}
		if i+1 < n {
			next := p[i+1]
			if next == '/' {
				clean = false
			} else if next == '.' {
				if i+2 == n || p[i+2] == '/' {
					clean = false
				} else if p[i+2] == '.' && (i+3 == n || p[i+3] == '/') {
					clean = false
				}
			}
		}
	}
	return clean, false
}

// isCleanPath reports whether p is a canonical URL path starting with '/' with
// no "//", "/./", "/../", trailing "/.", or trailing "/..".
func isCleanPath(p string) bool {
	clean, hasNul := checkPath(p)
	return clean && !hasNul
}

func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	// path.Clean removes trailing slash except for root; preserve it if present.
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	// Guarantee a single leading slash.
	for len(np) > 1 && np[0] == '/' && np[1] == '/' {
		np = np[1:]
	}
	return np
}

// HTTPServer returns the underlying [*http.Server], creating and configuring
// it with the server's [Config] on first call.
func (s *Server) HTTPServer() *http.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.httpServerLocked()
}

func (s *Server) httpServerLocked() *http.Server {
	if s.httpSrv == nil {
		s.httpSrv = &http.Server{
			Handler:           s,
			ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
			ReadTimeout:       s.cfg.ReadTimeout,
			WriteTimeout:      s.cfg.WriteTimeout,
			IdleTimeout:       s.cfg.IdleTimeout,
			MaxHeaderBytes:    s.cfg.MaxHeaderBytes,
			TLSConfig:         s.cfg.TLSConfig,
			BaseContext:       s.cfg.BaseContext,
		}
	}
	return s.httpSrv
}

// ListenAndServe listens on addr and serves HTTP requests.
func (s *Server) ListenAndServe(addr string) error {
	srv := s.HTTPServer()
	srv.Addr = addr
	return srv.ListenAndServe()
}

// ListenAndServeTLS listens on addr and serves HTTPS requests.
func (s *Server) ListenAndServeTLS(addr, certFile, keyFile string) error {
	srv := s.HTTPServer()
	srv.Addr = addr
	return srv.ListenAndServeTLS(certFile, keyFile)
}

// Serve accepts incoming connections on ln and serves HTTP requests.
func (s *Server) Serve(ln net.Listener) error {
	return s.HTTPServer().Serve(ln)
}

// ServeTLS accepts incoming connections on ln and serves HTTPS requests.
func (s *Server) ServeTLS(ln net.Listener, certFile, keyFile string) error {
	return s.HTTPServer().ServeTLS(ln, certFile, keyFile)
}

// ListenAndServeContext listens on addr and serves HTTP requests until ctx is
// canceled, then performs a graceful shutdown bounded by Config.ShutdownTimeout.
// Returns nil when shutdown completes cleanly after ctx cancellation.
func (s *Server) ListenAndServeContext(ctx context.Context, addr string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return s.ServeContext(ctx, ln)
}

// ServeContext serves requests on ln until ctx is canceled, then performs a
// graceful shutdown bounded by Config.ShutdownTimeout. Returns nil when
// shutdown completes cleanly after ctx cancellation.
func (s *Server) ServeContext(ctx context.Context, ln net.Listener) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		shutdownErr := s.Shutdown(shutdownCtx)
		serveErr := <-errCh
		if shutdownErr != nil {
			return shutdownErr
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
		return nil
	}
}

// Shutdown gracefully shuts down the underlying [http.Server] and closes any
// filesystem roots opened by [Server.StaticDir].
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.httpSrv
	roots := s.staticRoots
	s.staticRoots = nil
	s.mu.Unlock()

	var errs []error
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	for _, rc := range roots {
		if err := (*rc).Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close immediately closes the underlying [http.Server] and any filesystem
// roots opened by [Server.StaticDir].
func (s *Server) Close() error {
	s.mu.Lock()
	srv := s.httpSrv
	roots := s.staticRoots
	s.staticRoots = nil
	s.mu.Unlock()

	var errs []error
	if srv != nil {
		if err := srv.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, rc := range roots {
		if err := (*rc).Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
