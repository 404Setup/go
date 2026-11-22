// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const (
	methodGET = iota
	methodHEAD
	methodPOST
	methodPUT
	methodPATCH
	methodDELETE
	methodOPTIONS
	methodCONNECT
	methodTRACE
	numStdMethods
)

var stdMethodNames = [numStdMethods]string{
	methodGET:     http.MethodGet,
	methodHEAD:    http.MethodHead,
	methodPOST:    http.MethodPost,
	methodPUT:     http.MethodPut,
	methodPATCH:   http.MethodPatch,
	methodDELETE:  http.MethodDelete,
	methodOPTIONS: http.MethodOptions,
	methodCONNECT: http.MethodConnect,
	methodTRACE:   http.MethodTrace,
}

func methodIndex(m string) int {
	switch len(m) {
	case 3:
		if m == http.MethodGet {
			return methodGET
		}
		if m == http.MethodPut {
			return methodPUT
		}
	case 4:
		if m == http.MethodPost {
			return methodPOST
		}
		if m == http.MethodHead {
			return methodHEAD
		}
	case 5:
		if m == http.MethodPatch {
			return methodPATCH
		}
		if m == http.MethodTrace {
			return methodTRACE
		}
	case 6:
		if m == http.MethodDelete {
			return methodDELETE
		}
	case 7:
		if m == http.MethodOptions {
			return methodOPTIONS
		}
		if m == http.MethodConnect {
			return methodCONNECT
		}
	}
	return -1
}

func isValidHTTPMethod(m string) bool {
	if len(m) == 0 {
		return false
	}
	for i := 0; i < len(m); i++ {
		b := m[i]
		// RFC 9110 tchar
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') {
			continue
		}
		switch b {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

// RouteInfo describes a registered route.
type RouteInfo struct {
	Method  string
	Pattern string
}

type segKind uint8

const (
	segStatic segKind = iota
	segParam
	segWildcard
)

type patternSegment struct {
	kind    segKind
	literal string // static literal or parameter name
}

type parsedPattern struct {
	raw           string
	segments      []patternSegment
	paramNames    []string
	trailingSlash bool
	isStatic      bool
}

func isValidParamName(name string) bool {
	if len(name) == 0 {
		return false
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' {
			continue
		}
		if i > 0 && ((b >= '0' && b <= '9') || b == '-') {
			continue
		}
		return false
	}
	return true
}

func parsePattern(raw string) (parsedPattern, error) {
	if len(raw) == 0 || raw[0] != '/' {
		return parsedPattern{}, fmt.Errorf("route pattern %q must start with '/'", raw)
	}
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		if b < 0x20 || b == 0x7f {
			return parsedPattern{}, fmt.Errorf("route pattern %q contains control character", raw)
		}
	}
	if !isCleanPath(raw) {
		return parsedPattern{}, fmt.Errorf("route pattern %q is not a clean canonical path", raw)
	}

	p := parsedPattern{
		raw:      raw,
		isStatic: true,
	}
	if raw == "/" {
		p.trailingSlash = true
		return p, nil
	}

	body := raw[1:]
	if body[len(body)-1] == '/' {
		p.trailingSlash = true
		body = body[:len(body)-1]
	}

	parts := strings.Split(body, "/")
	p.segments = make([]patternSegment, 0, len(parts))
	for i, part := range parts {
		if strings.IndexByte(part, '{') >= 0 || strings.IndexByte(part, '}') >= 0 {
			if len(part) < 3 || part[0] != '{' || part[len(part)-1] != '}' {
				return parsedPattern{}, fmt.Errorf("route pattern %q has malformed parameter segment %q", raw, part)
			}
			inner := part[1 : len(part)-1]
			if strings.IndexByte(inner, '{') >= 0 || strings.IndexByte(inner, '}') >= 0 {
				return parsedPattern{}, fmt.Errorf("route pattern %q has nested braces in segment %q", raw, part)
			}
			p.isStatic = false
			if strings.HasSuffix(inner, "...") {
				if i != len(parts)-1 || p.trailingSlash {
					return parsedPattern{}, fmt.Errorf("route pattern %q catch-all %q must be the final segment without trailing slash", raw, part)
				}
				name := inner[:len(inner)-3]
				if !isValidParamName(name) {
					return parsedPattern{}, fmt.Errorf("route pattern %q has invalid catch-all parameter name %q", raw, name)
				}
				if slices.Contains(p.paramNames, name) {
					return parsedPattern{}, fmt.Errorf("route pattern %q has duplicate parameter name %q", raw, name)
				}
				p.paramNames = append(p.paramNames, name)
				p.segments = append(p.segments, patternSegment{kind: segWildcard, literal: name})
				continue
			}
			if !isValidParamName(inner) {
				return parsedPattern{}, fmt.Errorf("route pattern %q has invalid parameter name %q", raw, inner)
			}
			if slices.Contains(p.paramNames, inner) {
				return parsedPattern{}, fmt.Errorf("route pattern %q has duplicate parameter name %q", raw, inner)
			}
			p.paramNames = append(p.paramNames, inner)
			p.segments = append(p.segments, patternSegment{kind: segParam, literal: inner})
			continue
		}
		literal := part
		if strings.IndexByte(part, '%') >= 0 {
			u, err := url.PathUnescape(part)
			if err != nil {
				return parsedPattern{}, fmt.Errorf("route pattern %q has invalid percent-encoding in segment %q: %w", raw, part, err)
			}
			literal = u
			p.isStatic = false
		}
		p.segments = append(p.segments, patternSegment{kind: segStatic, literal: literal})
	}
	return p, nil
}

