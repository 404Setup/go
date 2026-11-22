// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	syncv2 "sync/v2"
)

const (
	maxInlineParams   = 8
	maxInlineValues   = 4
	maxPooledJSONSize = 64 << 10
)

type jsonEncoderBuf struct {
	buf bytes.Buffer
	enc *json.Encoder
}

var jsonBufPool = syncv2.NewPool(func() *jsonEncoderBuf {
	j := &jsonEncoderBuf{}
	j.enc = json.NewEncoder(&j.buf)
	return j
})

type writerOnly struct {
	io.Writer
}

type responseWriter struct {
	w       http.ResponseWriter
	status  int
	size    int64
	written bool
}

func (rw *responseWriter) Header() http.Header {
	return rw.w.Header()
}

func (rw *responseWriter) WriteHeader(code int) {
	if rw.written {
		return
	}
	// 1xx informational responses (except 101 Switching Protocols) do not
	// finalize the response header.
	if code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols {
		rw.w.WriteHeader(code)
		return
	}
	rw.status = code
	rw.written = true
	rw.w.WriteHeader(code)
}

func (rw *responseWriter) Write(p []byte) (int, error) {
	if !rw.written {
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.w.Write(p)
	rw.size += int64(n)
	return n, err
}

func (rw *responseWriter) WriteString(s string) (int, error) {
	if !rw.written {
		rw.WriteHeader(http.StatusOK)
	}
	n, err := io.WriteString(rw.w, s)
	rw.size += int64(n)
	return n, err
}

func (rw *responseWriter) ReadFrom(r io.Reader) (int64, error) {
	if !rw.written {
		rw.WriteHeader(http.StatusOK)
	}
	if rf, ok := rw.w.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		rw.size += n
		return n, err
	}
	return io.Copy(writerOnly{rw}, r)
}

// Unwrap returns the underlying [http.ResponseWriter] for [http.ResponseController].
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.w
}

type contextKV struct {
	key string
	val any
}

// Context carries request state, matched route parameters, request-scoped
// values, middleware control flow, and response helpers for a single HTTP
// request.
//
// Context instances are pooled and reused across requests. A Context must not
// be retained or accessed after the handler returns.
type Context struct {
	server *Server
	req    *http.Request
	rw     responseWriter
	writer http.ResponseWriter

	handlers     []HandlerFunc
	index        int
	routePattern string

	paramNames []string
	paramBuf   [maxInlineParams]string
	paramExtra []string
	nParams    int

	inlineValues [maxInlineValues]contextKV
	nValues      int
	valuesMap    map[string]any

	query       url.Values
	queryParsed bool

	maxBodyBytes int64
	errHandled   bool
}

type jsonReadBuf struct {
	buf bytes.Buffer
	lr  io.LimitedReader
}

var jsonReadBufPool = syncv2.NewPool(func() *jsonReadBuf {
	return &jsonReadBuf{}
})

var contextPool = syncv2.NewPool(func() *Context {
	return &Context{}
})

func acquireContext(s *Server, w http.ResponseWriter, r *http.Request) *Context {
	c := contextPool.Get()
	c.server = s
	c.req = r
	c.rw.w = w
	c.index = -1
	return c
}

func releaseContext(c *Context) {
	// Scrub all references to prevent memory retention and cross-request leakage.
	if c.nParams > 0 {
		limit := c.nParams
		if limit > maxInlineParams {
			limit = maxInlineParams
		}
		for i := 0; i < limit; i++ {
			c.paramBuf[i] = ""
		}
		if len(c.paramExtra) > 0 {
			clear(c.paramExtra)
			if cap(c.paramExtra) <= 32 {
				c.paramExtra = c.paramExtra[:0]
			} else {
				c.paramExtra = nil
			}
		}
		c.nParams = 0
		c.paramNames = nil
	}
	if c.nValues > 0 {
		for i := 0; i < c.nValues; i++ {
			c.inlineValues[i] = contextKV{}
		}
		c.nValues = 0
	}
	if len(c.valuesMap) > 32 {
		c.valuesMap = nil
	} else if len(c.valuesMap) > 0 {
		clear(c.valuesMap)
	}
	c.server = nil
	c.req = nil
	c.rw = responseWriter{}
	if c.writer != nil {
		c.writer = nil
	}
	c.handlers = nil
	c.routePattern = ""
	if c.queryParsed {
		c.query = nil
		c.queryParsed = false
	}
	c.maxBodyBytes = 0
	c.errHandled = false
	contextPool.Put(c)
}

