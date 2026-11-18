// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRoutingFirstSegment(t *testing.T) {
	for _, test := range []struct {
		in   string
		want []string
	}{
		{"/a/b/c", []string{"a", "b", "c"}},
		{"/a/b/", []string{"a", "b", "/"}},
		{"/", []string{"/"}},
		{"/a/%62/c", []string{"a", "b", "c"}},
		{"/a%2Fb%2fc", []string{"a/b/c"}},
	} {
		var got []string
		rest := test.in
		for len(rest) > 0 {
			var seg string
			seg, rest = firstSegment(rest)
			got = append(got, seg)
		}
		if !slices.Equal(got, test.want) {
			t.Errorf("%q: got %v, want %v", test.in, got, test.want)
		}
	}
}

// TODO: test host and method
var testTree *routingNode

func getTestTree() *routingNode {
	if testTree == nil {
		testTree = buildTree("/a", "/a/b", "/a/{x}",
			"/g/h/i", "/g/{x}/j",
			"/a/b/{x...}", "/a/b/{y}", "/a/b/{$}")
	}
	return testTree
}

func buildTree(pats ...string) *routingNode {
	root := &routingNode{}
	for _, p := range pats {
		pat, err := parsePattern(p)
		if err != nil {
			panic(err)
		}
		root.addPattern(pat, nil)
	}
	return root
}

