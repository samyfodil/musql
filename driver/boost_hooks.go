package driver

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"math"
	"time"
	"unsafe"

	"github.com/samyfodil/musql/engine"
	"github.com/samyfodil/musql/internal/boostcore"
)

// The driver's half of package boost: its connections run queries through a
// per-connection statement cache and hand back their rows whole, skipping
// database/sql's per-query work.
func init() {
	boostcore.H = boostcore.Hooks{
		Open: func(dsn string) (any, error) {
			dc, err := (&Driver{}).Open(dsn)
			if err != nil {
				return nil, err
			}
			return dc, nil
		},
		IsConn: func(dc any) bool {
			_, ok := dc.(*Conn)
			return ok
		},
		Reset: func(c any) {
			c.(*Conn).ResetSession(context.Background())
		},
		Query: func(ctx context.Context, c any, query string, args []any) (boostcore.Rows, error) {
			r, err := c.(*Conn).boostQuery(ctx, query, args)
			if err != nil {
				return nil, err
			}
			return r, nil
		},
		Exec: func(ctx context.Context, c any, query string, args []any) (sql.Result, error) {
			return c.(*Conn).boostExec(ctx, query, args)
		},
		Begin: func(ctx context.Context, c any, opts *sql.TxOptions) (sqldriver.Tx, error) {
			var o sqldriver.TxOptions
			if opts != nil {
				o = sqldriver.TxOptions{Isolation: sqldriver.IsolationLevel(opts.Isolation), ReadOnly: opts.ReadOnly}
			}
			return c.(*Conn).BeginTx(ctx, o)
		},
		Close: func(c any) error { return c.(*Conn).Close() },
	}
}

// maxBoostStmts bounds the statements cached per connection; past it the
// cache starts over.
const maxBoostStmts = 256

// boostStmt returns the statement prepared on c for query, preparing it once.
func (c *Conn) boostStmt(query string) (*Stmt, error) {
	if st := c.boostStmts[query]; st != nil {
		return st, nil
	}
	st := new(Stmt)
	if err := c.prepareInto(query, st); err != nil {
		return nil, parityErr(err)
	}
	if c.boostStmts == nil || len(c.boostStmts) >= maxBoostStmts {
		c.boostStmts = map[string]*Stmt{}
	}
	c.boostStmts[query] = st
	return st, nil
}

// boostArgs converts args as database/sql does before a driver sees them: a
// Valuer's value, then the driver's own check, then the default converter for
// what the driver leaves to it. The slice is c's scratch, valid until the
// next call.
func (c *Conn) boostArgs(st *Stmt, args []any) ([]sqldriver.NamedValue, error) {
	if want := st.NumInput(); want >= 0 && len(args) != want {
		return nil, fmt.Errorf("sql: expected %d arguments, got %d", want, len(args))
	}
	nvs := c.boostNamed[:0]
	for i, a := range args {
		nv := sqldriver.NamedValue{Ordinal: i + 1, Value: a}
		if na, ok := a.(sql.NamedArg); ok {
			nv.Name, nv.Value = na.Name, na.Value
		}
		if v, ok := nv.Value.(sqldriver.Valuer); ok {
			var err error
			if nv.Value, err = v.Value(); err != nil {
				return nil, err
			}
		}
		if err := c.CheckNamedValue(&nv); errors.Is(err, sqldriver.ErrSkip) {
			if nv.Value, err = sqldriver.DefaultParameterConverter.ConvertValue(nv.Value); err != nil {
				return nil, fmt.Errorf("sql: converting argument $%d type: %w", i+1, err)
			}
		} else if err != nil {
			return nil, err
		}
		nvs = append(nvs, nv)
	}
	c.boostNamed = nvs
	return nvs, nil
}

func (c *Conn) boostQuery(ctx context.Context, query string, args []any) (*Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st, err := c.boostStmt(query)
	if err != nil {
		return nil, err
	}
	nvs, err := c.boostArgs(st, args)
	if err != nil {
		return nil, err
	}
	dr, err := st.QueryContext(ctx, nvs)
	clear(nvs)
	if err != nil {
		return nil, err
	}
	r, ok := dr.(*Rows)
	if !ok {
		return nil, fmt.Errorf("driver: boost got rows of type %T", dr)
	}
	return r, nil
}

