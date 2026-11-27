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
// [Server.GET], [Server.POST], or [Server.Handle], and can be organized into
// prefixed or middleware-scoped groups with [Server.Group], [Server.Route], or
// [Server.With]. Patterns must start with "/" and support three segment forms:
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
//     [Context.FormFiles], [Context.BodyReader], and [Context.BodyBytes].
//   - [Context.ClientIP] and [Context.Scheme] ignore X-Forwarded-For,
//     X-Real-IP, and X-Forwarded-Proto unless the direct peer matches
//     Config.TrustedProxies.
//   - [AllowHosts] protects against Host header injection and DNS rebinding,
//     and [RateLimit]/[RateLimitWithConfig] provide bounded sharded token-bucket
//     rate limiting.
//   - [Server.StaticDir] confines filesystem access using [os.OpenRoot] and
//     disables directory listing by default; use [Context.DownloadFS] with a
//     confined [fs.FS] when serving user-selected downloads.
//
// # Streaming & Responses
//
// In addition to [Context.Text], [Context.HTML], [Context.Bytes], [Context.JSON],
// [Context.Stream], [Context.FileFS], and [Context.DownloadFS], handlers can
// stream Server-Sent Events via [Context.SSE] and [SSEWriter] with automatic
// compression bypass and field-injection protection.
//
// # Compression
//
// By default, [*Server] provides built-in gzip and deflate codecs and
// automatically decompresses incoming request bodies (bounded to protect against
// decompression bombs) and negotiates compressed responses per RFC 9110.
// Already-compressed responses and files are skipped automatically.
//
// Use [Server.SetCompressor], [Server.SetDecompressor],
// [Server.SetCompression], [Server.SetDecompression],
// [Server.SetCompressionOrder], [NewGzipDecompressor],
// [NewDeflateDecompressor], or [CompressionConfig] to customize, replace,
// or disable encodings (including zstd and brotli). Call
// [Context.DisableCompression] inside a handler or middleware to disable
// compression for a single response.
package server