func TestRoutingAddPattern(t *testing.T) {
	want := `"":
    "":
        "a":
            "/a"
            "":
                "/a/{x}"
            "b":
                "/a/b"
                "":
                    "/a/b/{y}"
                "/":
                    "/a/b/{$}"
                MULTI:
                    "/a/b/{x...}"
        "g":
            "":
                "j":
                    "/g/{x}/j"
            "h":
                "i":
                    "/g/h/i"
`

	var b strings.Builder
	getTestTree().print(&b, 0)
	got := b.String()
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

type testCase struct {
	method, host, path string
	wantPat            string // "" for nil (no match)
	wantMatches        []string
}

func TestRoutingNodeMatch(t *testing.T) {

	test := func(tree *routingNode, tests []testCase) {
		t.Helper()
		for _, test := range tests {
			gotNode, gotMatches := tree.match(test.host, test.method, test.path)
			got := ""
			if gotNode != nil {
				got = gotNode.pattern.String()
			}
			if got != test.wantPat {
				t.Errorf("%s, %s, %s: got %q, want %q", test.host, test.method, test.path, got, test.wantPat)
			}
			if !slices.Equal(gotMatches, test.wantMatches) {
				t.Errorf("%s, %s, %s: got matches %v, want %v", test.host, test.method, test.path, gotMatches, test.wantMatches)
			}
		}
	}

	test(getTestTree(), []testCase{
		{"GET", "", "/a", "/a", nil},
		{"Get", "", "/b", "", nil},
		{"Get", "", "/a/b", "/a/b", nil},
		{"Get", "", "/a/c", "/a/{x}", []string{"c"}},
		{"Get", "", "/a/b/", "/a/b/{$}", nil},
		{"Get", "", "/a/b/c", "/a/b/{y}", []string{"c"}},
		{"Get", "", "/a/b/c/d", "/a/b/{x...}", []string{"c/d"}},
		{"Get", "", "/g/h/i", "/g/h/i", nil},
		{"Get", "", "/g/h/j", "/g/{x}/j", []string{"h"}},
	})

	tree := buildTree(
		"/item/",
		"POST /item/{user}",
		"GET /item/{user}",
		"/item/{user}",
		"/item/{user}/{id}",
		"/item/{user}/new",
		"/item/{$}",
		"POST alt.com/item/{user}",
		"GET /headwins",
		"HEAD /headwins",
		"/path/{p...}")

	test(tree, []testCase{
		{"GET", "", "/item/jba",
			"GET /item/{user}", []string{"jba"}},
		{"POST", "", "/item/jba",
			"POST /item/{user}", []string{"jba"}},
		{"HEAD", "", "/item/jba",
			"GET /item/{user}", []string{"jba"}},
		{"get", "", "/item/jba",
			"/item/{user}", []string{"jba"}}, // method matches are case-sensitive
		{"POST", "", "/item/jba/17",
			"/item/{user}/{id}", []string{"jba", "17"}},
		{"GET", "", "/item/jba/new",
			"/item/{user}/new", []string{"jba"}},
		{"GET", "", "/item/",
			"/item/{$}", []string{}},
		{"GET", "", "/item/jba/17/line2",
			"/item/", nil},
		{"POST", "alt.com", "/item/jba",
			"POST alt.com/item/{user}", []string{"jba"}},
		{"GET", "alt.com", "/item/jba",
			"GET /item/{user}", []string{"jba"}},
		{"GET", "", "/item",
			"", nil}, // does not match
		{"GET", "", "/headwins",
			"GET /headwins", nil},
		{"HEAD", "", "/headwins", // HEAD is more specific than GET
			"HEAD /headwins", nil},
		{"GET", "", "/path/to/file",
			"/path/{p...}", []string{"to/file"}},
		{"GET", "", "/path/*",
			"/path/{p...}", []string{"*"}},
	})

	// A pattern ending in {$} should only match URLS with a trailing slash.
	pat1 := "/a/b/{$}"
	test(buildTree(pat1), []testCase{
		{"GET", "", "/a/b", "", nil},
		{"GET", "", "/a/b/", pat1, nil},
		{"GET", "", "/a/b/c", "", nil},
		{"GET", "", "/a/b/c/d", "", nil},
	})

	// A pattern ending in a single wildcard should not match a trailing slash URL.
	pat2 := "/a/b/{w}"
	test(buildTree(pat2), []testCase{
		{"GET", "", "/a/b", "", nil},
		{"GET", "", "/a/b/", "", nil},
		{"GET", "", "/a/b/c", pat2, []string{"c"}},
		{"GET", "", "/a/b/c/d", "", nil},
	})

	// A pattern ending in a multi wildcard should match both URLs.
	pat3 := "/a/b/{w...}"
	test(buildTree(pat3), []testCase{
		{"GET", "", "/a/b", "", nil},
		{"GET", "", "/a/b/", pat3, []string{""}},
		{"GET", "", "/a/b/c", pat3, []string{"c"}},
		{"GET", "", "/a/b/c/d", pat3, []string{"c/d"}},
	})

	// All three of the above should work together.
	test(buildTree(pat1, pat2, pat3), []testCase{
		{"GET", "", "/a/b", "", nil},
		{"GET", "", "/a/b/", pat1, nil},
		{"GET", "", "/a/b/c", pat2, []string{"c"}},
		{"GET", "", "/a/b/c/d", pat3, []string{"c/d"}},
	})
}

func TestMatchingMethods(t *testing.T) {
	hostTree := buildTree("GET a.com/", "PUT b.com/", "POST /foo/{x}")
	for _, test := range []struct {
		name       string
		tree       *routingNode
		host, path string
		want       string
	}{
		{
			"post",
			buildTree("POST /"), "", "/foo",
			"POST",
		},
		{
			"get",
			buildTree("GET /"), "", "/foo",
			"GET,HEAD",
		},
		{
			"host",
			hostTree, "", "/foo",
			"",
		},
		{
			"host",
			hostTree, "", "/foo/bar",
			"POST",
		},
		{
			"host2",
			hostTree, "a.com", "/foo/bar",
			"GET,HEAD,POST",
		},
		{
			"host3",
			hostTree, "b.com", "/bar",
			"PUT",
		},
		{
			// This case shouldn't come up because we only call matchingMethods
			// when there was no match, but we include it for completeness.
			"empty",
			buildTree("/"), "", "/",
			"",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ms := map[string]bool{}
			test.tree.matchingMethods(test.host, test.path, ms)
			got := strings.Join(slices.Sorted(maps.Keys(ms)), ",")
			if got != test.want {
				t.Errorf("got %s, want %s", got, test.want)
			}
		})
	}
}