func (c *Conn) boostExec(ctx context.Context, query string, args []any) (sql.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st, err := c.boostStmt(query)
	if err != nil {
		return nil, err
	}
	nvs, err := c.boostArgs(st, args)
	if err != nil {
		return nil, err
	}
	res, err := st.ExecContext(ctx, nvs)
	clear(nvs)
	return res, err
}

// Len, Null and Scan make *Rows a boostcore.Rows.

func (r *Rows) Len() int { return len(r.rows) }

func (r *Rows) Null(row, col int) bool { return r.rows[row][col].Typ == engine.Null }

func (r *Rows) Scan(row, col int, dest any) error {
	v := r.rows[row][col]
	if p, ok := dest.(*string); ok && (v.Typ == engine.Text || v.Typ == engine.Blob) {
		*p = r.boostString(row, v.S)
		return nil
	}
	return r.scanFast(col, v, dest)
}

// boostString copies b into a chunk shared with the strings of the next few
// rows, as valueSlab does for database/sql: one allocation for many strings.
// A chunk is written once per string and never reused, so each string stays
// valid for as long as its holder keeps it.
func (r *Rows) boostString(row int, b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if cap(r.boostText)-len(r.boostText) < len(b) {
		n := 0
		for _, rr := range r.rows[row:min(len(r.rows), row+valueSlabRows)] {
			for _, v := range rr {
				if v.Typ == engine.Text || v.Typ == engine.Blob {
					n += len(v.S)
				}
			}
		}
		r.boostText = make([]byte, 0, max(n, len(b)))
	}
	start := len(r.boostText)
	r.boostText = append(r.boostText, b...)
	return unsafe.String(&r.boostText[start], len(b))
}

// scanFast stores column i's value v into d.
func (r *Rows) scanFast(i int, v engine.Value, d any) error {
	if i < len(r.timeCols) && r.timeCols[i] {
		if tv, ok := declTypeTimeValue(v, r.loc); ok {
			switch p := d.(type) {
			case *time.Time:
				*p = tv
				return nil
			case *any:
				*p = tv
				return nil
			}
		}
	}
	switch p := d.(type) {
	case *any:
		*p = engineValueToDriver(v)
		return nil
	case sql.Scanner:
		return p.Scan(engineValueToDriver(v))
	}
	if v.Typ == engine.Null {
		if p, ok := d.(*[]byte); ok {
			*p = nil
			return nil
		}
		return fmt.Errorf("converting NULL to %T is unsupported", d)
	}
	switch p := d.(type) {
	case *int64:
		if v.Typ == engine.Int {
			*p = v.I
			return nil
		}
	case *int:
		if v.Typ == engine.Int && v.I >= math.MinInt && v.I <= math.MaxInt {
			*p = int(v.I)
			return nil
		}
	case *int32:
		if v.Typ == engine.Int && v.I >= math.MinInt32 && v.I <= math.MaxInt32 {
			*p = int32(v.I)
			return nil
		}
	case *float64:
		switch v.Typ {
		case engine.Float:
			*p = v.F
			return nil
		case engine.Int:
			*p = float64(v.I)
			return nil
		}
	case *bool:
		if v.Typ == engine.Int {
			*p = v.I != 0
			return nil
		}
	case *string:
		if v.Typ == engine.Text || v.Typ == engine.Blob {
			*p = string(v.S)
			return nil
		}
	case *[]byte:
		if v.Typ == engine.Text || v.Typ == engine.Blob {
			*p = append([]byte{}, v.S...)
			return nil
		}
	}
	return fmt.Errorf("unsupported scan of %s into %T", typeName(v.Typ), d)
}

func typeName(t engine.ValueType) string {
	switch t {
	case engine.Int:
		return "INTEGER"
	case engine.Float:
		return "REAL"
	case engine.Text:
		return "TEXT"
	case engine.Blob:
		return "BLOB"
	}
	return "NULL"
}
