// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"bufio"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	syncv2 "sync/v2"
)

const (
	defaultCompressMinLength = 1024
	maxPooledCompressBuf     = 4096
	maxContentEncodingLayers = 4
	compressedInputHeadroom  = 64 << 10
)

var (
	headerValGzip               = []string{"gzip"}[:1:1]
	headerValDeflate            = []string{"deflate"}[:1:1]
	headerValZstd               = []string{"zstd"}[:1:1]
	headerValBrotli             = []string{"br"}[:1:1]
	headerValVaryAcceptEncoding = []string{"Accept-Encoding"}[:1:1]
)

// Encoding identifies an HTTP content coding (RFC 9110 Section 8.4).
type Encoding string

const (
	// EncodingNone disables compression or represents no content coding.
	EncodingNone Encoding = ""

	// EncodingGzip is the gzip content coding (RFC 1952).
	EncodingGzip Encoding = "gzip"

	// EncodingDeflate is the zlib-wrapped deflate content coding
	// (RFC 1950 / RFC 9110 Section 8.4.2).
	EncodingDeflate Encoding = "deflate"

	// EncodingZstd is the Zstandard content coding (RFC 8878).
	EncodingZstd Encoding = "zstd"

	// EncodingBrotli is the Brotli content coding (RFC 7932, wire token "br").
	EncodingBrotli Encoding = "br"
)

const (
	encIdxGzip = iota
	encIdxDeflate
	encIdxZstd
	encIdxBrotli
	numEncodings
)

var defaultEncodingOrder = []Encoding{
	EncodingZstd,
	EncodingBrotli,
	EncodingGzip,
	EncodingDeflate,
}

func encodingIndex(enc Encoding) (int, Encoding, bool) {
	s := string(enc)
	switch len(s) {
	case 2:
		if asciiEqualFold(s, "br") {
			return encIdxBrotli, EncodingBrotli, true
		}
	case 4:
		if asciiEqualFold(s, "gzip") {
			return encIdxGzip, EncodingGzip, true
		}
		if asciiEqualFold(s, "zstd") {
			return encIdxZstd, EncodingZstd, true
		}
	case 6:
		if asciiEqualFold(s, "x-gzip") {
			return encIdxGzip, EncodingGzip, true
		}
		if asciiEqualFold(s, "brotli") {
			return encIdxBrotli, EncodingBrotli, true
		}
	case 7:
		if asciiEqualFold(s, "deflate") {
			return encIdxDeflate, EncodingDeflate, true
		}
	}
	return -1, EncodingNone, false
}

func headerSliceForEncoding(enc Encoding) []string {
	switch enc {
	case EncodingGzip:
		return headerValGzip
	case EncodingDeflate:
		return headerValDeflate
	case EncodingZstd:
		return headerValZstd
	case EncodingBrotli:
		return headerValBrotli
	default:
		return []string{string(enc)}
	}
}

// Compressor creates streaming compression writers for HTTP response bodies.
//
// Returned [io.WriteCloser] values that implement Reset(io.Writer) may be
// pooled and reused across requests by the server. Returned writers that
// implement Flush() error (or Flush()) are flushed when the response is flushed.
type Compressor interface {
	NewWriter(dst io.Writer) (io.WriteCloser, error)
}

// CompressorFunc is a function adapter implementing [Compressor].
type CompressorFunc func(dst io.Writer) (io.WriteCloser, error)

// NewWriter implements [Compressor].
func (f CompressorFunc) NewWriter(dst io.Writer) (io.WriteCloser, error) {
	return f(dst)
}

// Decompressor creates streaming decompression readers for HTTP request bodies.
//
// Returned [io.ReadCloser] values that implement Reset(io.Reader) error may be
// pooled and reused across requests by the server.
type Decompressor interface {
	NewReader(src io.Reader) (io.ReadCloser, error)
}

// DecompressorFunc is a function adapter implementing [Decompressor].
type DecompressorFunc func(src io.Reader) (io.ReadCloser, error)

// NewReader implements [Decompressor].
func (f DecompressorFunc) NewReader(src io.Reader) (io.ReadCloser, error) {
	return f(src)
}

type resettableWriter interface {
	Reset(io.Writer)
}

type resettableReader interface {
	Reset(io.Reader) error
}

// NewGzipCompressor returns a [Compressor] using [compress/gzip] at level.
// Level 0 uses [gzip.DefaultCompression]. It panics if level is outside the
// valid [compress/gzip] range.
func NewGzipCompressor(level int) Compressor {
	if level == 0 {
		level = gzip.DefaultCompression
	}
	if level < gzip.HuffmanOnly || level > gzip.BestCompression {
		panic(fmt.Sprintf("server: invalid gzip compression level %d", level))
	}
	return CompressorFunc(func(dst io.Writer) (io.WriteCloser, error) {
		return gzip.NewWriterLevel(dst, level)
	})
}

// NewDeflateCompressor returns a [Compressor] using [compress/zlib] (RFC 1950
// zlib-wrapped deflate per RFC 9110 Section 8.4.2) at level. Level 0 uses
// [zlib.DefaultCompression]. It panics if level is outside the valid
// [compress/zlib] range.
func NewDeflateCompressor(level int) Compressor {
	if level == 0 {
		level = zlib.DefaultCompression
	}
	if level < zlib.HuffmanOnly || level > zlib.BestCompression {
		panic(fmt.Sprintf("server: invalid deflate compression level %d", level))
	}
	return CompressorFunc(func(dst io.Writer) (io.WriteCloser, error) {
		return zlib.NewWriterLevel(dst, level)
	})
}