func (n *routingNode) print(w io.Writer, level int) {
	indent := strings.Repeat("    ", level)
	if n.pattern != nil {
		fmt.Fprintf(w, "%s%q\n", indent, n.pattern)
	}
	if n.emptyChild != nil {
		fmt.Fprintf(w, "%s%q:\n", indent, "")
		n.emptyChild.print(w, level+1)
	}

	var keys []string
	n.children.eachPair(func(k string, _ *routingNode) bool {
		keys = append(keys, k)
		return true
	})
	slices.Sort(keys)

	for _, k := range keys {
		fmt.Fprintf(w, "%s%q:\n", indent, k)
		n, _ := n.children.find(k)
		n.print(w, level+1)
	}

	if n.multiChild != nil {
		fmt.Fprintf(w, "%sMULTI:\n", indent)
		n.multiChild.print(w, level+1)
	}
}

type muxTestResponseWriter struct {
	hdr  Header
	code int
	body []byte
}

func newMuxTestResponseWriter() *muxTestResponseWriter {
	return &muxTestResponseWriter{hdr: make(Header)}
}

func (w *muxTestResponseWriter) Header() Header         { return w.hdr }
func (w *muxTestResponseWriter) WriteHeader(code int) { w.code = code }
func (w *muxTestResponseWriter) Write(p []byte) (int, error) {
	w.body = append(w.body, p...)
	return len(p), nil
}

