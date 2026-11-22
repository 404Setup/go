// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	syncv2 "sync/v2"
)

const (
	defaultCompressMinLength = 1024
	maxPooledCompressBuf     = 4096
)

var (
	headerValGzip               = []string{"gzip"}[:1:1]
	headerValVaryAcceptEncoding = []string{"Accept-Encoding"}[:1:1]
)

// CompressConfig configures [CompressWithConfig].
type CompressConfig struct {
	// Level is the gzip compression level (from [gzip.HuffmanOnly] to
	// [gzip.BestCompression]). Default (0) uses [gzip.DefaultCompression].
	Level int

	// MinLength is the minimum uncompressed response body size in bytes before
	// gzip compression is applied. Responses smaller than MinLength are sent
	// uncompressed. Default (<= 0) is 1024 bytes.
	MinLength int

	// Skip optionally skips compression for matching requests. Because
	// compressing responses that combine secrets (such as CSRF tokens) with
	// attacker-controlled input can expose length side channels (BREACH), use
	// Skip to exclude sensitive dynamic endpoints when appropriate.
	Skip func(*Context) bool
}

// Compress returns gzip response compression middleware with default settings
// ([gzip.DefaultCompression], 1024-byte minimum body length).
func Compress() HandlerFunc {
	return CompressWithConfig(CompressConfig{})
}

// CompressWithConfig returns gzip response compression middleware configured
// with cfg. It panics if cfg.Level is outside the valid [compress/gzip] range.
func CompressWithConfig(cfg CompressConfig) HandlerFunc {
	level := cfg.Level
	if level == 0 {
		level = gzip.DefaultCompression
	}
	if level < gzip.HuffmanOnly || level > gzip.BestCompression {
		panic(fmt.Sprintf("server: invalid gzip compression level %d", level))
	}
	minLen := cfg.MinLength
	if minLen <= 0 {
		minLen = defaultCompressMinLength
	}

	gzPool := syncv2.NewPool(func() *gzip.Writer {
		w, _ := gzip.NewWriterLevel(io.Discard, level)
		return w
	})

	cwPool := syncv2.NewPool(func() *compressResponseWriter {
		return &compressResponseWriter{}
	})

	return func(c *Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}
		if c.Method() == http.MethodHead {
			return c.Next()
		}

		// Always advertise Vary: Accept-Encoding so caches key on encoding.
		addVaryToken(c.respHeader(), "Accept-Encoding")

		if !acceptsGzip(c.canonicalReqHeader("Accept-Encoding")) {
			return c.Next()
		}

		prevWriter := c.writer
		prevCW := c.cw
		cw := cwPool.Get()
		cw.rw = c.ResponseWriter()
		cw.pool = gzPool
		cw.minLength = minLen
		cw.status = 0
		cw.writtenBytes = 0
		cw.wroteHeader = false
		cw.mode = compressUndecided
		cw.gz = nil
		cw.buf = cw.buf[:0]

		c.writer = cw
		c.cw = cw
		var panicked = true
		defer func() {
			c.writer = prevWriter
			c.cw = prevCW
			if panicked {
				// On panic, discard the gzip writer rather than returning
				// potentially half-written state to the pool.
				cw.gz = nil
			} else {
				cw.finish()
			}
			cw.rw = nil
			cw.pool = nil
			if cap(cw.buf) > maxPooledCompressBuf {
				cw.buf = nil
			} else {
				cw.buf = cw.buf[:0]
			}
			cwPool.Put(cw)
		}()

		err := c.Next()
		panicked = false
		return err
	}
}

type compressMode uint8

const (
	compressUndecided compressMode = iota
	compressEnabled
	compressBypassed
)

type compressResponseWriter struct {
	rw           http.ResponseWriter
	pool         *syncv2.Pool[*gzip.Writer]
	gz           *gzip.Writer
	buf          []byte
	minLength    int
	status       int
	writtenBytes int64
	wroteHeader  bool
	mode         compressMode
}

func (w *compressResponseWriter) Header() http.Header {
	return w.rw.Header()
}

func (w *compressResponseWriter) Unwrap() http.ResponseWriter {
	return w.rw
}

func (w *compressResponseWriter) shouldBypassHeaders() bool {
	h := w.rw.Header()
	if len(h["Content-Encoding"]) > 0 && h["Content-Encoding"][0] != "" {
		return true
	}
	if len(h["Upgrade"]) > 0 && h["Upgrade"][0] != "" {
		return true
	}
	if ct := h["Content-Type"]; len(ct) > 0 && ct[0] != "" && isIncompressibleContentType(ct[0]) {
		return true
	}
	return false
}