type routeEntry struct {
	method     string
	pattern    string
	paramNames []string
	chain      []HandlerFunc
}

type staticTable struct {
	singleKey string
	singleVal *routeEntry
	m         map[string]*routeEntry
	lenMask   uint64
	minLen    int
	maxLen    int
	count     int
}

func (st *staticTable) add(path string, entry *routeEntry) {
	l := len(path)
	if st.count == 0 {
		st.singleKey = path
		st.singleVal = entry
		st.minLen = l
		st.maxLen = l
		st.lenMask = 1 << (uint(l) & 63)
		st.count = 1
		return
	}
	if st.m == nil {
		st.m = make(map[string]*routeEntry, 4)
		st.m[st.singleKey] = st.singleVal
		st.singleKey = ""
		st.singleVal = nil
	}
	if l < st.minLen {
		st.minLen = l
	}
	if l > st.maxLen {
		st.maxLen = l
	}
	st.lenMask |= 1 << (uint(l) & 63)
	st.m[path] = entry
	st.count++
}

func (st *staticTable) lookup(path string) *routeEntry {
	if st.count == 0 {
		return nil
	}
	l := len(path)
	if l < st.minLen || l > st.maxLen || (st.lenMask&(1<<(uint(l)&63))) == 0 {
		return nil
	}
	if st.m != nil {
		return st.m[path]
	}
	if path == st.singleKey {
		return st.singleVal
	}
	return nil
}

func (st *staticTable) clone() staticTable {
	c := staticTable{
		singleKey: st.singleKey,
		singleVal: st.singleVal,
		lenMask:   st.lenMask,
		minLen:    st.minLen,
		maxLen:    st.maxLen,
		count:     st.count,
	}
	if st.m != nil {
		c.m = make(map[string]*routeEntry, len(st.m))
		for k, v := range st.m {
			c.m[k] = v
		}
	}
	return c
}

const maxLinearChildren = 8

type childEntry struct {
	seg  string
	node *treeNode
}

type treeNode struct {
	leaf          *routeEntry
	staticSlice   []childEntry
	staticMap     map[string]*treeNode
	slashChild    *treeNode
	paramChild    *treeNode
	wildcardChild *treeNode
}

func (n *treeNode) clone() *treeNode {
	if n == nil {
		return nil
	}
	c := &treeNode{
		leaf:          n.leaf,
		slashChild:    n.slashChild.clone(),
		paramChild:    n.paramChild.clone(),
		wildcardChild: n.wildcardChild.clone(),
	}
	if len(n.staticSlice) > 0 {
		c.staticSlice = make([]childEntry, len(n.staticSlice))
		for i, e := range n.staticSlice {
			c.staticSlice[i] = childEntry{seg: e.seg, node: e.node.clone()}
		}
	} else if n.staticMap != nil {
		c.staticMap = make(map[string]*treeNode, len(n.staticMap))
		for k, v := range n.staticMap {
			c.staticMap[k] = v.clone()
		}
	}
	return c
}

