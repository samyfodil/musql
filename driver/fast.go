package driver

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/samyfodil/musql/engine"
)

// FastStmt runs one query on a *sql.Conn without database/sql's per-query
// work: no pool round-trip, no driver.Rows, and no boxing of each value into
// an interface. Each call reaches the connection through sql.Conn.Raw and runs
// on exactly the path db.Query does, so a transaction, a pragma or another
// connection's commit is seen the same way.
//
// Values scan into *int64, *int, *int32, *float64, *string, *[]byte, *bool,
// *time.Time (for a column the connection coerces to time), *any, and any
// sql.Scanner. Any other destination is an error, not a conversion: use
// database/sql for those. A FastStmt belongs to its *sql.Conn and, like it, is
// not for concurrent use.
type FastStmt struct {
	conn  *sql.Conn
	query string
	// st is the statement prepared on the connection, once, exactly as
	// db.Query prepares it.
	st     Stmt
	stConn *Conn
	named  []sqldriver.NamedValue
}

// PrepareFast returns a FastStmt for query on conn. Nothing runs until the
// first call; the statement is then prepared on conn's driver connection as
// db.Prepare would, and compiled and cached by the engine as for any query.
func PrepareFast(conn *sql.Conn, query string) *FastStmt {
	return &FastStmt{conn: conn, query: query}
}

// QueryRow runs the statement and scans its first row into dest, returning
// sql.ErrNoRows when there is none.
func (s *FastStmt) QueryRow(ctx context.Context, args []any, dest ...any) error {
	found := false
	err := s.Query(ctx, args, func(scan func(dest ...any) error) error {
		found = true
		if err := scan(dest...); err != nil {
			return err
		}
		return errStopRows
	})
	if err != nil && !errors.Is(err, errStopRows) {
		return err
	}
	if !found {
		return sql.ErrNoRows
	}
	return nil
}

// Query runs the statement and calls each once per row, in order, with a scan
// function for that row. Returning an error from each stops and returns it.
func (s *FastStmt) Query(ctx context.Context, args []any, each func(scan func(dest ...any) error) error) error {
	return s.conn.Raw(func(dc any) error {
		c, ok := dc.(*Conn)
		if !ok {
			return fmt.Errorf("driver: FastStmt needs a musql connection, not %T", dc)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.stConn != c {
			s.st = Stmt{}
			if err := c.prepareInto(s.query, &s.st); err != nil {
				return parityErr(err)
			}
			s.stConn = c
		}
		if want := s.st.NumInput(); want >= 0 && len(args) != want {
			return fmt.Errorf("sql: expected %d arguments, got %d", want, len(args))
		}
		// The conversions database/sql applies before a driver sees an
		// argument: a Valuer's value, then the driver's own check, then the
		// default converter for what the driver leaves to it.
		named := s.named[:0]
		for i, a := range args {
			nv := sqldriver.NamedValue{Ordinal: i + 1, Value: a}
			if na, ok := a.(sql.NamedArg); ok {
				nv.Name, nv.Value = na.Name, na.Value
			}
			if v, ok := nv.Value.(sqldriver.Valuer); ok {
				var err error
				if nv.Value, err = v.Value(); err != nil {
					return err
				}
			}
			if err := c.CheckNamedValue(&nv); errors.Is(err, sqldriver.ErrSkip) {
				if nv.Value, err = sqldriver.DefaultParameterConverter.ConvertValue(nv.Value); err != nil {
					return fmt.Errorf("sql: converting argument $%d type: %w", i+1, err)
				}
			} else if err != nil {
				return err
			}
			named = append(named, nv)
		}
		s.named = named
		dr, err := s.st.queryContextImpl(ctx, named)
		clear(named)
		if err != nil {
			return err
		}
		r, ok := dr.(*Rows)
		if !ok {
			return fmt.Errorf("driver: FastStmt got rows of type %T", dr)
		}
		var row []engine.Value
		scan := func(dest ...any) error {
			if len(dest) != len(row) {
				return fmt.Errorf("sql: expected %d destination arguments in Scan, not %d", len(row), len(dest))
			}
			for i, d := range dest {
				if err := r.scanFast(i, row[i], d); err != nil {
					return fmt.Errorf("sql: Scan error on column index %d: %w", i, err)
				}
			}
			return nil
		}
		for _, row = range r.rows {
			if err := each(scan); err != nil {
				return err
			}
		}
		return nil
	})
}

// errStopRows ends a Query from inside its callback.
var errStopRows = errors.New("driver: stop rows")

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
			*p = append((*p)[:0:0], v.S...)
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
