// Package boost runs queries on a musql database faster than database/sql
// does, with a database/sql-like API.
//
// database/sql spends about as long on a point read as the engine does: a
// pool round trip, a driver.Rows, and each value boxed into an interface and
// converted again in Scan. boost skips that work. Its connections keep their
// prepared statements, results come back whole, and values are copied
// straight into typed destinations.
//
//	db, err := boost.Open("app.musq")
//	rows, err := db.Query(ctx, "SELECT id, name FROM users WHERE age > ?", 18)
//	defer rows.Close()
//	for rows.Next(&id, &name) {
//		...
//	}
//	err = rows.Err()
//
// Open owns its connections; Wrap runs on an existing *sql.DB's pool, which
// costs a pool round trip per query but shares the pool with other code.
//
// A destination is a *int64, *int, *int32, *float64, *bool, *string,
// *[]byte, *time.Time (for a column the connection coerces to time), *any, or
// an sql.Scanner. Any other destination is an error rather than a
// conversion, and so is NULL into anything but *any, *[]byte or a Scanner
// such as sql.NullString. Each value is what database/sql scans into the same
// destination.
package boost

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"sync"

	_ "github.com/samyfodil/musql/driver" // fills in boostcore.H
	"github.com/samyfodil/musql/internal/boostcore"
)

// DB is a musql database. It is safe for concurrent use.
type DB struct {
	std  *sql.DB // set by Wrap
	pool *pool   // set by Open
}

// Open opens the database at dsn, which takes the driver's DSN form, with its
// own connections. By default it opens as many as are asked for at once and
// keeps two idle.
func Open(dsn string) (*DB, error) {
	c, err := boostcore.H.Open(dsn)
	if err != nil {
		return nil, err
	}
	p := &pool{dsn: dsn, maxIdle: 2, open: 1, idle: []any{c}}
	return &DB{pool: p}, nil
}

// Wrap runs queries on db's connections, which must be musql's.
// Exec runs through db itself.
func Wrap(db *sql.DB) (*DB, error) {
	sc, err := db.Conn(context.Background())
	if err != nil {
		return nil, err
	}
	defer sc.Close()
	if err := sc.Raw(func(dc any) error {
		if !boostcore.H.IsConn(dc) {
			return fmt.Errorf("boost: Wrap needs a musql *sql.DB, not one on %T", db.Driver())
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return &DB{std: db}, nil
}

// Close closes the connections Open opened; one in use closes when it is
// returned. A wrapped *sql.DB is left open.
func (db *DB) Close() error {
	if db.pool == nil {
		return nil
	}
	return db.pool.close()
}

// SetMaxOpenConns limits the open connections, as *sql.DB's method does; 0
// means no limit.
func (db *DB) SetMaxOpenConns(n int) {
	if db.std != nil {
		db.std.SetMaxOpenConns(n)
		return
	}
	db.pool.mu.Lock()
	db.pool.maxOpen = max(n, 0)
	db.pool.mu.Unlock()
}

// SetMaxIdleConns limits the idle connections kept for reuse, as *sql.DB's
// method does.
func (db *DB) SetMaxIdleConns(n int) {
	if db.std != nil {
		db.std.SetMaxIdleConns(n)
		return
	}
	db.pool.mu.Lock()
	db.pool.maxIdle = max(n, 0)
	idle := db.pool.trimIdle()
	db.pool.mu.Unlock()
	for _, c := range idle {
		boostcore.H.Close(c)
	}
}

// Query runs query and returns its rows, which hold a connection until they
// are exhausted or closed.
func (db *DB) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	if db.std != nil {
		sc, err := db.std.Conn(ctx)
		if err != nil {
			return nil, err
		}
		r, err := rawQuery(ctx, sc, query, args)
		if err != nil {
			sc.Close()
			return nil, err
		}
		return &Rows{r: r, pos: -1, sc: sc}, nil
	}
	c, err := db.pool.get(ctx)
	if err != nil {
		return nil, err
	}
	r, err := boostcore.H.Query(ctx, c, query, args)
	if err != nil {
		db.pool.put(c, isBadConn(err))
		return nil, err
	}
	return &Rows{r: r, pos: -1, pool: db.pool, c: c}, nil
}

// QueryRow runs query for its first row; Row.Scan reports sql.ErrNoRows
// when there is none.
func (db *DB) QueryRow(ctx context.Context, query string, args ...any) *Row {
	rows, err := db.Query(ctx, query, args...)
	return &Row{rows: rows, err: err}
}

// Exec runs a statement that returns no rows.
func (db *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if db.std != nil {
		return db.std.ExecContext(ctx, query, args...)
	}
	c, err := db.pool.get(ctx)
	if err != nil {
		return nil, err
	}
	res, err := boostcore.H.Exec(ctx, c, query, args)
	db.pool.put(c, isBadConn(err))
	return res, err
}

// Begin starts a transaction on one connection, held until Commit or
// Rollback. Unlike *sql.Tx, it is not rolled back when ctx ends.
func (db *DB) Begin(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	if db.std != nil {
		sc, err := db.std.Conn(ctx)
		if err != nil {
			return nil, err
		}
		stx, err := sc.BeginTx(ctx, opts)
		if err != nil {
			sc.Close()
			return nil, err
		}
		return &Tx{sc: sc, stx: stx}, nil
	}
	c, err := db.pool.get(ctx)
	if err != nil {
		return nil, err
	}
	dtx, err := boostcore.H.Begin(ctx, c, opts)
	if err != nil {
		db.pool.put(c, isBadConn(err))
		return nil, err
	}
	return &Tx{pool: db.pool, c: c, dtx: dtx}, nil
}

// rawQuery runs query on sc's driver connection.
func rawQuery(ctx context.Context, sc *sql.Conn, query string, args []any) (boostcore.Rows, error) {
	var r boostcore.Rows
	err := sc.Raw(func(dc any) error {
		if !boostcore.H.IsConn(dc) {
			return fmt.Errorf("boost: not a musql connection: %T", dc)
		}
		var err error
		r, err = boostcore.H.Query(ctx, dc, query, args)
		return err
	})
	return r, err
}

func isBadConn(err error) bool { return errors.Is(err, sqldriver.ErrBadConn) }

// pool holds Open's connections. A returned connection goes to the longest
// waiter first, then to the idle list.
type pool struct {
	dsn string

	mu      sync.Mutex
	idle    []any
	open    int // connections open or being opened
	maxOpen int
	maxIdle int
	waiters []chan handoff
	closed  bool
}

// handoff is what a waiter receives: a connection, or nil with permission to
// open one in a slot already counted, or an error.
type handoff struct {
	c   any
	err error
}

var errClosed = errors.New("boost: database is closed")

func (p *pool) get(ctx context.Context) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errClosed
	}
	if n := len(p.idle); n > 0 {
		c := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		boostcore.H.Reset(c)
		return c, nil
	}
	if p.maxOpen == 0 || p.open < p.maxOpen {
		p.open++
		p.mu.Unlock()
		return p.dial()
	}
	ch := make(chan handoff, 1)
	p.waiters = append(p.waiters, ch)
	p.mu.Unlock()
	select {
	case h := <-ch:
		return p.take(h)
	case <-ctx.Done():
		p.mu.Lock()
		for i, w := range p.waiters {
			if w == ch {
				p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
				p.mu.Unlock()
				return nil, ctx.Err()
			}
		}
		p.mu.Unlock()
		// Handed something after all: pass it on.
		if c, err := p.take(<-ch); err == nil {
			p.put(c, false)
		}
		return nil, ctx.Err()
	}
}

