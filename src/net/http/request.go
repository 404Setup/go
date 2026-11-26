// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// HTTP Request reading and parsing.

package http

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http/httptrace"
	"net/http/internal/ascii"
	"net/textproto"
	"net/url"
	urlpkg "net/url"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"sync/v2"
	"unsafe" // for linkname and zero-copy string conversions

	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/idna"
)

const (
	defaultMaxMemory = 32 << 20 // 32 MB
)

// ErrMissingFile is returned by FormFile when the provided file field name
// is either not present in the request or not a file field.
var ErrMissingFile = errors.New("http: no such file")

// ProtocolError represents an HTTP protocol error.
//
// Deprecated: Not all errors in the http package related to protocol errors
// are of type ProtocolError.
type ProtocolError struct {
	ErrorString string
}

func (pe *ProtocolError) Error() string { return pe.ErrorString }

// Is lets http.ErrNotSupported match errors.ErrUnsupported.
func (pe *ProtocolError) Is(err error) bool {
	return pe == ErrNotSupported && err == errors.ErrUnsupported
}

var (
	// ErrNotSupported indicates that a feature is not supported.
	//
	// It is returned by ResponseController methods to indicate that
	// the handler does not support the method, and by the Push method
	// of Pusher implementations to indicate that HTTP/2 Push support
	// is not available.
	ErrNotSupported = &ProtocolError{"feature not supported"}

	// Deprecated: ErrUnexpectedTrailer is no longer returned by
	// anything in the net/http package. Callers should not
	// compare errors against this variable.
	ErrUnexpectedTrailer = &ProtocolError{"trailer header without chunked transfer encoding"}

	// ErrMissingBoundary is returned by Request.MultipartReader when the
	// request's Content-Type does not include a "boundary" parameter.
	ErrMissingBoundary = &ProtocolError{"no multipart boundary param in Content-Type"}

	// ErrNotMultipart is returned by Request.MultipartReader when the
	// request's Content-Type is not multipart/form-data.
	ErrNotMultipart = &ProtocolError{"request Content-Type isn't multipart/form-data"}

	// Deprecated: ErrHeaderTooLong is no longer returned by
	// anything in the net/http package. Callers should not
	// compare errors against this variable.
	ErrHeaderTooLong = &ProtocolError{"header too long"}

	// Deprecated: ErrShortBody is no longer returned by
	// anything in the net/http package. Callers should not
	// compare errors against this variable.
	ErrShortBody = &ProtocolError{"entity body too short"}

	// Deprecated: ErrMissingContentLength is no longer returned by
	// anything in the net/http package. Callers should not
	// compare errors against this variable.
	ErrMissingContentLength = &ProtocolError{"missing ContentLength in HEAD response"}
)

func badStringError(what, val string) error { return fmt.Errorf("%s %q", what, val) }

// Headers that Request.Write handles itself and should be skipped.
var reqWriteExcludeHeader = map[string]bool{
	"Host":              true, // not in Header map anyway
	"User-Agent":        true,
	"Content-Length":    true,
	"Transfer-Encoding": true,
	"Trailer":           true,
}

// A Request represents an HTTP request received by a server
// or to be sent by a client.
//
// The field semantics differ slightly between client and server
// usage. In addition to the notes on the fields below, see the
// documentation for [Request.Write] and [RoundTripper].
type Request struct {
	// Method specifies the HTTP method (GET, POST, PUT, etc.).
	// For client requests, an empty string means GET.
	Method string

	// URL specifies either the URI being requested (for server
	// requests) or the URL to access (for client requests).
	//
	// For server requests, the URL is parsed from the URI
	// supplied on the Request-Line as stored in RequestURI.  For
	// most requests, fields other than Path and RawQuery will be
	// empty. (See RFC 7230, Section 5.3)
	//
	// For client requests, the URL's Host specifies the server to
	// connect to, while the Request's Host field optionally
	// specifies the Host header value to send in the HTTP
	// request.
	URL *url.URL

	// The protocol version for incoming server requests.
	//
	// For client requests, these fields are ignored. The HTTP
	// client code always uses either HTTP/1.1 or HTTP/2.
	// See the docs on Transport for details.
	Proto      string // "HTTP/1.0"
	ProtoMajor int    // 1
	ProtoMinor int    // 0

	// Header contains the request header fields either received
	// by the server or to be sent by the client.
	//
	// If a server received a request with header lines,
	//
	//	Host: example.com
	//	accept-encoding: gzip, deflate
	//	Accept-Language: en-us
	//	fOO: Bar
	//	foo: two
	//
	// then
	//
	//	Header = map[string][]string{
	//		"Accept-Encoding": {"gzip, deflate"},
	//		"Accept-Language": {"en-us"},
	//		"Foo": {"Bar", "two"},
	//	}
	//
	// For incoming requests, the Host header is promoted to the
	// Request.Host field and removed from the Header map.
	//
	// HTTP defines that header names are case-insensitive. The
	// request parser implements this by using CanonicalHeaderKey,
	// making the first character and any characters following a
	// hyphen uppercase and the rest lowercase.
	//
	// For client requests, certain headers such as Content-Length
	// and Connection are automatically written when needed and
	// values in Header may be ignored. See the documentation
	// for the Request.Write method.
	Header Header

	// Body is the request's body.
	//
	// For client requests, a nil body means the request has no
	// body, such as a GET request. The HTTP Client's Transport
	// is responsible for calling the Close method.
	//
	// For server requests, the Request Body is always non-nil
	// but will return EOF immediately when no body is present.
	// The Server will close the request body. The ServeHTTP
	// Handler does not need to.
	//
	// Body must allow Read to be called concurrently with Close.
	// In particular, calling Close should unblock a Read waiting
	// for input.
	Body io.ReadCloser

	// GetBody defines an optional func to return a new copy of
	// Body. It is used for client requests when a redirect requires
	// reading the body more than once. Use of GetBody still
	// requires setting Body.
	//
	// For server requests, it is unused.
	GetBody func() (io.ReadCloser, error)

	// ContentLength records the length of the associated content.
	// The value -1 indicates that the length is unknown.
	// Values >= 0 indicate that the given number of bytes may
	// be read from Body.
	//
	// For client requests, a value of 0 with a non-nil Body is
	// also treated as unknown.
	ContentLength int64

	// TransferEncoding lists the transfer encodings from outermost to
	// innermost. An empty list denotes the "identity" encoding.
	// TransferEncoding can usually be ignored; chunked encoding is
	// automatically added and removed as necessary when sending and
	// receiving requests.
	TransferEncoding []string

	// Close indicates whether to close the connection after
	// replying to this request (for servers) or after sending this
	// request and reading its response (for clients).
	//
	// For server requests, the HTTP server handles this automatically
	// and this field is not needed by Handlers.
	//
	// For client requests, setting this field prevents re-use of
	// TCP connections between requests to the same hosts, as if
	// Transport.DisableKeepAlives were set.
	Close    bool
	ctxState uint32

	// For server requests, Host specifies the host on which the
	// URL is sought. For HTTP/1 (per RFC 7230, section 5.4), this
	// is either the value of the "Host" header or the host name
	// given in the URL itself. For HTTP/2, it is the value of the
	// ":authority" pseudo-header field.
	// It may be of the form "host:port". For international domain
	// names, Host may be in Punycode or Unicode form. Use
	// golang.org/x/net/idna to convert it to either format if
	// needed.
	// To prevent DNS rebinding attacks, server Handlers should
	// validate that the Host header has a value for which the
	// Handler considers itself authoritative. The included
	// ServeMux supports patterns registered to particular host
	// names and thus protects its registered Handlers.
	//
	// For client requests, Host optionally overrides the Host
	// header to send. If empty, the Request.Write method uses
	// the value of URL.Host. Host may contain an international
	// domain name.
	Host string

	// Form contains the parsed form data, including both the URL
	// field's query parameters and the PATCH, POST, or PUT form data.
	// This field is only available after ParseForm is called.
	// The HTTP client ignores Form and uses Body instead.
	Form url.Values

	// PostForm contains the parsed form data from PATCH, POST
	// or PUT body parameters.
	//
	// This field is only available after ParseForm is called.
	// The HTTP client ignores PostForm and uses Body instead.
	PostForm url.Values

	// MultipartForm is the parsed multipart form, including file uploads.
	// This field is only available after ParseMultipartForm is called.
	// The HTTP client ignores MultipartForm and uses Body instead.
	MultipartForm *multipart.Form

	// Trailer specifies additional headers that are sent after the request
	// body.
	//
	// For server requests, the Trailer map initially contains only the
	// trailer keys, with nil values. (The client declares which trailers it
	// will later send.)  While the handler is reading from Body, it must
	// not reference Trailer. After reading from Body returns EOF, Trailer
	// can be read again and will contain non-nil values, if they were sent
	// by the client.
	//
	// For client requests, Trailer must be initialized to a map containing
	// the trailer keys to later send. The values may be nil or their final
	// values. The ContentLength must be 0 or -1, to send a chunked request.
	// After the HTTP request is sent the map values can be updated while
	// the request body is read. Once the body returns EOF, the caller must
	// not mutate Trailer.
	//
	// Writing a request whose Trailer contains a key with invalid bytes
	// (such as CR or LF), or such a value present when Write begins,
	// returns an error.
	//
	// Few HTTP clients, servers, or proxies support HTTP trailers.
	Trailer Header

	// RemoteAddr allows HTTP servers and other software to record
	// the network address that sent the request, usually for
	// logging. This field is not filled in by ReadRequest and
	// has no defined format. The HTTP server in this package
	// sets RemoteAddr to an "IP:port" address before invoking a
	// handler.
	// This field is ignored by the HTTP client.
	RemoteAddr string

	// RequestURI is the unmodified request-target of the
	// Request-Line (RFC 7230, Section 3.1.1) as sent by the client
	// to a server. Usually the URL field should be used instead.
	// It is an error to set this field in an HTTP client request.
	RequestURI string

	// TLS allows HTTP servers and other software to record
	// information about the TLS connection on which the request
	// was received. This field is not filled in by ReadRequest.
	// The HTTP server in this package sets the field for
	// TLS-enabled connections before invoking a handler;
	// otherwise it leaves the field nil.
	// This field is ignored by the HTTP client.
	TLS *tls.ConnectionState

	// Cancel is an optional channel whose closure indicates that the client
	// request should be regarded as canceled. Not all implementations of
	// RoundTripper may support Cancel.
	//
	// For server requests, this field is not applicable.
	//
	// Deprecated: Set the Request's context with NewRequestWithContext
	// instead. If a Request's Cancel field and context are both
	// set, it is undefined whether Cancel is respected.
	Cancel <-chan struct{}

	// Response is the redirect response which caused this request
	// to be created. This field is only populated during client
	// redirects.
	Response *Response

	// Pattern is the [ServeMux] pattern that matched the request.
	// It is empty if the request was not matched against a pattern.
	Pattern string

	// ctx is either the client or server context. It should only
	// be modified via copying the whole Request using Clone or WithContext.
	// It is unexported to prevent people from using Context wrong
	// and mutating the contexts held by callers of the same request.
	ctx context.Context

	// The following fields are for requests matched by ServeMux.
	pat       *pattern           // the pattern that matched
	matches   []string           // values for the matching wildcards in pat, followed by SetPathValue key/value pairs
	cancelCtx context.CancelFunc // for lazy server request context cancellation
}

