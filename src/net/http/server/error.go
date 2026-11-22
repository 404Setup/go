// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"errors"
	"fmt"
	"net/http"
)

// Sentinel errors returned by request decoding helpers.
// These can be matched with [errors.Is].
var (
	// ErrBodyTooLarge is wrapped when a request body exceeds the configured limit.
	ErrBodyTooLarge = errors.New("request body too large")

	// ErrUnsupportedMediaType is wrapped when a request Content-Type is not
	// supported by the decoder (for example, non-JSON Content-Type in BindJSON).
	ErrUnsupportedMediaType = errors.New("unsupported media type")

	// ErrInvalidJSON is wrapped when a request body contains malformed or
	// unexpected JSON.
	ErrInvalidJSON = errors.New("invalid JSON payload")
)

// HTTPError represents an HTTP error with a status code, a safe public message
// intended for clients, and an optional underlying internal error.
//
// Default error handlers never expose Err to clients.
type HTTPError struct {
	Code    int
	Message string
	Err     error
}

// NewError creates an [*HTTPError] with the given HTTP status code and public
// client message. If message is empty, [http.StatusText](code) is used.
func NewError(code int, message string) *HTTPError {
	if message == "" {
		message = http.StatusText(code)
	}
	return &HTTPError{
		Code:    code,
		Message: message,
	}
}

// WrapError creates an [*HTTPError] carrying both a safe public message and an
// underlying internal error for logging and [errors.Is]/[errors.As] inspection.
func WrapError(code int, message string, err error) *HTTPError {
	he := NewError(code, message)
	he.Err = err
	return he
}

// Error implements the error interface.
func (e *HTTPError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err != nil {
		return fmt.Sprintf("http %d: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("http %d: %s", e.Code, e.Message)
}

// Unwrap returns the underlying internal error, if any.
func (e *HTTPError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ErrorHandler processes non-nil errors returned by handlers and middleware.
type ErrorHandler func(*Context, error)

// classifyError extracts a safe HTTP status code and public message from err.
// Internal errors are never exposed directly.
func classifyError(err error) (code int, msg string, internal bool) {
	if err == nil {
		return http.StatusOK, "", false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		code = he.Code
		if code < 400 || code > 599 {
			code = http.StatusInternalServerError
		}
		msg = he.Message
		if msg == "" {
			msg = http.StatusText(code)
		}
		return code, msg, code >= 500
	}
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return http.StatusRequestEntityTooLarge, "request body too large", false
	}
	return http.StatusInternalServerError, "internal server error", true
}

var (
	headerValNosniff   = []string{"nosniff"}[:1:1]
	contentTypeText    = []string{"text/plain; charset=utf-8"}[:1:1]
	contentTypeHTML    = []string{"text/html; charset=utf-8"}[:1:1]
	contentTypeJSON    = []string{"application/json; charset=utf-8"}[:1:1]
)

// DefaultErrorHandler writes a plain-text error response with
// X-Content-Type-Options: nosniff. If a response has already been written, it
// does nothing. Internal errors are logged when a server logger is configured
// and are never exposed to the client.
func DefaultErrorHandler(c *Context, err error) {
	if err == nil {
		return
	}
	code, msg, internal := classifyError(err)
	if internal && c.server != nil && c.server.logger != nil {
		c.server.logger.ErrorContext(c.Context(), "http request error",
			"method", c.Method(),
			"path", c.Path(),
			"status", code,
			"error", err.Error(),
		)
	}
	if c.Written() {
		return
	}
	h := c.respHeader()
	delete(h, "Content-Length")
	delete(h, "Content-Encoding")
	h["X-Content-Type-Options"] = headerValNosniff
	h["Content-Type"] = contentTypeText
	c.writeHeader(code)
	_, _ = c.writeString(msg)
}

type jsonErrorBody struct {
	Error string `json:"error"`
}

// JSONErrorHandler writes a JSON error response of the form {"error":"..."}
// with X-Content-Type-Options: nosniff. If a response has already been written,
// it does nothing. Internal errors are logged when a server logger is
// configured and are never exposed to the client.
func JSONErrorHandler(c *Context, err error) {
	if err == nil {
		return
	}
	code, msg, internal := classifyError(err)
	if internal && c.server != nil && c.server.logger != nil {
		c.server.logger.ErrorContext(c.Context(), "http request error",
			"method", c.Method(),
			"path", c.Path(),
			"status", code,
			"error", err.Error(),
		)
	}
	if c.Written() {
		return
	}
	h := c.respHeader()
	delete(h, "Content-Length")
	delete(h, "Content-Encoding")
	h["X-Content-Type-Options"] = headerValNosniff
	_ = c.JSON(code, jsonErrorBody{Error: msg})
}
