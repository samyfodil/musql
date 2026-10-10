package libsql

import (
	"database/sql"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/samyfodil/musql/driver"
	_ "github.com/tursodatabase/go-libsql"
)

func open(t testing.TB, name, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(name, dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func engines(t testing.TB) (musql, libsql *sql.DB) {
	return open(t, driver.DriverName, ":memory:"), open(t, "libsql", "file::memory:")
}

// answer is one statement's outcome, rendered so two engines compare equal
// exactly when they agree: every column's type and value, or the error text.
func answer(db *sql.DB, q string) string { return answerArgs(db, q) }

func answerArgs(db *sql.DB, q string, args ...any) string {
	rows, err := db.Query(q, args...)
	if err != nil {
		return "error: " + errText(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var b strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "error: " + errText(err)
		}
		for _, v := range vals {
			switch x := v.(type) {
			case []byte:
				fmt.Fprintf(&b, "x'%x' ", x)
			case float64:
				fmt.Fprintf(&b, "%v(%x) ", x, math.Float64bits(x))
			default:
				fmt.Fprintf(&b, "%T(%v) ", x, x)
			}
		}
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		return "error: " + errText(err)
	}
	return b.String()
}

// errText is the message SQLite produced, without either driver's wrapping.
func errText(err error) string {
	s := err.Error()
	if i := strings.Index(s, "SQLite failure: `"); i >= 0 {
		s = strings.TrimSuffix(s[i+len("SQLite failure: `"):], "`")
	}
	return strings.TrimPrefix(s, "engine: ")
}

// TestVectorFunctions runs every vector function over a matrix of inputs:
// text and blobs of every type, malformed ones, and values of the wrong type.
func TestVectorFunctions(t *testing.T) {
	m, l := engines(t)
	texts := []string{
		"'[]'", "'[1]'", "'[1,2,3]'", "' [ 1 , 2.5 , -3e2 ] '", "'[1e40, -1e-50, 0]'",
		"'[0.1,0.2,0.3,0.4,0.5]'", "'[1 2, 3]'", "'[1,,2]'", "'[1,2'", "'1,2]'", "'[1]x'",
		"'[abc]'", "'[1,2,]'", "'[-0, 0]'", "'[3.4028235e38, 1.17549435e-38, 6e-45]'",
		"'[65504, 65520, 1e-7, -2.5]'", "'[1,1,1,1]'", "'[0.25,-0.75,1.5,2,3,-4,5.5,0.001,9]'",
	}
	blobs := []string{
		"x''", "x'0000803f'", "x'0000803f00'", "x'000000000000f03f02'", "x'0000000000f03f02'",
		"x'0003'", "x'05'", "x'ff0003'", "x'00003c05'", "x'003c0005'", "x'0000803f01'",
		"x'0102030400000000000000000000000004'", "x'07'", "x'0000803f0000004001'",
	}
	others := []string{"NULL", "1", "1.5"}
	enc := []string{"vector", "vector32", "vector64", "vector1bit", "vector8", "vector16", "vectorb16"}

	var qs []string
	for _, in := range append(append(append([]string{}, texts...), blobs...), others...) {
		for _, f := range enc {
			qs = append(qs, fmt.Sprintf("SELECT %s(%s)", f, in))
			qs = append(qs, fmt.Sprintf("SELECT vector_extract(%s(%s))", f, in))
		}
		qs = append(qs, fmt.Sprintf("SELECT vector_extract(%s)", in))
	}
	r := rand.New(rand.NewPCG(1, 2))
	for range 60 {
		n := 1 + r.IntN(40)
		a, b := randVec(r, n), randVec(r, n)
		for _, f := range enc {
			for _, d := range []string{"vector_distance_cos", "vector_distance_l2"} {
				qs = append(qs, fmt.Sprintf("SELECT %s(%s('%s'), %s('%s'))", d, f, a, f, b))
			}
		}
	}
	qs = append(qs,
		"SELECT vector_distance_cos('[1]', '[1,2]')",
		"SELECT vector_distance_cos(vector('[1]'), vector64('[1]'))",
		"SELECT vector_distance_l2(vector1bit('[1,-1]'), vector1bit('[1,1]'))",
		"SELECT vector_distance_cos('[0,0]', '[0,0]')",
		"SELECT vector_distance_cos(vector8('[2,2,2]'), vector8('[2,2,2]'))",
		"SELECT vector()", "SELECT vector('[1]', '[2]')", "SELECT vector_distance_cos('[1]')",
	)
	diffs := 0
	for _, q := range qs {
		if undefinedInLibSQL(q) {
			continue
		}
		if got, want := answer(m, q), answer(l, q); got != want {
			diffs++
			if diffs <= 40 {
				t.Errorf("%s\n musql:  %s\n libsql: %s", q, got, want)
			}
		}
	}
	t.Logf("%d statements, %d differ", len(qs), diffs)
}

func randVec(r *rand.Rand, n int) string {
	p := make([]string, n)
	for i := range p {
		p[i] = fmt.Sprintf("%.6g", r.NormFloat64()*float64(1+r.IntN(3)))
	}
	return "[" + strings.Join(p, ",") + "]"
}

// undefinedInLibSQL reports the statements whose libSQL answer is read from
// uninitialized memory: vector_extract() of a blob whose data size disagrees
// with its dimensions (libSQL 0.2.3 tests that parse failure with "< 0" while
// it returns SQLITE_ERROR, 1). musql reports the size error instead.
func undefinedInLibSQL(q string) bool {
	return q == "SELECT vector_extract(x'0102030400000000000000000000000004')"
}
