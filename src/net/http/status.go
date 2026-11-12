// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

// HTTP status codes as registered with IANA.
// See: https://www.iana.org/assignments/http-status-codes/http-status-codes.xhtml
const (
	StatusContinue           = 100 // RFC 9110, 15.2.1
	StatusSwitchingProtocols = 101 // RFC 9110, 15.2.2
	StatusProcessing         = 102 // RFC 2518, 10.1
	StatusEarlyHints         = 103 // RFC 8297

	StatusOK                   = 200 // RFC 9110, 15.3.1
	StatusCreated              = 201 // RFC 9110, 15.3.2
	StatusAccepted             = 202 // RFC 9110, 15.3.3
	StatusNonAuthoritativeInfo = 203 // RFC 9110, 15.3.4
	StatusNoContent            = 204 // RFC 9110, 15.3.5
	StatusResetContent         = 205 // RFC 9110, 15.3.6
	StatusPartialContent       = 206 // RFC 9110, 15.3.7
	StatusMultiStatus          = 207 // RFC 4918, 11.1
	StatusAlreadyReported      = 208 // RFC 5842, 7.1
	StatusIMUsed               = 226 // RFC 3229, 10.4.1

	StatusMultipleChoices   = 300 // RFC 9110, 15.4.1
	StatusMovedPermanently  = 301 // RFC 9110, 15.4.2
	StatusFound             = 302 // RFC 9110, 15.4.3
	StatusSeeOther          = 303 // RFC 9110, 15.4.4
	StatusNotModified       = 304 // RFC 9110, 15.4.5
	StatusUseProxy          = 305 // RFC 9110, 15.4.6
	_                       = 306 // RFC 9110, 15.4.7 (Unused)
	StatusTemporaryRedirect = 307 // RFC 9110, 15.4.8
	StatusPermanentRedirect = 308 // RFC 9110, 15.4.9

	StatusBadRequest                   = 400 // RFC 9110, 15.5.1
	StatusUnauthorized                 = 401 // RFC 9110, 15.5.2
	StatusPaymentRequired              = 402 // RFC 9110, 15.5.3
	StatusForbidden                    = 403 // RFC 9110, 15.5.4
	StatusNotFound                     = 404 // RFC 9110, 15.5.5
	StatusMethodNotAllowed             = 405 // RFC 9110, 15.5.6
	StatusNotAcceptable                = 406 // RFC 9110, 15.5.7
	StatusProxyAuthRequired            = 407 // RFC 9110, 15.5.8
	StatusRequestTimeout               = 408 // RFC 9110, 15.5.9
	StatusConflict                     = 409 // RFC 9110, 15.5.10
	StatusGone                         = 410 // RFC 9110, 15.5.11
	StatusLengthRequired               = 411 // RFC 9110, 15.5.12
	StatusPreconditionFailed           = 412 // RFC 9110, 15.5.13
	StatusRequestEntityTooLarge        = 413 // RFC 9110, 15.5.14
	StatusRequestURITooLong            = 414 // RFC 9110, 15.5.15
	StatusUnsupportedMediaType         = 415 // RFC 9110, 15.5.16
	StatusRequestedRangeNotSatisfiable = 416 // RFC 9110, 15.5.17
	StatusExpectationFailed            = 417 // RFC 9110, 15.5.18
	StatusTeapot                       = 418 // RFC 9110, 15.5.19 (Unused)
	StatusMisdirectedRequest           = 421 // RFC 9110, 15.5.20
	StatusUnprocessableEntity          = 422 // RFC 9110, 15.5.21
	StatusLocked                       = 423 // RFC 4918, 11.3
	StatusFailedDependency             = 424 // RFC 4918, 11.4
	StatusTooEarly                     = 425 // RFC 8470, 5.2.
	StatusUpgradeRequired              = 426 // RFC 9110, 15.5.22
	StatusPreconditionRequired         = 428 // RFC 6585, 3
	StatusTooManyRequests              = 429 // RFC 6585, 4
	StatusRequestHeaderFieldsTooLarge  = 431 // RFC 6585, 5
	StatusUnavailableForLegalReasons   = 451 // RFC 7725, 3

	StatusInternalServerError           = 500 // RFC 9110, 15.6.1
	StatusNotImplemented                = 501 // RFC 9110, 15.6.2
	StatusBadGateway                    = 502 // RFC 9110, 15.6.3
	StatusServiceUnavailable            = 503 // RFC 9110, 15.6.4
	StatusGatewayTimeout                = 504 // RFC 9110, 15.6.5
	StatusHTTPVersionNotSupported       = 505 // RFC 9110, 15.6.6
	StatusVariantAlsoNegotiates         = 506 // RFC 2295, 8.1
	StatusInsufficientStorage           = 507 // RFC 4918, 11.5
	StatusLoopDetected                  = 508 // RFC 5842, 7.2
	StatusNotExtended                   = 510 // RFC 2774, 7
	StatusNetworkAuthenticationRequired = 511 // RFC 6585, 6
)

