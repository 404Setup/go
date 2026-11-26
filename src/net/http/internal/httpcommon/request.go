// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httpcommon

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"
)

func headerFieldSize(name, value string) uint64 {
	// Matches hpack.HeaderField.Size (RFC 7541 section 4.1):
	// len(name) + len(value) + 32 as uint32.
	return uint64(uint32(len(name) + len(value) + 32))
}

var (
	ErrRequestHeaderListSize = errors.New("request header list larger than peer's advertised limit")
)

// Request is a subset of http.Request.
// It'd be simpler to pass an *http.Request, of course, but we can't depend on net/http
// without creating a dependency cycle.
type Request struct {
	URL                 *url.URL
	Method              string
	Host                string
	Header              map[string][]string
	Trailer             map[string][]string
	ActualContentLength int64 // 0 means 0, -1 means unknown
}

// EncodeHeadersParam is parameters to EncodeHeaders.
type EncodeHeadersParam struct {
	Request Request

	// AddGzipHeader indicates that an "accept-encoding: gzip" header should be
	// added to the request.
	AddGzipHeader bool

	// PeerMaxHeaderListSize, when non-zero, is the peer's MAX_HEADER_LIST_SIZE setting.
	PeerMaxHeaderListSize uint64

	// DefaultUserAgent is the User-Agent header to send when the request
	// neither contains a User-Agent nor disables it.
	DefaultUserAgent string
}

// EncodeHeadersResult is the result of EncodeHeaders.
type EncodeHeadersResult struct {
	HasBody     bool
	HasTrailers bool
}