// Request returns the underlying [*http.Request].
func (c *Context) Request() *http.Request {
	return c.req
}

// ResponseWriter returns the [http.ResponseWriter] for this request.
// It implements Unwrap() [http.ResponseWriter] for use with [http.NewResponseController].
func (c *Context) ResponseWriter() http.ResponseWriter {
	if c.writer != nil {
		return c.writer
	}
	return &c.rw
}

func (c *Context) respHeader() http.Header {
	if c.writer != nil {
		return c.writer.Header()
	}
	return c.rw.w.Header()
}

// Context returns the request's [context.Context].
func (c *Context) Context() context.Context {
	return c.req.Context()
}

// SetContext replaces the request's [context.Context] using [http.Request.WithContext].
func (c *Context) SetContext(ctx context.Context) {
	c.req = c.req.WithContext(ctx)
}

// Method returns the HTTP request method.
func (c *Context) Method() string {
	return c.req.Method
}

// Path returns the request URL path.
func (c *Context) Path() string {
	return c.req.URL.Path
}

// RoutePattern returns the matched route pattern (for example, "/users/{id}"),
// or "" if no route matched.
func (c *Context) RoutePattern() string {
	return c.routePattern
}

// StatusCode returns the HTTP status code written to the response, or 0 if no
// response header has been written yet.
func (c *Context) StatusCode() int {
	return c.rw.status
}

// BytesWritten returns the number of response body bytes written so far.
func (c *Context) BytesWritten() int64 {
	return c.rw.size
}

// Written reports whether the response header has been written.
func (c *Context) Written() bool {
	return c.rw.written
}

// Next executes the next handler in the middleware chain and returns its error.
// Returning from middleware without calling Next stops the chain immediately.
func (c *Context) Next() error {
	c.index++
	if c.index < len(c.handlers) {
		return c.handlers[c.index](c)
	}
	return nil
}

// HandleError invokes the server's configured [ErrorHandler] for err if an
// error has not already been handled for this request.
func (c *Context) HandleError(err error) {
	if err == nil || c.errHandled {
		return
	}
	c.errHandled = true
	if c.server != nil && c.server.errorHandler != nil {
		c.server.errorHandler(c, err)
		return
	}
	DefaultErrorHandler(c, err)
}

// Param returns the value of the named path parameter, or "" if not present.
func (c *Context) Param(name string) string {
	for i, k := range c.paramNames {
		if k == name {
			if i < c.nParams {
				if i < maxInlineParams {
					return c.paramBuf[i]
				}
				return c.paramExtra[i-maxInlineParams]
			}
			return ""
		}
	}
	return ""
}

// ParamInt returns the named path parameter parsed as an int.
func (c *Context) ParamInt(name string) (int, error) {
	v := c.Param(name)
	if v == "" {
		return 0, fmt.Errorf("missing parameter %q", name)
	}
	return strconv.Atoi(v)
}

// ParamInt64 returns the named path parameter parsed as an int64.
func (c *Context) ParamInt64(name string) (int64, error) {
	v := c.Param(name)
	if v == "" {
		return 0, fmt.Errorf("missing parameter %q", name)
	}
	return strconv.ParseInt(v, 10, 64)
}

// QueryValues returns the parsed URL query parameters, caching the result for
// the lifetime of the request.
func (c *Context) QueryValues() url.Values {
	if !c.queryParsed {
		c.query = c.req.URL.Query()
		c.queryParsed = true
	}
	return c.query
}