func (n *treeNode) findStatic(seg string) *treeNode {
	if n.staticMap != nil {
		return n.staticMap[seg]
	}
	for i := range n.staticSlice {
		if n.staticSlice[i].seg == seg {
			return n.staticSlice[i].node
		}
	}
	return nil
}

func (n *treeNode) addStatic(seg string) *treeNode {
	if existing := n.findStatic(seg); existing != nil {
		return existing
	}
	child := &treeNode{}
	if n.staticMap != nil {
		n.staticMap[seg] = child
		return child
	}
	if len(n.staticSlice) < maxLinearChildren {
		n.staticSlice = append(n.staticSlice, childEntry{seg: seg, node: child})
		return child
	}
	m := make(map[string]*treeNode, len(n.staticSlice)+1)
	for _, e := range n.staticSlice {
		m[e.seg] = e.node
	}
	m[seg] = child
	n.staticSlice = nil
	n.staticMap = m
	return child
}

func (n *treeNode) insert(p parsedPattern, entry *routeEntry) error {
	cur := n
	for _, seg := range p.segments {
		switch seg.kind {
		case segStatic:
			cur = cur.addStatic(seg.literal)
		case segParam:
			if cur.paramChild == nil {
				cur.paramChild = &treeNode{}
			}
			cur = cur.paramChild
		case segWildcard:
			if cur.wildcardChild == nil {
				cur.wildcardChild = &treeNode{}
			}
			cur = cur.wildcardChild
		}
	}
	if p.trailingSlash {
		if cur.slashChild == nil {
			cur.slashChild = &treeNode{}
		}
		cur = cur.slashChild
	}
	if cur.leaf != nil {
		return fmt.Errorf("route %s %q conflicts with existing route %q", entry.method, p.raw, cur.leaf.pattern)
	}
	cur.leaf = entry
	return nil
}

type paramCapture struct {
	buf   *[maxInlineParams]string
	extra *[]string
	n     int
}

func (pc *paramCapture) push(val string) int {
	mark := pc.n
	if pc.buf != nil {
		if pc.n < maxInlineParams {
			pc.buf[pc.n] = val
		} else {
			if len(*pc.extra) == 0 {
				*pc.extra = make([]string, maxInlineParams, maxInlineParams*2)
				copy(*pc.extra, pc.buf[:])
			}
			*pc.extra = append(*pc.extra, val)
		}
	}
	pc.n++
	return mark
}

func (pc *paramCapture) rollback(mark int) {
	pc.n = mark
	if pc.extra != nil && len(*pc.extra) > mark {
		*pc.extra = (*pc.extra)[:mark]
	}
}

// nextSegment splits path (which must begin with '/') into its first segment
// and the remaining path (either "" or starting with '/').
// If escaped is true, percent-encoded sequences in the segment are unescaped.
func nextSegment(path string, escaped bool) (seg, rest string, ok bool) {
	s := path[1:]
	slash := strings.IndexByte(s, '/')
	if slash >= 0 {
		seg = s[:slash]
		rest = s[slash:]
	} else {
		seg = s
		rest = ""
	}
	if escaped && strings.IndexByte(seg, '%') >= 0 {
		u, err := url.PathUnescape(seg)
		if err != nil {
			return "", "", false
		}
		seg = u
	}
	return seg, rest, true
}

