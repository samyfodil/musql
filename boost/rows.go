package boost

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/samyfodil/musql/internal/boostcore"
)

// Rows is a query's result. It holds a connection until Next reports false
// or Close is called.
type Rows struct {
	r   boostcore.Rows
	pos int
	err error

	// The connection to give back: a pooled one, or a wrapped *sql.DB's.
	// Neither is set for rows read inside a transaction.
	pool *pool
	c    any
	sc   *sql.Conn
}

// Next moves to the next row and, given destinations, scans it into them. It
// reports false when there are no more rows or a scan failed, which Err tells
// apart, and then releases the connection.
func (rs *Rows) Next(dest ...any) bool {
	if rs.r == nil {
		return false
	}
	rs.pos++
	if rs.pos >= rs.r.Len() {
		rs.Close()
		return false
	}
	if len(dest) > 0 {
		if err := rs.Scan(dest...); err != nil {
			rs.err = err
			rs.Close()
			return false
		}
	}
	return true
}

// Scan scans the current row into dest, one destination per column.
func (rs *Rows) Scan(dest ...any) error {
	if rs.r == nil || rs.pos < 0 || rs.pos >= rs.r.Len() {
		return errors.New("sql: Scan called without calling Next")
	}
	if n := len(rs.r.Columns()); len(dest) != n {
		return fmt.Errorf("sql: expected %d destination arguments in Scan, not %d", n, len(dest))
	}
	for i, d := range dest {
		if err := rs.r.Scan(rs.pos, i, d); err != nil {
			return fmt.Errorf("sql: Scan error on column index %d, name %q: %w", i, rs.r.Columns()[i], err)
		}
	}
	return nil
}

// Columns returns the result's column names.
func (rs *Rows) Columns() []string {
	if rs.r == nil {
		return nil
	}
	return rs.r.Columns()
}

// Err returns the error that ended Next early, if any.
func (rs *Rows) Err() error { return rs.err }

// Close releases the connection. It is safe to call more than once.
func (rs *Rows) Close() error {
	rs.r = nil
	switch {
	case rs.pool != nil:
		rs.pool.put(rs.c, false)
		rs.pool, rs.c = nil, nil
	case rs.sc != nil:
		err := rs.sc.Close()
		rs.sc = nil
		if !errors.Is(err, sql.ErrConnDone) {
			return err
		}
	}
	return nil
}

// Row is QueryRow's result.
type Row struct {
	rows *Rows
	err  error
}

// Scan scans the first row into dest, or reports sql.ErrNoRows.
func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer r.rows.Close()
	if !r.rows.Next(dest...) {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	return nil
}

// Err returns the error running the query, if any.
func (r *Row) Err() error { return r.err }

// Tx is a transaction on one connection.
type Tx struct {
	// Open's connection and the driver's transaction on it, or a wrapped
	// *sql.DB's connection and its transaction.
	pool *pool
	c    any
	dtx  interface {
		Commit() error
		Rollback() error
	}
	sc  *sql.Conn
	stx *sql.Tx

	done bool
}

// Query runs query inside the transaction.
func (tx *Tx) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	if tx.done {
		return nil, sql.ErrTxDone
	}
	var (
		r   boostcore.Rows
		err error
	)
	if tx.sc != nil {
		r, err = rawQuery(ctx, tx.sc, query, args)
	} else {
		r, err = boostcore.H.Query(ctx, tx.c, query, args)
	}
	if err != nil {
		return nil, err
	}
	return &Rows{r: r, pos: -1}, nil
}

// QueryRow runs query inside the transaction for its first row.
func (tx *Tx) QueryRow(ctx context.Context, query string, args ...any) *Row {
	rows, err := tx.Query(ctx, query, args...)
	return &Row{rows: rows, err: err}
}

// Exec runs a statement that returns no rows inside the transaction.
func (tx *Tx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if tx.done {
		return nil, sql.ErrTxDone
	}
	if tx.stx != nil {
		return tx.stx.ExecContext(ctx, query, args...)
	}
	return boostcore.H.Exec(ctx, tx.c, query, args)
}

// Commit commits the transaction and releases its connection.
func (tx *Tx) Commit() error {
	return tx.end(true)
}

// Rollback rolls the transaction back and releases its connection.
func (tx *Tx) Rollback() error {
	return tx.end(false)
}

func (tx *Tx) end(commit bool) error {
	if tx.done {
		return sql.ErrTxDone
	}
	tx.done = true
	if tx.stx != nil {
		var err error
		if commit {
			err = tx.stx.Commit()
		} else {
			err = tx.stx.Rollback()
		}
		if cerr := tx.sc.Close(); err == nil && !errors.Is(cerr, sql.ErrConnDone) {
			err = cerr
		}
		return err
	}
	var err error
	if commit {
		err = tx.dtx.Commit()
	} else {
		err = tx.dtx.Rollback()
	}
	tx.pool.put(tx.c, isBadConn(err))
	return err
}