const maxRawQueryFastScan = 4096

// Query returns the first value associated with the given query key, or "".
func (c *Context) Query(key string) string {
	if c.queryParsed {
		return c.query.Get(key)
	}
	raw := c.req.URL.RawQuery
	if raw == "" {
		return ""
	}
	if len(raw) > maxRawQueryFastScan {
		return c.QueryValues().Get(key)
	}
	return lookupRawQuery(raw, key)
}

// lookupRawQuery scans raw for the first valid occurrence of target without
// allocating a url.Values map, matching net/url.ParseQuery semantics.
func lookupRawQuery(raw, target string) string {
	for raw != "" {
		var part string
		part, raw, _ = strings.Cut(raw, "&")
		if part == "" || strings.IndexByte(part, ';') >= 0 {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		if strings.IndexByte(k, '%') >= 0 || strings.IndexByte(k, '+') >= 0 {
			u, err := url.QueryUnescape(k)
			if err != nil {
				continue
			}
			k = u
		}
		if k != target {
			continue
		}
		if strings.IndexByte(v, '%') >= 0 || strings.IndexByte(v, '+') >= 0 {
			u, err := url.QueryUnescape(v)
			if err != nil {
				continue
			}
			return u
		}
		return v
	}
	return ""
}

// QueryDefault returns the first value for key, or def if key is missing or empty.
func (c *Context) QueryDefault(key, def string) string {
	if v := c.Query(key); v != "" {
		return v
	}
	return def
}

// QueryInt returns the query parameter parsed as an int. If the parameter is
// absent or empty, it returns (def, nil). If present but invalid, it returns an error.
func (c *Context) QueryInt(key string, def int) (int, error) {
	v := c.Query(key)
	if v == "" {
		return def, nil
	}
	return strconv.Atoi(v)
}

// QueryInt64 returns the query parameter parsed as an int64. If the parameter
// is absent or empty, it returns (def, nil). If present but invalid, it returns an error.
func (c *Context) QueryInt64(key string, def int64) (int64, error) {
	v := c.Query(key)
	if v == "" {
		return def, nil
	}
	return strconv.ParseInt(v, 10, 64)
}

// QueryBool returns the query parameter parsed as a bool using [strconv.ParseBool].
// If the parameter is absent or empty, it returns (def, nil). If present but
// invalid, it returns an error.
func (c *Context) QueryBool(key string, def bool) (bool, error) {
	v := c.Query(key)
	if v == "" {
		return def, nil
	}
	return strconv.ParseBool(v)
}

// Header returns the first value of the named request header.
func (c *Context) Header(key string) string {
	return c.req.Header.Get(key)
}

// SetHeader sets a response header.
func (c *Context) SetHeader(key, value string) {
	c.respHeader().Set(key, value)
}

// AddHeader appends a value to a response header.
func (c *Context) AddHeader(key, value string) {
	c.respHeader().Add(key, value)
}

// Cookie returns the named cookie from the request.
func (c *Context) Cookie(name string) (*http.Cookie, error) {
	return c.req.Cookie(name)
}

// SetCookie adds a Set-Cookie header to the response.
func (c *Context) SetCookie(cookie *http.Cookie) {
	http.SetCookie(c.ResponseWriter(), cookie)
}

// Set stores a request-scoped key-value pair on the Context.
func (c *Context) Set(key string, val any) {
	for i := 0; i < c.nValues; i++ {
		if c.inlineValues[i].key == key {
			c.inlineValues[i].val = val
			return
		}
	}
	if c.nValues < maxInlineValues {
		c.inlineValues[c.nValues] = contextKV{key: key, val: val}
		c.nValues++
		return
	}
	if c.valuesMap == nil {
		c.valuesMap = make(map[string]any, 4)
	}
	c.valuesMap[key] = val
}

// Get retrieves a request-scoped value previously stored via [Context.Set].
func (c *Context) Get(key string) (any, bool) {
	for i := 0; i < c.nValues; i++ {
		if c.inlineValues[i].key == key {
			return c.inlineValues[i].val, true
		}
	}
	if c.valuesMap != nil {
		v, ok := c.valuesMap[key]
		return v, ok
	}
	return nil, false
}

// Delete removes a request-scoped value previously stored via [Context.Set].
func (c *Context) Delete(key string) {
	for i := 0; i < c.nValues; i++ {
		if c.inlineValues[i].key == key {
			last := c.nValues - 1
			c.inlineValues[i] = c.inlineValues[last]
			c.inlineValues[last] = contextKV{}
			c.nValues = last
			return
		}
	}
	if c.valuesMap != nil {
		delete(c.valuesMap, key)
	}
}

// GetValue retrieves a typed request-scoped value stored via [Context.Set].
func GetValue[T any](c *Context, key string) (T, bool) {
	v, ok := c.Get(key)
	if !ok {
		var zero T
		return zero, false
	}
	tv, ok := v.(T)
	return tv, ok
}

// SetMaxBodyBytes overrides the maximum request body size for this request.
// A negative value disables the limit for this request.
func (c *Context) SetMaxBodyBytes(limit int64) {
	c.maxBodyBytes = limit
}

func (c *Context) effectiveMaxBodyBytes() int64 {
	if c.maxBodyBytes != 0 {
		return c.maxBodyBytes
	}
	if c.server != nil {
		return c.server.maxBodyBytes
	}
	return defaultMaxBodyBytes
}

// markRequestTooLarge notifies the underlying net/http server to close the
// connection after an oversized request body, matching http.MaxBytesReader.
func (c *Context) markRequestTooLarge() {
	if c.req.Body == nil {
		return
	}
	var b [1]byte
	_, _ = http.MaxBytesReader(c.ResponseWriter(), c.req.Body, 0).Read(b[:])
}

// readBodyInto reads c.req.Body into jrb.buf using jrb.lr (zero heap
// allocations on the happy path) while strictly enforcing limit when > 0.
func (c *Context) readBodyInto(jrb *jsonReadBuf, limit int64) error {
	if limit > 0 && c.req.ContentLength > limit {
		c.markRequestTooLarge()
		return WrapError(http.StatusRequestEntityTooLarge, "request body too large", ErrBodyTooLarge)
	}
	if c.req.ContentLength > 0 {
		grow := c.req.ContentLength
		if grow > maxPooledJSONSize {
			grow = maxPooledJSONSize
		}
		jrb.buf.Grow(int(grow))
	}

	var err error
	if limit > 0 {
		jrb.lr.R = c.req.Body
		jrb.lr.N = limit + 1
		_, err = jrb.buf.ReadFrom(&jrb.lr)
		jrb.lr.R = nil
		if err == nil && int64(jrb.buf.Len()) > limit {
			c.markRequestTooLarge()
			return WrapError(http.StatusRequestEntityTooLarge, "request body too large", ErrBodyTooLarge)
		}
	} else {
		_, err = jrb.buf.ReadFrom(c.req.Body)
	}
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return WrapError(http.StatusRequestEntityTooLarge, "request body too large", ErrBodyTooLarge)
		}
		return err
	}
	return nil
}

