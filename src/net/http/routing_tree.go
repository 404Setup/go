// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements a decision tree for fast matching of requests to
// patterns.
//
// The root of the tree branches on the host of the request.
// The next level branches on the method.
// The remaining levels branch on consecutive segments of the path.
//
// The "more specific wins" precedence rule can result in backtracking.
// For example, given the patterns
//     /a/b/z
//     /a/{x}/c
// we will first try to match the path "/a/b/c" with /a/b/z, and
// when that fails we will try against /a/{x}/c.

package http

import (
	"slices"
	"strings"
)

// A routingNode is a node in the decision tree.
// The same struct is used for leaf and interior nodes.
type routingNode struct {
	// A leaf node holds a single pattern and the Handler it was registered
	// with.
	pattern *pattern
	handler Handler

	// An interior node maps parts of the incoming request to child nodes.
	// special children keys:
	//     "/"	trailing slash (resulting from {$})
	//	   ""   single wildcard
	children   mapping[string, *routingNode]
	multiChild *routingNode // child with multi wildcard
	emptyChild *routingNode // optimization: child with key ""
	namedMulti bool
	multiLeaf  bool

	// Precomputed exact-path lookup table attached only to method-level nodes
	// in immutable ServeMux snapshots (cloneSnapshot). Kept behind a single
	// pointer so interior/leaf routingNodes stay compact.
	static *staticRoutes
}

type staticRoutes struct {
	key     string
	val     *routingNode
	m       map[string]*routingNode
	lenMask uint64 // bit (len & 63) set for each static route length
	minLen  int
	maxLen  int
	only    bool // true if every pattern in this method subtree is in staticRoutes
}

// cloneSnapshot returns an independent immutable snapshot of the decision tree
// rooted at root, with per-method O(1) static route tables populated.
func (root *routingNode) cloneSnapshot() *routingNode {
	c := root.clone()
	if c == nil {
		return nil
	}
	populateHostStaticTables(c.emptyChild)
	c.children.eachPair(func(_ string, hostNode *routingNode) bool {
		populateHostStaticTables(hostNode)
		return true
	})
	return c
}

func populateHostStaticTables(hostNode *routingNode) {
	if hostNode == nil {
		return
	}
	populateMethodStaticTable(hostNode.emptyChild)
	hostNode.children.eachPair(func(_ string, methodNode *routingNode) bool {
		populateMethodStaticTable(methodNode)
		return true
	})
}

func populateMethodStaticTable(methodNode *routingNode) {
	if methodNode == nil {
		return
	}
	var entries []entry[string, *routingNode]
	allStatic := collectStaticLeaves(methodNode, &entries)
	if len(entries) == 0 {
		return
	}
	st := &staticRoutes{
		minLen: len(entries[0].key),
		maxLen: len(entries[0].key),
		only:   allStatic,
	}
	if len(entries) == 1 {
		st.key = entries[0].key
		st.val = entries[0].value
		st.lenMask = 1 << (uint(len(st.key)) & 63)
	} else {
		m := make(map[string]*routingNode, len(entries))
		for _, e := range entries {
			l := len(e.key)
			if l < st.minLen {
				st.minLen = l
			}
			if l > st.maxLen {
				st.maxLen = l
			}
			st.lenMask |= 1 << (uint(l) & 63)
			m[e.key] = e.value
		}
		st.m = m
	}
	methodNode.static = st
}

func collectStaticLeaves(n *routingNode, out *[]entry[string, *routingNode]) (allStatic bool) {
	if n == nil {
		return true
	}
	allStatic = true
	if n.pattern != nil {
		if key, ok := staticPatternPath(n.pattern); ok {
			*out = append(*out, entry[string, *routingNode]{key: key, value: n})
		} else {
			allStatic = false
		}
	}
	if n.emptyChild != nil || n.multiChild != nil {
		allStatic = false
	}
	n.children.eachPair(func(_ string, child *routingNode) bool {
		if !collectStaticLeaves(child, out) {
			allStatic = false
		}
		return true
	})
	return allStatic
}

