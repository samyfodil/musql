package libsql

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// TestVectorIndexStatements compares libSQL's vector index DDL, its write-time
// checks and vector_top_k's argument errors, statement by statement. Left out:
// statements libSQL 0.2.3 crashes on (k = 0, libsql_vector_idx() with no
// argument), and the shadow tables it lists in sqlite_schema.
func TestVectorIndexStatements(t *testing.T) {
	m, l := engines(t)
	for _, s := range []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, e F32_BLOB(3), x TEXT)",
		"INSERT INTO t VALUES (1, vector('[1,0,0]'), 'a'), (2, vector('[0,1,0]'), 'b'), (3, vector('[1,1,0]'), 'c'), (4, NULL, 'd'), (5, vector('[2,0.5,0]'), 'e')",
		"CREATE INDEX i ON t(libsql_vector_idx(e))",
		"CREATE INDEX IF NOT EXISTS i ON t(libsql_vector_idx(e))",
		"CREATE INDEX i ON t(libsql_vector_idx(e))",
		"SELECT * FROM vector_top_k('i', vector('[1,0.1,0]'), 2)",
		"SELECT id FROM vector_top_k('i', '[1,0.1,0]', 10)",
		"SELECT * FROM vector_top_k('main.i', vector('[1,0.1,0]'), 2)",
		"SELECT rowid, typeof(id) FROM vector_top_k('i', vector('[1,0.1,0]'), 2)",
		"SELECT t.x FROM vector_top_k('i', vector('[0,1,0]'), 3) v JOIN t ON t.rowid = v.id",
		"SELECT id, distance FROM vector_top_k('i', vector('[1,0.1,0]'), 2)",
		"SELECT * FROM vector_top_k('i', vector('[1,0.1]'), 2)",
		"SELECT * FROM vector_top_k('i', vector64('[1,0,0]'), 10)",
		"SELECT * FROM vector_top_k('i', vector8('[1,0,0]'), 10)",
		"SELECT * FROM vector_top_k('i', x'00', 10)",
		"SELECT * FROM vector_top_k('i', NULL, 2)",
		"SELECT * FROM vector_top_k('i', vector('[1,0,0]'), -1)",
		"SELECT * FROM vector_top_k('i', vector('[1,0,0]'), 2.5)",
		"SELECT * FROM vector_top_k('i', vector('[1,0,0]'), 2.0)",
		"SELECT * FROM vector_top_k('i', vector('[1,0,0]'), '2')",
		"SELECT * FROM vector_top_k('i', vector('[1,0,0]'), NULL)",
		"SELECT * FROM vector_top_k('i', vector('[1,0,0]'))",
		"SELECT * FROM vector_top_k('i', vector('[1,0,0]'), 2, 3)",
		"SELECT * FROM vector_top_k(NULL, vector('[1,0,0]'), 2)",
		"SELECT * FROM vector_top_k(1, vector('[1,0,0]'), 2)",
		"SELECT * FROM vector_top_k('nope', vector('[1,0,0]'), 2)",
		"SELECT * FROM vector_top_k('I', vector('[1,0,0]'), 2)",
		"CREATE INDEX x1 ON t(x)",
		"SELECT * FROM vector_top_k('x1', vector('[1,0,0]'), 2)",

		// The write-time checks.
		"INSERT INTO t VALUES (6, vector('[1,2]'), 'f')",
		"INSERT INTO t VALUES (7, vector64('[1,2,3]'), 'g')",
		"INSERT INTO t VALUES (8, vector1bit('[1,0,1]'), 'h')",
		"INSERT INTO t VALUES (9, 'notvec', 'i')",
		"INSERT INTO t VALUES (10, '[1,2,3]', 'j')",
		"INSERT INTO t VALUES (11, 5, 'k')",
		"INSERT INTO t VALUES (12, NULL, 'l')",
		"SELECT id FROM vector_top_k('i', vector('[1,2,3]'), 1)",

		// The DDL's checks.
		"CREATE INDEX k1 ON t(libsql_vector_idx(e, 'metric=bogus'))",
		"CREATE INDEX k2 ON t(libsql_vector_idx(e, 'bogus=1'))",
		"CREATE INDEX k3 ON t(libsql_vector_idx(x))",
		"CREATE INDEX k4 ON t(libsql_vector_idx(e), id)",
		"CREATE INDEX k5 ON t(libsql_vector_idx(e || x))",
		"CREATE INDEX k6 ON t(libsql_vector_idx(e, 5))",
		"CREATE INDEX k7 ON t(libsql_vector_idx(e, 'max_neighbors=abc'))",
		"CREATE INDEX k8 ON t(libsql_vector_idx(e, 'max_neighbors=-3'))",
		"CREATE INDEX k9 ON t(libsql_vector_idx(e, 'metric = l2'))",
		"CREATE INDEX k10 ON t(libsql_vector_idx(e, 'metric'))",
		"CREATE INDEX k11 ON t(libsql_vector_idx(e, 'alpha=x'))",
		"CREATE INDEX k12 ON t(libsql_vector_idx(e COLLATE nocase))",
		"CREATE INDEX k13 ON t(libsql_vector_idx(e, 'compress_neighbors=float1bit', 'metric=l2'))",
		"CREATE INDEX k14 ON t(libsql_vector_idx(rowid))",
		"CREATE TABLE ty(a TEXT, b BLOB, c F32_BLOB, d F32_BLOB(0), e F32_BLOB(3x), f F32_BLOB(3) NOT NULL, g FLOAT32 ( 2 ), h f32_blob(2), i F32_BLOB(70000))",
		"CREATE INDEX tya ON ty(libsql_vector_idx(a))",
		"CREATE INDEX tyb ON ty(libsql_vector_idx(b))",
		"CREATE INDEX tyc ON ty(libsql_vector_idx(c))",
		"CREATE INDEX tyd ON ty(libsql_vector_idx(d))",
		"CREATE INDEX tye ON ty(libsql_vector_idx(e))",
		"CREATE INDEX tyf ON ty(libsql_vector_idx(f))",
		"CREATE INDEX tyg ON ty(libsql_vector_idx(g))",
		"CREATE INDEX tyh ON ty(libsql_vector_idx(h))",
		"CREATE INDEX tyi ON ty(libsql_vector_idx(i))",

		// Options libSQL accepts.
		"CREATE TABLE o(e F32_BLOB(2))",
		"CREATE INDEX o1 ON o(libsql_vector_idx(e, 'METRIC=L2'))",
		"CREATE INDEX o2 ON o(libsql_vector_idx(e, 'metric=co'))",
		"CREATE INDEX o3 ON o(libsql_vector_idx(e, 'type=diskann', 'alpha=1.2', 'search_l=200', 'insert_l=70', 'max_neighbors=20'))",
		"CREATE INDEX o4 ON o(libsql_vector_idx(e, 'compress_neighbors=float8', 'metric=cosine', 'metric=l2'))",
		"CREATE UNIQUE INDEX o5 ON o(libsql_vector_idx(e))",

		// Rows already in the table are checked when the index is created.
		"CREATE TABLE b1(e F32_BLOB(2))",
		"INSERT INTO b1 VALUES (vector('[1,2,3]'))",
		"CREATE INDEX b1i ON b1(libsql_vector_idx(e))",
		"CREATE TABLE b2(e F32_BLOB(2))",
		"INSERT INTO b2 VALUES ('abc')",
		"CREATE INDEX b2i ON b2(libsql_vector_idx(e))",
		"CREATE TABLE b3(e F32_BLOB(2))",
		"INSERT INTO b3 VALUES (5)",
		"CREATE INDEX b3i ON b3(libsql_vector_idx(e))",

		"SELECT libsql_vector_idx('[1,2]'), libsql_vector_idx(1), libsql_vector_idx(NULL), libsql_vector_idx(x'0000803f', 'metric=l2')",
		"DROP INDEX i",
		"SELECT * FROM vector_top_k('i', vector('[1,0.1,0]'), 2)",
	} {
		if got, want := answer(m, s), answer(l, s); got != want {
			t.Errorf("%s\n musql:  %s\n libSQL: %s", s, got, want)
		}
	}
}