const (
	ctxStateLazy     = 1 << 0
	ctxStateLocked   = 1 << 1
	ctxStateCreated  = 1 << 2
	ctxStateCanceled = 1 << 3
)

type serverReqContextProvider interface {
	lazyServerRequestContext() (context.Context, context.CancelFunc, bool)
}

func (r *Request) lazyContext() context.Context {
	for {
		s := atomic.LoadUint32(&r.ctxState)
		if s&ctxStateLocked == 0 && atomic.CompareAndSwapUint32(&r.ctxState, s, s|ctxStateLocked) {
			if s&ctxStateCreated == 0 {
				base := r.ctx
				if p, ok := base.(serverReqContextProvider); ok && s&ctxStateCanceled == 0 {
					if ctx, cancel, ok := p.lazyServerRequestContext(); ok {
						r.cancelCtx = cancel
						r.ctx = ctx
						s |= ctxStateCreated
						atomic.StoreUint32(&r.ctxState, s)
						return ctx
					}
				}
				if base == nil {
					base = context.Background()
				}
				ctx, cancel := context.WithCancel(base)
				if s&ctxStateCanceled != 0 {
					cancel()
				} else {
					r.cancelCtx = cancel
				}
				r.ctx = ctx
				s |= ctxStateCreated
			}
			ctx := r.ctx
			atomic.StoreUint32(&r.ctxState, s)
			return ctx
		}
		runtime.Gosched()
	}
}

func (r *Request) cancelServerCtx() {
	if atomic.CompareAndSwapUint32(&r.ctxState, ctxStateLazy, ctxStateLazy|ctxStateCanceled) {
		return
	}
	for {
		s := atomic.LoadUint32(&r.ctxState)
		if s&ctxStateLocked == 0 && atomic.CompareAndSwapUint32(&r.ctxState, s, s|ctxStateLocked) {
			cancel := r.cancelCtx
			r.cancelCtx = nil
			atomic.StoreUint32(&r.ctxState, s|ctxStateCanceled)
			if cancel != nil {
				cancel()
			}
			return
		}
		runtime.Gosched()
	}
}

// Context returns the request's context. To change the context, use
// [Request.Clone] or [Request.WithContext].
//
// The returned context is always non-nil; it defaults to the
// background context.
//
// For outgoing client requests, the context controls cancellation.
//
// For incoming server requests, the context is canceled when the
// client's connection closes, the request is canceled (with HTTP/2),
// or when the ServeHTTP method returns.
func (r *Request) Context() context.Context {
	if s := atomic.LoadUint32(&r.ctxState); s != 0 {
		if s&ctxStateCreated != 0 {
			return r.ctx
		}
		return r.lazyContext()
	}
	if r.ctx != nil {
		return r.ctx
	}
	return context.Background()
}

// copyNonContextFieldsTo copies all fields of r except ctx, ctxState, and
// cancelCtx into dst. This avoids data races when r uses a lazy server request
// context that may be materialized or canceled concurrently.
func (r *Request) copyNonContextFieldsTo(dst *Request) {
	dst.Method = r.Method
	dst.URL = r.URL
	dst.Proto = r.Proto
	dst.ProtoMajor = r.ProtoMajor
	dst.ProtoMinor = r.ProtoMinor
	dst.Header = r.Header
	dst.Body = r.Body
	dst.GetBody = r.GetBody
	dst.ContentLength = r.ContentLength
	dst.TransferEncoding = r.TransferEncoding
	dst.Close = r.Close
	dst.Host = r.Host
	dst.Form = r.Form
	dst.PostForm = r.PostForm
	dst.MultipartForm = r.MultipartForm
	dst.Trailer = r.Trailer
	dst.RemoteAddr = r.RemoteAddr
	dst.RequestURI = r.RequestURI
	dst.TLS = r.TLS
	dst.Cancel = r.Cancel
	dst.Response = r.Response
	dst.Pattern = r.Pattern
	dst.pat = r.pat
	dst.matches = r.matches
}

// shallowCopyTo copies r into dst, safely preserving r's context without
// racing on lazy server context fields.
func (r *Request) shallowCopyTo(dst *Request) {
	r.copyNonContextFieldsTo(dst)
	if atomic.LoadUint32(&r.ctxState) != 0 {
		dst.ctx = r.Context()
	} else {
		dst.ctx = r.ctx
	}
}

// shallowCopy returns a shallow copy of r that safely preserves r's context
// without racing on lazy server context fields.
func (r *Request) shallowCopy() *Request {
	r2 := new(Request)
	r.shallowCopyTo(r2)
	return r2
}

// WithContext returns a shallow copy of r with its context changed
// to ctx. The provided ctx must be non-nil.
//
// For outgoing client request, the context controls the entire
// lifetime of a request and its response: obtaining a connection,
// sending the request, and reading the response headers and body.
//
// To create a new request with a context, use [NewRequestWithContext].
// To make a deep copy of a request with a new context, use [Request.Clone].
func (r *Request) WithContext(ctx context.Context) *Request {
	if ctx == nil {
		panic("nil context")
	}
	r2 := new(Request)
	r.copyNonContextFieldsTo(r2)
	r2.ctx = ctx
	return r2
}

// Clone returns a deep copy of r with its context changed to ctx.
// The provided ctx must be non-nil.
//
// Clone only makes a shallow copy of the Body field.
//
// For an outgoing client request, the context controls the entire
// lifetime of a request and its response: obtaining a connection,
// sending the request, and reading the response headers and body.
func (r *Request) Clone(ctx context.Context) *Request {
	if ctx == nil {
		panic("nil context")
	}
	var r2 *Request
	if r.URL != nil {
		ru := new(requestWithURL)
		r2 = &ru.req
		r.copyNonContextFieldsTo(r2)
		ru.url = *r.URL
		if r.URL.User != nil {
			u2 := *r.URL.User
			ru.url.User = &u2
		}
		r2.URL = &ru.url
	} else {
		r2 = new(Request)
		r.copyNonContextFieldsTo(r2)
	}
	r2.ctx = ctx
	r2.Header = r.Header.Clone()
	r2.Trailer = r.Trailer.Clone()
	if s := r.TransferEncoding; s != nil {
		s2 := make([]string, len(s))
		copy(s2, s)
		r2.TransferEncoding = s2
	}
	r2.Form = cloneURLValues(r.Form)
	r2.PostForm = cloneURLValues(r.PostForm)
	r2.MultipartForm = cloneMultipartForm(r.MultipartForm)

	// Copy matches and SetPathValue entries. See issue 61410.
	if s := r.matches; s != nil {
		s2 := make([]string, len(s))
		copy(s2, s)
		r2.matches = s2
	}
	return r2
}

// ProtoAtLeast reports whether the HTTP protocol used
// in the request is at least major.minor.
func (r *Request) ProtoAtLeast(major, minor int) bool {
	return r.ProtoMajor > major ||
		r.ProtoMajor == major && r.ProtoMinor >= minor
}

// UserAgent returns the client's User-Agent, if sent in the request.
func (r *Request) UserAgent() string {
	return r.Header.Get("User-Agent")
}

// Cookies parses and returns the HTTP cookies sent with the request.
func (r *Request) Cookies() []*Cookie {
	return readCookies(r.Header, "")
}

// CookiesNamed parses and returns the named HTTP cookies sent with the request
// or an empty slice if none matched.
func (r *Request) CookiesNamed(name string) []*Cookie {
	if name == "" {
		return []*Cookie{}
	}
	return readCookies(r.Header, name)
}

// ErrNoCookie is returned by Request's Cookie method when a cookie is not found.
var ErrNoCookie = errors.New("http: named cookie not present")