func staticPatternPath(p *pattern) (string, bool) {
	if len(p.segments) == 0 {
		return "", false
	}
	size := 0
	for i, seg := range p.segments {
		if seg.wild || seg.multi {
			return "", false
		}
		if seg.s == "/" {
			if i != len(p.segments)-1 {
				return "", false
			}
			size++
			continue
		}
		if seg.s == "" || strings.IndexByte(seg.s, '/') >= 0 || strings.IndexByte(seg.s, '%') >= 0 {
			return "", false
		}
		size += 1 + len(seg.s)
	}
	var b strings.Builder
	b.Grow(size)
	for _, seg := range p.segments {
		if seg.s == "/" {
			b.WriteByte('/')
		} else {
			b.WriteByte('/')
			b.WriteString(seg.s)
		}
	}
	return b.String(), true
}

func (st *staticRoutes) lookup(path string) *routingNode {
	l := len(path)
	if l < st.minLen || l > st.maxLen || (st.lenMask&(1<<(uint(l)&63))) == 0 {
		return nil
	}
	if st.m != nil {
		return st.m[path]
	}
	if path == st.key {
		return st.val
	}
	return nil
}

// clone returns an independent immutable snapshot of the subtree rooted at n,
// sharing only immutable pattern metadata and handler values.
func (n *routingNode) clone() *routingNode {
	if n == nil {
		return nil
	}
	c := &routingNode{
		pattern:    n.pattern,
		handler:    n.handler,
		multiChild: n.multiChild.clone(),
		emptyChild: n.emptyChild.clone(),
		namedMulti: n.namedMulti,
		multiLeaf:  n.multiLeaf,
	}
	if len(n.children.s) > 0 {
		c.children.s = make([]entry[string, *routingNode], len(n.children.s))
		for i, e := range n.children.s {
			c.children.s[i] = entry[string, *routingNode]{key: e.key, value: e.value.clone()}
		}
	} else if len(n.children.m) > 0 {
		c.children.m = make(map[string]*routingNode, len(n.children.m))
		for k, v := range n.children.m {
			c.children.m[k] = v.clone()
		}
	}
	return c
}

// addPattern adds a pattern and its associated Handler to the tree
// at root.
func (root *routingNode) addPattern(p *pattern, h Handler) {
	// First level of tree is host.
	n := root.addChild(p.host)
	// Second level of tree is method.
	n = n.addChild(p.method)
	// Remaining levels are path.
	n.addSegments(p.segments, p, h)
}

// addSegments adds the given segments to the tree rooted at n.
// If there are no segments, then n is a leaf node that holds
// the given pattern and handler.
func (n *routingNode) addSegments(segs []segment, p *pattern, h Handler) {
	if len(segs) == 0 {
		n.set(p, h)
		return
	}
	seg := segs[0]
	if seg.multi {
		if len(segs) != 1 {
			panic("multi wildcard not last")
		}
		c := &routingNode{}
		n.multiChild = c
		c.set(p, h)
	} else if seg.wild {
		n.addChild("").addSegments(segs[1:], p, h)
	} else {
		n.addChild(seg.s).addSegments(segs[1:], p, h)
	}
}

// set sets the pattern and handler for n, which
// must be a leaf node.
func (n *routingNode) set(p *pattern, h Handler) {
	if n.pattern != nil || n.handler != nil {
		panic("non-nil leaf fields")
	}
	n.pattern = p
	n.handler = h
	if len(p.segments) > 0 {
		last := p.lastSegment()
		n.multiLeaf = last.multi
		n.namedMulti = last.multi && last.s != ""
	}
}

// addChild adds a child node with the given key to n
// if one does not exist, and returns the child.
func (n *routingNode) addChild(key string) *routingNode {
	if key == "" {
		if n.emptyChild == nil {
			n.emptyChild = &routingNode{}
		}
		return n.emptyChild
	}
	if c := n.findChild(key); c != nil {
		return c
	}
	c := &routingNode{}
	n.children.add(key, c)
	return c
}