var statusTextTable = [...]string{
	StatusContinue - 100:                      "Continue",
	StatusSwitchingProtocols - 100:            "Switching Protocols",
	StatusProcessing - 100:                    "Processing",
	StatusEarlyHints - 100:                    "Early Hints",
	StatusOK - 100:                            "OK",
	StatusCreated - 100:                       "Created",
	StatusAccepted - 100:                      "Accepted",
	StatusNonAuthoritativeInfo - 100:          "Non-Authoritative Information",
	StatusNoContent - 100:                     "No Content",
	StatusResetContent - 100:                  "Reset Content",
	StatusPartialContent - 100:                "Partial Content",
	StatusMultiStatus - 100:                   "Multi-Status",
	StatusAlreadyReported - 100:               "Already Reported",
	StatusIMUsed - 100:                        "IM Used",
	StatusMultipleChoices - 100:               "Multiple Choices",
	StatusMovedPermanently - 100:              "Moved Permanently",
	StatusFound - 100:                         "Found",
	StatusSeeOther - 100:                      "See Other",
	StatusNotModified - 100:                   "Not Modified",
	StatusUseProxy - 100:                      "Use Proxy",
	StatusTemporaryRedirect - 100:             "Temporary Redirect",
	StatusPermanentRedirect - 100:             "Permanent Redirect",
	StatusBadRequest - 100:                    "Bad Request",
	StatusUnauthorized - 100:                  "Unauthorized",
	StatusPaymentRequired - 100:               "Payment Required",
	StatusForbidden - 100:                     "Forbidden",
	StatusNotFound - 100:                      "Not Found",
	StatusMethodNotAllowed - 100:              "Method Not Allowed",
	StatusNotAcceptable - 100:                 "Not Acceptable",
	StatusProxyAuthRequired - 100:             "Proxy Authentication Required",
	StatusRequestTimeout - 100:                "Request Timeout",
	StatusConflict - 100:                      "Conflict",
	StatusGone - 100:                          "Gone",
	StatusLengthRequired - 100:                "Length Required",
	StatusPreconditionFailed - 100:            "Precondition Failed",
	StatusRequestEntityTooLarge - 100:         "Request Entity Too Large",
	StatusRequestURITooLong - 100:             "Request URI Too Long",
	StatusUnsupportedMediaType - 100:          "Unsupported Media Type",
	StatusRequestedRangeNotSatisfiable - 100:  "Requested Range Not Satisfiable",
	StatusExpectationFailed - 100:             "Expectation Failed",
	StatusTeapot - 100:                        "I'm a teapot",
	StatusMisdirectedRequest - 100:            "Misdirected Request",
	StatusUnprocessableEntity - 100:           "Unprocessable Entity",
	StatusLocked - 100:                        "Locked",
	StatusFailedDependency - 100:              "Failed Dependency",
	StatusTooEarly - 100:                      "Too Early",
	StatusUpgradeRequired - 100:               "Upgrade Required",
	StatusPreconditionRequired - 100:          "Precondition Required",
	StatusTooManyRequests - 100:               "Too Many Requests",
	StatusRequestHeaderFieldsTooLarge - 100:   "Request Header Fields Too Large",
	StatusUnavailableForLegalReasons - 100:    "Unavailable For Legal Reasons",
	StatusInternalServerError - 100:           "Internal Server Error",
	StatusNotImplemented - 100:                "Not Implemented",
	StatusBadGateway - 100:                    "Bad Gateway",
	StatusServiceUnavailable - 100:            "Service Unavailable",
	StatusGatewayTimeout - 100:                "Gateway Timeout",
	StatusHTTPVersionNotSupported - 100:       "HTTP Version Not Supported",
	StatusVariantAlsoNegotiates - 100:         "Variant Also Negotiates",
	StatusInsufficientStorage - 100:           "Insufficient Storage",
	StatusLoopDetected - 100:                  "Loop Detected",
	StatusNotExtended - 100:                   "Not Extended",
	StatusNetworkAuthenticationRequired - 100: "Network Authentication Required",
}

var (
	statusLines11 [len(statusTextTable)]string
	statusLines10 [len(statusTextTable)]string
)

func init() {
	for i, text := range statusTextTable {
		if text != "" {
			c := i + 100
			c0 := byte(c/100 + '0')
			c1 := byte((c/10)%10 + '0')
			c2 := byte(c%10 + '0')
			statusLines11[i] = "HTTP/1.1 " + string([]byte{c0, c1, c2}) + " " + text + "\r\n"
			statusLines10[i] = "HTTP/1.0 " + string([]byte{c0, c1, c2}) + " " + text + "\r\n"
		}
	}
}

// StatusText returns a text for the HTTP status code. It returns the empty
// string if the code is unknown.
func StatusText(code int) string {
	idx := code - 100
	if uint(idx) < uint(len(statusTextTable)) {
		return statusTextTable[idx]
	}
	return ""
}

func statusLine(is11 bool, code int) string {
	idx := code - 100
	if uint(idx) < uint(len(statusLines11)) {
		if is11 {
			return statusLines11[idx]
		}
		return statusLines10[idx]
	}
	return ""
}