// Cookie returns the named cookie provided in the request or
// [ErrNoCookie] if not found.
// If multiple cookies match the given name, only one cookie will
// be returned.
func (r *Request) Cookie(name string) (*Cookie, error) {
	if name == "" {
		return nil, ErrNoCookie
	}
	if c := readCookie(r.Header, name); c != nil {
		return c, nil
	}
	return nil, ErrNoCookie
}

// AddCookie adds a cookie to the request. Per RFC 6265 section 5.4,
// AddCookie does not attach more than one [Cookie] header field. That
// means all cookies, if any, are written into the same line,
// separated by semicolon.
// AddCookie only sanitizes c's name and value, and does not sanitize
// a Cookie header already present in the request.
func (r *Request) AddCookie(c *Cookie) {
	s := sanitizeCookieName(c.Name) + "=" + sanitizeCookieValue(c.Value, c.Quoted)
	if c := r.Header.Get("Cookie"); c != "" {
		r.Header.Set("Cookie", c+"; "+s)
	} else {
		r.Header.Set("Cookie", s)
	}
}

// Referer returns the referring URL, if sent in the request.
//
// Referer is misspelled as in the request itself, a mistake from the
// earliest days of HTTP.  This value can also be fetched from the
// [Header] map as Header["Referer"]; the benefit of making it available
// as a method is that the compiler can diagnose programs that use the
// alternate (correct English) spelling req.Referrer() but cannot
// diagnose programs that use Header["Referrer"].
func (r *Request) Referer() string {
	return r.Header.Get("Referer")
}

// multipartByReader is a sentinel value.
// Its presence in Request.MultipartForm indicates that parsing of the request
// body has been handed off to a MultipartReader instead of ParseMultipartForm.
var multipartByReader = &multipart.Form{
	Value: make(map[string][]string),
	File:  make(map[string][]*multipart.FileHeader),
}

// MultipartReader returns a MIME multipart reader if this is a
// multipart/form-data or a multipart/mixed POST request, else returns nil and an error.
// Use this function instead of [Request.ParseMultipartForm] to
// process the request body as a stream.
func (r *Request) MultipartReader() (*multipart.Reader, error) {
	if r.MultipartForm == multipartByReader {
		return nil, errors.New("http: MultipartReader called twice")
	}
	if r.MultipartForm != nil {
		return nil, errors.New("http: multipart handled by ParseMultipartForm")
	}
	r.MultipartForm = multipartByReader
	return r.multipartReader(true)
}

func (r *Request) multipartReader(allowMixed bool) (*multipart.Reader, error) {
	v := r.Header.Get("Content-Type")
	if v == "" {
		return nil, ErrNotMultipart
	}
	if r.Body == nil {
		return nil, errors.New("missing form body")
	}
	d, params, err := mime.ParseMediaType(v)
	if err != nil || !(d == "multipart/form-data" || allowMixed && d == "multipart/mixed") {
		return nil, ErrNotMultipart
	}
	boundary, ok := params["boundary"]
	if !ok {
		return nil, ErrMissingBoundary
	}
	return multipart.NewReader(r.Body, boundary), nil
}

// isH2Upgrade reports whether r represents the http2 "client preface"
// magic string.
func (r *Request) isH2Upgrade() bool {
	return r.Method == "PRI" && len(r.Header) == 0 && r.URL.Path == "*" && r.Proto == "HTTP/2.0"
}

// Return value if nonempty, def otherwise.
func valueOrDefault(value, def string) string {
	if value != "" {
		return value
	}
	return def
}

// NOTE: This is not intended to reflect the actual Go version being used.
// It was changed at the time of Go 1.1 release because the former User-Agent
// had ended up blocked by some intrusion detection systems.
// See https://codereview.appspot.com/7532043.
const (
	defaultUserAgent       = "Go-http-client/1.1"
	defaultUserAgentHeader = "User-Agent: " + defaultUserAgent + "\r\n"
)

func writeReqLine(w io.Writer, a, b, c, d string) error {
	if bufw, ok := w.(*bufio.Writer); ok {
		avail := bufw.Available()
		if len(a)+len(b)+len(c)+len(d) <= avail {
			buf := bufw.AvailableBuffer()
			buf = append(buf, a...)
			buf = append(buf, b...)
			buf = append(buf, c...)
			buf = append(buf, d...)
			_, err := bufw.Write(buf)
			return err
		}
	}
	var stack [256]byte
	buf := stack[:0]
	if len(a)+len(b)+len(c)+len(d) > len(stack) {
		buf = make([]byte, 0, len(a)+len(b)+len(c)+len(d))
	}
	buf = append(buf, a...)
	buf = append(buf, b...)
	buf = append(buf, c...)
	buf = append(buf, d...)
	_, err := w.Write(buf)
	return err
}

func writeReqLineQuery(w io.Writer, method, path, query string) error {
	total := len(method) + 1 + len(path) + 1 + len(query) + len(" HTTP/1.1\r\n")
	if bufw, ok := w.(*bufio.Writer); ok {
		if total <= bufw.Available() {
			buf := bufw.AvailableBuffer()
			buf = append(buf, method...)
			buf = append(buf, ' ')
			buf = append(buf, path...)
			buf = append(buf, '?')
			buf = append(buf, query...)
			buf = append(buf, " HTTP/1.1\r\n"...)
			_, err := bufw.Write(buf)
			return err
		}
	}
	var stack [256]byte
	buf := stack[:0]
	if total > len(stack) {
		buf = make([]byte, 0, total)
	}
	buf = append(buf, method...)
	buf = append(buf, ' ')
	buf = append(buf, path...)
	buf = append(buf, '?')
	buf = append(buf, query...)
	buf = append(buf, " HTTP/1.1\r\n"...)
	_, err := w.Write(buf)
	return err
}

// Write writes an HTTP/1.1 request, which is the header and body, in wire format.
// This method consults the following fields of the request:
//
//	Host
//	URL
//	Method (defaults to "GET")
//	Header
//	ContentLength
//	TransferEncoding
//	Body
//
// If Body is present, Content-Length is <= 0 and [Request.TransferEncoding]
// hasn't been set to "identity", Write adds "Transfer-Encoding:
// chunked" to the header. Body is closed after it is sent.
//
// Header values for Host, Content-Length, Transfer-Encoding,
// and Trailer are not used; these are derived from other Request fields.
// If the Header does not contain a User-Agent value, Write uses
// "Go-http-client/1.1".
func (r *Request) Write(w io.Writer) error {
	return r.write(w, false, nil, false, false, nil)
}

// WriteProxy is like [Request.Write] but writes the request in the form
// expected by an HTTP proxy. In particular, [Request.WriteProxy] writes the
// initial Request-URI line of the request with an absolute URI, per
// section 5.3 of RFC 7230, including the scheme and host.
// In either case, WriteProxy also writes a Host header, using
// either r.Host or r.URL.Host.
func (r *Request) WriteProxy(w io.Writer) error {
	return r.write(w, true, nil, false, false, nil)
}

// errMissingHost is returned by Write when there is no Host or URL present in
// the Request.
var errMissingHost = errors.New("http: Request.Write on Request with no Host or URL set")

