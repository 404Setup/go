// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	contentTypeSSE   = []string{"text/event-stream"}[:1:1]
	headerValCacheNo = []string{"no-cache"}[:1:1]
)

// SSEEvent represents a single Server-Sent Events message.
type SSEEvent struct {
	// ID sets the optional event stream "id" field. Must not contain CR, LF, or NUL.
	ID string

	// Event sets the optional event stream "event" type. Must not contain CR, LF, or NUL.
	Event string

	// Data is the event payload. Newlines (\r\n, \r, \n) are safely split into
	// separate "data:" lines to prevent SSE field injection.
	Data string

	// Retry sets the optional client reconnection time in milliseconds when > 0.
	Retry time.Duration
}

// SSEWriter writes Server-Sent Events (text/event-stream) responses.
//
// An SSEWriter is bound to the lifetime of the request handler that created it
// and must not be used concurrently or after the handler returns.
// If the server has a positive Config.WriteTimeout, handlers streaming long-lived
// SSE responses can clear or extend the per-write deadline via
// [Context.ResponseController].
type SSEWriter struct {
	w      http.ResponseWriter
	rc     *http.ResponseController
	buf    bytes.Buffer
	isHead bool
}

// SSE initializes a Server-Sent Events stream for the request, disables
// response compression, sets Content-Type: text/event-stream and
// Cache-Control: no-cache, writes status 200 OK, and flushes headers to the
// client.
func (c *Context) SSE() (*SSEWriter, error) {
	if c.Written() {
		return nil, errors.New("server: response already written")
	}
	c.DisableCompression()

	h := c.respHeader()
	delete(h, "Content-Length")
	delete(h, "Content-Encoding")
	h["Content-Type"] = contentTypeSSE
	if len(h["Cache-Control"]) == 0 {
		h["Cache-Control"] = headerValCacheNo
	}
	if len(h["X-Content-Type-Options"]) == 0 {
		h["X-Content-Type-Options"] = headerValNosniff
	}

	c.writeHeader(http.StatusOK)
	w := c.ResponseWriter()
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return nil, err
	}
	return &SSEWriter{
		w:      w,
		rc:     rc,
		isHead: c.req.Method == http.MethodHead,
	}, nil
}

func hasSSEControl(s string) bool {
	return strings.IndexAny(s, "\r\n\x00") >= 0
}

// Send writes ev to the event stream and flushes it immediately.
func (s *SSEWriter) Send(ev SSEEvent) error {
	if s == nil || s.w == nil {
		return errors.New("server: nil SSEWriter")
	}
	if hasSSEControl(ev.ID) {
		return errors.New("server: SSE event ID contains CR, LF, or NUL")
	}
	if hasSSEControl(ev.Event) {
		return errors.New("server: SSE event name contains CR, LF, or NUL")
	}
	if s.isHead {
		return nil
	}

	s.buf.Reset()
	if ev.ID != "" {
		s.buf.WriteString("id: ")
		s.buf.WriteString(ev.ID)
		s.buf.WriteByte('\n')
	}
	if ev.Event != "" {
		s.buf.WriteString("event: ")
		s.buf.WriteString(ev.Event)
		s.buf.WriteByte('\n')
	}
	if ev.Retry > 0 {
		ms := int64(ev.Retry / time.Millisecond)
		if ms > 0 {
			s.buf.WriteString("retry: ")
			s.buf.WriteString(strconv.FormatInt(ms, 10))
			s.buf.WriteByte('\n')
		}
	}
	if ev.Data != "" {
		writeSSEPrefixedLines(&s.buf, "data: ", ev.Data)
	} else if ev.Event != "" || (ev.ID == "" && ev.Retry <= 0) {
		s.buf.WriteString("data:\n")
	}
	s.buf.WriteByte('\n')

	if _, err := s.w.Write(s.buf.Bytes()); err != nil {
		return err
	}
	return s.Flush()
}

// SendData writes a plain data-only SSE event and flushes it immediately.
func (s *SSEWriter) SendData(data string) error {
	return s.Send(SSEEvent{Data: data})
}

// SendJSON marshals v as JSON and writes it as an SSE event (with optional
// event name) and flushes it immediately.
func (s *SSEWriter) SendJSON(event string, v any) error {
	if s == nil || s.w == nil {
		return errors.New("server: nil SSEWriter")
	}
	if hasSSEControl(event) {
		return errors.New("server: SSE event name contains CR, LF, or NUL")
	}
	jeb := jsonBufPool.Get()
	jeb.buf.Reset()
	err := jeb.enc.Encode(v)
	if err != nil {
		if jeb.buf.Cap() <= maxPooledJSONSize {
			jsonBufPool.Put(jeb)
		}
		return err
	}
	b := jeb.buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	if s.isHead {
		if jeb.buf.Cap() <= maxPooledJSONSize {
			jsonBufPool.Put(jeb)
		}
		return nil
	}

	s.buf.Reset()
	if event != "" {
		s.buf.WriteString("event: ")
		s.buf.WriteString(event)
		s.buf.WriteByte('\n')
	}
	s.buf.WriteString("data: ")
	s.buf.Write(b)
	s.buf.WriteString("\n\n")
	if jeb.buf.Cap() <= maxPooledJSONSize {
		jsonBufPool.Put(jeb)
	}
	if _, err := s.w.Write(s.buf.Bytes()); err != nil {
		return err
	}
	return s.Flush()
}

// Comment writes an SSE comment line (useful for keep-alive heartbeats) and
// flushes it immediately.
func (s *SSEWriter) Comment(text string) error {
	if s == nil || s.w == nil {
		return errors.New("server: nil SSEWriter")
	}
	if strings.IndexByte(text, 0) >= 0 {
		return errors.New("server: SSE comment contains NUL byte")
	}
	if s.isHead {
		return nil
	}
	s.buf.Reset()
	if text == "" {
		s.buf.WriteString(":\n\n")
	} else {
		writeSSEPrefixedLines(&s.buf, ": ", text)
		s.buf.WriteByte('\n')
	}
	if _, err := s.w.Write(s.buf.Bytes()); err != nil {
		return err
	}
	return s.Flush()
}

// Flush flushes buffered SSE data to the underlying connection.
func (s *SSEWriter) Flush() error {
	if s == nil || s.rc == nil {
		return errors.New("server: nil SSEWriter")
	}
	err := s.rc.Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func writeSSEPrefixedLines(dst *bytes.Buffer, prefix, s string) {
	for {
		idx := strings.IndexAny(s, "\r\n")
		if idx < 0 {
			dst.WriteString(prefix)
			dst.WriteString(s)
			dst.WriteByte('\n')
			return
		}
		dst.WriteString(prefix)
		dst.WriteString(s[:idx])
		dst.WriteByte('\n')
		if s[idx] == '\r' && idx+1 < len(s) && s[idx+1] == '\n' {
			s = s[idx+2:]
		} else {
			s = s[idx+1:]
		}
	}
}
