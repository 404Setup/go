// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/server"
)

func ExampleServer() {
	s := server.New()
	s.Use(server.Recover(), server.SecurityHeaders())

	api := s.Group("/api/v1")
	api.GET("/users/{id}", func(c *server.Context) error {
		return c.JSON(http.StatusOK, map[string]string{
			"id": c.Param("id"),
		})
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/users/42", nil))
	fmt.Println(rec.Code, rec.Body.String())
	// Output:
	// 200 {"id":"42"}
}