// extraHeaders may be nil
// waitForContinue may be nil
// always closes body
func (r *Request) write(w io.Writer, usingProxy bool, extraHeaders Header, addedGzip, addedClose bool, waitForContinue func() bool) (err error) {
	trace := httptrace.ContextClientTrace(r.Context())
	if trace != nil && trace.WroteRequest != nil {
		defer func() {
			trace.WroteRequest(httptrace.WroteRequestInfo{
				Err: err,
			})
		}()
	}
	closed := false
	defer func() {
		if closed {
			return
		}
		if closeErr := r.closeBody(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	// Find the target host. Prefer the Host: header, but if that
	// is not given, use the host from the request URL.
	//
	// Clean the host, in case it arrives with unexpected stuff in it.
	host := r.Host
	if host == "" {
		if r.URL == nil {
			return errMissingHost
		}
		host = r.URL.Host
	}
	host, err = httpguts.PunycodeHostPort(host)
	if err != nil {
		return err
	}
	// Validate that the Host header is a valid header in general,
	// but don't validate the host itself. This is sufficient to avoid
	// header or request smuggling via the Host field.
	// The server can (and will, if it's a net/http server) reject
	// the request if it doesn't consider the host valid.
	if !httpguts.ValidHostHeader(host) {
		// Historically, we would truncate the Host header after '/' or ' '.
		// Some users have relied on this truncation to convert a network
		// address such as Unix domain socket path into a valid, ignored
		// Host header (see https://go.dev/issue/61431).
		//
		// We don't preserve the truncation, because sending an altered
		// header field opens a smuggling vector. Instead, zero out the
		// Host header entirely if it isn't valid. (An empty Host is valid;
		// see RFC 9112 Section 3.2.)
		//
		// Return an error if we're sending to a proxy, since the proxy
		// probably can't do anything useful with an empty Host header.
		if !usingProxy {
			host = ""
		} else {
			return errors.New("http: invalid Host header")
		}
	}

	// According to RFC 6874, an HTTP client, proxy, or other
	// intermediary must remove any IPv6 zone identifier attached
	// to an outgoing URI.
	host = removeZone(host)

	var ruri, ruriQuery string
	var ruriHasQuery bool
	if !(usingProxy && r.URL.Scheme != "" && r.URL.Opaque == "") && !(r.Method == "CONNECT" && r.URL.Path == "") && r.URL.Opaque == "" {
		ruri = r.URL.EscapedPath()
		if ruri == "" {
			ruri = "/"
		}
		ruriHasQuery = r.URL.ForceQuery || r.URL.RawQuery != ""
		ruriQuery = r.URL.RawQuery
		if stringContainsCTLByte(ruri) || (ruriQuery != "" && stringContainsCTLByte(ruriQuery)) {
			return errors.New("net/http: can't write control character in Request.URL")
		}
	} else {
		ruri = r.URL.RequestURI()
		if usingProxy && r.URL.Scheme != "" && r.URL.Opaque == "" {
			ruri = r.URL.Scheme + "://" + host + ruri
		} else if r.Method == "CONNECT" && r.URL.Path == "" {
			// CONNECT requests normally give just the host and port, not a full URL.
			ruri = host
			if r.URL.Opaque != "" {
				ruri = r.URL.Opaque
			}
		}
		if stringContainsCTLByte(ruri) {
			return errors.New("net/http: can't write control character in Request.URL")
		}
	}

	// :protocol is only valid in HTTP/2 and HTTP/3. Transport allows it through
	// header validation because it has not selected a connection yet. If the
	// request reaches this HTTP/1 writer, reject it since it's not allowed.
	if r.Header.has(":protocol") {
		return errors.New("net/http: :protocol pseudo-header is not allowed in HTTP/1.x")
	}

	// TODO: validate r.Method too? At least it's less likely to
	// come from an attacker (more likely to be a constant in
	// code).

	// Wrap the writer in a bufio Writer if it's not already buffered.
	// Don't always call NewWriter, as that forces a bytes.Buffer
	// and other small bufio Writers to have a minimum 4k buffer
	// size.
	var bw *bufio.Writer
	if _, ok := w.(io.ByteWriter); !ok {
		bw = bufio.NewWriter(w)
		w = bw
	}

	method := valueOrDefault(r.Method, "GET")
	hasUA := r.Header.has("User-Agent")
	wrotePreamble := false
	if bufw, ok := w.(*bufio.Writer); ok {
		need := len(method) + 1 + len(ruri) + len(" HTTP/1.1\r\nHost: ") + len(host) + 2
		if ruriHasQuery {
			need += 1 + len(ruriQuery)
		}
		if !hasUA {
			need += len(defaultUserAgentHeader)
		}
		if need <= bufw.Available() {
			buf := bufw.AvailableBuffer()
			buf = append(buf, method...)
			buf = append(buf, ' ')
			buf = append(buf, ruri...)
			if ruriHasQuery {
				buf = append(buf, '?')
				buf = append(buf, ruriQuery...)
			}
			buf = append(buf, " HTTP/1.1\r\nHost: "...)
			buf = append(buf, host...)
			buf = append(buf, '\r', '\n')
			if !hasUA {
				buf = append(buf, defaultUserAgentHeader...)
			}
			if _, err = bufw.Write(buf); err != nil {
				return err
			}
			if trace != nil && trace.WroteHeaderField != nil {
				trace.WroteHeaderField("Host", []string{host})
				if !hasUA {
					trace.WroteHeaderField("User-Agent", []string{defaultUserAgent})
				}
			}
			wrotePreamble = true
		}
	}
	if !wrotePreamble {
		if ruriHasQuery {
			if err = writeReqLineQuery(w, method, ruri, ruriQuery); err != nil {
				return err
			}
		} else {
			if err = writeReqLine(w, method, " ", ruri, " HTTP/1.1\r\n"); err != nil {
				return err
			}
		}

		// Header lines
		if err = writeReqLine(w, "Host: ", host, "\r\n", ""); err != nil {
			return err
		}
		if trace != nil && trace.WroteHeaderField != nil {
			trace.WroteHeaderField("Host", []string{host})
		}

		// Use the defaultUserAgent unless the Header contains one, which
		// may be blank to not send the header.
		if !hasUA {
			if _, err = io.WriteString(w, defaultUserAgentHeader); err != nil {
				return err
			}
			if trace != nil && trace.WroteHeaderField != nil {
				trace.WroteHeaderField("User-Agent", []string{defaultUserAgent})
			}
		}
	}
	if hasUA {
		if userAgent := r.Header.Get("User-Agent"); userAgent != "" {
			if strings.IndexByte(userAgent, '\r') >= 0 || strings.IndexByte(userAgent, '\n') >= 0 {
				userAgent = headerNewlineToSpace.Replace(userAgent)
			}
			userAgent = textproto.TrimString(userAgent)
			if err = writeReqLine(w, "User-Agent: ", userAgent, "\r\n", ""); err != nil {
				return err
			}
			if trace != nil && trace.WroteHeaderField != nil {
				trace.WroteHeaderField("User-Agent", []string{userAgent})
			}
		}
	}

	// Process Body,ContentLength,Close,Trailer
	tw, err := newTransferWriter(r)
	if err != nil {
		return err
	}
	err = tw.writeHeader(w, trace)
	if err != nil {
		return err
	}

	err = r.Header.writeSubset(w, reqWriteExcludeHeader, trace)
	if err != nil {
		return err
	}

	if extraHeaders != nil {
		err = extraHeaders.write(w, trace)
		if err != nil {
			return err
		}
	}
	if addedGzip && !addedClose && (trace == nil || trace.WroteHeaderField == nil) {
		_, err = io.WriteString(w, "Accept-Encoding: gzip\r\n\r\n")
		if err != nil {
			return err
		}
	} else {
		if addedGzip {
			if _, err = io.WriteString(w, "Accept-Encoding: gzip\r\n"); err != nil {
				return err
			}
			if trace != nil && trace.WroteHeaderField != nil {
				trace.WroteHeaderField("Accept-Encoding", []string{"gzip"})
			}
		}
		if addedClose {
			if _, err = io.WriteString(w, "Connection: close\r\n"); err != nil {
				return err
			}
			if trace != nil && trace.WroteHeaderField != nil {
				trace.WroteHeaderField("Connection", []string{"close"})
			}
		}

		_, err = io.WriteString(w, "\r\n")
		if err != nil {
			return err
		}
	}

	if trace != nil && trace.WroteHeaders != nil {
		trace.WroteHeaders()
	}

	// Flush and wait for 100-continue if expected.
	if waitForContinue != nil {
		if bw, ok := w.(*bufio.Writer); ok {
			err = bw.Flush()
			if err != nil {
				return err
			}
		}
		if trace != nil && trace.Wait100Continue != nil {
			trace.Wait100Continue()
		}
		if !waitForContinue() {
			closed = true
			r.closeBody()
			return nil
		}
	}

	if bw, ok := w.(*bufio.Writer); ok && tw.FlushHeaders {
		if err := bw.Flush(); err != nil {
			return err
		}
	}

	// Write body and trailer
	closed = true
	err = tw.writeBody(w)
	if err != nil {
		if tw.bodyReadError == err {
			err = requestBodyReadError{err}
		}
		return err
	}

	if bw != nil {
		return bw.Flush()
	}
	return nil
}

// requestBodyReadError wraps an error from (*Request).write to indicate
// that the error came from a Read call on the Request.Body.
// This error type should not escape the net/http package to users.
type requestBodyReadError struct{ error }

func idnaASCII(v string) (string, error) {
	// TODO: Follow the WHATWG URL Specification.
	//
	// WHATWG accepts all ASCII-only names (although sometimes with advisory
	// validation errors), so skipping the relatively expensive IDNA processing
	// on them is fine.
	if ascii.Is(v) {
		return v, nil
	}
	return idna.Lookup.ToASCII(v)
}

// removeZone removes IPv6 zone identifier from host.
// E.g., "[fe80::1%en0]:8080" to "[fe80::1]:8080"
func removeZone(host string) string {
	if !strings.HasPrefix(host, "[") {
		return host
	}
	i := strings.LastIndex(host, "]")
	if i < 0 {
		return host
	}
	j := strings.LastIndex(host[:i], "%")
	if j < 0 {
		return host
	}
	return host[:j] + host[i:]
}

// ParseHTTPVersion parses an HTTP version string according to RFC 7230, section 2.6.
// "HTTP/1.0" returns (1, 0, true). Note that strings without
// a minor version, such as "HTTP/2", are not valid.
func ParseHTTPVersion(vers string) (major, minor int, ok bool) {
	switch vers {
	case "HTTP/1.1":
		return 1, 1, true
	case "HTTP/1.0":
		return 1, 0, true
	}
	if !strings.HasPrefix(vers, "HTTP/") {
		return 0, 0, false
	}
	if len(vers) != len("HTTP/X.Y") {
		return 0, 0, false
	}
	if vers[6] != '.' {
		return 0, 0, false
	}
	maj, err := strconv.ParseUint(vers[5:6], 10, 0)
	if err != nil {
		return 0, 0, false
	}
	min, err := strconv.ParseUint(vers[7:8], 10, 0)
	if err != nil {
		return 0, 0, false
	}
	return int(maj), int(min), true
}

func validMethod(method string) bool {
	/*
	     Method         = "OPTIONS"                ; Section 9.2
	                    | "GET"                    ; Section 9.3
	                    | "HEAD"                   ; Section 9.4
	                    | "POST"                   ; Section 9.5
	                    | "PUT"                    ; Section 9.6
	                    | "DELETE"                 ; Section 9.7
	                    | "TRACE"                  ; Section 9.8
	                    | "CONNECT"                ; Section 9.9
	                    | extension-method
	   extension-method = token
	     token          = 1*<any CHAR except CTLs or separators>
	*/
	switch method {
	case "GET", "POST", "HEAD", "PUT", "DELETE", "CONNECT", "OPTIONS", "TRACE", "PATCH":
		return true
	}
	return isToken(method)
}

// NewRequest wraps [NewRequestWithContext] using [context.Background].
func NewRequest(method, url string, body io.Reader) (*Request, error) {
	return NewRequestWithContext(context.Background(), method, url, body)
}

func getNoBody() (io.ReadCloser, error) { return NoBody, nil }

// NewRequestWithContext returns a new [Request] given a method, URL, and
// optional body.
//
// If the provided body is also an [io.Closer], the returned
// [Request.Body] is set to body and will be closed (possibly
// asynchronously) by the Client methods Do, Post, and PostForm,
// and [Transport.RoundTrip].
//
// NewRequestWithContext returns a Request suitable for use with
// [Client.Do] or [Transport.RoundTrip]. To create a request for use with
// testing a Server Handler, either use the [net/http/httptest.NewRequest] function,
// use [ReadRequest], or manually update the Request fields.
// For an outgoing client request, the context
// controls the entire lifetime of a request and its response:
// obtaining a connection, sending the request, and reading the
// response headers and body. See the [Request] type's documentation for
// the difference between inbound and outbound request fields.
//
// If body is of type [*bytes.Buffer], [*bytes.Reader], or
// [*strings.Reader], the returned request's ContentLength is set to its
// exact value (instead of -1), GetBody is populated (so 307 and 308
// redirects can replay the body), and Body is set to [NoBody] if the
// ContentLength is 0.
func NewRequestWithContext(ctx context.Context, method, url string, body io.Reader) (*Request, error) {
	if method == "" {
		// We document that "" means "GET" for Request.Method, and people have
		// relied on that from NewRequest, so keep that working.
		// We still enforce validMethod for non-empty methods.
		method = "GET"
	}
	if !validMethod(method) {
		return nil, fmt.Errorf("net/http: invalid method %q", method)
	}
	if ctx == nil {
		return nil, errors.New("net/http: nil Context")
	}
	var ru *requestWithURL
	var u *urlpkg.URL
	var simpleURL urlpkg.URL
	hasSimpleURL := parseSimpleHTTPURL(url, &simpleURL)
	if !hasSimpleURL {
		var err error
		u, err = urlpkg.Parse(url)
		if err != nil {
			return nil, err
		}
	}
	var rc io.ReadCloser
	var contentLength int64
	var getBody func() (io.ReadCloser, error)
	if body != nil {
		switch v := body.(type) {
		case *bytes.Buffer:
			contentLength = int64(v.Len())
			if contentLength == 0 {
				rc = NoBody
				getBody = getNoBody
			} else {
				rc = io.NopCloser(body)
				buf := v.Bytes()
				getBody = func() (io.ReadCloser, error) {
					r := bytes.NewReader(buf)
					return io.NopCloser(r), nil
				}
			}
		case *bytes.Reader:
			contentLength = int64(v.Len())
			if contentLength == 0 {
				rc = NoBody
				getBody = getNoBody
			} else {
				rc = io.NopCloser(body)
				snapshot := *v
				getBody = func() (io.ReadCloser, error) {
					r := snapshot
					return io.NopCloser(&r), nil
				}
			}
		case *strings.Reader:
			contentLength = int64(v.Len())
			if contentLength == 0 {
				rc = NoBody
				getBody = getNoBody
			} else {
				rc = io.NopCloser(body)
				snapshot := *v
				getBody = func() (io.ReadCloser, error) {
					r := snapshot
					return io.NopCloser(&r), nil
				}
			}
		default:
			var ok bool
			if rc, ok = body.(io.ReadCloser); !ok {
				rc = io.NopCloser(body)
			}
			// This is where we'd set it to -1 (at least
			// if body != NoBody) to mean unknown, but
			// that broke people during the Go 1.8 testing
			// period. People depend on it being 0 I
			// guess. Maybe retry later. See Issue 18117.
		}
	}
	if hasSimpleURL {
		// The host's colon:port should be normalized. See Issue 14836.
		simpleURL.Host = strings.TrimSuffix(simpleURL.Host, ":")
		ru = &requestWithURL{
			req: Request{
				ctx:           ctx,
				Method:        method,
				Proto:         "HTTP/1.1",
				ProtoMajor:    1,
				ProtoMinor:    1,
				Header:        make(Header),
				Body:          rc,
				ContentLength: contentLength,
				GetBody:       getBody,
				Host:          simpleURL.Host,
			},
			url: simpleURL,
		}
		ru.req.URL = &ru.url
		return &ru.req, nil
	}
	// The host's colon:port should be normalized. See Issue 14836.
	u.Host = strings.TrimSuffix(u.Host, ":")
	req := &Request{
		ctx:           ctx,
		Method:        method,
		URL:           u,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(Header),
		Body:          rc,
		ContentLength: contentLength,
		GetBody:       getBody,
		Host:          u.Host,
	}

	return req, nil
}

// parseSimpleHTTPURL parses common ASCII http:// and https:// URLs without
// percent-escapes, fragments, userinfo, or IPv6 brackets directly into dst,
// matching net/url.Parse.
func parseSimpleHTTPURL(rawurl string, dst *urlpkg.URL) bool {
	var scheme, rest string
	if strings.HasPrefix(rawurl, "http://") {
		scheme = "http"
		rest = rawurl[len("http://"):]
	} else if strings.HasPrefix(rawurl, "https://") {
		scheme = "https"
		rest = rawurl[len("https://"):]
	} else {
		return false
	}
	if len(rest) == 0 {
		return false
	}
	authEnd := len(rest)
	colon := -1
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c == '/' || c == '?' {
			authEnd = i
			break
		}
		if c == ':' {
			if colon >= 0 {
				return false
			}
			colon = i
			continue
		}
		if colon >= 0 {
			if c < '0' || c > '9' {
				return false
			}
			continue
		}
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	if authEnd == 0 || colon == 0 {
		return false
	}
	host := rest[:authEnd]
	if authEnd == len(rest) {
		*dst = urlpkg.URL{
			Scheme: scheme,
			Host:   host,
		}
		return true
	}
	tail := rest[authEnd:]
	qIdx := -1
	for i := 0; i < len(tail); i++ {
		c := tail[i]
		if c <= ' ' || c >= 0x7f || c == '#' || c == '%' || c == '\\' {
			return false
		}
		if c == '?' && qIdx < 0 {
			qIdx = i
		}
	}
	var path, rawQuery string
	var forceQuery bool
	if qIdx < 0 {
		path = tail
	} else {
		path = tail[:qIdx]
		if qIdx == len(tail)-1 {
			forceQuery = true
		} else {
			rawQuery = tail[qIdx+1:]
		}
	}
	*dst = urlpkg.URL{
		Scheme:     scheme,
		Host:       host,
		Path:       path,
		ForceQuery: forceQuery,
		RawQuery:   rawQuery,
	}
	return true
}

// BasicAuth returns the username and password provided in the request's
// Authorization header, if the request uses HTTP Basic Authentication.
// See RFC 2617, Section 2.
func (r *Request) BasicAuth() (username, password string, ok bool) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", "", false
	}
	return parseBasicAuth(auth)
}