// BodyBytes reads and returns the request body, enforcing the configured
// maximum body size.
func (c *Context) BodyBytes() ([]byte, error) {
	if c.req.Body == nil || c.req.Body == http.NoBody {
		return nil, nil
	}
	limit := c.effectiveMaxBodyBytes()
	jrb := jsonReadBufPool.Get()
	jrb.buf.Reset()
	err := c.readBodyInto(jrb, limit)
	if err != nil {
		if jrb.buf.Cap() <= maxPooledJSONSize {
			jrb.buf.Reset()
			jsonReadBufPool.Put(jrb)
		}
		return nil, err
	}
	out := make([]byte, jrb.buf.Len())
	copy(out, jrb.buf.Bytes())
	if jrb.buf.Cap() <= maxPooledJSONSize {
		jrb.buf.Reset()
		jsonReadBufPool.Put(jrb)
	}
	return out, nil
}

// ParseForm parses URL query parameters and URL-encoded form body while
// enforcing the configured maximum body size.
func (c *Context) ParseForm() (url.Values, error) {
	limit := c.effectiveMaxBodyBytes()
	if limit > 0 && c.req.Body != nil {
		if c.req.ContentLength > limit {
			c.markRequestTooLarge()
			return nil, WrapError(http.StatusRequestEntityTooLarge, "request body too large", ErrBodyTooLarge)
		}
		c.req.Body = http.MaxBytesReader(c.ResponseWriter(), c.req.Body, limit)
	}
	if err := c.req.ParseForm(); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, WrapError(http.StatusRequestEntityTooLarge, "request body too large", ErrBodyTooLarge)
		}
		return nil, WrapError(http.StatusBadRequest, "invalid form data", err)
	}
	return c.req.Form, nil
}

