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

	// Compression configures automatic request decompression and response
	// compression. Zero values enable gzip and deflate codecs with safe defaults.
	Compression CompressionConfig
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
	compSnap       atomic.Pointer[compressionSnapshot]
	httpSrv        *http.Server
	staticRoots    []staticRootCloser
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
		proxies = make([]netip.Prefix, 0, len(cfg.TrustedProxies))
		for _, p := range cfg.TrustedProxies {
			if !p.IsValid() {
				continue
			}
			addr := p.Addr()
			bits := p.Bits()
			if addr.Is4In6() {
				if bits < 96 {
					continue
				}
				addr = addr.Unmap()
				bits -= 96
			}
			proxies = append(proxies, netip.PrefixFrom(addr, bits))
		}
	}

	compSnap := newCompressionSnapshot(cfg.Compression)

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
	s.compSnap.Store(compSnap)
	return s
}

// SetCompressor registers or replaces the response compressor for enc
// ([EncodingGzip], [EncodingDeflate], [EncodingZstd], or [EncodingBrotli]).
// Passing a nil Compressor disables response compression for enc.
// It panics if enc is not one of the supported content codings.
func (s *Server) SetCompressor(enc Encoding, c Compressor) {
	idx, _, ok := encodingIndex(enc)
	if !ok {
		panic(fmt.Sprintf("server: unsupported compression encoding %q", enc))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.compSnap.Load().clone()
	next.compressors[idx] = newCodecHolder(c)
	next.recomputeFlags()
	s.compSnap.Store(next)
}

// SetDecompressor registers or replaces the request decompressor for enc
// ([EncodingGzip], [EncodingDeflate], [EncodingZstd], or [EncodingBrotli]).
// Passing a nil Decompressor disables automatic request decompression for enc.
// It panics if enc is not one of the supported content codings.
func (s *Server) SetDecompressor(enc Encoding, d Decompressor) {
	idx, _, ok := encodingIndex(enc)
	if !ok {
		panic(fmt.Sprintf("server: unsupported decompression encoding %q", enc))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.compSnap.Load().clone()
	next.decompressors[idx] = newDecompressorHolder(d)
	next.recomputeFlags()
	s.compSnap.Store(next)
}

// SetCompression enables or disables automatic response compression on the server.
func (s *Server) SetCompression(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.compSnap.Load().clone()
	next.disabled = !enabled
	s.compSnap.Store(next)
}

// SetDecompression enables or disables automatic request body decompression on
// the server.
func (s *Server) SetDecompression(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.compSnap.Load().clone()
	next.disableDecompression = !enabled
	s.compSnap.Store(next)
}

// SetCompressionOrder sets the server's preferred response encodings in
// priority order. Calling SetCompressionOrder() with no arguments resets the
// default priority (zstd, br, gzip, deflate). Passing only [EncodingNone]
// disables response compression until an encoding is re-added.
func (s *Server) SetCompressionOrder(encodings ...Encoding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.compSnap.Load().clone()
	if len(encodings) == 0 {
		next.order = defaultEncodingOrder
	} else {
		next.order = normalizeEncodingOrder(encodings)
	}
	next.recomputeFlags()
	s.compSnap.Store(next)
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
	for _, mw := range middlewares {
		if mw == nil {
			panic("server: nil middleware")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.root.middlewares = append(s.root.middlewares, middlewares...)
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
	next := s.snap.Load().cloneShallow()
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

// Route creates a subgroup at prefix and passes it to fn for scoped route
// registration. It panics if fn is nil.
func (s *Server) Route(prefix string, fn func(*Group)) {
	s.root.Route(prefix, fn)
}

// Handle registers handlers for method and pattern.
// If method is "", the route matches any HTTP method as a fallback after
// method-specific routes.
func (s *Server) Handle(method, pattern string, handlers ...HandlerFunc) {
	s.root.Handle(method, pattern, handlers...)
}

// Match registers handlers for each HTTP method in methods matching pattern.
func (s *Server) Match(methods []string, pattern string, handlers ...HandlerFunc) {
	s.root.Match(methods, pattern, handlers...)
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

// Prefix returns the group's full URL path prefix.
func (g *Group) Prefix() string {
	return g.prefix
}

// Use appends middleware to the group for subsequently registered routes and
// child groups.
func (g *Group) Use(middlewares ...HandlerFunc) {
	for _, mw := range middlewares {
		if mw == nil {
			panic("server: nil middleware")
		}
	}
	g.server.mu.Lock()
	defer g.server.mu.Unlock()
	g.middlewares = append(g.middlewares, middlewares...)
	if g == g.server.root {
		g.server.rebuildFallbackChainsLocked()
	}
}

// Group creates a nested subgroup inheriting this group's prefix and current
// middleware slice.
func (g *Group) Group(prefix string, middlewares ...HandlerFunc) *Group {
	g.server.mu.Lock()
	defer g.server.mu.Unlock()
	if prefix != "" {
		if prefix[0] != '/' {
			panic(fmt.Sprintf("server: group prefix %q must start with '/'", prefix))
		}
		if hasASCIIControl(prefix) {
			panic(fmt.Sprintf("server: group prefix %q contains control character", prefix))
		}
		if !isCleanPath(prefix) {
			panic(fmt.Sprintf("server: group prefix %q is not a clean canonical path", prefix))
		}
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

// Route creates a nested subgroup at prefix and passes it to fn for scoped
// route registration. It panics if fn is nil.
func (g *Group) Route(prefix string, fn func(*Group)) {
	if fn == nil {
		panic("server: nil Route callback")
	}
	fn(g.Group(prefix))
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

	cur := g.server.snap.Load()
	next := cur.cloneShallow()
	mr := next.cloneOrCreateMethodRouter(method, parsed.isStatic)
	if err := mr.add(parsed, entry); err != nil {
		panic("server: " + err.Error())
	}
	displayMethod := method
	if displayMethod == "" {
		displayMethod = "*"
	}
	routes := make([]RouteInfo, len(cur.routes)+1)
	copy(routes, cur.routes)
	routes[len(cur.routes)] = RouteInfo{
		Method:  displayMethod,
		Pattern: fullPattern,
	}
	next.routes = routes
	g.server.snap.Store(next)
}

// Match registers handlers on the group for each HTTP method in methods
// matching pattern. It panics if methods is empty.
func (g *Group) Match(methods []string, pattern string, handlers ...HandlerFunc) {
	if len(methods) == 0 {
		panic("server: Match requires at least one HTTP method")
	}
	for _, m := range methods {
		g.Handle(m, pattern, handlers...)
	}
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
	g.Match(anyMethods, pattern, handlers...)
}

// HandleHTTP registers a standard [http.Handler] for method and pattern.
func (g *Group) HandleHTTP(method, pattern string, h http.Handler) {
	g.Handle(method, pattern, AdaptHandler(h))
}

// Mount attaches a standard [http.Handler] at prefix, stripping prefix from
// the request URL path before forwarding.
func (g *Group) Mount(prefix string, h http.Handler) {
	if h == nil {
		panic("server: nil http.Handler")
	}
	fullPrefix := joinPattern(g.prefix, prefix)
	if strings.ContainsAny(fullPrefix, "{}%") {
		panic(fmt.Sprintf("server: mount prefix %q must not contain parameters or percent-encoding", fullPrefix))
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
				if strings.HasPrefix(u2.RawPath, cleanPref) &&
					(len(u2.RawPath) == len(cleanPref) || u2.RawPath[len(cleanPref)] == '/') {
					rp := u2.RawPath[len(cleanPref):]
					if rp == "" {
						rp = "/"
					}
					u2.RawPath = rp
				} else {
					// Escaped prefix cannot be stripped verbatim; clear RawPath
					// so URL.EscapedPath() stays synchronized with Path.
					u2.RawPath = ""
				}
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

func (s *Server) execChain(c *Context, chain []HandlerFunc) {
	cs := s.compSnap.Load()
	if (c.req.Body != nil && c.req.Body != http.NoBody) ||
		(!cs.disabled && cs.hasAnyCompressor && c.req.Method != http.MethodHead && len(c.req.Header["Accept-Encoding"]) > 0) {
		s.execChainWithCompression(c, chain, cs)
		return
	}
	var err error
	if len(chain) == 1 {
		err = chain[0](c)
	} else {
		c.handlers = chain
		err = c.Next()
	}
	if err != nil {
		c.HandleError(err)
	}
}

//go:noinline
func (s *Server) execChainWithCompression(c *Context, chain []HandlerFunc, cs *compressionSnapshot) {
	var cw *compressResponseWriter
	if !cs.disabled && cs.hasAnyCompressor && c.req.Method != http.MethodHead && !c.noCompress {
		if ae := c.req.Header["Accept-Encoding"]; len(ae) > 0 && (cs.skip == nil || !cs.skip(c)) {
			if enc, holder := negotiateEncoding(ae, cs); holder != nil {
				cw = cwPool.Get()
				cw.reset(c.ResponseWriter(), holder, enc, cs.minLength, c.req.URL.Path)
				c.writer = cw
				c.cw = cw
			}
		}
	}

	origReq := c.req
	origBody := origReq.Body
	if decompErr := prepareRequestDecompression(c, cs); decompErr != nil {
		if cw != nil {
			c.HandleError(decompErr)
			c.writer = nil
			c.cw = nil
			cw.release(false)
			return
		}
		c.HandleError(decompErr)
		return
	}

	panicked := true
	if cw != nil || c.decomp != nil {
		defer func() {
			if c.decomp != nil {
				if origReq.Body == c.decomp || c.bodyLimitApplied > 0 {
					origReq.Body = origBody
				}
				c.decomp.finish(panicked)
				c.decomp = nil
			}
			if cw != nil {
				c.writer = nil
				c.cw = nil
				cw.release(panicked)
			}
		}()
	}

	var err error
	if len(chain) == 1 {
		err = chain[0](c)
	} else {
		c.handlers = chain
		err = c.Next()
	}
	if err != nil {
		c.HandleError(err)
	}
	panicked = false
}

// ServeHTTP implements [http.Handler].
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqPath := r.URL.Path
	rawPath := r.URL.RawPath
	snap := s.snap.Load()

	// Fast path for unescaped requests: every static literal in staticTable and
	// dynTree was validated by parsePattern to be clean and NUL-free, and
	// matchUnescaped validates captured parameters and wildcards inline.
	if rawPath == "" {
		if r.Method == http.MethodGet {
			if mr := snap.methods[methodGET]; mr != nil {
				if entry := mr.static.lookup(reqPath); entry != nil {
					c := acquireContext(s, w, r)
					c.entry = entry
					s.execChain(c, entry.chain)
					releaseContext(c)
					return
				}
				if !mr.onlyStatic && len(reqPath) > 0 && reqPath[0] == '/' {
					c := acquireContext(s, w, r)
					pc := paramCapture{
						buf:   &c.paramBuf,
						extra: &c.paramExtra,
					}
					if entry := mr.dynTree.matchUnescaped(reqPath, &pc); entry != nil {
						c.nParams = pc.n
						c.entry = entry
						s.execChain(c, entry.chain)
						releaseContext(c)
						return
					}
					if pc.n > 0 {
						pc.rollback(0)
					}
					releaseContext(c)
				}
			}
			if snap.anyMethod != nil && len(reqPath) > 0 && reqPath[0] == '/' {
				if s.tryServeUnescapedAny(snap, w, r, reqPath) {
					return
				}
			}
		} else {
			if entry := snap.matchStaticSlow(r.Method, reqPath); entry != nil {
				c := acquireContext(s, w, r)
				c.entry = entry
				s.execChain(c, entry.chain)
				releaseContext(c)
				return
			}
			if len(reqPath) > 0 && reqPath[0] == '/' {
				c := acquireContext(s, w, r)
				pc := paramCapture{
					buf:   &c.paramBuf,
					extra: &c.paramExtra,
				}
				mIdx := methodIndex(r.Method)
				if entry := snap.matchAfterStaticIdx(mIdx, r.Method, reqPath, &pc); entry != nil {
					c.nParams = pc.n
					c.entry = entry
					s.execChain(c, entry.chain)
					releaseContext(c)
					return
				}
				if pc.n > 0 {
					pc.rollback(0)
				}
				releaseContext(c)
			}
		}
	}

	s.serveSlow(snap, w, r, reqPath, rawPath)
}

//go:noinline
func (s *Server) tryServeUnescapedAny(snap *routerSnapshot, w http.ResponseWriter, r *http.Request, reqPath string) bool {
	c := acquireContext(s, w, r)
	pc := paramCapture{
		buf:   &c.paramBuf,
		extra: &c.paramExtra,
	}
	if entry := snap.anyMethod.match(reqPath, false, &pc); entry != nil {
		c.nParams = pc.n
		c.entry = entry
		s.execChain(c, entry.chain)
		releaseContext(c)
		return true
	}
	if pc.n > 0 {
		pc.rollback(0)
	}
	releaseContext(c)
	return false
}

//go:noinline
func (s *Server) serveSlow(snap *routerSnapshot, w http.ResponseWriter, r *http.Request, reqPath, rawPath string) {
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
	mIdx := methodIndex(r.Method)
	if escaped {
		pc := paramCapture{
			buf:   &c.paramBuf,
			extra: &c.paramExtra,
		}
		if entry := snap.matchIdx(mIdx, r.Method, matchPath, true, &pc); entry != nil {
			c.nParams = pc.n
			c.entry = entry
			s.execChain(c, entry.chain)
			releaseContext(c)
			return
		}
		if pc.n > 0 {
			pc.rollback(0)
		}
	}

	// Optional trailing-slash redirect (safe against open redirects).
	if s.cfg.RedirectTrailingSlash && len(matchPath) > 1 && matchPath[0] == '/' && r.Method != http.MethodConnect {
		var alt string
		if matchPath[len(matchPath)-1] == '/' {
			alt = matchPath[:len(matchPath)-1]
		} else {
			alt = matchPath + "/"
		}
		if snap.matchIdx(mIdx, r.Method, alt, escaped, nil) != nil {
			releaseContext(c)
			// Preserve percent-encoding when RawPath was set so encoded
			// slashes (%2F) are not flattened by the redirect.
			s.safeRedirect(w, r, alt)
			return
		}
	}

	// Check if other HTTP methods match this path (405 Method Not Allowed).
	if len(matchPath) > 0 && matchPath[0] == '/' {
		var allowBuf [numStdMethods]string
		allowed := snap.allowedMethods(matchPath, escaped, allowBuf[:0])
		if len(allowed) > 0 {
			c.respHeader()["Allow"] = []string{strings.Join(allowed, ", ")}
			s.execChain(c, snap.methodNotAllowed)
			releaseContext(c)
			return
		}
	}

	// 404 Not Found.
	s.execChain(c, snap.notFoundChain)
	releaseContext(c)
}

func hasASCIIControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
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
	if hasASCIIControl(targetPath) || hasASCIIControl(r.URL.RawQuery) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
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
		if s.cfg.Logger != nil {
			s.httpSrv.ErrorLog = slog.NewLogLogger(s.cfg.Logger.Handler(), slog.LevelError)
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

// ListenAndServeTLSContext listens on addr and serves HTTPS requests until ctx
// is canceled, then performs a graceful shutdown bounded by Config.ShutdownTimeout.
// Returns nil when shutdown completes cleanly after ctx cancellation.
func (s *Server) ListenAndServeTLSContext(ctx context.Context, addr, certFile, keyFile string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return s.ServeTLSContext(ctx, ln, certFile, keyFile)
}

// ServeContext serves requests on ln until ctx is canceled, then performs a
// graceful shutdown bounded by Config.ShutdownTimeout. Returns nil when
// shutdown completes cleanly after ctx cancellation.
func (s *Server) ServeContext(ctx context.Context, ln net.Listener) error {
	return s.serveWithContext(ctx, func() error {
		return s.Serve(ln)
	})
}

// ServeTLSContext serves HTTPS requests on ln until ctx is canceled, then
// performs a graceful shutdown bounded by Config.ShutdownTimeout. Returns nil
// when shutdown completes cleanly after ctx cancellation.
func (s *Server) ServeTLSContext(ctx context.Context, ln net.Listener, certFile, keyFile string) error {
	return s.serveWithContext(ctx, func() error {
		return s.ServeTLS(ln, certFile, keyFile)
	})
}

func (s *Server) serveWithContext(ctx context.Context, serveFn func() error) error {
	_ = s.HTTPServer()
	errCh := make(chan error, 1)
	go func() {
		errCh <- serveFn()
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
		if err := rc.Close(); err != nil {
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
		if err := rc.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
