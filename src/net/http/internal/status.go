// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package internal

// StatusText returns the standard reason phrase for status code, or "".
func StatusText(code int) string {
	idx := code - 100
	if uint(idx) < uint(len(statusTextTable)) {
		return statusTextTable[idx]
	}
	return ""
}

// StatusLine returns the preformatted HTTP/1.x status line
// ("HTTP/1.1 200 OK\r\n" or "HTTP/1.0 200 OK\r\n") for known status codes, or "".
func StatusLine(is11 bool, code int) string {
	idx := code - 100
	if uint(idx) < uint(len(statusLines11)) {
		if is11 {
			return statusLines11[idx]
		}
		return statusLines10[idx]
	}
	return ""
}

// StatusCodeText returns the preformatted "NNN Text" status string
// (e.g., "200 OK") for known status codes, or "".
func StatusCodeText(code int) string {
	idx := code - 100
	if uint(idx) < uint(len(statusCodeTexts)) {
		return statusCodeTexts[idx]
	}
	return ""
}

// StatusCodeString returns the preformatted decimal status code string
// (e.g., "200") for known status codes, or "".
func StatusCodeString(code int) string {
	idx := code - 100
	if uint(idx) < uint(len(statusCodeStrings)) {
		return statusCodeStrings[idx]
	}
	return ""
}

var statusTextTable = [412]string{
	100 - 100: "Continue",
	101 - 100: "Switching Protocols",
	102 - 100: "Processing",
	103 - 100: "Early Hints",

	200 - 100: "OK",
	201 - 100: "Created",
	202 - 100: "Accepted",
	203 - 100: "Non-Authoritative Information",
	204 - 100: "No Content",
	205 - 100: "Reset Content",
	206 - 100: "Partial Content",
	207 - 100: "Multi-Status",
	208 - 100: "Already Reported",
	226 - 100: "IM Used",

	300 - 100: "Multiple Choices",
	301 - 100: "Moved Permanently",
	302 - 100: "Found",
	303 - 100: "See Other",
	304 - 100: "Not Modified",
	305 - 100: "Use Proxy",
	307 - 100: "Temporary Redirect",
	308 - 100: "Permanent Redirect",

	400 - 100: "Bad Request",
	401 - 100: "Unauthorized",
	402 - 100: "Payment Required",
	403 - 100: "Forbidden",
	404 - 100: "Not Found",
	405 - 100: "Method Not Allowed",
	406 - 100: "Not Acceptable",
	407 - 100: "Proxy Authentication Required",
	408 - 100: "Request Timeout",
	409 - 100: "Conflict",
	410 - 100: "Gone",
	411 - 100: "Length Required",
	412 - 100: "Precondition Failed",
	413 - 100: "Request Entity Too Large",
	414 - 100: "Request URI Too Long",
	415 - 100: "Unsupported Media Type",
	416 - 100: "Requested Range Not Satisfiable",
	417 - 100: "Expectation Failed",
	418 - 100: "I'm a teapot",
	421 - 100: "Misdirected Request",
	422 - 100: "Unprocessable Entity",
	423 - 100: "Locked",
	424 - 100: "Failed Dependency",
	425 - 100: "Too Early",
	426 - 100: "Upgrade Required",
	428 - 100: "Precondition Required",
	429 - 100: "Too Many Requests",
	431 - 100: "Request Header Fields Too Large",
	451 - 100: "Unavailable For Legal Reasons",

	500 - 100: "Internal Server Error",
	501 - 100: "Not Implemented",
	502 - 100: "Bad Gateway",
	503 - 100: "Service Unavailable",
	504 - 100: "Gateway Timeout",
	505 - 100: "HTTP Version Not Supported",
	506 - 100: "Variant Also Negotiates",
	507 - 100: "Insufficient Storage",
	508 - 100: "Loop Detected",
	510 - 100: "Not Extended",
	511 - 100: "Network Authentication Required",
}