// EncodeHeaders constructs request headers common to HTTP/2 and HTTP/3.
// It validates a request and calls headerf with each pseudo-header and header
// for the request.
// The headerf function is called with the validated, canonicalized header name.
func EncodeHeaders(ctx context.Context, param EncodeHeadersParam, headerf func(name, value string)) (res EncodeHeadersResult, _ error) {
	req := param.Request

	// Check for invalid connection-level headers.
	if len(req.Header) > 0 {
		if err := checkConnHeaders(req.Header); err != nil {
			return res, err
		}
	}

	if req.URL == nil {
		return res, errors.New("Request.URL is nil")
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	host, err := httpguts.PunycodeHostPort(host)
	if err != nil {
		return res, err
	}
	if !httpguts.ValidHostHeader(host) {
		return res, errors.New("invalid Host header")
	}

	// isNormalConnect is true if this is a non-extended CONNECT request.
	isNormalConnect := false
	var protocol string
	if len(req.Header) > 0 {
		if vv := req.Header[":protocol"]; len(vv) > 0 {
			protocol = vv[0]
		}
	}
	if req.Method == "CONNECT" && protocol == "" {
		isNormalConnect = true
	} else if protocol != "" && req.Method != "CONNECT" {
		return res, errors.New("invalid :protocol header in non-CONNECT request")
	}

	// Validate the path, except for non-extended CONNECT requests which have no path.
	var path string
	if !isNormalConnect {
		path = req.URL.RequestURI()
		if !validPseudoPath(path) {
			orig := path
			path = strings.TrimPrefix(path, req.URL.Scheme+"://"+host)
			if !validPseudoPath(path) {
				if req.URL.Opaque != "" {
					return res, fmt.Errorf("invalid request :path %q from URL.Opaque = %q", orig, req.URL.Opaque)
				} else {
					return res, fmt.Errorf("invalid request :path %q", orig)
				}
			}
		}
	}

	// Validate headers and, when PeerMaxHeaderListSize is enforced, accumulate
	// the request header list size in the same pass to avoid iterating req.Header
	// three times before polluting encoder state.
	checkSize := param.PeerMaxHeaderListSize > 0
	var hlSize uint64
	var didUA bool
	for k, vv := range req.Header {
		if !httpguts.ValidHeaderFieldName(k) && k != ":protocol" {
			return res, fmt.Errorf("invalid HTTP header name %q", k)
		}
		for _, v := range vv {
			if !httpguts.ValidHeaderFieldValue(v) {
				// Don't include the value in the error,
				// because it may be sensitive.
				return res, fmt.Errorf("invalid HTTP header value for header %q", k)
			}
		}
		if !checkSize {
			continue
		}
		switch len(k) {
		case 4:
			if k == "Host" || asciiEqualFold(k, "host") {
				continue
			}
		case 6:
			if k == "Cookie" || asciiEqualFold(k, "cookie") {
				for _, v := range vv {
					for {
						p := strings.IndexByte(v, ';')
						if p < 0 {
							break
						}
						hlSize += headerFieldSize("cookie", v[:p])
						p++
						for p+1 <= len(v) && v[p] == ' ' {
							p++
						}
						v = v[p:]
					}
					if len(v) > 0 {
						hlSize += headerFieldSize("cookie", v)
					}
				}
				continue
			}
		case 7:
			if k == "Upgrade" || asciiEqualFold(k, "upgrade") {
				continue
			}
		case 9:
			if k == ":protocol" {
				continue
			}
		case 10:
			if k == "User-Agent" || asciiEqualFold(k, "user-agent") {
				didUA = true
				if len(vv) < 1 || vv[0] == "" {
					continue
				}
				hlSize += headerFieldSize("user-agent", vv[0])
				continue
			} else if k == "Connection" || k == "Keep-Alive" ||
				asciiEqualFold(k, "connection") || asciiEqualFold(k, "keep-alive") {
				continue
			}
		case 14:
			if k == "Content-Length" || asciiEqualFold(k, "content-length") {
				continue
			}
		case 16:
			if k == "Proxy-Connection" || asciiEqualFold(k, "proxy-connection") {
				continue
			}
		case 17:
			if k == "Transfer-Encoding" || asciiEqualFold(k, "transfer-encoding") {
				continue
			}
		}
		for _, v := range vv {
			hlSize += headerFieldSize(k, v)
		}
	}

	var trailers string
	if len(req.Trailer) > 0 {
		if err := validateHeaders(req.Trailer); err != "" {
			return res, fmt.Errorf("invalid HTTP trailer %s", err)
		}
		trailers, err = commaSeparatedTrailers(req.Trailer)
		if err != nil {
			return res, err
		}
	}

	m := req.Method
	if m == "" {
		m = "GET"
	}
	var contentLengthStr string
	sendCL := shouldSendReqContentLength(req.Method, req.ActualContentLength)
	if sendCL {
		contentLengthStr = strconv.FormatInt(req.ActualContentLength, 10)
	}

	if checkSize {
		hlSize += headerFieldSize(":authority", host)
		hlSize += headerFieldSize(":method", m)
		if !isNormalConnect {
			hlSize += headerFieldSize(":path", path)
			hlSize += headerFieldSize(":scheme", req.URL.Scheme)
		}
		if protocol != "" {
			hlSize += headerFieldSize(":protocol", protocol)
		}
		if trailers != "" {
			hlSize += headerFieldSize("trailer", trailers)
		}
		if sendCL {
			hlSize += headerFieldSize("content-length", contentLengthStr)
		}
		if param.AddGzipHeader {
			hlSize += headerFieldSize("accept-encoding", "gzip")
		}
		if !didUA {
			hlSize += headerFieldSize("user-agent", param.DefaultUserAgent)
		}
		if hlSize > param.PeerMaxHeaderListSize {
			return res, ErrRequestHeaderListSize
		}
	}

	trace := httptrace.ContextClientTrace(ctx)
	traceWrote := trace != nil && trace.WroteHeaderField != nil
	emitLower := func(name, value string) {
		headerf(name, value)
		if traceWrote {
			trace.WroteHeaderField(name, []string{value})
		}
	}

	// 8.1.2.3 Request Pseudo-Header Fields
	// The :path pseudo-header field includes the path and query parts of the
	// target URI (the path-absolute production and optionally a '?' character
	// followed by the query production, see Sections 3.3 and 3.4 of
	// [RFC3986]).
	emitLower(":authority", host)
	emitLower(":method", m)
	if !isNormalConnect {
		emitLower(":path", path)
		emitLower(":scheme", req.URL.Scheme)
	}
	if protocol != "" {
		emitLower(":protocol", protocol)
	}
	if trailers != "" {
		emitLower("trailer", trailers)
	}

	didUA = false
	for k, vv := range req.Header {
		switch len(k) {
		case 4:
			if k == "Host" || asciiEqualFold(k, "host") {
				// Host is :authority, already sent.
				continue
			}
		case 6:
			if k == "Cookie" || asciiEqualFold(k, "cookie") {
				// Per 8.1.2.5 To allow for better compression efficiency, the
				// Cookie header field MAY be split into separate header fields,
				// each with one or more cookie-pairs.
				for _, v := range vv {
					for {
						p := strings.IndexByte(v, ';')
						if p < 0 {
							break
						}
						emitLower("cookie", v[:p])
						p++
						// strip space after semicolon if any.
						for p+1 <= len(v) && v[p] == ' ' {
							p++
						}
						v = v[p:]
					}
					if len(v) > 0 {
						emitLower("cookie", v)
					}
				}
				continue
			}
		case 7:
			if k == "Upgrade" || asciiEqualFold(k, "upgrade") {
				continue
			}
		case 9:
			if k == ":protocol" {
				// :protocol pseudo-header was already sent above.
				continue
			}
		case 10:
			if k == "User-Agent" || asciiEqualFold(k, "user-agent") {
				// Match Go's http1 behavior: at most one
				// User-Agent. If set to nil or empty string,
				// then omit it. Otherwise if not mentioned,
				// include the default (below).
				didUA = true
				if len(vv) < 1 || vv[0] == "" {
					continue
				}
				emitLower("user-agent", vv[0])
				continue
			} else if k == "Connection" || k == "Keep-Alive" ||
				asciiEqualFold(k, "connection") || asciiEqualFold(k, "keep-alive") {
				continue
			}
		case 14:
			if k == "Content-Length" || asciiEqualFold(k, "content-length") {
				// Content-Length is automatic, set below.
				continue
			}
		case 16:
			if k == "Proxy-Connection" || asciiEqualFold(k, "proxy-connection") {
				continue
			}
		case 17:
			if k == "Transfer-Encoding" || asciiEqualFold(k, "transfer-encoding") {
				continue
			}
		}

		if len(vv) == 0 {
			continue
		}
		name, ascii := LowerHeader(k)
		if !ascii {
			// Skip writing invalid headers. Per RFC 7540, Section 8.1.2, header
			// field names have to be ASCII characters (just as in HTTP/1.x).
			continue
		}
		for _, v := range vv {
			emitLower(name, v)
		}
	}
	if sendCL {
		emitLower("content-length", contentLengthStr)
	}
	if param.AddGzipHeader {
		emitLower("accept-encoding", "gzip")
	}
	if !didUA {
		emitLower("user-agent", param.DefaultUserAgent)
	}

	res.HasBody = req.ActualContentLength != 0
	res.HasTrailers = trailers != ""
	return res, nil
}

// IsRequestGzip reports whether we should add an Accept-Encoding: gzip header
// for a request.
func IsRequestGzip(method string, header map[string][]string, disableCompression bool) bool {
	// TODO(bradfitz): this is a copy of the logic in net/http. Unify somewhere?
	if !disableCompression &&
		len(header["Accept-Encoding"]) == 0 &&
		len(header["Range"]) == 0 &&
		method != "HEAD" {
		// Request gzip only, not deflate. Deflate is ambiguous and
		// not as universally supported anyway.
		// See: https://zlib.net/zlib_faq.html#faq39
		//
		// Note that we don't request this for HEAD requests,
		// due to a bug in nginx:
		//   http://trac.nginx.org/nginx/ticket/358
		//   https://golang.org/issue/5522
		//
		// We don't request gzip if the request is for a range, since
		// auto-decoding a portion of a gzipped document will just fail
		// anyway. See https://golang.org/issue/8923
		return true
	}
	return false
}

// checkConnHeaders checks whether req has any invalid connection-level headers.
//
// https://www.rfc-editor.org/rfc/rfc9114.html#section-4.2-3
// https://www.rfc-editor.org/rfc/rfc9113.html#section-8.2.2-1
//
// Certain headers are special-cased as okay but not transmitted later.
// For example, we allow "Transfer-Encoding: chunked", but drop the header when encoding.
func checkConnHeaders(h map[string][]string) error {
	if vv := h["Upgrade"]; len(vv) > 0 && (vv[0] != "" && vv[0] != "chunked") {
		return fmt.Errorf("invalid Upgrade request header: %q", vv)
	}
	if vv := h["Transfer-Encoding"]; len(vv) > 0 && (len(vv) > 1 || vv[0] != "" && vv[0] != "chunked") {
		return fmt.Errorf("invalid Transfer-Encoding request header: %q", vv)
	}
	if vv := h["Connection"]; len(vv) > 0 && (len(vv) > 1 || vv[0] != "" && !asciiEqualFold(vv[0], "close") && !asciiEqualFold(vv[0], "keep-alive")) {
		return fmt.Errorf("invalid Connection request header: %q", vv)
	}
	return nil
}

func commaSeparatedTrailers(trailer map[string][]string) (string, error) {
	n := len(trailer)
	if n == 0 {
		return "", nil
	}
	if n == 1 {
		for k := range trailer {
			k = CanonicalHeader(k)
			switch k {
			case "Transfer-Encoding", "Trailer", "Content-Length":
				return "", fmt.Errorf("invalid Trailer key %q", k)
			}
			return k, nil
		}
	}
	var stack [8]string
	var keys []string
	if n <= len(stack) {
		keys = stack[:0]
	} else {
		keys = make([]string, 0, n)
	}
	for k := range trailer {
		k = CanonicalHeader(k)
		switch k {
		case "Transfer-Encoding", "Trailer", "Content-Length":
			return "", fmt.Errorf("invalid Trailer key %q", k)
		}
		keys = append(keys, k)
	}
	if len(keys) <= 16 {
		for i := 1; i < len(keys); i++ {
			cur := keys[i]
			j := i - 1
			for j >= 0 && keys[j] > cur {
				keys[j+1] = keys[j]
				j--
			}
			keys[j+1] = cur
		}
	} else {
		slices.Sort(keys)
	}
	return strings.Join(keys, ","), nil
}

// validPseudoPath reports whether v is a valid :path pseudo-header
// value. It must be either:
//
//   - a non-empty string starting with '/'
//   - the string '*', for OPTIONS requests.
//
// For now this is only used a quick check for deciding when to clean
// up Opaque URLs before sending requests from the Transport.
// See golang.org/issue/16847
//
// We used to enforce that the path also didn't start with "//", but
// Google's GFE accepts such paths and Chrome sends them, so ignore
// that part of the spec. See golang.org/issue/19103.
func validPseudoPath(v string) bool {
	return (len(v) > 0 && v[0] == '/') || v == "*"
}

func validateHeaders(hdrs map[string][]string) string {
	for k, vv := range hdrs {
		if !httpguts.ValidHeaderFieldName(k) && k != ":protocol" {
			return fmt.Sprintf("name %q", k)
		}
		for _, v := range vv {
			if !httpguts.ValidHeaderFieldValue(v) {
				// Don't include the value in the error,
				// because it may be sensitive.
				return fmt.Sprintf("value for header %q", k)
			}
		}
	}
	return ""
}

// shouldSendReqContentLength reports whether we should send
// a "content-length" request header. This logic is basically a copy of the net/http
// transferWriter.shouldSendContentLength.
// The contentLength is the corrected contentLength (so 0 means actually 0, not unknown).
// -1 means unknown.
func shouldSendReqContentLength(method string, contentLength int64) bool {
	if contentLength > 0 {
		return true
	}
	if contentLength < 0 {
		return false
	}
	// For zero bodies, whether we send a content-length depends on the method.
	// It also kinda doesn't matter for http2 either way, with END_STREAM.
	switch method {
	case "POST", "PUT", "PATCH":
		return true
	default:
		return false
	}
}

// ServerRequestParam is parameters to NewServerRequest.
type ServerRequestParam struct {
	Method                  string
	Scheme, Authority, Path string
	Protocol                string
	Header                  map[string][]string
}

// ServerRequestResult is the result of NewServerRequest.
type ServerRequestResult struct {
	// Various http.Request fields.
	Host       string
	URL        *url.URL
	RequestURI string
	Trailer    map[string][]string

	NeedsContinue bool // client provided an "Expect: 100-continue" header

	// If the request should be rejected, this is a short string suitable for passing
	// to the http2 package's CountError function.
	// It might be a bit odd to return errors this way rather than returning an error,
	// but this ensures we don't forget to include a CountError reason.
	InvalidReason string
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

// parseCleanOriginRequestURI parses an unescaped origin-form request URI
// (starting with '/') optionally followed by '?' and a query without ASCII
// control bytes. It returns nil if rawURI needs full url.ParseRequestURI.
func parseCleanOriginRequestURI(rawURI string) *url.URL {
	if len(rawURI) == 0 || rawURI[0] != '/' {
		return nil
	}
	for i := 0; i < len(rawURI); i++ {
		c := rawURI[i]
		if !cleanPathTable[c] {
			if c == '?' && i > 0 {
				for j := i + 1; j < len(rawURI); j++ {
					qc := rawURI[j]
					if qc < ' ' || qc == 0x7f {
						return nil
					}
				}
				if i == len(rawURI)-1 {
					return &url.URL{Path: rawURI[:i], ForceQuery: true}
				}
				return &url.URL{Path: rawURI[:i], RawQuery: rawURI[i+1:]}
			}
			return nil
		}
	}
	return &url.URL{Path: rawURI}
}

func parseCleanOriginRequestURIInto(rawURI string, dst *url.URL) bool {
	if len(rawURI) == 0 || rawURI[0] != '/' {
		return false
	}
	for i := 0; i < len(rawURI); i++ {
		c := rawURI[i]
		if !cleanPathTable[c] {
			if c == '?' && i > 0 {
				for j := i + 1; j < len(rawURI); j++ {
					qc := rawURI[j]
					if qc < ' ' || qc == 0x7f {
						return false
					}
				}
				if i == len(rawURI)-1 {
					*dst = url.URL{Path: rawURI[:i], ForceQuery: true}
					return true
				}
				*dst = url.URL{Path: rawURI[:i], RawQuery: rawURI[i+1:]}
				return true
			}
			return false
		}
	}
	*dst = url.URL{Path: rawURI}
	return true
}

func NewServerRequest(rp ServerRequestParam) ServerRequestResult {
	return NewServerRequestInto(rp, nil)
}

func NewServerRequestInto(rp ServerRequestParam, urlDst *url.URL) ServerRequestResult {
	var needsContinue bool
	var trailer map[string][]string
	authority := rp.Authority

	if len(rp.Header) > 0 {
		if expect := rp.Header["Expect"]; len(expect) > 0 {
			needsContinue = httpguts.HeaderValuesContainsToken(expect, "100-continue")
			if needsContinue {
				delete(rp.Header, "Expect")
			}
		}
		// Merge Cookie headers into one "; "-delimited value.
		if cookies := rp.Header["Cookie"]; len(cookies) > 1 {
			rp.Header["Cookie"] = []string{strings.Join(cookies, "; ")}
		}

		// Setup Trailers
		if trVals, ok := rp.Header["Trailer"]; ok {
			for _, v := range trVals {
				for key := range strings.SplitSeq(v, ",") {
					key = CanonicalHeader(textproto.TrimString(key))
					switch key {
					case "Transfer-Encoding", "Trailer", "Content-Length":
						// Bogus. (copy of http1 rules)
						// Ignore.
					default:
						if trailer == nil {
							trailer = make(map[string][]string)
						}
						trailer[key] = nil
					}
				}
			}
			delete(rp.Header, "Trailer")
		}

		if host := rp.Header["Host"]; len(host) == 1 {
			// HTTP/2 and HTTP/3 permit the Host header to be present,
			// but it must match the :authority pseudo-header.
			if authority == "" {
				authority = host[0]
			} else if host[0] != authority {
				return ServerRequestResult{
					InvalidReason: "authority_host_mismatch",
				}
			}
			delete(rp.Header, "Host")
		} else if len(host) > 1 {
			// HTTP/1.1 rejects any request containing more than one Host header.
			// HTTP/2 and HTTP/3 don't use the Host header, but reject multiple anyway.
			return ServerRequestResult{
				InvalidReason: "multiple_host_headers",
			}
		}
	}

	// "':authority' MUST NOT include the deprecated userinfo subcomponent
	// for "http" or "https" schemed URIs."
	// https://www.rfc-editor.org/rfc/rfc9113.html#section-8.3.1-2.3.8
	if strings.IndexByte(authority, '@') != -1 && (rp.Scheme == "http" || rp.Scheme == "https") {
		return ServerRequestResult{
			InvalidReason: "userinfo_in_authority",
		}
	}

	if authority != "" && !httpguts.ValidHostHeader(authority) {
		return ServerRequestResult{
			InvalidReason: "invalid_authority",
		}
	}

	var url_ *url.URL
	var requestURI string
	if rp.Method == "CONNECT" && rp.Protocol == "" {
		if urlDst != nil {
			*urlDst = url.URL{Host: authority}
			url_ = urlDst
		} else {
			url_ = &url.URL{Host: authority}
		}
		requestURI = authority // mimic HTTP/1 server behavior
	} else {
		// "[The :path] pseudo-header field MUST NOT be empty [...]"
		// https://www.rfc-editor.org/rfc/rfc9113.html#section-8.3.1-2.4.2
		if rp.Path == "" || (rp.Path[0] != '/' && rp.Path != "*") {
			return ServerRequestResult{
				InvalidReason: "bad_path",
			}
		}

		if urlDst != nil && parseCleanOriginRequestURIInto(rp.Path, urlDst) {
			url_ = urlDst
		} else if u := parseCleanOriginRequestURI(rp.Path); u != nil {
			url_ = u
		} else {
			var err error
			url_, err = url.ParseRequestURI(rp.Path)
			if err != nil {
				return ServerRequestResult{
					InvalidReason: "bad_path",
				}
			}
			if urlDst != nil {
				*urlDst = *url_
				url_ = urlDst
			}
		}
		requestURI = rp.Path
	}

	return ServerRequestResult{
		Host:          authority,
		URL:           url_,
		NeedsContinue: needsContinue,
		RequestURI:    requestURI,
		Trailer:       trailer,
	}
}