// parseBasicAuth parses an HTTP Basic Authentication string.
// "Basic QWxhZGRpbjpvcGVuIHNlc2FtZQ==" returns ("Aladdin", "open sesame", true).
//
// parseBasicAuth should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/sagernet/sing
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname parseBasicAuth
func parseBasicAuth(auth string) (username, password string, ok bool) {
	const prefix = "Basic "
	// Case insensitive prefix match. See Issue 22736.
	if len(auth) < len(prefix) || !ascii.EqualFold(auth[:len(prefix)], prefix) {
		return "", "", false
	}
	enc := auth[len(prefix):]
	if base64.StdEncoding.DecodedLen(len(enc)) <= maxStackBasicAuth {
		// Decode into a stack buffer so that valid credentials cost a
		// single allocation. Decode does not modify its source, so
		// viewing enc as bytes without a copy is safe.
		var buf [maxStackBasicAuth]byte
		n, err := base64.StdEncoding.Decode(buf[:], unsafe.Slice(unsafe.StringData(enc), len(enc)))
		if err != nil {
			return "", "", false
		}
		colon := bytes.IndexByte(buf[:n], ':')
		if colon < 0 {
			return "", "", false
		}
		cs := string(buf[:n])
		return cs[:colon], cs[colon+1:], true
	}
	c, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", "", false
	}
	colon := bytes.IndexByte(c, ':')
	if colon < 0 {
		return "", "", false
	}
	cs := string(c)
	return cs[:colon], cs[colon+1:], true
}

// SetBasicAuth sets the request's Authorization header to use HTTP
// Basic Authentication with the provided username and password.
//
// With HTTP Basic Authentication the provided username and password
// are not encrypted. It should generally only be used in an HTTPS
// request.
//
// The username may not contain a colon. Some protocols may impose
// additional requirements on pre-escaping the username and
// password. For instance, when used with OAuth2, both arguments must
// be URL encoded first with [url.QueryEscape].
func (r *Request) SetBasicAuth(username, password string) {
	r.Header.Set("Authorization", basicAuthHeader(username, password))
}

// parseRequestLine parses "GET /foo HTTP/1.1" into its three parts.
func parseRequestLine(line string) (method, requestURI, proto string, ok bool) {
	method, rest, ok1 := strings.Cut(line, " ")
	requestURI, proto, ok2 := strings.Cut(rest, " ")
	if !ok1 || !ok2 {
		return "", "", "", false
	}
	return method, requestURI, proto, true
}