// TestVectorTopK checks vector_top_k against the exact answer: libSQL's own
// ORDER BY vector_distance_*(...) LIMIT k over the same rows. libSQL's
// vector_top_k is approximate, so it is not the oracle; its brute-force query
// is. Every vector type, both metrics, NULL and text cells, a partial index,
// rows changed after the table was compacted, and a table large enough for
// the JIT search.
func TestVectorTopK(t *testing.T) {
	if !oracleUnfused() {
		t.Skip(oracleFusedNote)
	}
	m, l := engines(t)
	r := rand.New(rand.NewPCG(5, 6))
	exec := func(s string) {
		t.Helper()
		if _, err := m.Exec(s); err != nil {
			t.Fatal(s, err)
		}
		if _, err := l.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}
	type table struct {
		name, decl, enc, opts, where string
		n, dims                      int
	}
	tables := []table{
		{name: "f32", decl: "F32_BLOB", enc: "vector32", n: 400, dims: 8},
		{name: "f32l2", decl: "F32_BLOB", enc: "vector32", opts: ", 'metric=l2'", n: 400, dims: 8},
		{name: "f64", decl: "F64_BLOB", enc: "vector64", n: 300, dims: 6},
		{name: "f16", decl: "F16_BLOB", enc: "vector16", n: 300, dims: 6},
		{name: "fb16", decl: "FB16_BLOB", enc: "vectorb16", opts: ", 'metric=l2'", n: 300, dims: 6},
		{name: "f8", decl: "F8_BLOB", enc: "vector8", n: 300, dims: 6},
		{name: "bits", decl: "F1BIT_BLOB", enc: "vector1bit", n: 300, dims: 64},
		{name: "part", decl: "F32_BLOB", enc: "vector32", where: "tag % 3 <> 0", n: 400, dims: 8},
		{name: "big", decl: "F32_BLOB", enc: "vector32", n: 20000, dims: 32},
		{name: "bigl2", decl: "F32_BLOB", enc: "vector32", opts: ", 'metric=l2'", n: 20000, dims: 32},
	}
	for _, tb := range tables {
		exec(fmt.Sprintf("CREATE TABLE %s(id INTEGER PRIMARY KEY, tag INT, e %s(%d))", tb.name, tb.decl, tb.dims))
		var b strings.Builder
		for i := 1; i <= tb.n; i++ {
			v := fmt.Sprintf("%s('%s')", tb.enc, randVec(r, tb.dims))
			switch {
			case strings.HasPrefix(tb.name, "big"):
			case i%50 == 0:
				v = "NULL"
			case i%37 == 0 && tb.enc == "vector32":
				v = "'" + randVec(r, tb.dims) + "'" // a text cell, parsed as float32
			}
			if b.Len() > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "(%d, %d, %s)", i, i, v)
			if i%500 == 0 || i == tb.n {
				exec(fmt.Sprintf("INSERT INTO %s VALUES %s", tb.name, b.String()))
				b.Reset()
			}
		}
		where := ""
		if tb.where != "" {
			where = " WHERE " + tb.where
		}
		exec(fmt.Sprintf("CREATE INDEX %s_i ON %s(libsql_vector_idx(e%s))%s", tb.name, tb.name, tb.opts, where))
	}
	exec("VACUUM")
	for _, tb := range tables {
		exec(fmt.Sprintf("DELETE FROM %s WHERE id %% 23 = 0", tb.name))
		exec(fmt.Sprintf("UPDATE %s SET e = %s('%s') WHERE id %% 29 = 0", tb.name, tb.enc, randVec(r, tb.dims)))
	}
	for _, tb := range tables {
		dist := "vector_distance_cos"
		if tb.opts != "" {
			dist = "vector_distance_l2"
		}
		cond := "e IS NOT NULL"
		if tb.where != "" {
			cond += " AND " + tb.where
		}
		for _, k := range []int{1, 10, 57, tb.n} {
			q := fmt.Sprintf("%s('%s')", tb.enc, randVec(r, tb.dims))
			got := answer(m, fmt.Sprintf("SELECT id FROM vector_top_k('%s_i', %s, %d)", tb.name, q, k))
			want := answer(l, fmt.Sprintf("SELECT id FROM %s WHERE %s ORDER BY %s(e, %s), id LIMIT %d", tb.name, cond, dist, q, k))
			if got != want {
				t.Errorf("%s, k=%d: vector_top_k is not the exact top k\n musql: %.300s\n exact: %.300s", tb.name, k, got, want)
			}
		}
	}

	// A zero vector's cosine is NaN: it ranks after every other row.
	exec("CREATE TABLE z(e F32_BLOB(2))")
	exec("INSERT INTO z VALUES (vector('[0,0]')), (vector('[1,0]')), (vector('[0,1]'))")
	exec("CREATE INDEX zi ON z(libsql_vector_idx(e))")
	if got := answer(m, "SELECT id FROM vector_top_k('zi', vector('[1,0.1]'), 3)"); got != "int64(2) \nint64(3) \nint64(1) \n" {
		t.Errorf("zero vector: got %q, want it last", got)
	}
}
