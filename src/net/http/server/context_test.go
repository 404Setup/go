// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/server"
	"net/netip"
	"strings"
	"testing"
)

func TestContextParamsQueryValuesAndHeaders(t *testing.T) {
	s := server.New()
	s.GET("/items/{id}/{big}", func(c *server.Context) error {
		id, err := c.ParamInt("id")
		if err != nil {
			return server.NewError(http.StatusBadRequest, "bad id")
		}
		big, err := c.ParamInt64("big")
		if err != nil {
			return server.NewError(http.StatusBadRequest, "bad big")
		}
		page, err := c.QueryInt("page", 1)
		if err != nil {
			return server.NewError(http.StatusBadRequest, "bad page")
		}
		sort := c.QueryDefault("sort", "asc")

		// Store >4 values to exercise inline + map overflow storage.
		for i := range 6 {
			c.Set(fmt.Sprintf("k%d", i), i*10)
		}
		c.Set("k0", 999) // overwrite inline value
		v0, ok0 := server.GetValue[int](c, "k0")
		v5, ok5 := server.GetValue[int](c, "k5")
		_, okMissing := server.GetValue[string](c, "nope")
		_, okWrongType := server.GetValue[string](c, "k0")
		if !ok0 || v0 != 999 || !ok5 || v5 != 50 || okMissing || okWrongType {
			return server.NewError(http.StatusInternalServerError, "value store mismatch")
		}

		c.SetHeader("X-Custom", "one")
		c.AddHeader("X-Multi", "a")
		c.AddHeader("X-Multi", "b")
		c.SetCookie(&http.Cookie{Name: "sid", Value: "abc", HttpOnly: true})

		ck, err := c.Cookie("reqck")
		if err != nil || ck.Value != "hello" {
			return server.NewError(http.StatusBadRequest, "missing req cookie")
		}

		return c.JSON(http.StatusOK, map[string]any{
			"id":   id,
			"big":  big,
			"page": page,
			"sort": sort,
			"hdr":  c.Header("X-In"),
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/items/7/9000000000?page=3", nil)
	req.Header.Set("X-In", "ping")
	req.AddCookie(&http.Cookie{Name: "reqck", Value: "hello"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if rec.Header().Get("X-Custom") != "one" {
		t.Fatalf("X-Custom = %q", rec.Header().Get("X-Custom"))
	}
	if got := strings.Join(rec.Header().Values("X-Multi"), ","); got != "a,b" {
		t.Fatalf("X-Multi = %q", got)
	}
	if len(rec.Result().Cookies()) != 1 || rec.Result().Cookies()[0].Name != "sid" {
		t.Fatalf("cookies = %+v", rec.Result().Cookies())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["id"] != float64(7) || body["big"] != float64(9000000000) || body["page"] != float64(3) || body["sort"] != "asc" || body["hdr"] != "ping" {
		t.Fatalf("unexpected JSON body: %#v", body)
	}

	// Verify invalid QueryInt returns error rather than default.
	reqBad := httptest.NewRequest(http.MethodGet, "/items/7/9?page=notanint", nil)
	reqBad.AddCookie(&http.Cookie{Name: "reqck", Value: "hello"})
	recBad := httptest.NewRecorder()
	s.ServeHTTP(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("bad page status = %d, want 400", recBad.Code)
	}
}

func TestContextBindJSONAndStrict(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	s := server.New()
	s.POST("/loose", func(c *server.Context) error {
		var p payload
		if err := c.BindJSON(&p); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, p)
	})
	s.POST("/strict", func(c *server.Context) error {
		var p payload
		if err := c.BindJSONStrict(&p); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, p)
	})

	// Valid JSON with +json content type.
	req := httptest.NewRequest(http.MethodPost, "/loose", strings.NewReader(`{"name":"alice","age":30,"extra":1}`))
	req.Header.Set("Content-Type", "application/vnd.api+json; charset=utf-8")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || rec.Body.String() != `{"name":"alice","age":30}` {
		t.Fatalf("loose valid: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Strict rejects unknown fields.
	req = httptest.NewRequest(http.MethodPost, "/strict", strings.NewReader(`{"name":"alice","unknown":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("strict unknown field: status=%d, want 400", rec.Code)
	}

	// Empty body -> 400.
	req = httptest.NewRequest(http.MethodPost, "/loose", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body: status=%d, want 400", rec.Code)
	}
}

func TestContextResponsesAndJSONMarshalError(t *testing.T) {
	s := server.New()

	s.GET("/html", func(c *server.Context) error {
		return c.HTML(http.StatusOK, "<h1>hi</h1>")
	})
	s.GET("/bytes", func(c *server.Context) error {
		return c.Bytes(http.StatusAccepted, "application/octet-stream", []byte{1, 2, 3})
	})
	s.GET("/indent", func(c *server.Context) error {
		return c.JSONIndented(http.StatusOK, map[string]int{"a": 1}, "", "  ")
	})
	s.GET("/stream", func(c *server.Context) error {
		return c.Stream(http.StatusOK, "text/plain", bytes.NewReader([]byte("streamed")))
	})
	s.GET("/nocontent", func(c *server.Context) error {
		return c.NoContent()
	})
	s.GET("/badjson", func(c *server.Context) error {
		// Channels cannot be marshaled to JSON; response must not be partially written.
		return c.JSON(http.StatusOK, make(chan int))
	})
	s.GET("/redirect", func(c *server.Context) error {
		return c.Redirect(http.StatusFound, "/target")
	})
	s.GET("/flush", func(c *server.Context) error {
		_, _ = c.ResponseWriter().Write([]byte("part1"))
		if err := c.Flush(); err != nil {
			return err
		}
		_, _ = c.ResponseWriter().Write([]byte("part2"))
		return nil
	})
	s.GET("/ctx", func(c *server.Context) error {
		type k struct{}
		c.SetContext(context.WithValue(c.Context(), k{}, "val"))
		v, _ := c.Context().Value(k{}).(string)
		return c.Text(http.StatusOK, v)
	})

	// HTML
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/html", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" || rec.Body.String() != "<h1>hi</h1>" {
		t.Fatalf("html: %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}

	// Bytes
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/bytes", nil))
	if rec.Code != http.StatusAccepted || !bytes.Equal(rec.Body.Bytes(), []byte{1, 2, 3}) {
		t.Fatalf("bytes: %d %v", rec.Code, rec.Body.Bytes())
	}

	// JSONIndented
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/indent", nil))
	if rec.Code != 200 || rec.Body.String() != "{\n  \"a\": 1\n}" {
		t.Fatalf("indent: %d %q", rec.Code, rec.Body.String())
	}

	// Stream
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream", nil))
	if rec.Code != 200 || rec.Body.String() != "streamed" {
		t.Fatalf("stream: %d %q", rec.Code, rec.Body.String())
	}

	// NoContent
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nocontent", nil))
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("nocontent: %d %q", rec.Code, rec.Body.String())
	}

	// Bad JSON marshal must produce clean 500, not partial 200.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/badjson", nil))
	if rec.Code != http.StatusInternalServerError || rec.Body.String() != "internal server error" {
		t.Fatalf("badjson: %d %q", rec.Code, rec.Body.String())
	}

	// Redirect
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/redirect", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/target" {
		t.Fatalf("redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// Flush
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/flush", nil))
	if !rec.Flushed || rec.Body.String() != "part1part2" {
		t.Fatalf("flush: flushed=%v body=%q", rec.Flushed, rec.Body.String())
	}

	// SetContext
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ctx", nil))
	if rec.Code != 200 || rec.Body.String() != "val" {
		t.Fatalf("ctx: %d %q", rec.Code, rec.Body.String())
	}
}

func TestContextBodyBytesAndParseForm(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.MaxBodyBytes = 32
	s := server.NewWithConfig(cfg)

	s.POST("/raw", func(c *server.Context) error {
		b, err := c.BodyBytes()
		if err != nil {
			return err
		}
		return c.Text(http.StatusOK, string(b))
	})
	s.POST("/form", func(c *server.Context) error {
		vals, err := c.ParseForm()
		if err != nil {
			return err
		}
		return c.Text(http.StatusOK, vals.Get("k"))
	})

	// Within limit
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/raw", strings.NewReader("hello")))
	if rec.Code != 200 || rec.Body.String() != "hello" {
		t.Fatalf("raw ok: %d %q", rec.Code, rec.Body.String())
	}

	// Exceeds limit
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/raw", strings.NewReader(strings.Repeat("x", 64))))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("raw too large: %d, want 413", rec.Code)
	}

	// Form within limit
	req := httptest.NewRequest(http.MethodPost, "/form", strings.NewReader("k=val"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "val" {
		t.Fatalf("form ok: %d %q", rec.Code, rec.Body.String())
	}

	// Form exceeds limit
	req = httptest.NewRequest(http.MethodPost, "/form", strings.NewReader("k="+strings.Repeat("v", 64)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("form too large: %d, want 413", rec.Code)
	}

	// Verify WrapError & errors.Is work cleanly.
	wrapped := server.WrapError(http.StatusBadRequest, "bad", server.ErrInvalidJSON)
	if !errors.Is(wrapped, server.ErrInvalidJSON) {
		t.Fatal("errors.Is failed on WrapError")
	}
}

func TestContextDeleteQueryHelpersAndMultipart(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.MaxBodyBytes = 1024
	s := server.NewWithConfig(cfg)

	s.GET("/delete-and-query", func(c *server.Context) error {
		// Exercise Delete on both inline (<=4) and overflow map (>4) storage.
		for i := range 6 {
			c.Set(fmt.Sprintf("k%d", i), i)
		}
		c.Delete("k1") // inline
		c.Delete("k5") // overflow map
		c.Delete("missing")
		if _, ok := c.Get("k1"); ok {
			return server.NewError(http.StatusInternalServerError, "k1 not deleted")
		}
		if _, ok := c.Get("k5"); ok {
			return server.NewError(http.StatusInternalServerError, "k5 not deleted")
		}
		// Remaining inline keys must still be intact.
		if v, ok := server.GetValue[int](c, "k0"); !ok || v != 0 {
			return server.NewError(http.StatusInternalServerError, "k0 corrupted")
		}
		if v, ok := server.GetValue[int](c, "k3"); !ok || v != 3 {
			return server.NewError(http.StatusInternalServerError, "k3 corrupted")
		}
		// Re-inserting deleted key works.
		c.Set("k1", 42)
		if v, ok := server.GetValue[int](c, "k1"); !ok || v != 42 {
			return server.NewError(http.StatusInternalServerError, "k1 reinsert failed")
		}

		cursor, err := c.QueryInt64("cursor", 99)
		if err != nil {
			return server.NewError(http.StatusBadRequest, "bad cursor")
		}
		verbose, err := c.QueryBool("verbose", false)
		if err != nil {
			return server.NewError(http.StatusBadRequest, "bad verbose")
		}
		msg := c.Query("msg")
		semi := c.Query("bad")
		dup := c.Query("dup")
		return c.JSON(http.StatusOK, map[string]any{
			"cursor":  cursor,
			"verbose": verbose,
			"msg":     msg,
			"semi":    semi,
			"dup":     dup,
			"empty":   c.QueryDefault("flag", "fallback"),
		})
	})

	s.POST("/upload", func(c *server.Context) error {
		form, err := c.ParseMultipartForm(512)
		if err != nil {
			return err
		}
		f, hdr, err := c.FormFile("doc")
		if err != nil {
			return server.NewError(http.StatusBadRequest, "missing file")
		}
		defer f.Close()
		var content bytes.Buffer
		_, _ = content.ReadFrom(f)
		return c.Text(http.StatusOK, fmt.Sprintf("%s:%s:%s", form.Value["title"][0], hdr.Filename, content.String()))
	})

	// Query fast path with percent encoding, +, duplicate keys, semicolon skipped, and defaults.
	req := httptest.NewRequest(http.MethodGet, "/delete-and-query?cursor=9000000001&verbose=true&msg=hello+world%21&bad=1;2&dup=first&dup=second&flag", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["cursor"] != float64(9000000001) || got["verbose"] != true ||
		got["msg"] != "hello world!" || got["semi"] != "" || got["dup"] != "first" || got["empty"] != "fallback" {
		t.Fatalf("unexpected query result: %#v", got)
	}

	// Invalid QueryBool returns error.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/delete-and-query?verbose=maybe", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid QueryBool status = %d, want 400", rec.Code)
	}

	// Valid multipart upload.
	boundary := "testboundary123"
	mpBody := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"title\"\r\n\r\n" +
		"report\r\n" +
		"--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"doc\"; filename=\"a.txt\"\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"file-content\r\n" +
		"--" + boundary + "--\r\n"
	req = httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(mpBody))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "report:a.txt:file-content" {
		t.Fatalf("upload ok: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Oversized multipart upload exceeds MaxBodyBytes -> 413.
	bigMP := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"doc\"; filename=\"big.txt\"\r\n\r\n" +
		strings.Repeat("x", 2048) + "\r\n" +
		"--" + boundary + "--\r\n"
	req = httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(bigMP))
	req.ContentLength = -1 // chunked
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized multipart status = %d, want 413", rec.Code)
	}
}

func TestContextContentTypeBearerTokenSchemeAndFormValue(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.MaxBodyBytes = 32
	cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	s := server.NewWithConfig(cfg)

	s.POST("/inspect", func(c *server.Context) error {
		tok, ok := c.BearerToken()
		return c.JSON(http.StatusOK, map[string]any{
			"ct":     c.ContentType(),
			"tok":    tok,
			"tok_ok": ok,
			"scheme": c.Scheme(),
			"fv":     c.FormValue("field"),
		})
	})
	s.GET("/sniff", func(c *server.Context) error {
		_, err := c.ResponseWriter().Write([]byte("<!DOCTYPE html><html><body>sniffed</body></html>"))
		return err
	})

	// 1. Valid Bearer token, uppercase Content-Type with params, trusted X-Forwarded-Proto, and bounded FormValue.
	req := httptest.NewRequest(http.MethodPost, "/inspect?q=1", strings.NewReader("field=hello"))
	req.RemoteAddr = "10.1.2.3:1234"
	req.Header.Set("Content-Type", "Application/X-Www-Form-Urlencoded; charset=UTF-8")
	req.Header.Set("Authorization", "bearer   secret-jwt.token_123  ")
	req.Header.Set("X-Forwarded-Proto", "https, http")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["ct"] != "application/x-www-form-urlencoded" ||
		got["tok"] != "secret-jwt.token_123" ||
		got["tok_ok"] != true ||
		got["scheme"] != "https" ||
		got["fv"] != "hello" {
		t.Fatalf("unexpected inspect output: %#v", got)
	}

	// 2. Untrusted peer cannot spoof X-Forwarded-Proto; malformed Bearer tokens are rejected; oversized FormValue is blocked.
	for _, badAuth := range []string{"", "Basic dXNlcjpwYXNz", "Bearer", "Bearer   ", "Bearer a b", "Bearer a\x00b"} {
		req := httptest.NewRequest(http.MethodPost, "/inspect", strings.NewReader("field="+strings.Repeat("x", 64)))
		req.RemoteAddr = "198.51.100.10:4321"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-Proto", "https")
		if badAuth != "" {
			req.Header.Set("Authorization", badAuth)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res["tok_ok"] != false || res["tok"] != "" {
			t.Fatalf("auth %q should be rejected, got %#v", badAuth, res)
		}
		if res["scheme"] != "http" {
			t.Fatalf("untrusted X-Forwarded-Proto spoofed scheme: %#v", res)
		}
		if res["fv"] != "" {
			t.Fatalf("oversized FormValue should return empty, got %#v", res)
		}
	}

	// 3. Direct TLS sets scheme to https.
	req = httptest.NewRequest(http.MethodPost, "/inspect", nil)
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Content-Type", "invalid-media-type")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["scheme"] != "https" || got["ct"] != "" {
		t.Fatalf("TLS / invalid Content-Type: %#v", got)
	}

	// 4. Direct Write without WriteHeader preserves Content-Type sniffing on ResponseRecorder.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sniff", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("sniff: code=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestContextSetDeleteOverflowConsistencyAndQueryEdgeCases(t *testing.T) {
	s := server.New()

	s.GET("/kv", func(c *server.Context) error {
		// Fill 4 inline values + 2 overflow map values.
		for i := range 6 {
			c.Set(fmt.Sprintf("k%d", i), i)
		}
		// Delete one inline value so nValues drops below maxInlineValues (3 < 4).
		c.Delete("k0")
		// Update an existing overflow map key ("k4"). It must update in place in
		// valuesMap and not create a duplicate entry in inlineValues.
		c.Set("k4", 400)
		if v, ok := server.GetValue[int](c, "k4"); !ok || v != 400 {
			return server.NewError(http.StatusInternalServerError, fmt.Sprintf("k4=%v ok=%v, want 400", v, ok))
		}
		// Deleting "k4" now must completely remove it.
		c.Delete("k4")
		if v, ok := c.Get("k4"); ok {
			return server.NewError(http.StatusInternalServerError, fmt.Sprintf("deleted k4 still present: %v", v))
		}
		return c.NoContent()
	})

	s.GET("/q", func(c *server.Context) error {
		return c.JSON(http.StatusOK, map[string]string{
			"page":      c.Query("page"),
			"page_size": c.Query("page_size"),
			"p":         c.Query("p"),
			"flag":      c.Query("flag"),
			"esc_key":   c.Query("a b"),
			"recover":   c.Query("k"),
		})
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/kv", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("/kv failed: code=%d body=%q", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/q?page_size=25&p=1&flag&page=7&a+b=ok&k=%ZZ&k=semi;bad&k=good", nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/q status = %d", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["page"] != "7" || got["page_size"] != "25" || got["p"] != "1" ||
		got["flag"] != "" || got["esc_key"] != "ok" || got["recover"] != "good" {
		t.Fatalf("unexpected query values: %#v", got)
	}
}