const defaultMultipartMemory = 32 << 20 // 32 MiB

// ParseMultipartForm parses a multipart/form-data request body while enforcing
// the configured maximum body size. Up to maxMemory bytes of file parts are
// stored in memory, with the remainder stored in temporary files. If maxMemory
// is <= 0, a safe default (32 MiB, bounded by the effective MaxBodyBytes) is used.
func (c *Context) ParseMultipartForm(maxMemory int64) (*multipart.Form, error) {
	limit := c.effectiveMaxBodyBytes()
	if maxMemory <= 0 {
		maxMemory = defaultMultipartMemory
	}
	if limit > 0 {
		if maxMemory > limit {
			maxMemory = limit
		}
		if c.req.Body != nil {
			if c.req.ContentLength > limit {
				c.markRequestTooLarge()
				return nil, WrapError(http.StatusRequestEntityTooLarge, "request body too large", ErrBodyTooLarge)
			}
			c.req.Body = http.MaxBytesReader(c.ResponseWriter(), c.req.Body, limit)
		}
	}
	if err := c.req.ParseMultipartForm(maxMemory); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, WrapError(http.StatusRequestEntityTooLarge, "request body too large", ErrBodyTooLarge)
		}
		return nil, WrapError(http.StatusBadRequest, "invalid multipart form", err)
	}
	return c.req.MultipartForm, nil
}

// FormFile returns the first file for the provided multipart form key while
// enforcing the configured maximum body size.
func (c *Context) FormFile(name string) (multipart.File, *multipart.FileHeader, error) {
	if c.req.MultipartForm == nil {
		if _, err := c.ParseMultipartForm(0); err != nil {
			return nil, nil, err
		}
	}
	return c.req.FormFile(name)
}

// BindJSON decodes a single JSON value from the request body into dst.
//
// It enforces Config.MaxBodyBytes (returning 413 on overflow), rejects non-JSON
// Content-Type headers when present (returning 415), and rejects trailing data
// after the first JSON value (returning 400).
func (c *Context) BindJSON(dst any) error {
	return c.bindJSON(dst, false)
}

// BindJSONStrict is like [Context.BindJSON] but also rejects unknown JSON
// fields via [json.Decoder.DisallowUnknownFields].
func (c *Context) BindJSONStrict(dst any) error {
	return c.bindJSON(dst, true)
}