func (w *compressResponseWriter) WriteHeader(code int) {
	// Pass 1xx informational responses (except 101) through immediately.
	if code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols {
		w.rw.WriteHeader(code)
		return
	}
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code

	if code == http.StatusSwitchingProtocols ||
		code == http.StatusNoContent ||
		code == http.StatusNotModified ||
		w.shouldBypassHeaders() {
		w.mode = compressBypassed
		w.rw.WriteHeader(code)
	}
}

func (w *compressResponseWriter) bypassAndFlush(extra []byte) (int, error) {
	w.mode = compressBypassed
	code := w.status
	if code == 0 {
		code = http.StatusOK
	}
	if code != http.StatusOK || (len(w.buf) == 0 && len(extra) == 0) {
		w.rw.WriteHeader(code)
	}
	if len(w.buf) > 0 {
		_, err := w.rw.Write(w.buf)
		w.buf = w.buf[:0]
		if err != nil {
			return 0, err
		}
	}
	if len(extra) > 0 {
		return w.rw.Write(extra)
	}
	return 0, nil
}

func (w *compressResponseWriter) bypassAndFlushString(extra string) (int, error) {
	w.mode = compressBypassed
	code := w.status
	if code == 0 {
		code = http.StatusOK
	}
	if code != http.StatusOK || (len(w.buf) == 0 && len(extra) == 0) {
		w.rw.WriteHeader(code)
	}
	if len(w.buf) > 0 {
		_, err := w.rw.Write(w.buf)
		w.buf = w.buf[:0]
		if err != nil {
			return 0, err
		}
	}
	if len(extra) > 0 {
		return io.WriteString(w.rw, extra)
	}
	return 0, nil
}

func (w *compressResponseWriter) startGzip() error {
	h := w.rw.Header()
	delete(h, "Content-Length")
	delete(h, "Accept-Ranges")
	h["Content-Encoding"] = headerValGzip

	code := w.status
	if code == 0 {
		code = http.StatusOK
	}
	w.rw.WriteHeader(code)

	gz := w.pool.Get()
	gz.Reset(w.rw)
	w.gz = gz
	w.mode = compressEnabled

	if len(w.buf) > 0 {
		_, err := w.gz.Write(w.buf)
		w.buf = w.buf[:0]
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *compressResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.writtenBytes += int64(len(p))
	switch w.mode {
	case compressBypassed:
		return w.rw.Write(p)
	case compressEnabled:
		return w.gz.Write(p)
	}

	// Still undecided: buffer until reaching minLength.
	if len(w.buf)+len(p) < w.minLength {
		w.buf = append(w.buf, p...)
		return len(p), nil
	}

	// Sniff Content-Type from up to 512 bytes of uncompressed data before
	// deciding to compress.
	h := w.rw.Header()
	if len(h["Content-Type"]) == 0 {
		var sample []byte
		if len(w.buf) == 0 {
			sample = p
		} else if len(w.buf) >= 512 {
			sample = w.buf[:512]
		} else {
			var sniffBuf [512]byte
			n := copy(sniffBuf[:], w.buf)
			n += copy(sniffBuf[n:], p)
			sample = sniffBuf[:n]
		}
		h["Content-Type"] = []string{http.DetectContentType(sample)}
	}
	if w.shouldBypassHeaders() {
		return w.bypassAndFlush(p)
	}
	if err := w.startGzip(); err != nil {
		return 0, err
	}
	return w.gz.Write(p)
}

func (w *compressResponseWriter) writeGzipString(s string) (int, error) {
	total := 0
	for len(s) > 0 {
		chunk := s
		if len(chunk) > maxPooledCompressBuf {
			chunk = chunk[:maxPooledCompressBuf]
		}
		w.buf = append(w.buf[:0], chunk...)
		n, err := w.gz.Write(w.buf)
		w.buf = w.buf[:0]
		total += n
		if err != nil {
			return total, err
		}
		s = s[len(chunk):]
	}
	return total, nil
}

func (w *compressResponseWriter) WriteString(s string) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.writtenBytes += int64(len(s))
	switch w.mode {
	case compressBypassed:
		return io.WriteString(w.rw, s)
	case compressEnabled:
		return w.writeGzipString(s)
	}

	if len(w.buf)+len(s) < w.minLength {
		w.buf = append(w.buf, s...)
		return len(s), nil
	}

	h := w.rw.Header()
	if len(h["Content-Type"]) == 0 {
		var sniffBuf [512]byte
		n := copy(sniffBuf[:], w.buf)
		if n < len(sniffBuf) {
			n += copy(sniffBuf[n:], s)
		}
		h["Content-Type"] = []string{http.DetectContentType(sniffBuf[:n])}
	}
	if w.shouldBypassHeaders() {
		return w.bypassAndFlushString(s)
	}
	if err := w.startGzip(); err != nil {
		return 0, err
	}
	return w.writeGzipString(s)
}