// eofByteReader is a stateless io.Reader and io.ByteReader used to clear
// references held by pooled decompression readers without retaining request data.
type eofByteReader struct{}

func (eofByteReader) Read([]byte) (int, error) { return 0, io.EOF }
func (eofByteReader) ReadByte() (byte, error)  { return 0, io.EOF }

type deflateDecompressCloser struct {
	rc io.ReadCloser
}

func (d *deflateDecompressCloser) Read(p []byte) (int, error) {
	return d.rc.Read(p)
}

func (d *deflateDecompressCloser) Close() error {
	return d.rc.Close()
}

func (d *deflateDecompressCloser) Reset(src io.Reader) error {
	return d.rc.(zlib.Resetter).Reset(src, nil)
}

var (
	defaultGzipCompressor    = NewGzipCompressor(gzip.DefaultCompression)
	defaultDeflateCompressor = NewDeflateCompressor(zlib.DefaultCompression)

	defaultGzipDecompressor = DecompressorFunc(func(src io.Reader) (io.ReadCloser, error) {
		return gzip.NewReader(src)
	})
	defaultDeflateDecompressor = DecompressorFunc(func(src io.Reader) (io.ReadCloser, error) {
		r, err := zlib.NewReader(src)
		if err != nil {
			return nil, err
		}
		return &deflateDecompressCloser{rc: r}, nil
	})
)

type codecHolder struct {
	comp Compressor
	pool *syncv2.Pool[io.WriteCloser]
}

func newCodecHolder(c Compressor) *codecHolder {
	if c == nil {
		return nil
	}
	return &codecHolder{
		comp: c,
		pool: syncv2.NewPool(func() io.WriteCloser { return nil }),
	}
}

func (h *codecHolder) acquire(dst io.Writer) (io.WriteCloser, error) {
	if wc := h.pool.Get(); wc != nil {
		wc.(resettableWriter).Reset(dst)
		return wc, nil
	}
	return h.comp.NewWriter(dst)
}

func (h *codecHolder) release(wc io.WriteCloser, healthy bool) {
	if !healthy || wc == nil {
		return
	}
	if rw, ok := wc.(resettableWriter); ok {
		rw.Reset(io.Discard)
		h.pool.Put(wc)
	}
}

type decompressorHolder struct {
	decomp Decompressor
	pool   *syncv2.Pool[io.ReadCloser]
}

func newDecompressorHolder(d Decompressor) *decompressorHolder {
	if d == nil {
		return nil
	}
	return &decompressorHolder{
		decomp: d,
		pool:   syncv2.NewPool(func() io.ReadCloser { return nil }),
	}
}

func (h *decompressorHolder) acquire(src io.Reader) (io.ReadCloser, bool, error) {
	if rc := h.pool.Get(); rc != nil {
		if err := rc.(resettableReader).Reset(src); err != nil {
			return nil, false, err
		}
		return rc, true, nil
	}
	rc, err := h.decomp.NewReader(src)
	if err != nil {
		return nil, false, err
	}
	_, resettable := rc.(resettableReader)
	return rc, resettable, nil
}

func (h *decompressorHolder) release(rc io.ReadCloser, resettable bool, healthy bool) {
	if rc == nil {
		return
	}
	err := rc.Close()
	if !healthy || !resettable || err != nil {
		return
	}
	if rr, ok := rc.(resettableReader); ok {
		_ = rr.Reset(eofByteReader{})
		h.pool.Put(rc)
	}
}

// CompressionConfig configures automatic request decompression and response
// compression on a [*Server].
//
// Zero values enable gzip and deflate codecs by default, apply a 1024-byte
// minimum response length threshold, and bound automatic request decompression
// using Config.MaxBodyBytes.
type CompressionConfig struct {
	// Disabled disables automatic response compression when true.
	Disabled bool

	// DisableDecompression disables automatic request body decompression
	// when true.
	DisableDecompression bool

	// MinLength is the minimum uncompressed response body size in bytes before
	// compression is applied. Default (<= 0) is 1024 bytes.
	MinLength int

	// MaxDecompressedBytes caps the maximum uncompressed bytes produced by
	// automatic request decompression to prevent decompression bombs.
	// Default (0) uses the request's effective MaxBodyBytes (4 MiB by default).
	// Set to < 0 to disable the decompression cap.
	MaxDecompressedBytes int64

	// Encodings optionally specifies and orders the server's preferred
	// response encodings. Default priority is zstd, br, gzip, deflate (using
	// only encodings that have a registered Compressor). Passing EncodingNone
	// alone disables automatic response compression.
	Encodings []Encoding

	// Compressors optionally registers or overrides response compressors by
	// encoding. Setting an encoding to nil disables its default compressor.
	Compressors map[Encoding]Compressor

	// Decompressors optionally registers or overrides request decompressors by
	// encoding. Setting an encoding to nil disables its default decompressor.
	Decompressors map[Encoding]Decompressor

	// Skip optionally skips automatic response compression for matching requests.
	Skip func(*Context) bool
}

type compressionSnapshot struct {
	disabled             bool
	disableDecompression bool
	minLength            int
	maxDecompressedBytes int64
	order                []Encoding
	compressors          [numEncodings]*codecHolder
	decompressors        [numEncodings]*decompressorHolder
	hasAnyCompressor     bool
	hasAnyDecompressor   bool
	skip                 func(*Context) bool
}