func TestServeMuxSnapLifecycleAndRace(t *testing.T) {
	t.Run("LifecycleAndInvalidation", func(t *testing.T) {
		mux := NewServeMux()
		mux.HandleFunc("GET /users/{id}", func(w ResponseWriter, r *Request) {
			fmt.Fprintf(w, "user:%s", r.PathValue("id"))
		})
		mux.HandleFunc("/sub/", func(w ResponseWriter, r *Request) {
			io.WriteString(w, "sub")
		})

		req := &Request{
			Method: MethodGet,
			Host:   "example.com",
			URL:    &url.URL{Path: "/users/42"},
		}
		if got := mux.snap.Load(); got != nil {
			t.Fatalf("initial snap = %v, want nil", got)
		}
		if got := mux.snapReads.Load(); got != 0 {
			t.Fatalf("initial snapReads = %d, want 0", got)
		}

		for i := int32(1); i <= 3; i++ {
			_, pat := mux.Handler(req)
			if pat != "GET /users/{id}" {
				t.Fatalf("read %d pattern = %q, want GET /users/{id}", i, pat)
			}
			if mux.snap.Load() != nil {
				t.Fatalf("read %d unexpectedly built snapshot", i)
			}
			if got := mux.snapReads.Load(); got != i {
				t.Fatalf("read %d snapReads = %d, want %d", i, got, i)
			}
		}

		// 4th read builds the snapshot.
		_, pat := mux.Handler(req)
		if pat != "GET /users/{id}" {
			t.Fatalf("read 4 pattern = %q, want GET /users/{id}", pat)
		}
		snap1 := mux.snap.Load()
		if snap1 == nil {
			t.Fatal("read 4 did not build snapshot")
		}

		// Subsequent read uses snapshot cleanly.
		w := newMuxTestResponseWriter()
		mux.ServeHTTP(w, &Request{
			Method: MethodGet,
			Host:   "example.com",
			URL:    &url.URL{Path: "/users/7"},
		})
		if string(w.body) != "user:7" {
			t.Fatalf("snapshot ServeHTTP body = %q, want user:7", w.body)
		}

		// Registering a new route invalidates the snapshot and resets snapReads.
		mux.HandleFunc("GET /added/{id}", func(w ResponseWriter, r *Request) {
			fmt.Fprintf(w, "added:%s", r.PathValue("id"))
		})
		if mux.snap.Load() != nil {
			t.Fatal("snapshot not invalidated after HandleFunc")
		}
		if got := mux.snapReads.Load(); got != 0 {
			t.Fatalf("snapReads after HandleFunc = %d, want 0", got)
		}

		addedReq := &Request{
			Method: MethodGet,
			Host:   "example.com",
			URL:    &url.URL{Path: "/added/99"},
		}
		for i := int32(1); i <= 3; i++ {
			_, pat := mux.Handler(addedReq)
			if pat != "GET /added/{id}" {
				t.Fatalf("rebuild read %d pattern = %q, want GET /added/{id}", i, pat)
			}
			if mux.snap.Load() != nil {
				t.Fatalf("rebuild read %d unexpectedly built snapshot", i)
			}
			if got := mux.snapReads.Load(); got != i {
				t.Fatalf("rebuild read %d snapReads = %d, want %d", i, got, i)
			}
		}
		_, pat = mux.Handler(addedReq)
		if pat != "GET /added/{id}" {
			t.Fatalf("rebuild read 4 pattern = %q, want GET /added/{id}", pat)
		}
		snap2 := mux.snap.Load()
		if snap2 == nil || snap2 == snap1 {
			t.Fatalf("snapshot did not rebuild cleanly (snap2=%v, snap1=%v)", snap2, snap1)
		}
		w2 := newMuxTestResponseWriter()
		mux.ServeHTTP(w2, &Request{
			Method: MethodGet,
			Host:   "example.com",
			URL:    &url.URL{Path: "/added/99"},
		})
		if string(w2.body) != "added:99" {
			t.Fatalf("rebuilt snapshot ServeHTTP body = %q, want added:99", w2.body)
		}
	})

	t.Run("ExactMatchTrailingSlashPreAndPostSnap", func(t *testing.T) {
		mux := NewServeMux()
		mux.HandleFunc("/end/{$}", func(w ResponseWriter, r *Request) { io.WriteString(w, "end") })
		mux.HandleFunc("/p/{x...}", func(w ResponseWriter, r *Request) { io.WriteString(w, "p:"+r.PathValue("x")) })
		mux.HandleFunc("/sub/", func(w ResponseWriter, r *Request) { io.WriteString(w, "sub") })

		check := func(phase string) {
			t.Helper()
			cases := []struct {
				path     string
				wantCode int
				wantLoc  string
				wantBody string
			}{
				{"/end", StatusTemporaryRedirect, "/end/", ""},
				{"/end/", StatusOK, "", "end"},
				{"/p", StatusTemporaryRedirect, "/p/", ""},
				{"/p/item", StatusOK, "", "p:item"},
				{"/sub", StatusTemporaryRedirect, "/sub/", ""},
				{"/sub/deep", StatusOK, "", "sub"},
			}
			for _, tc := range cases {
				w := newMuxTestResponseWriter()
				w.code = StatusOK
				mux.ServeHTTP(w, &Request{
					Method: MethodGet,
					Host:   "example.com",
					URL:    &url.URL{Path: tc.path},
				})
				if w.code != tc.wantCode {
					t.Errorf("%s %s: status = %d, want %d", phase, tc.path, w.code, tc.wantCode)
				}
				if gotLoc := w.hdr.Get("Location"); gotLoc != tc.wantLoc {
					t.Errorf("%s %s: Location = %q, want %q", phase, tc.path, gotLoc, tc.wantLoc)
				}
				if tc.wantBody != "" && string(w.body) != tc.wantBody {
					t.Errorf("%s %s: body = %q, want %q", phase, tc.path, w.body, tc.wantBody)
				}
			}
		}

		// Reset snap state to exercise pre-snapshot check explicitly on separate read muxes,
		// then verify after snapshot build on the same mux.
		preMux := NewServeMux()
		preMux.HandleFunc("/end/{$}", func(w ResponseWriter, r *Request) { io.WriteString(w, "end") })
		preMux.HandleFunc("/p/{x...}", func(w ResponseWriter, r *Request) { io.WriteString(w, "p:"+r.PathValue("x")) })
		preMux.HandleFunc("/sub/", func(w ResponseWriter, r *Request) { io.WriteString(w, "sub") })
		for _, path := range []string{"/end", "/p", "/sub"} {
			tmpMux := NewServeMux()
			tmpMux.HandleFunc("/end/{$}", func(w ResponseWriter, r *Request) { io.WriteString(w, "end") })
			tmpMux.HandleFunc("/p/{x...}", func(w ResponseWriter, r *Request) { io.WriteString(w, "p:"+r.PathValue("x")) })
			tmpMux.HandleFunc("/sub/", func(w ResponseWriter, r *Request) { io.WriteString(w, "sub") })
			w := newMuxTestResponseWriter()
			w.code = StatusOK
			tmpMux.ServeHTTP(w, &Request{Method: MethodGet, Host: "example.com", URL: &url.URL{Path: path}})
			if tmpMux.snap.Load() != nil {
				t.Fatalf("pre-snap unexpectedly built on first read for %s", path)
			}
			if w.code != StatusTemporaryRedirect || w.hdr.Get("Location") != path+"/" {
				t.Errorf("pre-snap %s: code=%d Loc=%q", path, w.code, w.hdr.Get("Location"))
			}
		}
		preW := newMuxTestResponseWriter()
		preW.code = StatusOK
		preMux.ServeHTTP(preW, &Request{Method: MethodGet, Host: "example.com", URL: &url.URL{Path: "/p/item"}})
		if preMux.snap.Load() != nil {
			t.Fatal("pre-snap unexpectedly built on /p/item")
		}
		if preW.code != StatusOK || string(preW.body) != "p:item" {
			t.Errorf("pre-snap /p/item: code=%d body=%q", preW.code, preW.body)
		}

		// Warm up snapshot on mux and test post-snapshot paths.
		warmReq := &Request{Method: MethodGet, Host: "example.com", URL: &url.URL{Path: "/end/"}}
		for i := 0; i < 4; i++ {
			mux.Handler(warmReq)
		}
		if mux.snap.Load() == nil {
			t.Fatal("snapshot was not built after 4 reads")
		}
		check("post-snap")
	})

	t.Run("Allow405SingleAndSortedMulti", func(t *testing.T) {
		mux := NewServeMux()
		mux.HandleFunc("POST /onlypost", func(w ResponseWriter, r *Request) {})
		mux.HandleFunc("PUT /multi", func(w ResponseWriter, r *Request) {})
		mux.HandleFunc("GET /multi", func(w ResponseWriter, r *Request) {})
		mux.HandleFunc("DELETE /multi", func(w ResponseWriter, r *Request) {})
		mux.HandleFunc("POST /dir/", func(w ResponseWriter, r *Request) {})

		verifyAllow := func(phase, method, path, wantAllow string) {
			t.Helper()
			w := newMuxTestResponseWriter()
			mux.ServeHTTP(w, &Request{
				Method: method,
				Host:   "example.com",
				URL:    &url.URL{Path: path},
			})
			if w.code != StatusMethodNotAllowed {
				t.Fatalf("%s %s %s: status = %d, want 405", phase, method, path, w.code)
			}
			if got := w.hdr.Get("Allow"); got != wantAllow {
				t.Errorf("%s %s %s: Allow = %q, want %q", phase, method, path, got, wantAllow)
			}
		}

		verifyAllow("pre-snap", MethodGet, "/onlypost", "POST")
		verifyAllow("pre-snap", MethodPatch, "/multi", "DELETE, GET, HEAD, PUT")
		verifyAllow("pre-snap", MethodGet, "/dir", "POST")

		// Build snapshot and verify again.
		warmReq := &Request{Method: MethodGet, Host: "example.com", URL: &url.URL{Path: "/multi"}}
		for i := 0; i < 4; i++ {
			mux.Handler(warmReq)
		}
		if mux.snap.Load() == nil {
			t.Fatal("snapshot was not built")
		}
		verifyAllow("post-snap", MethodGet, "/onlypost", "POST")
		verifyAllow("post-snap", MethodPatch, "/multi", "DELETE, GET, HEAD, PUT")
		verifyAllow("post-snap", MethodGet, "/dir", "POST")
	})

	t.Run("StripHostPort", func(t *testing.T) {
		cases := []struct {
			in   string
			want string
		}{
			{"", ""},
			{"example.com", "example.com"},
			{"example.com:8080", "example.com"},
			{"example.com:", "example.com"},
			{"127.0.0.1:8080", "127.0.0.1"},
			{"[::1]:8080", "::1"},
			{"[::1]:", "::1"},
			{"[2001:db8::1]", "[2001:db8::1]"},
			{"2001:db8::1", "2001:db8::1"},
			{"[::1", "[::1"},
			{"foo]:80", "foo]:80"},
			{"foo[:80", "foo[:80"},
			{"[::1]:80]", "[::1]:80]"},
			{"[bad[host]:80", "[bad[host]:80"},
		}
		for _, tc := range cases {
			if got := stripHostPort(tc.in); got != tc.want {
				t.Errorf("stripHostPort(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})

	t.Run("ConcurrentReadersAndDynamicRoutes", func(t *testing.T) {
		mux := NewServeMux()
		mux.HandleFunc("GET /static/health", func(w ResponseWriter, r *Request) {
			io.WriteString(w, "ok")
		})
		mux.HandleFunc("GET /items/{id}", func(w ResponseWriter, r *Request) {
			io.WriteString(w, r.PathValue("id"))
		})

		var stop atomic.Bool
		var wg sync.WaitGroup
		const readers = 6
		for g := 0; g < readers; g++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for i := 0; !stop.Load() || i < 20; i++ {
					staticReq := &Request{
						Method: MethodGet,
						Host:   "example.com:8080",
						URL:    &url.URL{Path: "/static/health"},
					}
					_, pat := mux.Handler(staticReq)
					if pat != "GET /static/health" {
						t.Errorf("reader %d: static pat = %q", id, pat)
						return
					}
					w := newMuxTestResponseWriter()
					itemReq := &Request{
						Method: MethodGet,
						Host:   "[::1]:8080",
						URL:    &url.URL{Path: "/items/abc"},
					}
					mux.ServeHTTP(w, itemReq)
					if string(w.body) != "abc" {
						t.Errorf("reader %d: item body = %q, want abc", id, w.body)
						return
					}
				}
			}(g)
		}

		const dynRoutes = 50
		for i := 0; i < dynRoutes; i++ {
			tag := fmt.Sprintf("dyn-%d", i)
			mux.HandleFunc(fmt.Sprintf("GET /dyn/%d/{id}", i), func(w ResponseWriter, r *Request) {
				io.WriteString(w, tag+":"+r.PathValue("id"))
			})
		}
		stop.Store(true)
		wg.Wait()

		// Trigger snapshot rebuild after all mutations complete.
		healthReq := &Request{Method: MethodGet, Host: "example.com", URL: &url.URL{Path: "/static/health"}}
		for i := 0; i < 4; i++ {
			mux.Handler(healthReq)
		}
		if mux.snap.Load() == nil {
			t.Fatal("snapshot was not built after concurrent reads + dynamic registrations")
		}
		for i := 0; i < dynRoutes; i++ {
			w := newMuxTestResponseWriter()
			req := &Request{
				Method: MethodGet,
				Host:   "example.com",
				URL:    &url.URL{Path: fmt.Sprintf("/dyn/%d/val%d", i, i)},
			}
			mux.ServeHTTP(w, req)
			want := fmt.Sprintf("dyn-%d:val%d", i, i)
			if string(w.body) != want {
				t.Fatalf("route /dyn/%d: body = %q, want %q", i, w.body, want)
			}
		}
	})
}

func BenchmarkServeMuxSnap(b *testing.B) {
	mux := NewServeMux()
	noop := HandlerFunc(func(w ResponseWriter, r *Request) {})
	for _, pat := range []string{
		"GET /",
		"GET /api/v1/health",
		"GET /api/v1/users",
		"POST /api/v1/users",
		"GET /api/v1/users/{id}",
		"PUT /api/v1/users/{id}",
		"DELETE /api/v1/users/{id}",
		"GET /api/v1/users/{id}/posts/{postID}",
		"GET /api/v1/orgs/{org}/members/{member}",
		"GET /static/{path...}",
	} {
		mux.Handle(pat, noop)
	}
	warmReq := &Request{Method: MethodGet, Host: "example.com:443", URL: &url.URL{Path: "/api/v1/health"}}
	for i := 0; i < 4; i++ {
		mux.Handler(warmReq)
	}
	if mux.snap.Load() == nil {
		b.Fatal("expected snapshot to be built after 4 reads")
	}

	b.Run("Static", func(b *testing.B) {
		req := &Request{Method: MethodGet, Host: "example.com:8080", URL: &url.URL{Path: "/api/v1/health"}}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, pat := mux.Handler(req); pat == "" {
				b.Fatal("no match")
			}
		}
	})

	b.Run("Param", func(b *testing.B) {
		req := &Request{Method: MethodGet, Host: "[::1]:8080", URL: &url.URL{Path: "/api/v1/users/12345/posts/99"}}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, pat := mux.Handler(req); pat == "" {
				b.Fatal("no match")
			}
		}
	})
}