// FlushError implements the interface recognized by [http.ResponseController.Flush].
func (w *compressResponseWriter) FlushError() error {
	if w.mode == compressUndecided {
		if !w.wroteHeader {
			w.WriteHeader(http.StatusOK)
		}
		if w.mode == compressUndecided {
			if _, err := w.bypassAndFlush(nil); err != nil {
				return err
			}
		}
	}
	if w.mode == compressEnabled && w.gz != nil {
		if err := w.gz.Flush(); err != nil {
			return err
		}
	}
	return http.NewResponseController(w.rw).Flush()
}

// Flush implements [http.Flusher].
func (w *compressResponseWriter) Flush() {
	_ = w.FlushError()
}

func (w *compressResponseWriter) finish() {
	switch w.mode {
	case compressEnabled:
		if w.gz != nil {
			err := w.gz.Close()
			if err == nil {
				w.gz.Reset(io.Discard)
				w.pool.Put(w.gz)
			}
			w.gz = nil
		}
	case compressUndecided:
		if w.wroteHeader || len(w.buf) > 0 {
			_, _ = w.bypassAndFlush(nil)
		}
	}
}

// addVaryToken adds token to the Vary response header if not already present.
func addVaryToken(h http.Header, token string) {
	existing := h["Vary"]
	if len(existing) == 0 {
		if token == "Accept-Encoding" {
			h["Vary"] = headerValVaryAcceptEncoding
			return
		}
		if token == "Origin" {
			h["Vary"] = headerValVaryOrigin
			return
		}
		h["Vary"] = []string{token}
		return
	}
	for _, v := range existing {
		for part := v; part != ""; {
			var tok string
			tok, part, _ = strings.Cut(part, ",")
			if asciiEqualFold(trimOWS(tok), token) {
				return
			}
		}
	}
	h["Vary"] = append(existing, token)
}

// acceptsGzip reports whether an Accept-Encoding header permits gzip encoding
// per RFC 9110 Section 12.5.3 (respecting q=0 rejection).
func acceptsGzip(header string) bool {
	if header == "" {
		return false
	}
	gzipSpecified := false
	gzipAllowed := false
	starAllowed := false
	for header != "" {
		var item string
		item, header, _ = strings.Cut(header, ",")
		item = trimOWS(item)
		if item == "" {
			continue
		}
		coding, params, hasParams := strings.Cut(item, ";")
		coding = trimOWS(coding)
		qOK := true
		if hasParams {
			qOK = parseQualityPositive(params)
		}
		if asciiEqualFold(coding, "gzip") || asciiEqualFold(coding, "x-gzip") {
			gzipSpecified = true
			if qOK {
				gzipAllowed = true
			}
		} else if coding == "*" {
			starAllowed = qOK
		}
	}
	if gzipSpecified {
		return gzipAllowed
	}
	return starAllowed
}

func parseQualityPositive(params string) bool {
	for params != "" {
		var p string
		p, params, _ = strings.Cut(params, ";")
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		k = trimOWS(k)
		if len(k) != 1 || (k[0] != 'q' && k[0] != 'Q') {
			continue
		}
		f, err := strconv.ParseFloat(trimOWS(v), 64)
		if err != nil {
			return false
		}
		return f > 0
	}
	return true
}

func isIncompressibleContentType(ct string) bool {
	if semi := strings.IndexByte(ct, ';'); semi >= 0 {
		ct = ct[:semi]
	}
	ct = trimOWS(ct)
	if asciiEqualFold(ct, "text/event-stream") {
		return true
	}
	if asciiHasPrefixFold(ct, "image/") {
		return !asciiEqualFold(ct, "image/svg+xml")
	}
	if asciiHasPrefixFold(ct, "video/") || asciiHasPrefixFold(ct, "audio/") {
		return true
	}
	if asciiEqualFold(ct, "font/woff2") ||
		asciiEqualFold(ct, "application/gzip") ||
		asciiEqualFold(ct, "application/x-gzip") ||
		asciiEqualFold(ct, "application/zip") ||
		asciiEqualFold(ct, "application/zstd") ||
		asciiEqualFold(ct, "application/br") {
		return true
	}
	return false
}

func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func asciiHasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return asciiEqualFold(s[:len(prefix)], prefix)
}