func newCompressionSnapshot(cfg CompressionConfig) *compressionSnapshot {
	minLen := cfg.MinLength
	if minLen <= 0 {
		minLen = defaultCompressMinLength
	}

	cs := &compressionSnapshot{
		disabled:             cfg.Disabled,
		disableDecompression: cfg.DisableDecompression,
		minLength:            minLen,
		maxDecompressedBytes: cfg.MaxDecompressedBytes,
		skip:                 cfg.Skip,
	}

	cs.compressors[encIdxGzip] = newCodecHolder(defaultGzipCompressor)
	cs.compressors[encIdxDeflate] = newCodecHolder(defaultDeflateCompressor)
	cs.decompressors[encIdxGzip] = newDecompressorHolder(defaultGzipDecompressor)
	cs.decompressors[encIdxDeflate] = newDecompressorHolder(defaultDeflateDecompressor)

	for enc, comp := range cfg.Compressors {
		idx, _, ok := encodingIndex(enc)
		if !ok {
			panic(fmt.Sprintf("server: unsupported compression encoding %q", enc))
		}
		cs.compressors[idx] = newCodecHolder(comp)
	}
	for enc, decomp := range cfg.Decompressors {
		idx, _, ok := encodingIndex(enc)
		if !ok {
			panic(fmt.Sprintf("server: unsupported decompression encoding %q", enc))
		}
		cs.decompressors[idx] = newDecompressorHolder(decomp)
	}

	if cfg.Encodings != nil {
		cs.order = normalizeEncodingOrder(cfg.Encodings)
		if len(cs.order) == 0 {
			cs.disabled = true
		}
	} else {
		cs.order = defaultEncodingOrder
	}

	cs.recomputeFlags()
	return cs
}

func normalizeEncodingOrder(encodings []Encoding) []Encoding {
	if len(encodings) == 0 {
		return nil
	}
	var seen [numEncodings]bool
	out := make([]Encoding, 0, len(encodings))
	for _, enc := range encodings {
		if enc == EncodingNone || asciiEqualFold(string(enc), "none") || asciiEqualFold(string(enc), "off") || asciiEqualFold(string(enc), "identity") {
			continue
		}
		idx, norm, ok := encodingIndex(enc)
		if !ok {
			panic(fmt.Sprintf("server: unsupported compression encoding %q", enc))
		}
		if !seen[idx] {
			seen[idx] = true
			out = append(out, norm)
		}
	}
	return out
}

func (cs *compressionSnapshot) clone() *compressionSnapshot {
	next := *cs
	if cs.order != nil {
		next.order = append([]Encoding(nil), cs.order...)
	}
	return &next
}

func (cs *compressionSnapshot) recomputeFlags() {
	cs.hasAnyCompressor = false
	for _, enc := range cs.order {
		if idx, _, ok := encodingIndex(enc); ok && cs.compressors[idx] != nil {
			cs.hasAnyCompressor = true
			break
		}
	}
	cs.hasAnyDecompressor = false
	for _, h := range cs.decompressors {
		if h != nil {
			cs.hasAnyDecompressor = true
			break
		}
	}
}

var (
	cwPool = syncv2.NewPool(func() *compressResponseWriter {
		return &compressResponseWriter{}
	})
	decompPool = syncv2.NewPool(func() *decompressReadCloser {
		return &decompressReadCloser{}
	})
)

func negotiateEncoding(headers []string, cs *compressionSnapshot) (Encoding, *codecHolder) {
	if len(headers) == 0 || !cs.hasAnyCompressor || len(cs.order) == 0 {
		return EncodingNone, nil
	}

	// Fast path for a single unadorned encoding token.
	if len(headers) == 1 {
		h := headers[0]
		if strings.IndexByte(h, ',') < 0 && strings.IndexByte(h, ';') < 0 {
			tok := trimOWS(h)
			if tok == "*" {
				for _, enc := range cs.order {
					if idx, _, ok := encodingIndex(enc); ok && cs.compressors[idx] != nil {
						return enc, cs.compressors[idx]
					}
				}
				return EncodingNone, nil
			}
			if idx, norm, ok := encodingIndex(Encoding(tok)); ok && cs.compressors[idx] != nil {
				for _, allowed := range cs.order {
					if allowed == norm {
						return norm, cs.compressors[idx]
					}
				}
			}
			return EncodingNone, nil
		}
	}

	var specified [numEncodings]bool
	var qMax [numEncodings]int
	var denied [numEncodings]bool
	starQ := 0
	starDenied := false

	for _, header := range headers {
		for header != "" {
			var item string
			item, header, _ = strings.Cut(header, ",")
			item = trimOWS(item)
			if item == "" {
				continue
			}
			coding, params, hasParams := strings.Cut(item, ";")
			coding = trimOWS(coding)
			q := 1000
			if hasParams {
				var qok bool
				q, qok = parseQualityParam(params)
				if !qok {
					continue
				}
			}
			if coding == "*" {
				if q == 0 {
					starDenied = true
				} else if q > starQ {
					starQ = q
				}
				continue
			}
			if idx, _, ok := encodingIndex(Encoding(coding)); ok {
				specified[idx] = true
				if q == 0 {
					denied[idx] = true
				} else if q > qMax[idx] {
					qMax[idx] = q
				}
			}
		}
	}

	var bestEnc Encoding
	var bestHolder *codecHolder
	bestQ := 0
	for _, enc := range cs.order {
		idx, norm, ok := encodingIndex(enc)
		if !ok {
			continue
		}
		holder := cs.compressors[idx]
		if holder == nil {
			continue
		}
		var effQ int
		if specified[idx] {
			if denied[idx] {
				continue
			}
			effQ = qMax[idx]
		} else {
			if starDenied {
				continue
			}
			effQ = starQ
		}
		if effQ > bestQ {
			bestQ = effQ
			bestEnc = norm
			bestHolder = holder
		}
	}
	return bestEnc, bestHolder
}