var statusLines11 = [412]string{
	100 - 100: "HTTP/1.1 100 Continue\r\n",
	101 - 100: "HTTP/1.1 101 Switching Protocols\r\n",
	102 - 100: "HTTP/1.1 102 Processing\r\n",
	103 - 100: "HTTP/1.1 103 Early Hints\r\n",

	200 - 100: "HTTP/1.1 200 OK\r\n",
	201 - 100: "HTTP/1.1 201 Created\r\n",
	202 - 100: "HTTP/1.1 202 Accepted\r\n",
	203 - 100: "HTTP/1.1 203 Non-Authoritative Information\r\n",
	204 - 100: "HTTP/1.1 204 No Content\r\n",
	205 - 100: "HTTP/1.1 205 Reset Content\r\n",
	206 - 100: "HTTP/1.1 206 Partial Content\r\n",
	207 - 100: "HTTP/1.1 207 Multi-Status\r\n",
	208 - 100: "HTTP/1.1 208 Already Reported\r\n",
	226 - 100: "HTTP/1.1 226 IM Used\r\n",

	300 - 100: "HTTP/1.1 300 Multiple Choices\r\n",
	301 - 100: "HTTP/1.1 301 Moved Permanently\r\n",
	302 - 100: "HTTP/1.1 302 Found\r\n",
	303 - 100: "HTTP/1.1 303 See Other\r\n",
	304 - 100: "HTTP/1.1 304 Not Modified\r\n",
	305 - 100: "HTTP/1.1 305 Use Proxy\r\n",
	307 - 100: "HTTP/1.1 307 Temporary Redirect\r\n",
	308 - 100: "HTTP/1.1 308 Permanent Redirect\r\n",

	400 - 100: "HTTP/1.1 400 Bad Request\r\n",
	401 - 100: "HTTP/1.1 401 Unauthorized\r\n",
	402 - 100: "HTTP/1.1 402 Payment Required\r\n",
	403 - 100: "HTTP/1.1 403 Forbidden\r\n",
	404 - 100: "HTTP/1.1 404 Not Found\r\n",
	405 - 100: "HTTP/1.1 405 Method Not Allowed\r\n",
	406 - 100: "HTTP/1.1 406 Not Acceptable\r\n",
	407 - 100: "HTTP/1.1 407 Proxy Authentication Required\r\n",
	408 - 100: "HTTP/1.1 408 Request Timeout\r\n",
	409 - 100: "HTTP/1.1 409 Conflict\r\n",
	410 - 100: "HTTP/1.1 410 Gone\r\n",
	411 - 100: "HTTP/1.1 411 Length Required\r\n",
	412 - 100: "HTTP/1.1 412 Precondition Failed\r\n",
	413 - 100: "HTTP/1.1 413 Request Entity Too Large\r\n",
	414 - 100: "HTTP/1.1 414 Request URI Too Long\r\n",
	415 - 100: "HTTP/1.1 415 Unsupported Media Type\r\n",
	416 - 100: "HTTP/1.1 416 Requested Range Not Satisfiable\r\n",
	417 - 100: "HTTP/1.1 417 Expectation Failed\r\n",
	418 - 100: "HTTP/1.1 418 I'm a teapot\r\n",
	421 - 100: "HTTP/1.1 421 Misdirected Request\r\n",
	422 - 100: "HTTP/1.1 422 Unprocessable Entity\r\n",
	423 - 100: "HTTP/1.1 423 Locked\r\n",
	424 - 100: "HTTP/1.1 424 Failed Dependency\r\n",
	425 - 100: "HTTP/1.1 425 Too Early\r\n",
	426 - 100: "HTTP/1.1 426 Upgrade Required\r\n",
	428 - 100: "HTTP/1.1 428 Precondition Required\r\n",
	429 - 100: "HTTP/1.1 429 Too Many Requests\r\n",
	431 - 100: "HTTP/1.1 431 Request Header Fields Too Large\r\n",
	451 - 100: "HTTP/1.1 451 Unavailable For Legal Reasons\r\n",

	500 - 100: "HTTP/1.1 500 Internal Server Error\r\n",
	501 - 100: "HTTP/1.1 501 Not Implemented\r\n",
	502 - 100: "HTTP/1.1 502 Bad Gateway\r\n",
	503 - 100: "HTTP/1.1 503 Service Unavailable\r\n",
	504 - 100: "HTTP/1.1 504 Gateway Timeout\r\n",
	505 - 100: "HTTP/1.1 505 HTTP Version Not Supported\r\n",
	506 - 100: "HTTP/1.1 506 Variant Also Negotiates\r\n",
	507 - 100: "HTTP/1.1 507 Insufficient Storage\r\n",
	508 - 100: "HTTP/1.1 508 Loop Detected\r\n",
	510 - 100: "HTTP/1.1 510 Not Extended\r\n",
	511 - 100: "HTTP/1.1 511 Network Authentication Required\r\n",
}