func (c *Context) bindJSON(dst any, strict bool) error {
	if ct := c.req.Header.Get("Content-Type"); ct != "" && !isJSONContentType(ct) {
		return WrapError(http.StatusUnsupportedMediaType, "content-type must be application/json", ErrUnsupportedMediaType)
	}
	if c.req.Body == nil || c.req.Body == http.NoBody {
		return WrapError(http.StatusBadRequest, "request body is empty", ErrInvalidJSON)
	}
	limit := c.effectiveMaxBodyBytes()
	if limit > 0 && c.req.ContentLength > limit {
		c.markRequestTooLarge()
		return WrapError(http.StatusRequestEntityTooLarge, "request body too large", ErrBodyTooLarge)
	}

	jrb := jsonReadBufPool.Get()
	jrb.buf.Reset()
	if err := c.readBodyInto(jrb, limit); err != nil {
		if jrb.buf.Cap() <= maxPooledJSONSize {
			jrb.buf.Reset()
			jsonReadBufPool.Put(jrb)
		}
		var he *HTTPError
		if errors.As(err, &he) {
			return err
		}
		return WrapError(http.StatusBadRequest, "failed to read request body", err)
	}

	raw := trimJSONSpace(jrb.buf.Bytes())
	if len(raw) == 0 {
		if jrb.buf.Cap() <= maxPooledJSONSize {
			jrb.buf.Reset()
			jsonReadBufPool.Put(jrb)
		}
		return WrapError(http.StatusBadRequest, "request body is empty", ErrInvalidJSON)
	}

	var decodeErr error
	if !strict {
		// json.Unmarshal rejects trailing tokens after the first JSON value.
		decodeErr = json.Unmarshal(raw, dst)
	} else {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(dst); err != nil {
			decodeErr = err
		} else {
			var trailing struct{}
			if err := dec.Decode(&trailing); err != io.EOF {
				decodeErr = errors.New("trailing data after JSON value")
			}
		}
	}

	if jrb.buf.Cap() <= maxPooledJSONSize {
		jrb.buf.Reset()
		jsonReadBufPool.Put(jrb)
	}

	if decodeErr != nil {
		return WrapError(http.StatusBadRequest, "invalid JSON payload", fmt.Errorf("%w: %w", ErrInvalidJSON, decodeErr))
	}
	return nil
}