// limitedByteCountReader bounds raw compressed bytes read from the underlying
// request body to prevent compressed-header/block DoS bombs.
type limitedByteCountReader struct {
	r         io.Reader
	remaining int64
	exceeded  bool
}

func (l *limitedByteCountReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		var probe [1]byte
		n, err := l.r.Read(probe[:])
		if n > 0 {
			l.exceeded = true
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	return n, err
}

type decompressReadCloser struct {
	ctx             *Context
	rawSrc          io.ReadCloser
	srcLimit        limitedByteCountReader
	layerLimits     [maxContentEncodingLayers - 1]limitedByteCountReader
	layers          [maxContentEncodingLayers]*decompressorHolder
	opened          [maxContentEncodingLayers]io.ReadCloser
	openedPooled    [maxContentEncodingLayers]bool
	nLayers         int
	nOpened         int
	reader          io.Reader
	configuredLimit int64
	limit           int64
	srcErrLimit     int64
	decompressed    int64
	initialized     bool
	eof             bool
	failed          bool
	closedSrc       bool
	layersClosed    bool
	err             error
}

func (d *decompressReadCloser) limitExceeded() bool {
	if d.srcLimit.exceeded {
		return true
	}
	for i := 0; i < d.nLayers-1; i++ {
		if d.layerLimits[i].exceeded {
			return true
		}
	}
	return false
}

func (d *decompressReadCloser) init() error {
	d.initialized = true
	limit := d.configuredLimit
	if limit == 0 && d.ctx != nil {
		limit = d.ctx.effectiveMaxBodyBytes()
	}
	d.limit = limit
	d.srcErrLimit = limit

	var cur io.Reader = d.rawSrc
	var rawLimit int64
	if limit > 0 {
		rawLimit = limit
		if rawLimit <= (1<<63-1)-compressedInputHeadroom {
			rawLimit += compressedInputHeadroom
		} else {
			rawLimit = 1<<63 - 1
		}
		d.srcLimit = limitedByteCountReader{r: d.rawSrc, remaining: rawLimit}
		cur = &d.srcLimit
	} else if d.ctx != nil {
		if bodyLimit := d.ctx.effectiveMaxBodyBytes(); bodyLimit > 0 {
			d.srcErrLimit = bodyLimit
			rawLimit = bodyLimit
			if rawLimit <= (1<<63-1)-compressedInputHeadroom {
				rawLimit += compressedInputHeadroom
			} else {
				rawLimit = 1<<63 - 1
			}
			d.srcLimit = limitedByteCountReader{r: d.rawSrc, remaining: rawLimit}
			cur = &d.srcLimit
		}
	}

	for i := 0; i < d.nLayers; i++ {
		if rawLimit > 0 && i > 0 {
			d.layerLimits[i-1] = limitedByteCountReader{r: cur, remaining: rawLimit}
			cur = &d.layerLimits[i-1]
		}
		rc, resettable, err := d.layers[i].acquire(cur)
		if err != nil {
			d.failed = true
			d.closeOpenedLayers(false)
			d.err = WrapError(http.StatusBadRequest, "invalid compressed request body", fmt.Errorf("%w: %w", ErrInvalidCompressedBody, err))
			return d.err
		}
		d.opened[i] = rc
		d.openedPooled[i] = resettable
		d.nOpened = i + 1
		cur = rc
	}
	d.reader = cur
	return nil
}

func (d *decompressReadCloser) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	if !d.initialized {
		if err := d.init(); err != nil {
			return 0, err
		}
	}
	if d.limit > 0 && d.decompressed >= d.limit {
		// Probe for one more byte to verify whether the stream actually ends at limit.
		var probe [1]byte
		n, err := d.reader.Read(probe[:])
		exceeded := d.limitExceeded()
		if exceeded || n > 0 {
			d.failed = true
			if d.ctx != nil {
				d.ctx.markRequestTooLarge()
			}
			errLimit := d.limit
			if exceeded && errLimit <= 0 {
				errLimit = d.srcErrLimit
			}
			d.err = WrapError(http.StatusRequestEntityTooLarge, "request body too large", fmt.Errorf("%w: %w", ErrBodyTooLarge, &http.MaxBytesError{Limit: errLimit}))
			return 0, d.err
		}
		if err == io.EOF {
			d.eof = true
			d.err = io.EOF
			return 0, io.EOF
		}
		if err != nil {
			d.failed = true
			d.err = d.classifyReadError(err)
			return 0, d.err
		}
		return 0, nil
	}

	if d.limit > 0 && int64(len(p)) > d.limit-d.decompressed {
		p = p[:d.limit-d.decompressed]
	}

	n, err := d.reader.Read(p)
	if d.limitExceeded() {
		d.failed = true
		if d.ctx != nil {
			d.ctx.markRequestTooLarge()
		}
		errLimit := d.limit
		if errLimit <= 0 {
			errLimit = d.srcErrLimit
		}
		d.err = WrapError(http.StatusRequestEntityTooLarge, "request body too large", fmt.Errorf("%w: %w", ErrBodyTooLarge, &http.MaxBytesError{Limit: errLimit}))
		return 0, d.err
	}
	d.decompressed += int64(n)
	if err != nil {
		if err == io.EOF {
			d.eof = true
			d.err = io.EOF
			return n, io.EOF
		}
		d.failed = true
		d.err = d.classifyReadError(err)
		return n, d.err
	}
	return n, nil
}

