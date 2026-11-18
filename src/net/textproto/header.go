// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package textproto

// A MIMEHeader represents a MIME-style header mapping
// keys to sets of values.
type MIMEHeader map[string][]string

// Add adds the key, value pair to the header.
// It appends to any existing values associated with key.
func (h MIMEHeader) Add(key, value string) {
	key = CanonicalMIMEHeaderKey(key)
	h[key] = append(h[key], value)
}

// Set sets the header entries associated with key to
// the single element value. It replaces any existing
// values associated with key.
func (h MIMEHeader) Set(key, value string) {
	h[CanonicalMIMEHeaderKey(key)] = []string{value}
}

// Get gets the first value associated with the given key.
// It is case insensitive; [CanonicalMIMEHeaderKey] is used
// to canonicalize the provided key.
// If there are no values associated with the key, Get returns "".
// To use non-canonical keys, access the map directly.
func (h MIMEHeader) Get(key string) string {
	if len(h) > 0 {
		if v := h.lookup(key); len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// Values returns all values associated with the given key.
// It is case insensitive; [CanonicalMIMEHeaderKey] is
// used to canonicalize the provided key. To use non-canonical
// keys, access the map directly.
// The returned slice is not a copy.
func (h MIMEHeader) Values(key string) []string {
	if len(h) == 0 {
		return nil
	}
	return h.lookup(key)
}

// lookup returns the values stored under the canonical form of key. It is
// equivalent to h[CanonicalMIMEHeaderKey(key)] but does not allocate when
// key is not already canonical, since the canonical key is only needed
// for the duration of the map access.
func (h MIMEHeader) lookup(key string) []string {
	switch key {
	case "Accept",
		"Accept-Charset",
		"Accept-Encoding",
		"Accept-Language",
		"Accept-Ranges",
		"Authorization",
		"Cache-Control",
		"Connection",
		"Content-Disposition",
		"Content-Encoding",
		"Content-Language",
		"Content-Length",
		"Content-Location",
		"Content-Range",
		"Content-Security-Policy",
		"Content-Type",
		"Cookie",
		"Date",
		"Etag",
		"Expect",
		"Expires",
		"Forwarded",
		"From",
		"Host",
		"If-Match",
		"If-Modified-Since",
		"If-None-Match",
		"If-Range",
		"If-Unmodified-Since",
		"Last-Modified",
		"Link",
		"Location",
		"Max-Forwards",
		"Origin",
		"Pragma",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Range",
		"Referer",
		"Retry-After",
		"Server",
		"Set-Cookie",
		"Strict-Transport-Security",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
		"User-Agent",
		"Vary",
		"Via",
		"Warning",
		"Www-Authenticate",
		"X-Content-Type-Options",
		"X-Forwarded-For",
		"X-Forwarded-Host",
		"X-Forwarded-Proto",
		"X-Frame-Options",
		"X-Powered-By",
		"X-Real-Ip",
		"X-Request-Id",
		"X-Requested-With":
		return h[key]
	}
	// Keys that are already canonical, the common case here, are
	// recognized without a copy.
	upper := true
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !validHeaderFieldByte(c) {
			// Keys with a space or an invalid header field byte are
			// used as is, as CanonicalMIMEHeaderKey does.
			return h[key]
		}
		if upper {
			if 'a' <= c && c <= 'z' {
				return h.lookupNonCanonical(key)
			}
		} else if 'A' <= c && c <= 'Z' {
			return h.lookupNonCanonical(key)
		}
		upper = c == '-'
	}
	return h[key]
}

// lookupNonCanonical is the slow path of lookup. It canonicalizes key in a
// stack buffer, which has room for at most maxLookupKeyLen bytes.
func (h MIMEHeader) lookupNonCanonical(key string) []string {
	if len(key) > maxLookupKeyLen {
		return h[CanonicalMIMEHeaderKey(key)]
	}
	var buf [maxLookupKeyLen]byte
	upper := true
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !validHeaderFieldByte(c) {
			return h[key]
		}
		if upper && 'a' <= c && c <= 'z' {
			c -= toLower
		} else if !upper && 'A' <= c && c <= 'Z' {
			c += toLower
		}
		buf[i] = c
		upper = c == '-'
	}
	// The compiler does not copy the bytes for a map index of the form m[string(b)].
	return h[string(buf[:len(key)])]
}

// maxLookupKeyLen is the longest header key canonicalized on the stack by lookup.
const maxLookupKeyLen = 64

// Del deletes the values associated with key.
func (h MIMEHeader) Del(key string) {
	if len(h) == 0 {
		return
	}
	delete(h, CanonicalMIMEHeaderKey(key))
}
