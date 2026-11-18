// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httptest

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/internal"
	"net/textproto"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// ResponseRecorder is an implementation of [http.ResponseWriter] that
// records its mutations for later inspection in tests.
type ResponseRecorder struct {
	// Code is the HTTP response code set by WriteHeader.
	//
	// Note that if a Handler never calls WriteHeader or Write,
	// this might end up being 0, rather than the implicit
	// http.StatusOK. To get the implicit value, use the Result
	// method.
	Code int

	// HeaderMap contains the headers explicitly set by the Handler.
	// It is an internal detail.
	//
	// Deprecated: HeaderMap exists for historical compatibility
	// and should not be used. To access the headers returned by a handler,
	// use the Response.Header map as returned by the Result method.
	HeaderMap http.Header

	// Body is the buffer to which the Handler's Write calls are sent.
	// If nil, the Writes are silently discarded.
	Body *bytes.Buffer

	// Flushed is whether the Handler called Flush.
	Flushed bool

	result      *http.Response // cache of Result's return value
	snapHeader  http.Header    // snapshot of HeaderMap at first Write
	wroteHeader bool
}

// NewRecorder returns an initialized [ResponseRecorder].
func NewRecorder() *ResponseRecorder {
	return &ResponseRecorder{
		HeaderMap: make(http.Header),
		Body:      new(bytes.Buffer),
		Code:      200,
	}
}

// DefaultRemoteAddr is the default remote address to return in RemoteAddr if
// an explicit DefaultRemoteAddr isn't set on [ResponseRecorder].
const DefaultRemoteAddr = "1.2.3.4"

// Header implements [http.ResponseWriter]. It returns the response
// headers to mutate within a handler. To test the headers that were
// written after a handler completes, use the [ResponseRecorder.Result] method and see
// the returned Response value's Header.
func (rw *ResponseRecorder) Header() http.Header {
	m := rw.HeaderMap
	if m == nil {
		m = make(http.Header)
		rw.HeaderMap = m
	}
	return m
}

// writeHeader writes a header if it was not written yet and
// detects Content-Type if needed.
//
// bytes or str are the beginning of the response body.
// We pass both to avoid unnecessarily generate garbage
// in rw.WriteString which was created for performance reasons.
// Non-nil bytes win.
func (rw *ResponseRecorder) writeHeader(b []byte, str string) {
	if rw.wroteHeader {
		return
	}
	if len(str) > 512 {
		str = str[:512]
	}

	m := rw.Header()

	_, hasType := m["Content-Type"]
	te := m["Transfer-Encoding"]
	hasTE := len(te) > 0 && te[0] != ""
	if !hasType && !hasTE {
		if b == nil {
			if str != "" {
				var buf [512]byte
				n := copy(buf[:], str)
				b = buf[:n]
			}
		}
		m["Content-Type"] = []string{http.DetectContentType(b)}
	}

	rw.WriteHeader(200)
}

// Write implements http.ResponseWriter. The data in buf is written to
// rw.Body, if not nil.
func (rw *ResponseRecorder) Write(buf []byte) (int, error) {
	// Record the write, even if we're going to return an error.
	rw.writeHeader(buf, "")
	if rw.Body != nil {
		rw.Body.Write(buf)
	}
	if !bodyAllowedForStatus(rw.Code) {
		return 0, http.ErrBodyNotAllowed
	}
	return len(buf), nil
}

// WriteString implements [io.StringWriter]. The data in str is written
// to rw.Body, if not nil.
func (rw *ResponseRecorder) WriteString(str string) (int, error) {
	// Record the write, even if we're going to return an error.
	rw.writeHeader(nil, str)
	if rw.Body != nil {
		rw.Body.WriteString(str)
	}
	if !bodyAllowedForStatus(rw.Code) {
		return 0, http.ErrBodyNotAllowed
	}
	return len(str), nil
}

// bodyAllowedForStatus reports whether a given response status code
// permits a body. See RFC 7230, section 3.3.
func bodyAllowedForStatus(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == 204:
		return false
	case status == 304:
		return false
	}
	return true
}

func checkWriteHeaderCode(code int) {
	// Issue 22880: require valid WriteHeader status codes.
	// For now we only enforce that it's three digits.
	// In the future we might block things over 599 (600 and above aren't defined
	// at https://httpwg.org/specs/rfc7231.html#status.codes)
	// and we might block under 200 (once we have more mature 1xx support).
	// But for now any three digits.
	//
	// We used to send "HTTP/1.1 000 0" on the wire in responses but there's
	// no equivalent bogus thing we can realistically send in HTTP/2,
	// so we'll consistently panic instead and help people find their bugs
	// early. (We can't return an error from WriteHeader even if we wanted to.)
	if code < 100 || code > 999 {
		panic(fmt.Sprintf("invalid WriteHeader code %v", code))
	}
}

