// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type optRecorder struct {
	Code   int
	header Header
	Body   bytes.Buffer
}

func newOptRecorder() *optRecorder {
	return &optRecorder{Code: StatusOK, header: make(Header)}
}

func (r *optRecorder) Header() Header              { return r.header }
func (r *optRecorder) WriteHeader(code int)        { r.Code = code }
func (r *optRecorder) Write(p []byte) (int, error) { return r.Body.Write(p) }

func newOptRequest(method, target string) *Request {
	req, err := NewRequest(method, target, nil)
	if err != nil {
		panic(err)
	}
	return req
}

func TestServeMuxStaticSnapshot(t *testing.T) {
	mux := NewServeMux()
	h := func(name string) HandlerFunc {
		return func(w ResponseWriter, r *Request) {
			w.Header().Set("X-Handler", name)
			if id := r.PathValue("id"); id != "" {
				w.Header().Set("X-ID", id)
			}
		}
	}

	mux.HandleFunc("GET /health", h("get-health"))
	mux.HandleFunc("POST /health", h("post-health"))
	mux.HandleFunc("/exact/{$}", h("exact-slash"))
	mux.HandleFunc("GET /users/{id}", h("get-user"))
	mux.HandleFunc("example.com/host-only", h("host-only"))
	mux.HandleFunc("/host-only", h("any-host"))
	mux.HandleFunc("/a/b/c", h("abc"))

	check := func(method, target, host, wantHandler, wantID string) {
		t.Helper()
		req := newOptRequest(method, target)
		if host != "" {
			req.Host = host
		}
		rec := newOptRecorder()
		mux.ServeHTTP(rec, req)
		if got := rec.Header().Get("X-Handler"); got != wantHandler {
			t.Errorf("%s %s (Host:%q): handler = %q, want %q (status %d)", method, target, host, got, wantHandler, rec.Code)
		}
		if got := rec.Header().Get("X-ID"); got != wantID {
			t.Errorf("%s %s: id = %q, want %q", method, target, got, wantID)
		}
	}

	check("GET", "http://other.test/health", "", "get-health", "")
	check("HEAD", "http://other.test/health", "", "get-health", "")
	check("POST", "http://other.test/health", "", "post-health", "")
	check("GET", "http://other.test/exact/", "", "exact-slash", "")
	check("GET", "http://other.test/users/42", "", "get-user", "42")
	check("GET", "http://example.com/host-only", "example.com", "host-only", "")
	check("GET", "http://other.test/host-only", "other.test", "any-host", "")
	check("GET", "http://other.test/a/b/c", "", "abc", "")

	// Percent-encoded path matching must fall back from static table properly.
	check("GET", "http://other.test/%68ealth", "", "get-health", "")
	check("GET", "http://other.test/a/%62/c", "", "abc", "")

	// Pure static-only mux (staticOnly == true) with percent-encoded path.
	staticOnlyMux := NewServeMux()
	staticOnlyMux.HandleFunc("GET /alpha/beta", h("ab"))
	req := newOptRequest("GET", "http://other.test/%61lpha/beta")
	rec := newOptRecorder()
	staticOnlyMux.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Handler"); got != "ab" {
		t.Fatalf("staticOnly percent-encoded path: got %q, want %q", got, "ab")
	}

	// Verify 405 Method Not Allowed still includes Allow header.
	req405 := newOptRequest("DELETE", "http://other.test/health")
	rec405 := newOptRecorder()
	mux.ServeHTTP(rec405, req405)
	if rec405.Code != StatusMethodNotAllowed {
		t.Fatalf("DELETE /health status = %d, want 405", rec405.Code)
	}
	allow := rec405.Header().Get("Allow")
	if !strings.Contains(allow, "GET") || !strings.Contains(allow, "HEAD") || !strings.Contains(allow, "POST") {
		t.Fatalf("DELETE /health Allow = %q, want GET, HEAD, POST", allow)
	}
}