var textprotoReaderPool sync.Pool[*textproto.Reader]

func newTextprotoReader(br *bufio.Reader) *textproto.Reader {
	if v := textprotoReaderPool.Get(); v != nil {
		tr := v
		tr.R = br
		return tr
	}
	return textproto.NewReader(br)
}

func putTextprotoReader(r *textproto.Reader) {
	r.R = nil
	textprotoReaderPool.Put(r)
}

// ReadRequest reads and parses an incoming request from b.
//
// ReadRequest is a low-level function and should only be used for
// specialized applications; most code should use the [Server] to read
// requests and handle them via the [Handler] interface. ReadRequest
// only supports HTTP/1.x requests. For HTTP/2, use golang.org/x/net/http2.
func ReadRequest(b *bufio.Reader) (*Request, error) {
	req, _, _, err := readRequestLimit(b, math.MaxInt64, true)
	return req, err
}

// readMIMEHeader is defined in package [net/textproto].
//
//go:linkname readMIMEHeader net/textproto.readMIMEHeader
func readMIMEHeader(r *textproto.Reader, maxMemory, maxHeaders int64) (textproto.MIMEHeader, error)

//go:linkname readMIMEHeaderFast net/textproto.readMIMEHeaderFast
func readMIMEHeaderFast(b *bufio.Reader, maxMemory, maxHeaders int64, skipHost bool) (m textproto.MIMEHeader, haveHost bool, hostHeader string, hostCount int, handled bool, err error)

// readRequest should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/sagernet/sing
//   - github.com/v2fly/v2ray-core/v4
//   - github.com/v2fly/v2ray-core/v5
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname readRequest
func readRequest(b *bufio.Reader) (req *Request, err error) {
	req, _, _, err = readRequestLimit(b, math.MaxInt64, false)
	return req, err
}

type requestWithURL struct {
	req Request
	url url.URL
}

var cleanPathTable = [256]bool{
	'$': true, '&': true, '+': true, ',': true, '-': true, '.': true, '/': true,
	'0': true, '1': true, '2': true, '3': true, '4': true, '5': true,
	'6': true, '7': true, '8': true, '9': true, ':': true, ';': true,
	'=': true, '@': true,
	'A': true, 'B': true, 'C': true, 'D': true, 'E': true, 'F': true,
	'G': true, 'H': true, 'I': true, 'J': true, 'K': true, 'L': true,
	'M': true, 'N': true, 'O': true, 'P': true, 'Q': true, 'R': true,
	'S': true, 'T': true, 'U': true, 'V': true, 'W': true, 'X': true,
	'Y': true, 'Z': true, '_': true,
	'a': true, 'b': true, 'c': true, 'd': true, 'e': true, 'f': true,
	'g': true, 'h': true, 'i': true, 'j': true, 'k': true, 'l': true,
	'm': true, 'n': true, 'o': true, 'p': true, 'q': true, 'r': true,
	's': true, 't': true, 'u': true, 'v': true, 'w': true, 'x': true,
	'y': true, 'z': true, '~': true,
}

// scanCleanOriginURI checks whether b is a clean unescaped origin-form path
// (matching net/url.encodePath bytes) optionally followed by '?' and a query
// without control characters.
func scanCleanOriginURI(b []byte) (qIdx int, ok bool) {
	for i, c := range b {
		if !cleanPathTable[c] {
			if c == '?' && i > 0 {
				for _, qc := range b[i+1:] {
					if qc < ' ' || qc == 0x7f {
						return -1, false
					}
				}
				return i, true
			}
			return -1, false
		}
	}
	return -1, true
}

func tryReadHostOnlyHeader(b *bufio.Reader, maxHeaders int64) (host string, haveHost, ok bool) {
	n := b.Buffered()
	if n < 2 {
		return "", false, false
	}
	peek, err := b.Peek(n)
	if err != nil {
		return "", false, false
	}
	if peek[0] == '\r' && peek[1] == '\n' {
		b.Discard(2)
		return "", false, true
	}
	if maxHeaders < 1 || len(peek) < 8 {
		return "", false, false
	}
	if (peek[0] != 'H' && peek[0] != 'h') ||
		(peek[1] != 'o' && peek[1] != 'O') ||
		(peek[2] != 's' && peek[2] != 'S') ||
		(peek[3] != 't' && peek[3] != 'T') ||
		peek[4] != ':' {
		return "", false, false
	}
	nlIdx := bytes.IndexByte(peek[5:], '\n')
	if nlIdx < 0 {
		return "", false, false
	}
	lineEnd := 5 + nlIdx
	if len(peek) < lineEnd+3 || peek[lineEnd+1] != '\r' || peek[lineEnd+2] != '\n' {
		return "", false, false
	}
	v := peek[5:lineEnd]
	if len(v) > 0 && v[len(v)-1] == '\r' {
		v = v[:len(v)-1]
	}
	for len(v) > 0 && (v[0] == ' ' || v[0] == '\t') {
		v = v[1:]
	}
	for len(v) > 0 && (v[len(v)-1] == ' ' || v[len(v)-1] == '\t') {
		v = v[:len(v)-1]
	}
	for _, c := range v {
		if (c < ' ' || c == 0x7f) && c != '\t' {
			return "", false, false
		}
	}
	b.Discard(lineEnd + 3)
	switch string(v) {
	case "":
		return "", true, true
	case "golang.org":
		return "golang.org", true, true
	case "localhost":
		return "localhost", true, true
	case "localhost:8080":
		return "localhost:8080", true, true
	case "127.0.0.1":
		return "127.0.0.1", true, true
	default:
		return string(v), true, true
	}
}

func readLineBytes(b *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		l, more, err := b.ReadLine()
		if err != nil {
			return nil, err
		}
		if line == nil && !more {
			return l, nil
		}
		line = append(line, l...)
		if !more {
			break
		}
	}
	return line, nil
}