// findChild returns the child of n with the given key, or nil
// if there is no child with that key.
func (n *routingNode) findChild(key string) *routingNode {
	if key == "" {
		return n.emptyChild
	}
	if len(n.children.s) == 0 && n.children.m == nil {
		return nil
	}
	r, _ := n.children.find(key)
	return r
}

type matchBuf struct {
	buf   [8]string
	extra []string
	n     int
}

func (mb *matchBuf) push(s string) int {
	mark := mb.n
	if mb.extra == nil {
		if mb.n < len(mb.buf) {
			mb.buf[mb.n] = s
			mb.n++
			return mark
		}
		mb.extra = make([]string, len(mb.buf), len(mb.buf)*2)
		copy(mb.extra, mb.buf[:])
	}
	mb.extra = append(mb.extra, s)
	mb.n++
	return mark
}

func (mb *matchBuf) rollback(mark int) {
	mb.n = mark
	if mb.extra != nil {
		mb.extra = mb.extra[:mark]
	}
}

func (mb *matchBuf) result() []string {
	if mb.n == 0 {
		return nil
	}
	res := make([]string, mb.n)
	if mb.extra != nil {
		copy(res, mb.extra)
	} else {
		copy(res, mb.buf[:mb.n])
	}
	return res
}

// match returns the leaf node under root that matches the arguments, and a list
// of values for pattern wildcards in the order that the wildcards appear.
// For example, if the request path is "/a/b/c" and the pattern is "/{x}/b/{y}",
// then the second return value will be []string{"a", "c"}.
func (root *routingNode) match(host, method, path string) (*routingNode, []string) {
	if host != "" && (len(root.children.s) > 0 || root.children.m != nil) {
		// There is a host. If there is a pattern that specifies that host and it
		// matches, we are done. If the pattern doesn't match, fall through to
		// try patterns with no host.
		if c := root.findChild(host); c != nil {
			if l, m := c.matchMethodAndPath(method, path); l != nil {
				return l, m
			}
		}
	}
	return root.emptyChild.matchMethodAndPath(method, path)
}

// matchTrailingSlash is like match, but matches as if path had a trailing slash.
// It avoids allocating path + "/" when checking for trailing slash redirection.
func (root *routingNode) matchTrailingSlash(host, method, path string) *routingNode {
	if host != "" && (len(root.children.s) > 0 || root.children.m != nil) {
		if c := root.findChild(host); c != nil {
			if l := c.matchMethodAndPathTrailingSlash(method, path); l != nil {
				return l
			}
		}
	}
	return root.emptyChild.matchMethodAndPathTrailingSlash(method, path)
}

func (n *routingNode) matchMethodAndPathTrailingSlash(method, path string) *routingNode {
	if n == nil {
		return nil
	}
	if len(n.children.s) > 0 || n.children.m != nil {
		if c := n.findChild(method); c != nil {
			if l := c.matchPathTrailingSlash(path); l != nil {
				return l
			}
		}
		if method == "HEAD" {
			if c := n.findChild("GET"); c != nil {
				if l := c.matchPathTrailingSlash(path); l != nil {
					return l
				}
			}
		}
	}
	if n.emptyChild == nil {
		return nil
	}
	return n.emptyChild.matchPathTrailingSlash(path)
}

func (n *routingNode) matchPathTrailingSlash(path string) *routingNode {
	if n == nil {
		return nil
	}
	if path == "" {
		// All path segments matched; now match the virtual trailing slash.
		if len(n.children.s) > 0 || n.children.m != nil {
			if c := n.findChild("/"); c != nil && c.pattern != nil {
				return c
			}
		}
		if c := n.multiChild; c != nil {
			return c
		}
		return nil
	}
	if len(n.children.s) > 0 || n.children.m != nil || n.emptyChild != nil {
		seg, rest := firstSegment(path)
		if len(n.children.s) > 0 || n.children.m != nil {
			if c := n.findChild(seg); c != nil {
				if leaf := c.matchPathTrailingSlash(rest); leaf != nil {
					return leaf
				}
			}
		}
		if seg != "/" && n.emptyChild != nil {
			if leaf := n.emptyChild.matchPathTrailingSlash(rest); leaf != nil {
				return leaf
			}
		}
	}
	return n.multiChild
}

