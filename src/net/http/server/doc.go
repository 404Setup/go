// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package server provides a high-performance, secure-by-default HTTP server
// framework built on top of [net/http].
//
// It wraps [http.Server] and provides structured routing with path parameters,
// route groups, composable error-returning middleware chains, pooled request
// contexts, bounded request decoding, response helpers, safe static file
// serving, and graceful lifecycle management.
//
// # Routing
//
// Routes are registered on a [*Server] or [*Group] using method helpers such as
// [Server.GET], [Server.POST], or [Server.Handle]. Patterns must start with "/"
// and support three segment forms:
//
//   - Static literal segments: "/api/v1/health"
//   - Single-segment parameters: "/users/{id}/posts/{postID}"
//   - Catch-all trailing wildcards: "/files/{filepath...}"
//
// Static segments take precedence over single-segment parameters, which take
// precedence over catch-all wildcards. Ambiguous or conflicting registrations
// panic at registration time.
//
// # Handlers and Middleware
//
// Both handlers and middleware share the [HandlerFunc] signature
// func(*Context) error. Middleware executes downstream handlers by calling
// [Context.Next]. Returning without calling [Context.Next] stops the chain
// immediately, preventing accidental fall-through in authentication or
// authorization middleware.
//
// Non-nil errors returned from handlers or middleware are processed by the
// server's [ErrorHandler]. By default, only safe public messages from
// [*HTTPError] values are sent to clients; opaque internal errors produce a
// generic 500 Internal Server Error response without leaking error details.
//
// # Context Lifetime
//
// [*Context] values are pooled and reused across requests. A [*Context] must
// not be retained or accessed after the handler returns. To pass request-scoped
// cancellation or values to background goroutines, use [Context.Context] or
// copy the needed values before returning.
//
// # Security Defaults
//
// Servers created with [New] or [DefaultConfig] apply defensive defaults:
//
//   - Non-zero ReadHeaderTimeout, IdleTimeout, and MaxHeaderBytes on the
//     underlying [http.Server].
//   - Bounded request body reading via Config.MaxBodyBytes (default 4 MiB) for
//     [Context.BindJSON], [Context.BindJSONStrict], [Context.ParseForm],
//     [Context.FormValue], [Context.ParseMultipartForm], [Context.FormFile],
//     and [Context.BodyBytes].
//   - [Context.ClientIP] and [Context.Scheme] ignore X-Forwarded-For,
//     X-Real-IP, and X-Forwarded-Proto unless the direct peer matches
//     Config.TrustedProxies.
//   - [Server.StaticDir] confines filesystem access using [os.OpenRoot] and
//     disables directory listing by default.
package server