func (d *decompressReadCloser) classifyReadError(err error) error {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return WrapError(http.StatusRequestEntityTooLarge, "request body too large", fmt.Errorf("%w: %w", ErrBodyTooLarge, maxErr))
	}
	return WrapError(http.StatusBadRequest, "invalid compressed request body", fmt.Errorf("%w: %w", ErrInvalidCompressedBody, err))
}

func (d *decompressReadCloser) closeOpenedLayers(healthy bool) {
	if d.layersClosed {
		return
	}
	d.layersClosed = true
	for i := d.nOpened - 1; i >= 0; i-- {
		if d.opened[i] != nil {
			d.layers[i].release(d.opened[i], d.openedPooled[i], healthy)
			d.opened[i] = nil
			d.openedPooled[i] = false
		}
	}
	d.nOpened = 0
}

func (d *decompressReadCloser) Close() error {
	d.closeOpenedLayers(d.eof && !d.failed)
	if !d.closedSrc && d.rawSrc != nil {
		d.closedSrc = true
		return d.rawSrc.Close()
	}
	return nil
}

func (d *decompressReadCloser) finish(panicked bool) {
	d.closeOpenedLayers(d.eof && !d.failed && !panicked)
	for i := 0; i < d.nLayers; i++ {
		d.layers[i] = nil
	}
	for i := range d.layerLimits {
		d.layerLimits[i] = limitedByteCountReader{}
	}
	d.ctx = nil
	d.rawSrc = nil
	d.srcLimit = limitedByteCountReader{}
	d.reader = nil
	d.nLayers = 0
	d.nOpened = 0
	d.configuredLimit = 0
	d.limit = 0
	d.decompressed = 0
	d.initialized = false
	d.eof = false
	d.failed = false
	d.closedSrc = false
	d.layersClosed = false
	d.err = nil
	decompPool.Put(d)
}

func prepareRequestDecompression(c *Context, cs *compressionSnapshot) error {
	if cs == nil || cs.disableDecompression || !cs.hasAnyDecompressor {
		return nil
	}
	r := c.req
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	ceHeaders := r.Header["Content-Encoding"]
	if len(ceHeaders) == 0 {
		return nil
	}

	var tokensBuf [8]string
	tokens := tokensBuf[:0]
	for _, h := range ceHeaders {
		for h != "" {
			var tok string
			tok, h, _ = strings.Cut(h, ",")
			tok = trimOWS(tok)
			if tok == "" {
				continue
			}
			tokens = append(tokens, tok)
			if len(tokens) > maxContentEncodingLayers {
				return WrapError(http.StatusBadRequest, "too many content-encoding layers", ErrInvalidCompressedBody)
			}
		}
	}
	if len(tokens) == 0 {
		return nil
	}

	var matched [maxContentEncodingLayers]*decompressorHolder
	nMatched := 0
	remainingEnd := len(tokens)

	// Peel applied content codings from outermost (rightmost) to innermost.
	for i := len(tokens) - 1; i >= 0; i-- {
		tok := tokens[i]
		if asciiEqualFold(tok, "identity") {
			remainingEnd = i
			continue
		}
		idx, _, ok := encodingIndex(Encoding(tok))
		if !ok || cs.decompressors[idx] == nil {
			break
		}
		matched[nMatched] = cs.decompressors[idx]
		nMatched++
		remainingEnd = i
	}

	if remainingEnd == 0 && nMatched == 0 {
		// Only "identity" tokens were present; normalize header away.
		delete(r.Header, "Content-Encoding")
		return nil
	}
	if nMatched == 0 {
		// Outermost encoding has no registered decompressor; leave untouched.
		return nil
	}

	if remainingEnd == 0 {
		delete(r.Header, "Content-Encoding")
	} else {
		r.Header["Content-Encoding"] = []string{strings.Join(tokens[:remainingEnd], ", ")}
	}
	delete(r.Header, "Content-Length")
	r.ContentLength = -1

	d := decompPool.Get()
	d.ctx = c
	d.rawSrc = r.Body
	d.layers = matched
	d.nLayers = nMatched
	d.configuredLimit = cs.maxDecompressedBytes
	d.initialized = false
	d.eof = false
	d.failed = false
	d.closedSrc = false
	d.layersClosed = false
	d.err = nil

	r.Body = d
	c.decomp = d
	return nil
}

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
	holder := newCodecHolder(NewGzipCompressor(cfg.Level))
	minLen := cfg.MinLength
	if minLen <= 0 {
		minLen = defaultCompressMinLength
	}

	return func(c *Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			c.DisableCompression()
			return c.Next()
		}
		if c.noCompress || c.Method() == http.MethodHead {
			return c.Next()
		}

		// Always advertise Vary: Accept-Encoding so caches key on encoding.
		addVaryToken(c.respHeader(), "Accept-Encoding")

		if !acceptsGzip(c.req.Header["Accept-Encoding"]) {
			return c.Next()
		}

		prevWriter := c.writer
		prevCW := c.cw
		cw := cwPool.Get()
		cw.reset(c.ResponseWriter(), holder, EncodingGzip, minLen, c.req.URL.Path)

		c.writer = cw
		c.cw = cw
		panicked := true
		defer func() {
			c.writer = prevWriter
			c.cw = prevCW
			cw.release(panicked)
		}()

		err := c.Next()
		if err != nil && !c.errHandled {
			c.HandleError(err)
		}
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
	holder       *codecHolder
	encoding     Encoding
	encWriter    io.WriteCloser
	buf          []byte
	minLength    int
	reqPath      string
	status       int
	writtenBytes int64
	wroteHeader  bool
	disabled     bool
	mode         compressMode
}

