// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
)

type legacyDriver struct{}

func (legacyDriver) Open(string) (driver.Conn, error) { return legacyConn{}, nil }

type contextDriver struct{}

func (contextDriver) Open(string) (driver.Conn, error)               { panic("use connector") }
func (contextDriver) OpenConnector(string) (driver.Connector, error) { return connector{}, nil }

type connector struct{}

func (connector) Connect(context.Context) (driver.Conn, error) { return connection{}, nil }
func (connector) Driver() driver.Driver                        { return contextDriver{} }

type legacyConn struct{}

func (legacyConn) Prepare(string) (driver.Stmt, error) { return statement{}, nil }
func (legacyConn) Close() error                        { return nil }
func (legacyConn) Begin() (driver.Tx, error)           { return transaction{}, nil }

type connection struct{ legacyConn }

func (connection) Ping(context.Context) error { return nil }
func (connection) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &rows{}, nil
}
func (connection) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}
func (connection) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return transaction{}, nil
}
func (connection) CheckNamedValue(*driver.NamedValue) error { return nil }

type statement struct{}

func (statement) Close() error                               { return nil }
func (statement) NumInput() int                              { return -1 }
func (statement) Exec([]driver.Value) (driver.Result, error) { return driver.RowsAffected(1), nil }
func (statement) Query([]driver.Value) (driver.Rows, error)  { return &rows{}, nil }

type transaction struct{}

func (transaction) Commit() error   { return nil }
func (transaction) Rollback() error { return nil }

type rows struct{ done bool }

func (*rows) Columns() []string { return []string{"value"} }
func (*rows) Close() error      { return nil }
func (r *rows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(42)
	return nil
}

func init() {
	sql.Register("o3-legacy", legacyDriver{})
	sql.Register("o3-context", contextDriver{})
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
func main() {
	ctx := context.Background()
	drivers := sql.Drivers()
	if len(drivers) != 2 {
		panic("driver registration")
	}
	for _, name := range drivers {
		db, err := sql.Open(name, "")
		check(err)
		check(db.PingContext(ctx))
		var value int
		check(db.QueryRowContext(ctx, "select value", 1).Scan(&value))
		if value != 42 {
			panic("query result")
		}
		result, err := db.ExecContext(ctx, "update value", 1)
		check(err)
		n, err := result.RowsAffected()
		check(err)
		if n != 1 {
			panic("exec result")
		}
		stmt, err := db.PrepareContext(ctx, "select value")
		check(err)
		check(stmt.QueryRowContext(ctx).Scan(&value))
		check(stmt.Close())
		tx, err := db.BeginTx(ctx, nil)
		check(err)
		check(tx.Commit())
		tx, err = db.Begin()
		check(err)
		check(tx.Rollback())
		check(db.Close())
	}
	println("ok")
}