func readRequestLimit(b *bufio.Reader, maxHeaders int64, deleteHost bool) (req *Request, haveHost bool, hostHeader string, err error) {
	// First line: GET /index.html HTTP/1.0
	line, err := readLineBytes(b)
	if err != nil {
		return nil, false, "", err
	}

	i1 := bytes.IndexByte(line, ' ')
	if i1 < 0 {
		return nil, false, "", badStringError("malformed HTTP request", string(line))
	}
	methodBytes := line[:i1]
	rest := line[i1+1:]
	i2 := bytes.IndexByte(rest, ' ')
	if i2 < 0 {
		return nil, false, "", badStringError("malformed HTTP request", string(line))
	}
	uriBytes := rest[:i2]
	protoBytes := rest[i2+1:]

	var method string
	switch string(methodBytes) {
	case "GET":
		method = "GET"
	case "POST":
		method = "POST"
	case "HEAD":
		method = "HEAD"
	case "PUT":
		method = "PUT"
	case "DELETE":
		method = "DELETE"
	case "CONNECT":
		method = "CONNECT"
	case "OPTIONS":
		method = "OPTIONS"
	case "TRACE":
		method = "TRACE"
	case "PATCH":
		method = "PATCH"
	default:
		method = string(methodBytes)
		if !validMethod(method) {
			return nil, false, "", badStringError("invalid method", method)
		}
	}

	var proto string
	var protoMajor, protoMinor int
	switch string(protoBytes) {
	case "HTTP/1.1":
		proto = "HTTP/1.1"
		protoMajor = 1
		protoMinor = 1
	case "HTTP/1.0":
		proto = "HTTP/1.0"
		protoMajor = 1
		protoMinor = 0
	default:
		proto = string(protoBytes)
		var ok bool
		if protoMajor, protoMinor, ok = ParseHTTPVersion(proto); !ok {
			return nil, false, "", badStringError("malformed HTTP version", proto)
		}
	}

	ru := &requestWithURL{
		req: Request{
			Method:     method,
			Proto:      proto,
			ProtoMajor: protoMajor,
			ProtoMinor: protoMinor,
		},
	}
	req = &ru.req

	// CONNECT requests are used two different ways, and neither uses a full URL:
	// The standard use is to tunnel HTTPS through an HTTP proxy.
	// It looks like "CONNECT www.google.com:443 HTTP/1.1", and the parameter is
	// just the authority section of a URL. This information should go in req.URL.Host.
	//
	// The net/rpc package also uses CONNECT, but there the parameter is a path
	// that starts with a slash. It can be parsed with the regular URL parser,
	// and the path will end up in req.URL.Path, where it needs to be in order for
	// RPC to work.
	if len(uriBytes) == 1 && uriBytes[0] == '/' {
		req.RequestURI = "/"
		ru.url.Path = "/"
		req.URL = &ru.url
	} else if len(uriBytes) > 1 && uriBytes[0] == '/' && uriBytes[1] != '/' {
		if qIdx, ok := scanCleanOriginURI(uriBytes); ok {
			s := string(uriBytes)
			req.RequestURI = s
			if qIdx < 0 {
				ru.url.Path = s
			} else {
				ru.url.Path = s[:qIdx]
				if qIdx == len(s)-1 {
					ru.url.ForceQuery = true
				} else {
					ru.url.RawQuery = s[qIdx+1:]
				}
			}
			req.URL = &ru.url
		} else {
			req.RequestURI = string(uriBytes)
			if req.URL, err = url.ParseRequestURI(req.RequestURI); err != nil {
				return nil, false, "", err
			}
		}
	} else {
		req.RequestURI = string(uriBytes)
		rawurl := req.RequestURI
		justAuthority := req.Method == "CONNECT" && !strings.HasPrefix(rawurl, "/")
		if justAuthority {
			rawurl = "http://" + rawurl
		}

		if req.URL, err = url.ParseRequestURI(rawurl); err != nil {
			return nil, false, "", err
		}

		if justAuthority {
			// Strip the bogus "http://" back off.
			req.URL.Scheme = ""
		}
	}

	if deleteHost {
		if host, hasHost, ok := tryReadHostOnlyHeader(b, maxHeaders); ok {
			req.Header = make(Header)
			req.Host = req.URL.Host
			if req.Host == "" {
				req.Host = host
			}
			req.Close = req.ProtoMajor < 1 || (req.ProtoMajor == 1 && req.ProtoMinor == 0)
			req.Body = NoBody
			if req.Method == "PRI" && req.isH2Upgrade() {
				req.ContentLength = -1
				req.Close = true
			}
			return req, hasHost, host, nil
		}
	}

	// Subsequent lines: Key: value.
	mimeHeader, hdrHaveHost, hdrHostVal, hostCount, handled, err := readMIMEHeaderFast(b, math.MaxInt64, maxHeaders, deleteHost)
	if !handled {
		tp := newTextprotoReader(b)
		mimeHeader, err = readMIMEHeader(tp, math.MaxInt64, maxHeaders)
		putTextprotoReader(tp)
	}
	if err != nil {
		if err == io.EOF {
			return nil, false, "", io.ErrUnexpectedEOF
		}
		// TODO: Add a distinguishable error to net/textproto.
		if err.Error() == "message too large" {
			return nil, false, "", errTooLarge
		}
		return nil, false, "", err
	}
	req.Header = Header(mimeHeader)
	if handled && deleteHost {
		if hostCount > 1 {
			return nil, false, "", fmt.Errorf("too many Host headers")
		}
		haveHost = hdrHaveHost
		hostHeader = hdrHostVal
	} else {
		hosts := req.Header["Host"]
		if len(hosts) > 1 {
			return nil, false, "", fmt.Errorf("too many Host headers")
		}
		if len(hosts) > 0 {
			haveHost = true
			hostHeader = hosts[0]
		}
	}

	// RFC 7230, section 5.3: Must treat
	//	GET /index.html HTTP/1.1
	//	Host: www.google.com
	// and
	//	GET http://www.google.com/index.html HTTP/1.1
	//	Host: doesntmatter
	// the same. In the second case, any Host line is ignored.
	req.Host = req.URL.Host
	if req.Host == "" {
		req.Host = hostHeader
	}
	if deleteHost && haveHost && !handled {
		delete(req.Header, "Host")
	}

	if len(req.Header) == 0 {
		req.Close = req.ProtoMajor < 1 || (req.ProtoMajor == 1 && req.ProtoMinor == 0)
		req.Body = NoBody
		if req.Method == "PRI" && req.isH2Upgrade() {
			req.ContentLength = -1
			req.Close = true
		}
		return req, haveHost, hostHeader, nil
	}

	fixPragmaCacheControl(req.Header)

	req.Close = shouldClose(req.ProtoMajor, req.ProtoMinor, req.Header, false)

	err = readTransfer(req, b, maxHeaders)
	if err != nil {
		if err == io.EOF {
			return nil, false, "", io.ErrUnexpectedEOF
		}
		return nil, false, "", err
	}

	if req.isH2Upgrade() {
		// Because it's neither chunked, nor declared:
		req.ContentLength = -1

		// We want to give handlers a chance to hijack the
		// connection, but we need to prevent the Server from
		// dealing with the connection further if it's not
		// hijacked. Set Close to ensure that:
		req.Close = true
	}
	return req, haveHost, hostHeader, nil
}

// MaxBytesReader is similar to [io.LimitReader] but is intended for
// limiting the size of incoming request bodies. In contrast to
// io.LimitReader, MaxBytesReader's result is a ReadCloser, returns a
// non-nil error of type [*MaxBytesError] for a Read beyond the limit,
// and closes the underlying reader when its Close method is called.
//
// MaxBytesReader prevents clients from accidentally or maliciously
// sending a large request and wasting server resources. If possible,
// it tells the [ResponseWriter] to close the connection after the limit
// has been reached.
func MaxBytesReader(w ResponseWriter, r io.ReadCloser, n int64) io.ReadCloser {
	if n < 0 { // Treat negative limits as equivalent to 0.
		n = 0
	}
	return &maxBytesReader{w: w, r: r, i: n, n: n}
}

// MaxBytesError is returned by [MaxBytesReader] when its read limit is exceeded.
type MaxBytesError struct {
	Limit int64
}

func (e *MaxBytesError) Error() string {
	// Due to Hyrum's law, this text cannot be changed.
	return "http: request body too large"
}

type maxBytesReader struct {
	w   ResponseWriter
	r   io.ReadCloser // underlying reader
	i   int64         // max bytes initially, for MaxBytesError
	n   int64         // max bytes remaining
	err error         // sticky error
}

func (l *maxBytesReader) Read(p []byte) (n int, err error) {
	if l.err != nil {
		return 0, l.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	// If they asked for a 32KB byte read but only 5 bytes are
	// remaining, no need to read 32KB. 6 bytes will answer the
	// question of the whether we hit the limit or go past it.
	// 0 < len(p) < 2^63
	if int64(len(p))-1 > l.n {
		p = p[:l.n+1]
	}
	n, err = l.r.Read(p)

	if int64(n) <= l.n {
		l.n -= int64(n)
		l.err = err
		return n, err
	}

	n = int(l.n)
	l.n = 0

	// The server code and client code both use
	// maxBytesReader. This "requestTooLarge" check is
	// only used by the server code. To prevent binaries
	// which only using the HTTP Client code (such as
	// cmd/go) from also linking in the HTTP server, don't
	// use a static type assertion to the server
	// "*response" type. Check this interface instead:
	type requestTooLarger interface {
		requestTooLarge()
	}
	if res, ok := l.w.(requestTooLarger); ok {
		res.requestTooLarge()
	}
	l.err = &MaxBytesError{l.i}
	return n, l.err
}

func (l *maxBytesReader) Close() error {
	return l.r.Close()
}

func copyValues(dst, src url.Values) {
	if len(src) == 0 {
		return
	}
	if len(dst) == 0 {
		nv := 0
		for _, vs := range src {
			nv += len(vs)
		}
		var sv []string
		if nv > 0 {
			sv = make([]string, nv)
		}
		for k, vs := range src {
			if len(vs) > 0 {
				n := copy(sv, vs)
				dst[k] = sv[:n:n]
				sv = sv[n:]
			} else {
				dst[k] = nil
			}
		}
		return
	}
	for k, vs := range src {
		dst[k] = append(dst[k], vs...)
	}
}

const maxSmallPostFormSize = 4096

func readPostFormBody(r *Request, maxFormSize int64, isMaxBytes bool) ([]byte, error) {
	if cl := r.ContentLength; 0 < cl && cl < maxSmallPostFormSize && cl <= maxFormSize {
		// Read small bodies of known length into a closely sized slice, leaving
		// room for one extra byte so readers that return (n, nil) before EOF
		// still finish in a single allocation.
		b := make([]byte, 0, int(cl)+1)
		for {
			n, err := r.Body.Read(b[len(b):cap(b)])
			b = b[:len(b)+n]
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				return b, err
			}
			if len(b) == cap(b) {
				var reader io.Reader = r.Body
				if !isMaxBytes {
					reader = io.LimitReader(r.Body, maxFormSize-int64(len(b))+1)
				}
				rest, err := io.ReadAll(reader)
				b = append(b, rest...)
				return b, err
			}
		}
	}
	var reader io.Reader = r.Body
	if !isMaxBytes {
		reader = io.LimitReader(r.Body, maxFormSize+1)
	}
	return io.ReadAll(reader)
}

func parsePostForm(r *Request) (vs url.Values, err error) {
	if r.Body == nil {
		err = errors.New("missing form body")
		return
	}
	ct := r.Header.Get("Content-Type")
	switch ct {
	case "":
		// RFC 7231, section 3.1.1.5 - empty type
		//   MAY be treated as application/octet-stream
		return
	case "multipart/form-data":
		// handled by ParseMultipartForm (which is calling us, or should be)
		// TODO(bradfitz): there are too many possible
		// orders to call too many functions here.
		// Clean this up and write more tests.
		// request_test.go contains the start of this,
		// in TestParseMultipartFormOrder and others.
		return
	case "application/x-www-form-urlencoded":
	default:
		ct, _, err = mime.ParseMediaType(ct)
	}
	if ct != "application/x-www-form-urlencoded" {
		return
	}
	if r.Body == NoBody {
		return make(url.Values), err
	}
	maxFormSize := int64(1<<63 - 1)
	_, isMaxBytes := r.Body.(*maxBytesReader)
	if !isMaxBytes {
		maxFormSize = int64(10 << 20) // 10 MB is a lot of text.
	}
	b, e := readPostFormBody(r, maxFormSize, isMaxBytes)
	if e != nil {
		if err == nil {
			err = e
		}
		return
	}
	if int64(len(b)) > maxFormSize {
		err = errors.New("http: POST too large")
		return
	}
	if len(b) == 0 {
		return make(url.Values), err
	}
	var s string
	if cap(b)-len(b) <= len(b) {
		// b is owned by this function and has bounded spare capacity,
		// so the parsed values may safely alias it.
		s = unsafe.String(unsafe.SliceData(b), len(b))
	} else {
		// Copy small bodies read into a larger io.ReadAll buffer so
		// long-lived map entries do not pin the spare capacity.
		s = string(b)
	}
	vs, e = url.ParseQuery(s)
	if err == nil {
		err = e
	}
	return
}

