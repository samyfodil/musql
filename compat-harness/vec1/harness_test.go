package vec1

import (
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/driver"
)

// engines opens musql and the C oracle over fresh files, one connection each,
// so per-connection state (vec1_config, vec1's pending writes) is one
// connection's in both.
func engines(t testing.TB) (musql, oracle *sql.DB) {
	dir := t.TempDir()
	m, err := sql.Open(driver.DriverName, filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := openOracle(filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	m.SetMaxOpenConns(1)
	c.SetMaxOpenConns(1)
	t.Cleanup(func() {
		m.Close()
		c.Close()
	})
	return m, c
}

// cell is one value of a result as render prints it; f is the value of a
// REAL, whose bits the text carries.
type cell struct {
	s   string
	f   float64
	isF bool
}

// result is one statement's outcome: the column names, the rows, and the
// error that ended it, if any.
type result struct {
	cols []string
	rows [][]cell
	err  string // "error: ...\n", or empty
}

// String is the result as text, so two engines compare equal exactly when
// they agree: every column's type and value (a REAL by its bits), or the
// error text.
func (r result) String() string {
	var out strings.Builder
	for _, row := range r.rows {
		for i, c := range row {
			if i > 0 {
				out.WriteString("|")
			}
			out.WriteString(c.s)
		}
		out.WriteString("\n")
	}
	out.WriteString(r.err)
	return out.String()
}

// query runs q and collects its outcome.
func query(db *sql.DB, q string, args ...any) result {
	var res result
	rows, err := db.Query(q, args...)
	if err != nil {
		res.err = "error: " + errText(err) + "\n"
		return res
	}
	defer rows.Close()
	res.cols, _ = rows.Columns()
	for rows.Next() {
		vals := make([]any, len(res.cols))
		ptrs := make([]any, len(res.cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			res.err = "error: " + errText(err) + "\n"
			return res
		}
		row := make([]cell, len(vals))
		for i, v := range vals {
			switch x := v.(type) {
			case []byte:
				row[i].s = fmt.Sprintf("x'%x'", x)
			case float64:
				row[i] = cell{s: fmt.Sprintf("%v(%x)", x, math.Float64bits(x)), f: x, isF: true}
			case nil:
				row[i].s = "NULL"
			case string:
				row[i].s = fmt.Sprintf("'%s'", x)
			default:
				row[i].s = fmt.Sprintf("%v", x)
			}
		}
		res.rows = append(res.rows, row)
	}
	if err := rows.Err(); err != nil {
		res.err = "error: " + errText(err) + "\n"
	}
	return res
}

// render is one statement's outcome as text.
func render(db *sql.DB, q string, args ...any) string {
	return query(db, q, args...).String()
}

// errText is an error's message as both engines word it.
func errText(err error) string {
	s := err.Error()
	for strings.HasPrefix(s, "engine: ") {
		s = strings.TrimPrefix(s, "engine: ")
	}
	return s
}