func (w *compressResponseWriter) reset(rw http.ResponseWriter, holder *codecHolder, enc Encoding, minLen int, reqPath string) {
	w.rw = rw
	w.holder = holder
	w.encoding = enc
	w.encWriter = nil
	w.buf = w.buf[:0]
	w.minLength = minLen
	w.reqPath = reqPath
	w.status = 0
	w.writtenBytes = 0
	w.wroteHeader = false
	w.disabled = false
	w.mode = compressUndecided
}

func (w *compressResponseWriter) release(panicked bool) {
	if panicked {
		// On panic, discard the encoder writer rather than returning
		// potentially half-written state to the pool.
		w.encWriter = nil
	} else {
		w.finish()
	}
	w.rw = nil
	w.holder = nil
	w.encoding = EncodingNone
	w.reqPath = ""
	if cap(w.buf) > maxPooledCompressBuf {
		w.buf = nil
	} else {
		w.buf = w.buf[:0]
	}
	cwPool.Put(w)
}

func (w *compressResponseWriter) Header() http.Header {
	return w.rw.Header()
}

func (w *compressResponseWriter) Unwrap() http.ResponseWriter {
	return w.rw
}

func (w *compressResponseWriter) shouldBypassHeaders() bool {
	h := w.rw.Header()
	if ce := h["Content-Encoding"]; len(ce) > 0 {
		for _, v := range ce {
			if trimOWS(v) != "" {
				return true
			}
		}
	}
	if cr := h["Content-Range"]; len(cr) > 0 && cr[0] != "" {
		return true
	}
	if up := h["Upgrade"]; len(up) > 0 && up[0] != "" {
		return true
	}
	if cc := h["Cache-Control"]; len(cc) > 0 && hasCacheDirective(cc, "no-transform") {
		return true
	}
	var contentType string
	if ct := h["Content-Type"]; len(ct) > 0 && ct[0] != "" {
		contentType = ct[0]
		if isIncompressibleContentType(contentType) {
			return true
		}
	}
	if (contentType == "" || isGenericFallbackContentType(contentType)) && hasCompressedExtension(w.reqPath) {
		return true
	}
	return false
}