// matchMethodAndPath matches the method and path.
// Its return values are the same as [routingNode.match].
// The receiver should be a child of the root.
func (n *routingNode) matchMethodAndPath(method, path string) (*routingNode, []string) {
	if n == nil {
		return nil, nil
	}
	if len(n.children.s) > 0 || n.children.m != nil {
		if c := n.findChild(method); c != nil {
			if st := c.static; st != nil {
				if l := st.lookup(path); l != nil {
					return l, nil
				}
				if !st.only || strings.IndexByte(path, '%') >= 0 {
					return n.matchMethodAndPathTree(method, path, c)
				}
			} else {
				return n.matchMethodAndPathTree(method, path, c)
			}
		}
		if method == "HEAD" {
			if c := n.findChild("GET"); c != nil {
				if st := c.static; st != nil {
					if l := st.lookup(path); l != nil {
						return l, nil
					}
					if !st.only || strings.IndexByte(path, '%') >= 0 {
						return n.matchMethodAndPathTree(method, path, nil)
					}
				} else {
					return n.matchMethodAndPathTree(method, path, nil)
				}
			}
		}
	}
	// No exact method match; try patterns with no method.
	ec := n.emptyChild
	if ec == nil {
		return nil, nil
	}
	if st := ec.static; st != nil {
		if l := st.lookup(path); l != nil {
			return l, nil
		}
		if st.only && strings.IndexByte(path, '%') < 0 {
			return nil, nil
		}
	}
	return matchPathWithBuf(ec, path)
}

func matchPathWithBuf(n *routingNode, path string) (*routingNode, []string) {
	var mb matchBuf
	if l := n.matchPath(path, &mb); l != nil {
		return l, mb.result()
	}
	return nil, nil
}

func (n *routingNode) matchMethodAndPathTree(method, path string, methodChild *routingNode) (*routingNode, []string) {
	var mb matchBuf
	if methodChild != nil {
		if l := methodChild.matchPath(path, &mb); l != nil {
			return l, mb.result()
		}
		mb.rollback(0)
	}
	if method == "HEAD" {
		// GET matches HEAD too.
		if c := n.findChild("GET"); c != nil {
			if st := c.static; st != nil {
				if l := st.lookup(path); l != nil {
					return l, nil
				}
				if st.only && strings.IndexByte(path, '%') < 0 {
					goto tryEmpty
				}
			}
			if l := c.matchPath(path, &mb); l != nil {
				return l, mb.result()
			}
			mb.rollback(0)
		}
	}
tryEmpty:
	// No exact match; try patterns with no method.
	ec := n.emptyChild
	if ec == nil {
		return nil, nil
	}
	if st := ec.static; st != nil {
		if l := st.lookup(path); l != nil {
			return l, nil
		}
		if st.only && strings.IndexByte(path, '%') < 0 {
			return nil, nil
		}
	}
	if l := ec.matchPath(path, &mb); l != nil {
		return l, mb.result()
	}
	return nil, nil
}

