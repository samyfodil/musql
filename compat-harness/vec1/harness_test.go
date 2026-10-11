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

// render is one statement's outcome, so two engines compare equal exactly
// when they agree: every column's type and value (a REAL by its bits), or the
// error text.
func render(db *sql.DB, q string, args ...any) string {
	rows, err := db.Query(q, args...)
	if err != nil {
		return "error: " + errText(err) + "\n"
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "error: " + errText(err) + "\n"
		}
		for i, v := range vals {
			if i > 0 {
				out.WriteString("|")
			}
			switch x := v.(type) {
			case []byte:
				fmt.Fprintf(&out, "x'%x'", x)
			case float64:
				fmt.Fprintf(&out, "%v(%x)", x, math.Float64bits(x))
			case nil:
				out.WriteString("NULL")
			case string:
				fmt.Fprintf(&out, "'%s'", x)
			default:
				fmt.Fprintf(&out, "%v", x)
			}
		}
		out.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		out.WriteString("error: " + errText(err) + "\n")
	}
	return out.String()
}

// errText is an error's message as both engines word it.
func errText(err error) string {
	s := err.Error()
	for strings.HasPrefix(s, "engine: ") {
		s = strings.TrimPrefix(s, "engine: ")
	}
	return s
}