func TestServeMuxStaticSnapshotConcurrentRace(t *testing.T) {
	mux := NewServeMux()
	mux.HandleFunc("GET /init", func(w ResponseWriter, r *Request) {})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			mux.HandleFunc("GET /r/"+string(rune('a'+i%26))+"/"+string(rune('0'+i%10)), func(w ResponseWriter, r *Request) {})
		}
	}()
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := newOptRequest("GET", "http://example.com/init")
			for i := 0; i < 200; i++ {
				h, _ := mux.Handler(req)
				if h == nil {
					t.Errorf("nil handler")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestRequestCloneCoAllocAndShallowCopyRace(t *testing.T) {
	t.Run("CloneIndependence", func(t *testing.T) {
		u, err := url.Parse("https://alice:secret@example.com:8443/path?k=v#frag")
		if err != nil {
			t.Fatal(err)
		}
		orig := &Request{
			Method:           "POST",
			URL:              u,
			Header:           Header{"X-Test": {"a", "b"}},
			Trailer:          Header{"X-Tr": {"t"}},
			TransferEncoding: []string{"chunked"},
		}
		ctx := context.WithValue(context.Background(), struct{}{}, "v")
		cloned := orig.Clone(ctx)
		if cloned.Context() != ctx {
			t.Fatalf("cloned context mismatch")
		}
		if cloned.URL == orig.URL {
			t.Fatalf("cloned URL aliases original")
		}
		if cloned.URL.User == orig.URL.User {
			t.Fatalf("cloned URL.User aliases original")
		}
		cloned.URL.Path = "/changed"
		cloned.URL.User = url.User("bob")
		cloned.Header.Set("X-Test", "mutated")
		if orig.URL.Path != "/path" || orig.URL.User.Username() != "alice" || orig.Header.Get("X-Test") != "a" {
			t.Fatalf("mutating clone affected original: %+v %+v %+v", orig.URL, orig.URL.User, orig.Header)
		}

		// Nil URL must stay nil.
		nilURLReq := &Request{Method: "GET"}
		if c := nilURLReq.Clone(context.Background()); c.URL != nil {
			t.Fatalf("expected nil URL on clone, got %v", c.URL)
		}
	})

	t.Run("LazyContextShallowCopyRace", func(t *testing.T) {
		// Simulate a server Request with lazy context initialization (ctx == nil, baseCtx != nil)
		// accessed concurrently by Request.Context() and Transport/Client shallow copies.
		base := context.Background()
		for i := 0; i < 100; i++ {
			req := &Request{
				Method:   "GET",
				URL:      &url.URL{Scheme: "http", Host: "example.com", Path: "/"},
				ctx:      base,
				ctxState: ctxStateLazy,
			}
			var wg sync.WaitGroup
			wg.Add(3)
			go func() {
				defer wg.Done()
				_ = req.Context()
			}()
			go func() {
				defer wg.Done()
				cp := req.shallowCopy()
				_ = cp.Context()
			}()
			go func() {
				defer wg.Done()
				var dst Request
				req.shallowCopyTo(&dst)
				_ = dst.Context()
			}()
			wg.Wait()
		}
	})
}

func TestTransferWriterWriteBodyFastPaths(t *testing.T) {
	payload := "0123456789abcdefghijklmnopqrstuvwxyz"

	makeBody := func(kind string, wrapTracking bool) io.ReadCloser {
		var r io.Reader
		switch kind {
		case "bytes.Reader":
			r = bytes.NewReader([]byte(payload))
		case "bytes.Buffer":
			r = bytes.NewBufferString(payload)
		case "strings.Reader":
			r = strings.NewReader(payload)
		default:
			t.Fatalf("unknown kind %q", kind)
		}
		rc := io.NopCloser(r)
		if wrapTracking {
			rc = &readTrackingBody{ReadCloser: rc}
		}
		return rc
	}

	for _, kind := range []string{"bytes.Reader", "bytes.Buffer", "strings.Reader"} {
		for _, tracked := range []bool{false, true} {
			name := kind
			if tracked {
				name += "+tracked"
			}
			t.Run(name+"/ExactLength", func(t *testing.T) {
				tw := &transferWriter{
					Method:        "POST",
					Body:          makeBody(kind, tracked),
					BodyCloser:    io.NopCloser(nil),
					ContentLength: int64(len(payload)),
				}
				var out bytes.Buffer
				bw := bufio.NewWriter(&out)
				if err := tw.writeBody(bw); err != nil {
					t.Fatalf("writeBody error: %v", err)
				}
				if err := bw.Flush(); err != nil {
					t.Fatal(err)
				}
				if out.String() != payload {
					t.Fatalf("wrote %q, want %q", out.String(), payload)
				}
			})

			t.Run(name+"/ShortBody", func(t *testing.T) {
				tw := &transferWriter{
					Method:        "POST",
					Body:          makeBody(kind, tracked),
					BodyCloser:    io.NopCloser(nil),
					ContentLength: int64(len(payload) + 5),
				}
				var out bytes.Buffer
				bw := bufio.NewWriter(&out)
				err := tw.writeBody(bw)
				if err == nil || !strings.Contains(err.Error(), "ContentLength") {
					t.Fatalf("ShortBody err = %v, want ContentLength mismatch", err)
				}
			})

			t.Run(name+"/LongBody", func(t *testing.T) {
				tw := &transferWriter{
					Method:        "POST",
					Body:          makeBody(kind, tracked),
					BodyCloser:    io.NopCloser(nil),
					ContentLength: int64(len(payload) - 5),
				}
				var out bytes.Buffer
				bw := bufio.NewWriter(&out)
				err := tw.writeBody(bw)
				if err == nil || !strings.Contains(err.Error(), "ContentLength=") {
					t.Fatalf("LongBody err = %v, want ContentLength mismatch", err)
				}
			})
		}
	}

	// Generic reader with exact length and trailing-byte check (copyBodyWithLength).
	t.Run("GenericReaderExactAndLong", func(t *testing.T) {
		tw := &transferWriter{
			Method:        "POST",
			Body:          struct{ io.Reader }{strings.NewReader(payload)},
			ContentLength: int64(len(payload)),
		}
		var out bytes.Buffer
		bw := bufio.NewWriter(&out)
		if err := tw.writeBody(bw); err != nil {
			t.Fatalf("exact generic writeBody: %v", err)
		}
		_ = bw.Flush()
		if out.String() != payload {
			t.Fatalf("got %q, want %q", out.String(), payload)
		}

		twLong := &transferWriter{
			Method:        "POST",
			Body:          struct{ io.Reader }{strings.NewReader(payload + "EXTRA")},
			ContentLength: int64(len(payload)),
		}
		out.Reset()
		bw.Reset(&out)
		if err := twLong.writeBody(bw); err == nil || !strings.Contains(err.Error(), "ContentLength=") {
			t.Fatalf("long generic writeBody err = %v, want ContentLength mismatch", err)
		}
	})
}

type fakeDirEntry struct {
	name  string
	isDir bool
}

func (e fakeDirEntry) Name() string { return e.name }
func (e fakeDirEntry) IsDir() bool  { return e.isDir }
func (e fakeDirEntry) Type() fs.FileMode {
	if e.isDir {
		return fs.ModeDir
	}
	return 0
}
func (e fakeDirEntry) Info() (fs.FileInfo, error) { return fakeFileInfo{e}, nil }

type fakeFileInfo struct{ e fakeDirEntry }

func (fi fakeFileInfo) Name() string       { return fi.e.name }
func (fi fakeFileInfo) Size() int64        { return 0 }
func (fi fakeFileInfo) Mode() fs.FileMode  { return fi.e.Type() }
func (fi fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fi fakeFileInfo) IsDir() bool        { return fi.e.isDir }
func (fi fakeFileInfo) Sys() any           { return nil }

type fakeDirFile struct {
	entries []fs.DirEntry
	pos     int
}

func (f *fakeDirFile) Close() error                   { return nil }
func (f *fakeDirFile) Read([]byte) (int, error)       { return 0, io.EOF }
func (f *fakeDirFile) Seek(int64, int) (int64, error) { return 0, nil }
func (f *fakeDirFile) Stat() (fs.FileInfo, error) {
	return fakeFileInfo{fakeDirEntry{"dir", true}}, nil
}
func (f *fakeDirFile) Readdir(count int) ([]fs.FileInfo, error) { return nil, nil }
func (f *fakeDirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		out := f.entries[f.pos:]
		f.pos = len(f.entries)
		return out, nil
	}
	if f.pos >= len(f.entries) {
		return nil, io.EOF
	}
	end := f.pos + n
	if end > len(f.entries) {
		end = len(f.entries)
	}
	out := f.entries[f.pos:end]
	f.pos = end
	return out, nil
}

func TestDirListFormattingAndLargeFlush(t *testing.T) {
	entries := []fs.DirEntry{
		fakeDirEntry{name: "z.txt", isDir: false},
		fakeDirEntry{name: "a<b&c>\"'?.txt", isDir: false},
		fakeDirEntry{name: "sub#dir", isDir: true},
	}
	for i := 0; i < 150; i++ {
		entries = append(entries, fakeDirEntry{
			name:  "file_" + strings.Repeat("x", 20) + "_" + string(rune('a'+i%26)) + string(rune('0'+i/26)) + ".dat",
			isDir: false,
		})
	}
	f := &fakeDirFile{entries: entries}
	rec := newOptRecorder()
	req := newOptRequest("GET", "http://example.com/dir/")
	dirList(rec, req, f)

	body := rec.Body.String()
	if !strings.HasPrefix(body, "<!doctype html>\n<meta name=\"viewport\" content=\"width=device-width\">\n<meta name=\"color-scheme\" content=\"light dark\">\n<pre>\n") {
		t.Fatalf("unexpected prefix: %q", body[:min(len(body), 140)])
	}
	if !strings.HasSuffix(body, "</pre>\n") {
		t.Fatalf("unexpected suffix: %q", body[max(0, len(body)-32):])
	}
	// Verify HTML & URL escaping of special entry and sorted order before z.txt.
	specialName := `a<b&c>"'?.txt`
	wantSpecial := `<a href="` + (&url.URL{Path: specialName}).String() + `">` + htmlReplacer.Replace(specialName) + `</a>`
	idxSpecial := strings.Index(body, wantSpecial)
	idxSub := strings.Index(body, `<a href="sub%23dir/">sub#dir/</a>`)
	idxZ := strings.Index(body, `<a href="z.txt">z.txt</a>`)
	if idxSpecial < 0 || idxSub < 0 || idxZ < 0 {
		t.Fatalf("missing expected links (special=%d sub=%d z=%d)\nwantSpecial=%q\nbody[:240]=%q", idxSpecial, idxSub, idxZ, wantSpecial, body[:min(len(body), 240)])
	}
	if !(idxSpecial < idxSub && idxSub < idxZ) {
		t.Fatalf("entries not sorted: special=%d sub=%d z=%d", idxSpecial, idxSub, idxZ)
	}
	if len(body) <= 4096 {
		t.Fatalf("expected large directory output > 4096 bytes, got %d", len(body))
	}
}

func BenchmarkRequestClone(b *testing.B) {
	u, _ := url.Parse("https://example.com/api/v1/items?limit=20")
	req := &Request{
		Method: "GET",
		URL:    u,
		Header: Header{
			"Accept":     {"application/json"},
			"User-Agent": {"bench"},
		},
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = req.Clone(ctx)
	}
}

func BenchmarkTransferWriterInMemoryBody(b *testing.B) {
	payloadBytes := bytes.Repeat([]byte("a"), 1024)
	payloadStr := string(payloadBytes)

	b.Run("BytesReader", func(b *testing.B) {
		bw := bufio.NewWriter(io.Discard)
		br := bytes.NewReader(payloadBytes)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			br.Reset(payloadBytes)
			tw := &transferWriter{
				Method:        "POST",
				Body:          &readTrackingBody{ReadCloser: io.NopCloser(br)},
				ContentLength: int64(len(payloadBytes)),
			}
			if err := tw.writeBody(bw); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("StringsReader", func(b *testing.B) {
		bw := bufio.NewWriter(io.Discard)
		sr := strings.NewReader(payloadStr)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sr.Reset(payloadStr)
			tw := &transferWriter{
				Method:        "POST",
				Body:          &readTrackingBody{ReadCloser: io.NopCloser(sr)},
				ContentLength: int64(len(payloadStr)),
			}
			if err := tw.writeBody(bw); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkDirList(b *testing.B) {
	entries := make([]fs.DirEntry, 200)
	for i := range entries {
		entries[i] = fakeDirEntry{
			name:  "entry_" + string(rune('z'-i%26)) + "_" + strings.Repeat("a", 8) + ".txt",
			isDir: i%5 == 0,
		}
	}
	req := newOptRequest("GET", "http://example.com/dir/")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := newOptRecorder()
		f := &fakeDirFile{entries: entries}
		dirList(rec, req, f)
	}
}