// matchPath matches a path.
// Its return values are the same as [routingNode.match].
// matchPath calls itself recursively when backtracking is possible.
// Matched wildcard values are recorded into mb.
func (n *routingNode) matchPath(path string, mb *matchBuf) *routingNode {
	if n == nil {
		return nil
	}
	// If path is empty, then we are done.
	// If n is a leaf node, we found a match; return it.
	// If n is an interior node (which means it has a nil pattern),
	// then we failed to match.
	if path == "" {
		if n.pattern == nil {
			return nil
		}
		return n
	}
	if len(n.children.s) > 0 || n.children.m != nil || n.emptyChild != nil {
		if path == "/" {
			if len(n.children.s) > 0 || n.children.m != nil {
				if c := n.findChild("/"); c != nil {
					if l := c.matchPath("", mb); l != nil {
						return l
					}
				}
			}
		} else {
			// Get the first segment of path.
			seg, rest := firstSegment(path)
			// First try matching against patterns that have a literal for this position.
			// We know by construction that such patterns are more specific than those
			// with a wildcard at this position (they are either more specific, equivalent,
			// or overlap, and we ruled out the first two when the patterns were registered).
			if len(n.children.s) > 0 || n.children.m != nil {
				if c := n.findChild(seg); c != nil {
					if l := c.matchPath(rest, mb); l != nil {
						return l
					}
				}
			}
			// If matching a literal fails, try again with patterns that have a single
			// wildcard (represented by an empty string in the child mapping).
			// Again, by construction, patterns with a single wildcard must be more specific than
			// those with a multi wildcard.
			// We skip this step if the segment is a trailing slash, because single wildcards
			// don't match trailing slashes.
			if seg != "/" && n.emptyChild != nil {
				var mark int
				if mb != nil {
					mark = mb.push(seg)
				}
				if l := n.emptyChild.matchPath(rest, mb); l != nil {
					return l
				}
				if mb != nil {
					mb.rollback(mark)
				}
			}
		}
	}
	// Lastly, match the pattern (there can be at most one) that has a multi
	// wildcard in this position to the rest of the path.
	if c := n.multiChild; c != nil {
		// Don't record a match for a nameless wildcard (which arises from a
		// trailing slash in the pattern).
		if c.namedMulti && mb != nil {
			mb.push(pathUnescape(path[1:])) // remove initial slash
		}
		return c
	}
	return nil
}

// firstSegment splits path into its first segment, and the rest.
// The path must begin with "/".
// If path consists of only a slash, firstSegment returns ("/", "").
// The segment is returned unescaped, if possible.
func firstSegment(path string) (seg, rest string) {
	if path == "/" {
		return "/", ""
	}
	path = path[1:] // drop initial slash
	hasPercent := false
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c == '/' {
			if hasPercent {
				return pathUnescape(path[:i]), path[i:]
			}
			return path[:i], path[i:]
		}
		if c == '%' {
			hasPercent = true
		}
	}
	if hasPercent {
		return pathUnescape(path), ""
	}
	return path, ""
}

// matchingMethods adds to methodSet all the methods that would result in a
// match if passed to routingNode.match with the given host and path.
func (root *routingNode) matchingMethods(host, path string, methodSet map[string]bool) {
	if host != "" {
		root.findChild(host).matchingMethodsPath(path, methodSet)
	}
	root.emptyChild.matchingMethodsPath(path, methodSet)
	if methodSet["GET"] {
		methodSet["HEAD"] = true
	}
}

func (n *routingNode) matchingMethodsPath(path string, set map[string]bool) {
	if n == nil {
		return
	}
	n.children.eachPair(func(method string, c *routingNode) bool {
		if p := c.matchPath(path, nil); p != nil {
			set[method] = true
		}
		return true
	})
	// Don't look at the empty child. If there were an empty
	// child, it would match on any method, but we only
	// call this when we fail to match on a method.
}

func (root *routingNode) appendMatchingMethods(host, path string, trailingSlash bool, dst []string) []string {
	if host != "" {
		dst = root.findChild(host).appendMatchingMethodsPath(path, trailingSlash, dst)
	}
	dst = root.emptyChild.appendMatchingMethodsPath(path, trailingSlash, dst)
	return dst
}

func (n *routingNode) appendMatchingMethodsPath(path string, trailingSlash bool, dst []string) []string {
	if n == nil {
		return dst
	}
	if n.children.m != nil {
		for method, c := range n.children.m {
			var p *routingNode
			if trailingSlash {
				p = c.matchPathTrailingSlash(path)
			} else {
				p = c.matchPath(path, nil)
			}
			if p != nil && !slices.Contains(dst, method) {
				dst = append(dst, method)
			}
		}
	} else {
		for _, e := range n.children.s {
			var p *routingNode
			if trailingSlash {
				p = e.value.matchPathTrailingSlash(path)
			} else {
				p = e.value.matchPath(path, nil)
			}
			if p != nil && !slices.Contains(dst, e.key) {
				dst = append(dst, e.key)
			}
		}
	}
	return dst
}