func (n *treeNode) match(path string, escaped bool, pc *paramCapture) *routeEntry {
	if path == "" {
		return n.leaf
	}
	if path == "/" {
		if n.slashChild != nil && n.slashChild.leaf != nil {
			return n.slashChild.leaf
		}
		if n.wildcardChild != nil && n.wildcardChild.leaf != nil {
			if pc != nil {
				pc.push("")
			}
			return n.wildcardChild.leaf
		}
		return nil
	}

	if len(n.staticSlice) > 0 || n.staticMap != nil || n.paramChild != nil {
		seg, rest, ok := nextSegment(path, escaped)
		if !ok {
			return nil
		}
		if len(n.staticSlice) > 0 || n.staticMap != nil {
			if child := n.findStatic(seg); child != nil {
				if e := child.match(rest, escaped, pc); e != nil {
					return e
				}
			}
		}
		if n.paramChild != nil && seg != "" {
			var mark int
			if pc != nil {
				mark = pc.push(seg)
			}
			if e := n.paramChild.match(rest, escaped, pc); e != nil {
				return e
			}
			if pc != nil {
				pc.rollback(mark)
			}
		}
	}

	if n.wildcardChild != nil && n.wildcardChild.leaf != nil {
		rem := path[1:]
		if escaped && strings.IndexByte(rem, '%') >= 0 {
			u, err := url.PathUnescape(rem)
			if err != nil {
				return nil
			}
			rem = u
		}
		if pc != nil {
			pc.push(rem)
		}
		return n.wildcardChild.leaf
	}
	return nil
}

type methodRouter struct {
	static     staticTable
	tree       *treeNode
	dynTree    *treeNode
	onlyStatic bool
}

func newMethodRouter() *methodRouter {
	return &methodRouter{
		tree:       &treeNode{},
		onlyStatic: true,
	}
}

func (mr *methodRouter) clone() *methodRouter {
	if mr == nil {
		return nil
	}
	return &methodRouter{
		static:     mr.static.clone(),
		tree:       mr.tree.clone(),
		dynTree:    mr.dynTree.clone(),
		onlyStatic: mr.onlyStatic,
	}
}

func (mr *methodRouter) add(p parsedPattern, entry *routeEntry) error {
	if err := mr.tree.insert(p, entry); err != nil {
		return err
	}
	if p.isStatic {
		mr.static.add(p.raw, entry)
	} else {
		if mr.dynTree == nil {
			mr.dynTree = &treeNode{}
		}
		if err := mr.dynTree.insert(p, entry); err != nil {
			return err
		}
		mr.onlyStatic = false
	}
	return nil
}

func (mr *methodRouter) match(path string, escaped bool, pc *paramCapture) *routeEntry {
	if mr == nil {
		return nil
	}
	if !escaped {
		if e := mr.static.lookup(path); e != nil {
			return e
		}
		if mr.onlyStatic {
			return nil
		}
		return mr.dynTree.match(path, false, pc)
	}
	return mr.tree.match(path, true, pc)
}

func (mr *methodRouter) matchDynamic(path string, pc *paramCapture) *routeEntry {
	if mr == nil || mr.onlyStatic {
		return nil
	}
	return mr.dynTree.match(path, false, pc)
}

type routerSnapshot struct {
	methods          [numStdMethods]*methodRouter
	custom           map[string]*methodRouter
	anyMethod        *methodRouter
	notFoundChain    []HandlerFunc
	methodNotAllowed []HandlerFunc
	routes           []RouteInfo
}

func (rs *routerSnapshot) clone() *routerSnapshot {
	if rs == nil {
		return &routerSnapshot{}
	}
	c := &routerSnapshot{
		anyMethod:        rs.anyMethod.clone(),
		notFoundChain:    slices.Clone(rs.notFoundChain),
		methodNotAllowed: slices.Clone(rs.methodNotAllowed),
		routes:           slices.Clone(rs.routes),
	}
	for i := range rs.methods {
		c.methods[i] = rs.methods[i].clone()
	}
	if rs.custom != nil {
		c.custom = make(map[string]*methodRouter, len(rs.custom))
		for k, v := range rs.custom {
			c.custom[k] = v.clone()
		}
	}
	return c
}

func (rs *routerSnapshot) getMethodRouter(method string) *methodRouter {
	if method == "" {
		return rs.anyMethod
	}
	if idx := methodIndex(method); idx >= 0 {
		return rs.methods[idx]
	}
	if rs.custom != nil {
		return rs.custom[method]
	}
	return nil
}

func (rs *routerSnapshot) getOrCreateMethodRouter(method string) *methodRouter {
	if method == "" {
		if rs.anyMethod == nil {
			rs.anyMethod = newMethodRouter()
		}
		return rs.anyMethod
	}
	if idx := methodIndex(method); idx >= 0 {
		if rs.methods[idx] == nil {
			rs.methods[idx] = newMethodRouter()
		}
		return rs.methods[idx]
	}
	if rs.custom == nil {
		rs.custom = make(map[string]*methodRouter)
	}
	mr := rs.custom[method]
	if mr == nil {
		mr = newMethodRouter()
		rs.custom[method] = mr
	}
	return mr
}