func (p *pool) take(h handoff) (any, error) {
	if h.err != nil {
		return nil, h.err
	}
	if h.c == nil {
		return p.dial()
	}
	boostcore.H.Reset(h.c)
	return h.c, nil
}

// dial opens a connection in a slot already counted in p.open.
func (p *pool) dial() (any, error) {
	c, err := boostcore.H.Open(p.dsn)
	if err != nil {
		p.release()
		return nil, err
	}
	return c, nil
}

// release gives up a counted slot, passing it to a waiter if there is one.
func (p *pool) release() {
	p.mu.Lock()
	if len(p.waiters) > 0 && !p.closed {
		ch := p.waiters[0]
		p.waiters = p.waiters[1:]
		p.mu.Unlock()
		ch <- handoff{}
		return
	}
	p.open--
	p.mu.Unlock()
}

// put returns c; a bad or surplus connection is closed.
func (p *pool) put(c any, bad bool) {
	if bad {
		boostcore.H.Close(c)
		p.release()
		return
	}
	p.mu.Lock()
	if p.closed {
		p.open--
		p.mu.Unlock()
		boostcore.H.Close(c)
		return
	}
	if len(p.waiters) > 0 {
		ch := p.waiters[0]
		p.waiters = p.waiters[1:]
		p.mu.Unlock()
		ch <- handoff{c: c}
		return
	}
	if len(p.idle) < p.maxIdle {
		p.idle = append(p.idle, c)
		p.mu.Unlock()
		return
	}
	p.open--
	p.mu.Unlock()
	boostcore.H.Close(c)
}

// trimIdle drops idle connections over maxIdle and returns them to close.
// p.mu is held.
func (p *pool) trimIdle() []any {
	if len(p.idle) <= p.maxIdle {
		return nil
	}
	extra := p.idle[p.maxIdle:]
	p.idle = p.idle[:p.maxIdle:p.maxIdle]
	p.open -= len(extra)
	return extra
}

func (p *pool) close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	idle, waiters := p.idle, p.waiters
	p.idle, p.waiters = nil, nil
	p.open -= len(idle)
	p.mu.Unlock()
	for _, ch := range waiters {
		ch <- handoff{err: errClosed}
	}
	var first error
	for _, c := range idle {
		if err := boostcore.H.Close(c); err != nil && first == nil {
			first = err
		}
	}
	return first
}