var statusLines10 = [412]string{
	100 - 100: "HTTP/1.0 100 Continue\r\n",
	101 - 100: "HTTP/1.0 101 Switching Protocols\r\n",
	102 - 100: "HTTP/1.0 102 Processing\r\n",
	103 - 100: "HTTP/1.0 103 Early Hints\r\n",

	200 - 100: "HTTP/1.0 200 OK\r\n",
	201 - 100: "HTTP/1.0 201 Created\r\n",
	202 - 100: "HTTP/1.0 202 Accepted\r\n",
	203 - 100: "HTTP/1.0 203 Non-Authoritative Information\r\n",
	204 - 100: "HTTP/1.0 204 No Content\r\n",
	205 - 100: "HTTP/1.0 205 Reset Content\r\n",
	206 - 100: "HTTP/1.0 206 Partial Content\r\n",
	207 - 100: "HTTP/1.0 207 Multi-Status\r\n",
	208 - 100: "HTTP/1.0 208 Already Reported\r\n",
	226 - 100: "HTTP/1.0 226 IM Used\r\n",

	300 - 100: "HTTP/1.0 300 Multiple Choices\r\n",
	301 - 100: "HTTP/1.0 301 Moved Permanently\r\n",
	302 - 100: "HTTP/1.0 302 Found\r\n",
	303 - 100: "HTTP/1.0 303 See Other\r\n",
	304 - 100: "HTTP/1.0 304 Not Modified\r\n",
	305 - 100: "HTTP/1.0 305 Use Proxy\r\n",
	307 - 100: "HTTP/1.0 307 Temporary Redirect\r\n",
	308 - 100: "HTTP/1.0 308 Permanent Redirect\r\n",

	400 - 100: "HTTP/1.0 400 Bad Request\r\n",
	401 - 100: "HTTP/1.0 401 Unauthorized\r\n",
	402 - 100: "HTTP/1.0 402 Payment Required\r\n",
	403 - 100: "HTTP/1.0 403 Forbidden\r\n",
	404 - 100: "HTTP/1.0 404 Not Found\r\n",
	405 - 100: "HTTP/1.0 405 Method Not Allowed\r\n",
	406 - 100: "HTTP/1.0 406 Not Acceptable\r\n",
	407 - 100: "HTTP/1.0 407 Proxy Authentication Required\r\n",
	408 - 100: "HTTP/1.0 408 Request Timeout\r\n",
	409 - 100: "HTTP/1.0 409 Conflict\r\n",
	410 - 100: "HTTP/1.0 410 Gone\r\n",
	411 - 100: "HTTP/1.0 411 Length Required\r\n",
	412 - 100: "HTTP/1.0 412 Precondition Failed\r\n",
	413 - 100: "HTTP/1.0 413 Request Entity Too Large\r\n",
	414 - 100: "HTTP/1.0 414 Request URI Too Long\r\n",
	415 - 100: "HTTP/1.0 415 Unsupported Media Type\r\n",
	416 - 100: "HTTP/1.0 416 Requested Range Not Satisfiable\r\n",
	417 - 100: "HTTP/1.0 417 Expectation Failed\r\n",
	418 - 100: "HTTP/1.0 418 I'm a teapot\r\n",
	421 - 100: "HTTP/1.0 421 Misdirected Request\r\n",
	422 - 100: "HTTP/1.0 422 Unprocessable Entity\r\n",
	423 - 100: "HTTP/1.0 423 Locked\r\n",
	424 - 100: "HTTP/1.0 424 Failed Dependency\r\n",
	425 - 100: "HTTP/1.0 425 Too Early\r\n",
	426 - 100: "HTTP/1.0 426 Upgrade Required\r\n",
	428 - 100: "HTTP/1.0 428 Precondition Required\r\n",
	429 - 100: "HTTP/1.0 429 Too Many Requests\r\n",
	431 - 100: "HTTP/1.0 431 Request Header Fields Too Large\r\n",
	451 - 100: "HTTP/1.0 451 Unavailable For Legal Reasons\r\n",

	500 - 100: "HTTP/1.0 500 Internal Server Error\r\n",
	501 - 100: "HTTP/1.0 501 Not Implemented\r\n",
	502 - 100: "HTTP/1.0 502 Bad Gateway\r\n",
	503 - 100: "HTTP/1.0 503 Service Unavailable\r\n",
	504 - 100: "HTTP/1.0 504 Gateway Timeout\r\n",
	505 - 100: "HTTP/1.0 505 HTTP Version Not Supported\r\n",
	506 - 100: "HTTP/1.0 506 Variant Also Negotiates\r\n",
	507 - 100: "HTTP/1.0 507 Insufficient Storage\r\n",
	508 - 100: "HTTP/1.0 508 Loop Detected\r\n",
	510 - 100: "HTTP/1.0 510 Not Extended\r\n",
	511 - 100: "HTTP/1.0 511 Network Authentication Required\r\n",
}

