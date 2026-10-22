// Package boostcore connects package boost to the driver's connections
// without adding to the driver's public API. The driver fills in Hooks when
// it is initialised; boost calls them.
package boostcore

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
)

// Rows is a fully read result.
type Rows interface {
	Columns() []string
	Len() int
	// Null reports whether row's value in column col is NULL.
	Null(row, col int) bool
	// Scan stores row's value in column col into dest, which is a pointer
	// of a kind the driver scans directly (see package boost).
	Scan(row, col int, dest any) error
}

// Hooks are the driver operations boost uses. A connection is the driver's
// own; IsConn says whether a value from sql.Conn.Raw is one.
type Hooks struct {
	Open   func(dsn string) (any, error)
	IsConn func(dc any) bool
	// Reset readies a connection taken back out of a pool, as
	// database/sql's SessionResetter call does.
	Reset func(c any)
	Query func(ctx context.Context, c any, query string, args []any) (Rows, error)
	Exec  func(ctx context.Context, c any, query string, args []any) (sql.Result, error)
	Begin func(ctx context.Context, c any, opts *sql.TxOptions) (sqldriver.Tx, error)
	Close func(c any) error
}

// H is set by the driver package's init.
var H Hooks