func (w *compressResponseWriter) knownSmallContentLength() bool {
	h := w.rw.Header()
	if cl := h["Content-Length"]; len(cl) == 1 && cl[0] != "" {
		if n, err := strconv.ParseInt(cl[0], 10, 64); err == nil && n >= 0 && n < int64(w.minLength) {
			return true
		}
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

	if w.disabled ||
		code == http.StatusSwitchingProtocols ||
		code == http.StatusNoContent ||
		code == http.StatusResetContent ||
		code == http.StatusPartialContent ||
		code == http.StatusNotModified ||
		w.shouldBypassHeaders() ||
		w.knownSmallContentLength() {
		w.mode = compressBypassed
		w.rw.WriteHeader(code)
	}
}

func (w *compressResponseWriter) disable() {
	w.disabled = true
	if w.mode == compressUndecided {
		if w.wroteHeader || len(w.buf) > 0 {
			_, _ = w.bypassAndFlush(nil)
		} else {
			w.mode = compressBypassed
		}
	}
	for rw := w.rw; rw != nil; {
		if inner, ok := rw.(*compressResponseWriter); ok {
			inner.disable()
			break
		}
		u, ok := rw.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		rw = u.Unwrap()
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

func (w *compressResponseWriter) startCompress() (bool, error) {
	if w.holder == nil {
		return false, nil
	}
	encWriter, err := w.holder.acquire(w.rw)
	if err != nil {
		return false, err
	}

	h := w.rw.Header()
	delete(h, "Content-Length")
	delete(h, "Accept-Ranges")
	// RFC 9110 Section 8.8.3.3: weaken strong ETag when applying content coding.
	if etags := h["Etag"]; len(etags) == 1 {
		if et := etags[0]; len(et) >= 2 && et[0] == '"' {
			h["Etag"] = []string{"W/" + et}
		}
	}
	h["Content-Encoding"] = headerSliceForEncoding(w.encoding)
	addVaryToken(h, "Accept-Encoding")

	code := w.status
	if code == 0 {
		code = http.StatusOK
	}
	w.rw.WriteHeader(code)

	w.encWriter = encWriter
	w.mode = compressEnabled

	if len(w.buf) > 0 {
		_, err := w.encWriter.Write(w.buf)
		w.buf = w.buf[:0]
		if err != nil {
			return true, err
		}
	}
	return true, nil
}

func (w *compressResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	switch w.mode {
	case compressBypassed:
		n, err := w.rw.Write(p)
		w.writtenBytes += int64(n)
		return n, err
	case compressEnabled:
		n, err := w.encWriter.Write(p)
		w.writtenBytes += int64(n)
		return n, err
	}

	if w.disabled {
		n, err := w.bypassAndFlush(p)
		w.writtenBytes += int64(n)
		return n, err
	}

	// Still undecided: buffer until reaching minLength.
	if len(w.buf)+len(p) < w.minLength {
		w.buf = append(w.buf, p...)
		w.writtenBytes += int64(len(p))
		return len(p), nil
	}

	var sniffBuf [512]byte
	var sample []byte
	if len(w.buf) == 0 {
		if len(p) > 512 {
			sample = p[:512]
		} else {
			sample = p
		}
	} else if len(w.buf) >= 512 {
		sample = w.buf[:512]
	} else {
		n := copy(sniffBuf[:], w.buf)
		n += copy(sniffBuf[n:], p)
		sample = sniffBuf[:n]
	}

	// Sniff Content-Type from up to 512 bytes of uncompressed data before
	// deciding to compress.
	h := w.rw.Header()
	if _, hasCT := h["Content-Type"]; !hasCT {
		h["Content-Type"] = []string{http.DetectContentType(sample)}
	}
	if w.shouldBypassHeaders() || isPrecompressedIfOpaque(h["Content-Type"], sample) {
		n, err := w.bypassAndFlush(p)
		w.writtenBytes += int64(n)
		return n, err
	}
	started, err := w.startCompress()
	if !started {
		// Compressor creation failed before headers were committed; fall back uncompressed.
		n, berr := w.bypassAndFlush(p)
		w.writtenBytes += int64(n)
		return n, berr
	}
	if err != nil {
		return 0, err
	}
	n, err := w.encWriter.Write(p)
	w.writtenBytes += int64(n)
	return n, err
}

func (w *compressResponseWriter) writeCompressedString(s string) (int, error) {
	total := 0
	for len(s) > 0 {
		chunk := s
		if len(chunk) > maxPooledCompressBuf {
			chunk = chunk[:maxPooledCompressBuf]
		}
		w.buf = append(w.buf[:0], chunk...)
		n, err := w.encWriter.Write(w.buf)
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
	switch w.mode {
	case compressBypassed:
		n, err := io.WriteString(w.rw, s)
		w.writtenBytes += int64(n)
		return n, err
	case compressEnabled:
		n, err := w.writeCompressedString(s)
		w.writtenBytes += int64(n)
		return n, err
	}

	if w.disabled {
		n, err := w.bypassAndFlushString(s)
		w.writtenBytes += int64(n)
		return n, err
	}

	if len(w.buf)+len(s) < w.minLength {
		w.buf = append(w.buf, s...)
		w.writtenBytes += int64(len(s))
		return len(s), nil
	}

	var sniffBuf [512]byte
	sn := copy(sniffBuf[:], w.buf)
	if sn < len(sniffBuf) {
		sn += copy(sniffBuf[sn:], s)
	}
	sample := sniffBuf[:sn]

	h := w.rw.Header()
	if _, hasCT := h["Content-Type"]; !hasCT {
		h["Content-Type"] = []string{http.DetectContentType(sample)}
	}
	if w.shouldBypassHeaders() || isPrecompressedIfOpaque(h["Content-Type"], sample) {
		n, err := w.bypassAndFlushString(s)
		w.writtenBytes += int64(n)
		return n, err
	}
	started, err := w.startCompress()
	if !started {
		n, berr := w.bypassAndFlushString(s)
		w.writtenBytes += int64(n)
		return n, berr
	}
	if err != nil {
		return 0, err
	}
	n, err := w.writeCompressedString(s)
	w.writtenBytes += int64(n)
	return n, err
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
	if w.mode == compressEnabled && w.encWriter != nil {
		switch f := w.encWriter.(type) {
		case interface{ Flush() error }:
			if err := f.Flush(); err != nil {
				return err
			}
		case interface{ Flush() }:
			f.Flush()
		}
	}
	return http.NewResponseController(w.rw).Flush()
}

// Flush implements [http.Flusher].
func (w *compressResponseWriter) Flush() {
	_ = w.FlushError()
}

// Hijack implements [http.Hijacker].
func (w *compressResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.mode == compressEnabled {
		return nil, nil, errors.New("server: cannot hijack compressed response")
	}
	if w.mode == compressUndecided {
		if w.wroteHeader || len(w.buf) > 0 {
			if _, err := w.bypassAndFlush(nil); err != nil {
				return nil, nil, err
			}
		} else {
			w.mode = compressBypassed
		}
	}
	return http.NewResponseController(w.rw).Hijack()
}

func (w *compressResponseWriter) finish() {
	switch w.mode {
	case compressEnabled:
		if w.encWriter != nil {
			err := w.encWriter.Close()
			if w.holder != nil {
				w.holder.release(w.encWriter, err == nil)
			}
			w.encWriter = nil
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

// acceptsGzip reports whether the Accept-Encoding header values permit gzip
// encoding per RFC 9110 Section 12.5.3 (respecting q=0 rejection).
func acceptsGzip(headers []string) bool {
	if len(headers) == 0 {
		return false
	}
	if len(headers) == 1 && headers[0] == "gzip" {
		return true
	}
	gzipSpecified := false
	gzipAllowed := false
	gzipDenied := false
	starAllowed := false
	starDenied := false
	for _, header := range headers {
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
				} else {
					gzipDenied = true
				}
			} else if coding == "*" {
				if qOK {
					starAllowed = true
				} else {
					starDenied = true
				}
			}
		}
	}
	if gzipSpecified {
		return gzipAllowed && !gzipDenied
	}
	return starAllowed && !starDenied
}

// parseQValue parses an RFC 9110 Section 12.4.2 qvalue ("0".."1" with up to 3
// fractional digits) into an integer in [0, 1000].
func parseQValue(v string) (int, bool) {
	v = trimOWS(v)
	if len(v) == 0 || len(v) > 5 {
		return 0, false
	}
	switch v[0] {
	case '1':
		if len(v) == 1 {
			return 1000, true
		}
		if v[1] != '.' {
			return 0, false
		}
		for i := 2; i < len(v); i++ {
			if v[i] != '0' {
				return 0, false
			}
		}
		return 1000, true
	case '0':
		if len(v) == 1 {
			return 0, true
		}
		if v[1] != '.' {
			return 0, false
		}
		q := 0
		scale := 100
		for i := 2; i < len(v); i++ {
			d := v[i]
			if d < '0' || d > '9' {
				return 0, false
			}
			q += int(d-'0') * scale
			scale /= 10
		}
		return q, true
	default:
		return 0, false
	}
}

func parseQualityParam(params string) (int, bool) {
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
		return parseQValue(v)
	}
	return 1000, true
}

func parseQualityPositive(params string) bool {
	q, ok := parseQualityParam(params)
	return ok && q > 0
}

func hasCacheDirective(headers []string, directive string) bool {
	for _, h := range headers {
		for h != "" {
			var item string
			item, h, _ = strings.Cut(h, ",")
			name, _, _ := strings.Cut(item, "=")
			if asciiEqualFold(trimOWS(name), directive) {
				return true
			}
		}
	}
	return false
}

func isOctetStreamContentType(ct string) bool {
	if semi := strings.IndexByte(ct, ';'); semi >= 0 {
		ct = ct[:semi]
	}
	return asciiEqualFold(trimOWS(ct), "application/octet-stream")
}

func isGenericFallbackContentType(ct string) bool {
	if semi := strings.IndexByte(ct, ';'); semi >= 0 {
		ct = ct[:semi]
	}
	ct = trimOWS(ct)
	return asciiEqualFold(ct, "application/octet-stream") || asciiEqualFold(ct, "text/plain")
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
	if asciiEqualFold(ct, "font/woff") ||
		asciiEqualFold(ct, "font/woff2") ||
		asciiEqualFold(ct, "application/font-woff") ||
		asciiEqualFold(ct, "application/font-woff2") ||
		asciiEqualFold(ct, "application/gzip") ||
		asciiEqualFold(ct, "application/x-gzip") ||
		asciiEqualFold(ct, "application/zip") ||
		asciiEqualFold(ct, "application/zstd") ||
		asciiEqualFold(ct, "application/x-zstd") ||
		asciiEqualFold(ct, "application/br") ||
		asciiEqualFold(ct, "application/x-brotli") ||
		asciiEqualFold(ct, "application/x-bzip2") ||
		asciiEqualFold(ct, "application/x-xz") ||
		asciiEqualFold(ct, "application/x-7z-compressed") ||
		asciiEqualFold(ct, "application/x-rar-compressed") ||
		asciiEqualFold(ct, "application/vnd.rar") ||
		asciiEqualFold(ct, "application/x-deflate") ||
		asciiEqualFold(ct, "application/zlib") ||
		asciiEqualFold(ct, "application/grpc") ||
		asciiHasPrefixFold(ct, "application/grpc+") {
		return true
	}
	return false
}

func hasCompressedExtension(p string) bool {
	if p == "" {
		return false
	}
	ext := path.Ext(p)
	if ext == "" {
		return false
	}
	switch len(ext) {
	case 3:
		return asciiEqualFold(ext, ".gz") ||
			asciiEqualFold(ext, ".br") ||
			asciiEqualFold(ext, ".xz") ||
			asciiEqualFold(ext, ".7z")
	case 4:
		return asciiEqualFold(ext, ".zst") ||
			asciiEqualFold(ext, ".zip") ||
			asciiEqualFold(ext, ".bz2") ||
			asciiEqualFold(ext, ".rar")
	case 5:
		return asciiEqualFold(ext, ".gzip") ||
			asciiEqualFold(ext, ".zstd") ||
			asciiEqualFold(ext, ".woff")
	case 6:
		return asciiEqualFold(ext, ".woff2")
	}
	return false
}

func isPrecompressedIfOpaque(ctHeader []string, sample []byte) bool {
	if len(ctHeader) > 0 && ctHeader[0] != "" && !isOctetStreamContentType(ctHeader[0]) {
		return false
	}
	return isPrecompressedSample(sample)
}

func isPrecompressedSample(b []byte) bool {
	n := len(b)
	if n >= 3 && b[0] == 0x1f && b[1] == 0x8b && b[2] == 0x08 {
		// gzip (RFC 1952)
		return true
	}
	if n >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd {
		// zstd frame (RFC 8878)
		return true
	}
	if n >= 4 && (b[0]&0xf0) == 0x50 && b[1] == 0x2a && b[2] == 0x4d && b[3] == 0x18 {
		// zstd skippable frame
		return true
	}
	if n >= 4 && b[0] == 0x50 && b[1] == 0x4b && b[2] == 0x03 && b[3] == 0x04 {
		// zip
		return true
	}
	if n >= 3 && b[0] == 'B' && b[1] == 'Z' && b[2] == 'h' {
		// bzip2
		return true
	}
	if n >= 6 && b[0] == 0xfd && b[1] == 0x37 && b[2] == 0x7a && b[3] == 0x58 && b[4] == 0x5a && b[5] == 0x00 {
		// xz
		return true
	}
	if n >= 6 && b[0] == 0x37 && b[1] == 0x7a && b[2] == 0xbc && b[3] == 0xaf && b[4] == 0x27 && b[5] == 0x1c {
		// 7z
		return true
	}
	if n >= 2 && b[0] == 0x78 && (b[1] == 0x01 || b[1] == 0x5e || b[1] == 0x9c || b[1] == 0xda) {
		// zlib (RFC 1950) common CMF/FLG headers
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