// WriteHeader implements [http.ResponseWriter].
func (rw *ResponseRecorder) WriteHeader(code int) {
	if rw.wroteHeader {
		return
	}

	checkWriteHeaderCode(code)
	rw.Code = code
	rw.wroteHeader = true
	if rw.HeaderMap == nil {
		rw.HeaderMap = make(http.Header)
	}
	rw.snapHeader = rw.HeaderMap.Clone()
}

// Flush implements [http.Flusher]. To test whether Flush was
// called, see rw.Flushed.
func (rw *ResponseRecorder) Flush() {
	if !rw.wroteHeader {
		rw.WriteHeader(200)
	}
	rw.Flushed = true
}

type nopCloserBytesReader struct {
	r bytes.Reader
}

func (n *nopCloserBytesReader) Read(p []byte) (int, error) {
	return n.r.Read(p)
}

func (n *nopCloserBytesReader) WriteTo(w io.Writer) (int64, error) {
	return n.r.WriteTo(w)
}

func (*nopCloserBytesReader) Close() error {
	return nil
}

// Result returns the response generated by the handler.
//
// The returned Response will have at least its StatusCode,
// Header, Body, and optionally Trailer populated.
// More fields may be populated in the future, so callers should
// not DeepEqual the result in tests.
//
// The Response.Header is a snapshot of the headers at the time of the
// first write call, or at the time of this call, if the handler never
// did a write.
//
// The Response.Body is guaranteed to be non-nil and Body.Read call is
// guaranteed to not return any error other than [io.EOF].
//
// Result must only be called after the handler has finished running.
func (rw *ResponseRecorder) Result() *http.Response {
	if rw.result != nil {
		return rw.result
	}
	if rw.snapHeader == nil {
		rw.snapHeader = rw.HeaderMap.Clone()
	}
	res := &http.Response{
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		StatusCode: rw.Code,
		Header:     rw.snapHeader,
	}
	rw.result = res
	if res.StatusCode == 0 {
		res.StatusCode = 200
	}
	if s := internal.StatusCodeText(res.StatusCode); s != "" {
		res.Status = s
	} else if res.StatusCode >= 100 && res.StatusCode <= 999 {
		var buf [4]byte
		c := res.StatusCode
		buf[0] = byte('0' + c/100)
		buf[1] = byte('0' + (c/10)%10)
		buf[2] = byte('0' + c%10)
		buf[3] = ' '
		res.Status = string(buf[:])
	} else {
		res.Status = fmt.Sprintf("%03d %s", res.StatusCode, http.StatusText(res.StatusCode))
	}
	if rw.Body != nil && rw.Body.Len() > 0 {
		r := &nopCloserBytesReader{}
		r.r.Reset(rw.Body.Bytes())
		res.Body = r
	} else {
		res.Body = http.NoBody
	}
	var cl string
	if vv := res.Header["Content-Length"]; len(vv) > 0 {
		cl = vv[0]
	}
	res.ContentLength = parseContentLength(cl)

	if trailers, ok := rw.snapHeader["Trailer"]; ok {
		res.Trailer = make(http.Header, len(trailers))
		for _, k := range trailers {
			for {
				name, rest, hasComma := strings.Cut(k, ",")
				name = http.CanonicalHeaderKey(textproto.TrimString(name))
				if httpguts.ValidTrailerHeader(name) {
					if vv, ok := rw.HeaderMap[name]; ok {
						vv2 := make([]string, len(vv))
						copy(vv2, vv)
						res.Trailer[name] = vv2
					}
				}
				if !hasComma {
					break
				}
				k = rest
			}
		}
	}
	for k, vv := range rw.HeaderMap {
		if !strings.HasPrefix(k, http.TrailerPrefix) {
			continue
		}
		if res.Trailer == nil {
			res.Trailer = make(http.Header)
		}
		for _, v := range vv {
			res.Trailer.Add(strings.TrimPrefix(k, http.TrailerPrefix), v)
		}
	}
	return res
}

// parseContentLength trims whitespace from s and returns -1 if no value
// is set, or the value if it's >= 0.
//
// This a modified version of same function found in net/http/transfer.go. This
// one just ignores an invalid header.
func parseContentLength(cl string) int64 {
	cl = textproto.TrimString(cl)
	if cl == "" {
		return -1
	}
	n, err := strconv.ParseUint(cl, 10, 63)
	if err != nil {
		return -1
	}
	return int64(n)
}
