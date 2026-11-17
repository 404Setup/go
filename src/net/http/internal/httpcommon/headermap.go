// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httpcommon

import "net/textproto"

// LowerHeader returns the lowercase form of a header name,
// used on the wire for HTTP/2 and HTTP/3 requests.
func LowerHeader(v string) (lower string, ascii bool) {
	switch v {
	case "Accept":
		return "accept", true
	case "Accept-Charset":
		return "accept-charset", true
	case "Accept-Encoding":
		return "accept-encoding", true
	case "Accept-Language":
		return "accept-language", true
	case "Accept-Ranges":
		return "accept-ranges", true
	case "Age":
		return "age", true
	case "Access-Control-Allow-Credentials":
		return "access-control-allow-credentials", true
	case "Access-Control-Allow-Headers":
		return "access-control-allow-headers", true
	case "Access-Control-Allow-Methods":
		return "access-control-allow-methods", true
	case "Access-Control-Allow-Origin":
		return "access-control-allow-origin", true
	case "Access-Control-Expose-Headers":
		return "access-control-expose-headers", true
	case "Access-Control-Max-Age":
		return "access-control-max-age", true
	case "Access-Control-Request-Headers":
		return "access-control-request-headers", true
	case "Access-Control-Request-Method":
		return "access-control-request-method", true
	case "Allow":
		return "allow", true
	case "Authorization":
		return "authorization", true
	case "Cache-Control":
		return "cache-control", true
	case "Content-Disposition":
		return "content-disposition", true
	case "Content-Encoding":
		return "content-encoding", true
	case "Content-Language":
		return "content-language", true
	case "Content-Length":
		return "content-length", true
	case "Content-Location":
		return "content-location", true
	case "Content-Range":
		return "content-range", true
	case "Content-Type":
		return "content-type", true
	case "Cookie":
		return "cookie", true
	case "Date":
		return "date", true
	case "Etag":
		return "etag", true
	case "Expect":
		return "expect", true
	case "Expires":
		return "expires", true
	case "From":
		return "from", true
	case "Host":
		return "host", true
	case "If-Match":
		return "if-match", true
	case "If-Modified-Since":
		return "if-modified-since", true
	case "If-None-Match":
		return "if-none-match", true
	case "If-Unmodified-Since":
		return "if-unmodified-since", true
	case "Last-Modified":
		return "last-modified", true
	case "Link":
		return "link", true
	case "Location":
		return "location", true
	case "Max-Forwards":
		return "max-forwards", true
	case "Origin":
		return "origin", true
	case "Proxy-Authenticate":
		return "proxy-authenticate", true
	case "Proxy-Authorization":
		return "proxy-authorization", true
	case "Range":
		return "range", true
	case "Referer":
		return "referer", true
	case "Refresh":
		return "refresh", true
	case "Retry-After":
		return "retry-after", true
	case "Server":
		return "server", true
	case "Set-Cookie":
		return "set-cookie", true
	case "Strict-Transport-Security":
		return "strict-transport-security", true
	case "Trailer":
		return "trailer", true
	case "Transfer-Encoding":
		return "transfer-encoding", true
	case "User-Agent":
		return "user-agent", true
	case "Vary":
		return "vary", true
	case "Via":
		return "via", true
	case "Www-Authenticate":
		return "www-authenticate", true
	case "X-Forwarded-For":
		return "x-forwarded-for", true
	case "X-Forwarded-Proto":
		return "x-forwarded-proto", true
	}
	return asciiToLower(v)
}

// CanonicalHeader canonicalizes a header name. (For example, "host" becomes "Host".)
func CanonicalHeader(v string) string {
	if s, ok := CachedCanonicalHeader(v); ok {
		return s
	}
	return textproto.CanonicalMIMEHeaderKey(v)
}

// CachedCanonicalHeader returns the canonical form of a well-known header name.
func CachedCanonicalHeader(v string) (string, bool) {
	switch v {
	case "accept":
		return "Accept", true
	case "accept-charset":
		return "Accept-Charset", true
	case "accept-encoding":
		return "Accept-Encoding", true
	case "accept-language":
		return "Accept-Language", true
	case "accept-ranges":
		return "Accept-Ranges", true
	case "age":
		return "Age", true
	case "access-control-allow-credentials":
		return "Access-Control-Allow-Credentials", true
	case "access-control-allow-headers":
		return "Access-Control-Allow-Headers", true
	case "access-control-allow-methods":
		return "Access-Control-Allow-Methods", true
	case "access-control-allow-origin":
		return "Access-Control-Allow-Origin", true
	case "access-control-expose-headers":
		return "Access-Control-Expose-Headers", true
	case "access-control-max-age":
		return "Access-Control-Max-Age", true
	case "access-control-request-headers":
		return "Access-Control-Request-Headers", true
	case "access-control-request-method":
		return "Access-Control-Request-Method", true
	case "allow":
		return "Allow", true
	case "authorization":
		return "Authorization", true
	case "cache-control":
		return "Cache-Control", true
	case "content-disposition":
		return "Content-Disposition", true
	case "content-encoding":
		return "Content-Encoding", true
	case "content-language":
		return "Content-Language", true
	case "content-length":
		return "Content-Length", true
	case "content-location":
		return "Content-Location", true
	case "content-range":
		return "Content-Range", true
	case "content-type":
		return "Content-Type", true
	case "cookie":
		return "Cookie", true
	case "date":
		return "Date", true
	case "etag":
		return "Etag", true
	case "expect":
		return "Expect", true
	case "expires":
		return "Expires", true
	case "from":
		return "From", true
	case "host":
		return "Host", true
	case "if-match":
		return "If-Match", true
	case "if-modified-since":
		return "If-Modified-Since", true
	case "if-none-match":
		return "If-None-Match", true
	case "if-unmodified-since":
		return "If-Unmodified-Since", true
	case "last-modified":
		return "Last-Modified", true
	case "link":
		return "Link", true
	case "location":
		return "Location", true
	case "max-forwards":
		return "Max-Forwards", true
	case "origin":
		return "Origin", true
	case "proxy-authenticate":
		return "Proxy-Authenticate", true
	case "proxy-authorization":
		return "Proxy-Authorization", true
	case "range":
		return "Range", true
	case "referer":
		return "Referer", true
	case "refresh":
		return "Refresh", true
	case "retry-after":
		return "Retry-After", true
	case "server":
		return "Server", true
	case "set-cookie":
		return "Set-Cookie", true
	case "strict-transport-security":
		return "Strict-Transport-Security", true
	case "trailer":
		return "Trailer", true
	case "transfer-encoding":
		return "Transfer-Encoding", true
	case "user-agent":
		return "User-Agent", true
	case "vary":
		return "Vary", true
	case "via":
		return "Via", true
	case "www-authenticate":
		return "Www-Authenticate", true
	case "x-forwarded-for":
		return "X-Forwarded-For", true
	case "x-forwarded-proto":
		return "X-Forwarded-Proto", true
	}
	return "", false
}