// trimJSONSpace trims RFC 8259 JSON ASCII whitespace (SP, HTAB, LF, CR).
func trimJSONSpace(b []byte) []byte {
	start := 0
	for start < len(b) && isJSONSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isJSONSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// trimOWS trims RFC 9110 optional HTTP ASCII whitespace (SP and HTAB).
func trimOWS(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func isJSONContentType(ct string) bool {
	// Fast path for the common "application/json" and "application/json; charset=utf-8" cases.
	if ct == "application/json" || ct == "application/json; charset=utf-8" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	if mediaType == "application/json" {
		return true
	}
	return strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json")
}

// ClientIP returns the client IP address.
//
// By default, when Config.TrustedProxies is empty, ClientIP always returns the
// direct peer address from Request.RemoteAddr and ignores forwarding headers.
// When TrustedProxies is configured and the direct peer matches a trusted
// prefix, ClientIP walks X-Forwarded-For right-to-left skipping trusted hops
// and returns the first untrusted client IP (or the leftmost valid IP if all
// hops are trusted; when X-Forwarded-For is absent, falls back to X-Real-IP,
// then the peer address).
func (c *Context) ClientIP() string {
	addr := c.req.RemoteAddr
	peer, rawIPv4, ok := parseRemoteAddr(addr)
	if !ok {
		return addr
	}
	if c.server == nil || !c.server.isTrustedProxy(peer) {
		if rawIPv4 != "" {
			return rawIPv4
		}
		return peer.String()
	}

	// Walk X-Forwarded-For headers from right to left.
	if xffHeaders := c.req.Header.Values("X-Forwarded-For"); len(xffHeaders) > 0 {
		var leftmost netip.Addr
		sawValid := false
		for i := len(xffHeaders) - 1; i >= 0; i-- {
			hdr := xffHeaders[i]
			for len(hdr) > 0 {
				var part string
				if comma := strings.LastIndexByte(hdr, ','); comma >= 0 {
					part = hdr[comma+1:]
					hdr = hdr[:comma]
				} else {
					part = hdr
					hdr = ""
				}
				part = trimOWS(part)
				if part == "" {
					continue
				}
				ip, err := netip.ParseAddr(part)
				if err != nil || ip.Zone() != "" {
					if rawIPv4 != "" {
						return rawIPv4
					}
					return peer.String()
				}
				ip = ip.Unmap()
				if !c.server.isTrustedProxy(ip) {
					return ip.String()
				}
				leftmost = ip
				sawValid = true
			}
		}
		if sawValid {
			return leftmost.String()
		}
	}

	if xri := trimOWS(c.req.Header.Get("X-Real-IP")); xri != "" {
		if ip, err := netip.ParseAddr(xri); err == nil && ip.Zone() == "" {
			return ip.Unmap().String()
		}
	}

	if rawIPv4 != "" {
		return rawIPv4
	}
	return peer.String()
}

// parseRemoteAddr parses addr and returns the unmapped IP address, an optional
// zero-allocation canonical IPv4 substring of addr when available, and whether
// parsing succeeded.
func parseRemoteAddr(addr string) (ip netip.Addr, rawIPv4 string, ok bool) {
	if ap, err := netip.ParseAddrPort(addr); err == nil {
		a := ap.Addr()
		if a.Is4() {
			if colon := strings.LastIndexByte(addr, ':'); colon > 0 {
				return a, addr[:colon], true
			}
		}
		return a.Unmap(), "", true
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		if a, err := netip.ParseAddr(host); err == nil {
			if a.Is4() {
				return a, host, true
			}
			return a.Unmap(), "", true
		}
	}
	if a, err := netip.ParseAddr(addr); err == nil {
		if a.Is4() {
			return a, addr, true
		}
		return a.Unmap(), "", true
	}
	return netip.Addr{}, "", false
}

func (c *Context) writeHeader(code int) {
	if c.writer == nil {
		c.rw.WriteHeader(code)
		return
	}
	c.writer.WriteHeader(code)
}

func (c *Context) writeBytes(p []byte) (int, error) {
	if c.writer == nil {
		return c.rw.Write(p)
	}
	return c.writer.Write(p)
}

func (c *Context) writeString(s string) (int, error) {
	if c.writer == nil {
		return c.rw.WriteString(s)
	}
	return io.WriteString(c.writer, s)
}

func (c *Context) setDefaultContentType(ct string) {
	h := c.respHeader()
	if len(h["Content-Type"]) == 0 {
		h["Content-Type"] = []string{ct}
	}
}

// WriteStatus writes an HTTP status code with no response body.
func (c *Context) WriteStatus(code int) error {
	if c.writer == nil {
		c.rw.WriteHeader(code)
		return nil
	}
	c.writer.WriteHeader(code)
	return nil
}

// NoContent writes a 204 No Content response.
func (c *Context) NoContent() error {
	if c.writer == nil {
		c.rw.WriteHeader(http.StatusNoContent)
		return nil
	}
	c.writer.WriteHeader(http.StatusNoContent)
	return nil
}

// Text writes a UTF-8 plain-text response with status code.
func (c *Context) Text(code int, s string) error {
	c.setDefaultContentType("text/plain; charset=utf-8")
	c.writeHeader(code)
	_, err := c.writeString(s)
	return err
}

// HTML writes a UTF-8 HTML response with status code.
func (c *Context) HTML(code int, html string) error {
	c.setDefaultContentType("text/html; charset=utf-8")
	c.writeHeader(code)
	_, err := c.writeString(html)
	return err
}

// Bytes writes raw bytes with the given content type and status code.
func (c *Context) Bytes(code int, contentType string, data []byte) error {
	if contentType != "" {
		c.setDefaultContentType(contentType)
	}
	c.writeHeader(code)
	if len(data) == 0 {
		return nil
	}
	_, err := c.writeBytes(data)
	return err
}

// JSON serializes v as JSON into a pooled buffer before writing the response.
// If serialization fails, no headers or bytes are written and the error is returned.
func (c *Context) JSON(code int, v any) error {
	jeb := jsonBufPool.Get()
	jeb.buf.Reset()
	err := jeb.enc.Encode(v)
	if err != nil {
		if jeb.buf.Cap() <= maxPooledJSONSize {
			jeb.buf.Reset()
			jsonBufPool.Put(jeb)
		}
		return err
	}
	b := jeb.buf.Bytes()
	// Trim the trailing newline added by json.Encoder.Encode so output matches json.Marshal.
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	c.setDefaultContentType("application/json; charset=utf-8")
	c.writeHeader(code)
	_, werr := c.writeBytes(b)
	if jeb.buf.Cap() <= maxPooledJSONSize {
		jeb.buf.Reset()
		jsonBufPool.Put(jeb)
	}
	return werr
}

// JSONIndented serializes v as pretty-printed JSON before writing the response.
func (c *Context) JSONIndented(code int, v any, prefix, indent string) error {
	data, err := json.MarshalIndent(v, prefix, indent)
	if err != nil {
		return err
	}
	c.setDefaultContentType("application/json; charset=utf-8")
	c.writeHeader(code)
	_, err = c.writeBytes(data)
	return err
}

// Stream copies data from r to the response with the given status code and
// content type.
func (c *Context) Stream(code int, contentType string, r io.Reader) error {
	if contentType != "" {
		c.setDefaultContentType(contentType)
	}
	c.writeHeader(code)
	_, err := io.Copy(c.ResponseWriter(), r)
	return err
}

// Redirect responds with an HTTP redirect to target using the given 3xx status
// code. Control characters in target are rejected.
func (c *Context) Redirect(code int, target string) error {
	if code < 300 || code > 308 {
		return fmt.Errorf("server: invalid redirect status code %d", code)
	}
	for i := 0; i < len(target); i++ {
		b := target[i]
		if b < 0x20 || b == 0x7f {
			return errors.New("server: redirect target contains control character")
		}
	}
	http.Redirect(c.ResponseWriter(), c.req, target, code)
	return nil
}

// SetAttachment safely sets the Content-Disposition header to "attachment"
// using the base name of filename, preventing path and header injection.
func (c *Context) SetAttachment(filename string) {
	// Strip trailing path separators (both '/' and '\' across platforms).
	base := strings.TrimRight(filename, "/\\")
	if idx := strings.LastIndexAny(base, "/\\"); idx >= 0 {
		base = base[idx+1:]
	}
	if base == "" || base == "." || base == ".." {
		c.respHeader().Set("Content-Disposition", "attachment")
		return
	}
	// Strip ASCII control characters from filename before formatting.
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, base)
	if clean == "" || clean == "." || clean == ".." {
		c.respHeader().Set("Content-Disposition", "attachment")
		return
	}
	cd := mime.FormatMediaType("attachment", map[string]string{"filename": clean})
	if cd == "" {
		cd = "attachment"
	}
	c.respHeader().Set("Content-Disposition", cd)
}

// File serves the named local file using [http.ServeFile].
// Do not pass untrusted user input directly to File; use [Context.FileFS] or
// [Server.StaticDir] for user-controlled paths.
func (c *Context) File(path string) error {
	if strings.IndexByte(path, 0) >= 0 {
		return NewError(http.StatusBadRequest, "invalid file path")
	}
	http.ServeFile(c.ResponseWriter(), c.req, path)
	return nil
}

// FileFS serves the named file from fsys using [http.ServeFileFS].
func (c *Context) FileFS(fsys fs.FS, name string) error {
	if strings.IndexByte(name, 0) >= 0 {
		return NewError(http.StatusBadRequest, "invalid file path")
	}
	http.ServeFileFS(c.ResponseWriter(), c.req, fsys, name)
	return nil
}

// Flush flushes buffered response data to the client using [http.ResponseController].
func (c *Context) Flush() error {
	if !c.rw.written {
		c.writeHeader(http.StatusOK)
	}
	return http.NewResponseController(c.ResponseWriter()).Flush()
}