var statusCodeTexts = [412]string{
	100 - 100: "100 Continue",
	101 - 100: "101 Switching Protocols",
	102 - 100: "102 Processing",
	103 - 100: "103 Early Hints",

	200 - 100: "200 OK",
	201 - 100: "201 Created",
	202 - 100: "202 Accepted",
	203 - 100: "203 Non-Authoritative Information",
	204 - 100: "204 No Content",
	205 - 100: "205 Reset Content",
	206 - 100: "206 Partial Content",
	207 - 100: "207 Multi-Status",
	208 - 100: "208 Already Reported",
	226 - 100: "226 IM Used",

	300 - 100: "300 Multiple Choices",
	301 - 100: "301 Moved Permanently",
	302 - 100: "302 Found",
	303 - 100: "303 See Other",
	304 - 100: "304 Not Modified",
	305 - 100: "305 Use Proxy",
	307 - 100: "307 Temporary Redirect",
	308 - 100: "308 Permanent Redirect",

	400 - 100: "400 Bad Request",
	401 - 100: "401 Unauthorized",
	402 - 100: "402 Payment Required",
	403 - 100: "403 Forbidden",
	404 - 100: "404 Not Found",
	405 - 100: "405 Method Not Allowed",
	406 - 100: "406 Not Acceptable",
	407 - 100: "407 Proxy Authentication Required",
	408 - 100: "408 Request Timeout",
	409 - 100: "409 Conflict",
	410 - 100: "410 Gone",
	411 - 100: "411 Length Required",
	412 - 100: "412 Precondition Failed",
	413 - 100: "413 Request Entity Too Large",
	414 - 100: "414 Request URI Too Long",
	415 - 100: "415 Unsupported Media Type",
	416 - 100: "416 Requested Range Not Satisfiable",
	417 - 100: "417 Expectation Failed",
	418 - 100: "418 I'm a teapot",
	421 - 100: "421 Misdirected Request",
	422 - 100: "422 Unprocessable Entity",
	423 - 100: "423 Locked",
	424 - 100: "424 Failed Dependency",
	425 - 100: "425 Too Early",
	426 - 100: "426 Upgrade Required",
	428 - 100: "428 Precondition Required",
	429 - 100: "429 Too Many Requests",
	431 - 100: "431 Request Header Fields Too Large",
	451 - 100: "451 Unavailable For Legal Reasons",

	500 - 100: "500 Internal Server Error",
	501 - 100: "501 Not Implemented",
	502 - 100: "502 Bad Gateway",
	503 - 100: "503 Service Unavailable",
	504 - 100: "504 Gateway Timeout",
	505 - 100: "505 HTTP Version Not Supported",
	506 - 100: "506 Variant Also Negotiates",
	507 - 100: "507 Insufficient Storage",
	508 - 100: "508 Loop Detected",
	510 - 100: "510 Not Extended",
	511 - 100: "511 Network Authentication Required",
}

var statusCodeStrings = [412]string{
	100 - 100: "100",
	101 - 100: "101",
	102 - 100: "102",
	103 - 100: "103",

	200 - 100: "200",
	201 - 100: "201",
	202 - 100: "202",
	203 - 100: "203",
	204 - 100: "204",
	205 - 100: "205",
	206 - 100: "206",
	207 - 100: "207",
	208 - 100: "208",
	226 - 100: "226",

	300 - 100: "300",
	301 - 100: "301",
	302 - 100: "302",
	303 - 100: "303",
	304 - 100: "304",
	305 - 100: "305",
	307 - 100: "307",
	308 - 100: "308",

	400 - 100: "400",
	401 - 100: "401",
	402 - 100: "402",
	403 - 100: "403",
	404 - 100: "404",
	405 - 100: "405",
	406 - 100: "406",
	407 - 100: "407",
	408 - 100: "408",
	409 - 100: "409",
	410 - 100: "410",
	411 - 100: "411",
	412 - 100: "412",
	413 - 100: "413",
	414 - 100: "414",
	415 - 100: "415",
	416 - 100: "416",
	417 - 100: "417",
	418 - 100: "418",
	421 - 100: "421",
	422 - 100: "422",
	423 - 100: "423",
	424 - 100: "424",
	425 - 100: "425",
	426 - 100: "426",
	428 - 100: "428",
	429 - 100: "429",
	431 - 100: "431",
	451 - 100: "451",

	500 - 100: "500",
	501 - 100: "501",
	502 - 100: "502",
	503 - 100: "503",
	504 - 100: "504",
	505 - 100: "505",
	506 - 100: "506",
	507 - 100: "507",
	508 - 100: "508",
	510 - 100: "510",
	511 - 100: "511",
}