func (rs *routerSnapshot) matchStatic(method, path string) *routeEntry {
	if idx := methodIndex(method); idx >= 0 {
		mr := rs.methods[idx]
		if mr != nil {
			if e := mr.static.lookup(path); e != nil {
				return e
			}
		}
		// Only fall back to GET static routes if no dynamic HEAD routes could
		// take precedence over GET.
		if idx == methodHEAD && (mr == nil || mr.onlyStatic) {
			if gmr := rs.methods[methodGET]; gmr != nil {
				if e := gmr.static.lookup(path); e != nil {
					return e
				}
			}
		}
	} else if rs.custom != nil {
		if mr := rs.custom[method]; mr != nil {
			if e := mr.static.lookup(path); e != nil {
				return e
			}
		}
	}
	return nil
}

// matchAfterStatic matches a clean, unescaped path after matchStatic has
// already returned nil. It avoids redundant staticTable lookups on the method
// router.
func (rs *routerSnapshot) matchAfterStatic(method, path string, pc *paramCapture) *routeEntry {
	if idx := methodIndex(method); idx >= 0 {
		if mr := rs.methods[idx]; mr != nil {
			if e := mr.matchDynamic(path, pc); e != nil {
				return e
			}
			if pc != nil {
				pc.rollback(0)
			}
		}
		if idx == methodHEAD {
			if gmr := rs.methods[methodGET]; gmr != nil {
				if e := gmr.match(path, false, pc); e != nil {
					return e
				}
				if pc != nil {
					pc.rollback(0)
				}
			}
		}
	} else if rs.custom != nil {
		if mr := rs.custom[method]; mr != nil {
			if e := mr.matchDynamic(path, pc); e != nil {
				return e
			}
			if pc != nil {
				pc.rollback(0)
			}
		}
	}
	if rs.anyMethod != nil {
		if e := rs.anyMethod.match(path, false, pc); e != nil {
			return e
		}
	}
	return nil
}

func (rs *routerSnapshot) match(method, path string, escaped bool, pc *paramCapture) *routeEntry {
	if idx := methodIndex(method); idx >= 0 {
		if mr := rs.methods[idx]; mr != nil {
			if e := mr.match(path, escaped, pc); e != nil {
				return e
			}
			if pc != nil {
				pc.rollback(0)
			}
		}
		// RFC 9110: HEAD falls back to GET if no explicit HEAD route matched.
		if idx == methodHEAD {
			if mr := rs.methods[methodGET]; mr != nil {
				if e := mr.match(path, escaped, pc); e != nil {
					return e
				}
				if pc != nil {
					pc.rollback(0)
				}
			}
		}
	} else if rs.custom != nil {
		if mr := rs.custom[method]; mr != nil {
			if e := mr.match(path, escaped, pc); e != nil {
				return e
			}
			if pc != nil {
				pc.rollback(0)
			}
		}
	}
	if rs.anyMethod != nil {
		if e := rs.anyMethod.match(path, escaped, pc); e != nil {
			return e
		}
	}
	return nil
}

// allowedMethods appends all HTTP methods that match path into dst, sorted and
// deduplicated.
func (rs *routerSnapshot) allowedMethods(path string, escaped bool, dst []string) []string {
	hasGET := false
	hasHEAD := false
	for i := range rs.methods {
		mr := rs.methods[i]
		if mr == nil {
			continue
		}
		if mr.match(path, escaped, nil) != nil {
			name := stdMethodNames[i]
			dst = append(dst, name)
			if i == methodGET {
				hasGET = true
			} else if i == methodHEAD {
				hasHEAD = true
			}
		}
	}
	if hasGET && !hasHEAD {
		dst = append(dst, http.MethodHead)
	}
	for m, mr := range rs.custom {
		if mr.match(path, escaped, nil) != nil {
			dst = append(dst, m)
		}
	}
	if len(dst) > 1 {
		slices.Sort(dst)
	}
	return dst
}