// ParseForm populates r.Form and r.PostForm.
//
// For all requests, ParseForm parses the raw query from the URL and updates
// r.Form.
//
// For POST, PUT, and PATCH requests, it also reads the request body, parses it
// as a form and puts the results into both r.PostForm and r.Form. Request body
// parameters take precedence over URL query string values in r.Form.
//
// If the request Body's size has not already been limited by [MaxBytesReader],
// the size is capped at 10MB.
//
// For other HTTP methods, or when the Content-Type is not
// application/x-www-form-urlencoded, the request Body is not read, and
// r.PostForm is initialized to a non-nil, empty value.
//
// [Request.ParseMultipartForm] calls ParseForm automatically.
// ParseForm is idempotent.
func (r *Request) ParseForm() error {
	var err error
	if r.PostForm == nil {
		if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" {
			r.PostForm, err = parsePostForm(r)
		}
		if r.PostForm == nil {
			r.PostForm = make(url.Values)
		}
	}
	if r.Form == nil {
		if len(r.PostForm) > 0 {
			r.Form = make(url.Values, len(r.PostForm))
			copyValues(r.Form, r.PostForm)
		}
		var newValues url.Values
		if r.URL != nil && r.URL.RawQuery != "" {
			var e error
			newValues, e = url.ParseQuery(r.URL.RawQuery)
			if err == nil {
				err = e
			}
		}
		if r.Form == nil {
			if newValues == nil {
				newValues = make(url.Values)
			}
			r.Form = newValues
		} else if len(newValues) > 0 {
			copyValues(r.Form, newValues)
		}
	}
	return err
}

// ParseMultipartForm parses a request body as multipart/form-data.
// The whole request body is parsed and up to a total of maxMemory bytes of
// its file parts are stored in memory, with the remainder stored on
// disk in temporary files.
// ParseMultipartForm calls [Request.ParseForm] if necessary.
// If ParseForm returns an error, ParseMultipartForm returns it but also
// continues parsing the request body.
// After one call to ParseMultipartForm, subsequent calls have no effect.
func (r *Request) ParseMultipartForm(maxMemory int64) error {
	if r.MultipartForm == multipartByReader {
		return errors.New("http: multipart handled by MultipartReader")
	}
	var parseFormErr error
	if r.Form == nil {
		// Let errors in ParseForm fall through, and just
		// return it at the end.
		parseFormErr = r.ParseForm()
	}
	if r.MultipartForm != nil {
		return nil
	}

	mr, err := r.multipartReader(false)
	if err != nil {
		return err
	}

	f, err := mr.ReadForm(maxMemory)
	if err != nil {
		return err
	}

	if r.PostForm == nil {
		r.PostForm = make(url.Values)
	}
	for k, v := range f.Value {
		r.Form[k] = append(r.Form[k], v...)
		// r.PostForm should also be populated. See Issue 9305.
		r.PostForm[k] = append(r.PostForm[k], v...)
	}

	r.MultipartForm = f

	return parseFormErr
}

// FormValue returns the first value for the named component of the query.
// The precedence order:
//  1. application/x-www-form-urlencoded form body (POST, PUT, PATCH only)
//  2. query parameters (always)
//  3. multipart/form-data form body (always)
//
// FormValue calls [Request.ParseMultipartForm] and [Request.ParseForm]
// if necessary and ignores any errors returned by these functions.
// If key is not present, FormValue returns the empty string.
// To access multiple values of the same key, call ParseForm and
// then inspect [Request.Form] directly.
func (r *Request) FormValue(key string) string {
	if r.Form == nil {
		r.ParseMultipartForm(defaultMaxMemory)
	}
	if vs := r.Form[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// PostFormValue returns the first value for the named component of the POST,
// PUT, or PATCH request body. URL query parameters are ignored.
// PostFormValue calls [Request.ParseMultipartForm] and [Request.ParseForm] if necessary and ignores
// any errors returned by these functions.
// If key is not present, PostFormValue returns the empty string.
func (r *Request) PostFormValue(key string) string {
	if r.PostForm == nil {
		r.ParseMultipartForm(defaultMaxMemory)
	}
	if vs := r.PostForm[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// FormFile returns the first file for the provided form key.
// FormFile calls [Request.ParseMultipartForm] and [Request.ParseForm] if necessary.
func (r *Request) FormFile(key string) (multipart.File, *multipart.FileHeader, error) {
	if r.MultipartForm == multipartByReader {
		return nil, nil, errors.New("http: multipart handled by MultipartReader")
	}
	if r.MultipartForm == nil {
		err := r.ParseMultipartForm(defaultMaxMemory)
		if err != nil {
			return nil, nil, err
		}
	}
	if r.MultipartForm != nil && r.MultipartForm.File != nil {
		if fhs := r.MultipartForm.File[key]; len(fhs) > 0 {
			f, err := fhs[0].Open()
			return f, fhs[0], err
		}
	}
	return nil, nil, ErrMissingFile
}

// PathValue returns the value for the named path wildcard in the [ServeMux] pattern
// that matched the request.
// It returns the empty string if the request was not matched against a pattern
// or there is no such wildcard in the pattern.
//
// The value is unescaped. For example, if the pattern "/b/{bucket}" matches
// the path "/b/a%2fb", PathValue("bucket") returns "a/b".
func (r *Request) PathValue(name string) string {
	if i := r.patIndex(name); i >= 0 {
		return r.matches[i]
	}
	for i := r.patWildcardCount(); i+1 < len(r.matches); i += 2 {
		if r.matches[i] == name {
			return r.matches[i+1]
		}
	}
	return ""
}

// SetPathValue sets name to value, so that subsequent calls to r.PathValue(name)
// return value.
// It does not unescape value.
func (r *Request) SetPathValue(name, value string) {
	if i := r.patIndex(name); i >= 0 {
		r.matches[i] = value
		return
	}
	for i := r.patWildcardCount(); i+1 < len(r.matches); i += 2 {
		if r.matches[i] == name {
			r.matches[i+1] = value
			return
		}
	}
	r.matches = append(r.matches, name, value)
}

func (r *Request) patWildcardCount() int {
	if r.pat == nil {
		return 0
	}
	n := 0
	for _, seg := range r.pat.segments {
		if seg.wild && seg.s != "" {
			n++
		}
	}
	return n
}

// patIndex returns the index of name in the list of named wildcards of the
// request's pattern, or -1 if there is no such name.
func (r *Request) patIndex(name string) int {
	// The linear search seems expensive compared to a map, but just creating the map
	// takes a lot of time, and most patterns will just have a couple of wildcards.
	if r.pat == nil {
		return -1
	}
	i := 0
	for _, seg := range r.pat.segments {
		if seg.wild && seg.s != "" {
			if name == seg.s {
				return i
			}
			i++
		}
	}
	return -1
}

func (r *Request) expectsContinue() bool {
	return httpguts.HeaderValuesContainsToken(r.Header["Expect"], "100-continue")
}

func (r *Request) wantsHttp10KeepAlive() bool {
	if r.ProtoMajor != 1 || r.ProtoMinor != 0 {
		return false
	}
	return httpguts.HeaderValuesContainsToken(r.Header["Connection"], "keep-alive")
}

func (r *Request) wantsClose() bool {
	return r.Close || httpguts.HeaderValuesContainsToken(r.Header["Connection"], "close")
}

func (r *Request) closeBody() error {
	if r.Body == nil {
		return nil
	}
	return r.Body.Close()
}

func (r *Request) isReplayable() bool {
	if r.Body == nil || r.Body == NoBody || r.GetBody != nil {
		switch valueOrDefault(r.Method, "GET") {
		case "GET", "HEAD", "OPTIONS", "TRACE", "QUERY":
			return true
		}
		// The Idempotency-Key, while non-standard, is widely used to
		// mean a POST or other request is idempotent. See
		// https://golang.org/issue/19943#issuecomment-421092421
		if r.Header.has("Idempotency-Key") || r.Header.has("X-Idempotency-Key") {
			return true
		}
	}
	return false
}

// outgoingLength reports the Content-Length of this outgoing (Client) request.
// It maps 0 into -1 (unknown) when the Body is non-nil.
func (r *Request) outgoingLength() int64 {
	if r.Body == nil || r.Body == NoBody {
		return 0
	}
	if r.ContentLength != 0 {
		return r.ContentLength
	}
	return -1
}

// requestMethodUsuallyLacksBody reports whether the given request
// method is one that typically does not involve a request body.
// This is used by the Transport (via
// transferWriter.shouldSendChunkedRequestBody) to determine whether
// we try to test-read a byte from a non-nil Request.Body when
// Request.outgoingLength() returns -1. See the comments in
// shouldSendChunkedRequestBody.
func requestMethodUsuallyLacksBody(method string) bool {
	switch method {
	case "GET", "HEAD", "DELETE", "OPTIONS", "PROPFIND", "SEARCH":
		return true
	}
	return false
}

// requiresHTTP1 reports whether this request requires being sent on
// an HTTP/1 connection.
func (r *Request) requiresHTTP1() bool {
	return httpguts.HeaderValuesContainsToken(r.Header["Connection"], "upgrade") &&
		ascii.EqualFold(r.Header.Get("Upgrade"), "websocket")
}
