// Differential tests of GENERATED ALWAYS AS columns (STORED and VIRTUAL),
// including file format verification and record-slot correctness on UPDATE/DELETE.
package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var generatedColumnCorpus = [][]string{
	// STORED and VIRTUAL columns side by side; AS defaults to VIRTUAL.
	{`CREATE TABLE gc(a INTEGER, b INTEGER GENERATED ALWAYS AS (a*2) STORED, c INTEGER GENERATED ALWAYS AS (a+1) VIRTUAL)`, `INSERT INTO gc(a) VALUES(5),(10)`, `SELECT a,b,c FROM gc ORDER BY a`},
	{`CREATE TABLE g2(a INTEGER, b AS (a*2))`, `INSERT INTO g2 VALUES(5)`, `SELECT * FROM g2`},
	{`CREATE TABLE g3(a INTEGER, b INTEGER GENERATED ALWAYS AS (a*2))`, `INSERT INTO g3(a) VALUES(3)`, `SELECT a,b FROM g3`},
	// Virtual columns skip their record slot but keep declaration order.
	{`CREATE TABLE g7(a INTEGER, v AS (a+1) VIRTUAL, d INTEGER)`, `INSERT INTO g7(a,d) VALUES(5,7)`, `SELECT a,v,d FROM g7`},
	// One generated column referencing an earlier one.
	{`CREATE TABLE g5(a INTEGER, b AS (a||'x') STORED, c AS (b||'y'))`, `INSERT INTO g5(a) VALUES(7)`, `SELECT * FROM g5`},
	// The computed value takes the column's declared affinity.
	{`CREATE TABLE g6(a INTEGER, b TEXT AS (a*2) STORED)`, `INSERT INTO g6(a) VALUES(4)`, `SELECT b, typeof(b) FROM g6`},
	// A generated column may reference the INTEGER PRIMARY KEY rowid alias.
	{`CREATE TABLE pk(id INTEGER PRIMARY KEY, b AS (id*2))`, `INSERT INTO pk(id) VALUES(4)`, `SELECT id,b FROM pk`},

	// Supplying a value for a generated column is an error, on INSERT and on
	// UPDATE alike.
	{`CREATE TABLE g4(a INTEGER, b AS (a*2))`, `INSERT INTO g4(a,b) VALUES(1,2)`},
	{`CREATE TABLE ug(a INTEGER, b AS (a*2))`, `INSERT INTO ug(a) VALUES(1)`, `UPDATE ug SET b=5`},
	// ... and a generated column cannot be the PRIMARY KEY of a rowid table.
	{`CREATE TABLE bad(a INTEGER, b INTEGER PRIMARY KEY AS (a*2))`},

	// Recomputation on UPDATE, and use in WHERE / ORDER BY / DELETE.
	{`CREATE TABLE g8(a INTEGER, b AS (a*2))`, `INSERT INTO g8(a) VALUES(3)`, `UPDATE g8 SET a=10`, `SELECT * FROM g8`},
	{`CREATE TABLE g9(a INTEGER, b AS (a*2))`, `INSERT INTO g9(a) VALUES(3)`, `SELECT * FROM g9 WHERE b=6`},
	{`CREATE TABLE og(a INTEGER, b AS (a*2))`, `INSERT INTO og(a) VALUES(3),(1),(2)`, `SELECT b FROM og ORDER BY b`},
	{`CREATE TABLE ga(a INTEGER, b AS (a*2))`, `INSERT INTO ga(a) VALUES(3)`, `DELETE FROM ga WHERE b=6`, `SELECT count(*) FROM ga`},

	// Constraints over generated columns enforce computed values.
	{`CREATE TABLE i1(a INTEGER, b AS (a*2))`, `CREATE INDEX ix ON i1(b)`, `INSERT INTO i1(a) VALUES(3),(5)`, `SELECT a,b FROM i1 WHERE b=6`},
	{`CREATE TABLE i2(a INTEGER, b AS (a*2) STORED)`, `CREATE INDEX ix2 ON i2(b)`, `INSERT INTO i2(a) VALUES(3),(5)`, `SELECT a,b FROM i2 WHERE b=10`},
	{`CREATE TABLE u1(a INTEGER, b AS (a*2) STORED UNIQUE)`, `INSERT INTO u1(a) VALUES(3)`, `INSERT INTO u1(a) VALUES(3)`, `SELECT count(*) FROM u1`},
	{`CREATE TABLE n1(a INTEGER, b AS (a*2) NOT NULL)`, `INSERT INTO n1(a) VALUES(NULL)`, `SELECT count(*) FROM n1`},
	{`CREATE TABLE c1(a INTEGER, b AS (a*2) STORED CHECK(b < 10))`, `INSERT INTO c1(a) VALUES(3)`, `INSERT INTO c1(a) VALUES(8)`, `SELECT count(*) FROM c1`},

	// Virtual columns positioned before other columns, with UPDATE/DELETE/UPSERT/triggers.
	{`CREATE TABLE w1(k TEXT PRIMARY KEY, a INTEGER, c1 INTEGER AS (a), c2 INTEGER AS (a+1), b INTEGER, x INTEGER, y INTEGER)`,
		`INSERT INTO w1(k,a,b) VALUES('p',7,8)`, `UPDATE w1 SET b=99 WHERE k='p'`, `SELECT * FROM w1`, `PRAGMA integrity_check`},
	// References to columns after a virtual one resolve correctly.
	{`CREATE TABLE w2(k TEXT PRIMARY KEY, g INTEGER AS (1), b INTEGER, x INTEGER)`,
		`INSERT INTO w2(k,b) VALUES('p',8)`, `UPDATE w2 SET b=b+1 WHERE k='p'`, `SELECT * FROM w2`},
	// UPSERT DO UPDATE re-reads through the same conversion.
	{`CREATE TABLE w3(k TEXT PRIMARY KEY, a INTEGER, comp INTEGER AS (a), b INTEGER, x INTEGER)`,
		`INSERT INTO w3(k,a,b) VALUES('p',0,0)`,
		`INSERT INTO w3(k,b) VALUES('p',5) ON CONFLICT(k) DO UPDATE SET b=excluded.b`, `SELECT * FROM w3`},
	// Secondary indexes past a virtual column maintain consistency on UPDATE/DELETE.
	{`CREATE TABLE w4(k INTEGER PRIMARY KEY, g INTEGER AS (k*10), v INTEGER)`, `CREATE INDEX w4v ON w4(v)`,
		`INSERT INTO w4(k,v) VALUES(1,100),(2,200)`, `UPDATE w4 SET v=300 WHERE k=1`,
		`SELECT k,g,v FROM w4 ORDER BY k`, `SELECT k FROM w4 WHERE v=300`, `SELECT k FROM w4 WHERE v=100`, `PRAGMA integrity_check`},
	{`CREATE TABLE w5(k INTEGER PRIMARY KEY, g INTEGER AS (k*10), v INTEGER)`, `CREATE INDEX w5v ON w5(v)`,
		`INSERT INTO w5(k,v) VALUES(1,100),(2,200),(3,300)`, `DELETE FROM w5 WHERE k=2`,
		`SELECT k,g,v FROM w5 ORDER BY k`, `SELECT k FROM w5 WHERE v=200`, `SELECT k FROM w5 WHERE v>50 ORDER BY k`, `PRAGMA integrity_check`},
	// Indexes on virtual columns stay consistent across updates.
	{`CREATE TABLE w6(k INTEGER PRIMARY KEY, g INTEGER AS (v*2), v INTEGER)`, `CREATE INDEX w6g ON w6(g)`,
		`INSERT INTO w6(k,v) VALUES(1,5),(2,6)`, `UPDATE w6 SET v=9 WHERE k=1`,
		`SELECT k,g,v FROM w6 ORDER BY k`, `SELECT k FROM w6 WHERE g=18`, `SELECT k FROM w6 WHERE g=10`, `PRAGMA integrity_check`},
	// Triggers correctly reference columns across virtual columns via OLD./NEW.
	{`CREATE TABLE w7(k INTEGER PRIMARY KEY, g INTEGER AS (1), b INTEGER, x INTEGER)`, `CREATE TABLE log(o INTEGER, n INTEGER)`,
		`CREATE TRIGGER w7t AFTER UPDATE ON w7 BEGIN INSERT INTO log VALUES(OLD.b, NEW.b); END`,
		`INSERT INTO w7(k,b,x) VALUES(1,8,4)`, `UPDATE w7 SET b=9 WHERE k=1`, `SELECT * FROM w7`, `SELECT * FROM log`},
}

// TestGeneratedColumnsWriteFuzz randomizes column positions to catch record-slot bugs
// that the hand-written corpus may miss by always placing virtual columns last.
func TestGeneratedColumnsWriteFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260728))
	const iters = 200
	for i := 0; i < iters; i++ {
		// Column 0 is always the INTEGER PRIMARY KEY; the rest are random flavors.
		ncol := 3 + rng.Intn(4)
		defs := []string{"c0 INTEGER PRIMARY KEY"}
		var plain []string // the columns an UPDATE/INSERT may assign
		for j := 1; j < ncol; j++ {
			name := fmt.Sprintf("c%d", j)
			switch rng.Intn(3) {
			case 0:
				defs = append(defs, name+" INTEGER AS (c0*10)") // VIRTUAL (the default)
			case 1:
				defs = append(defs, name+" INTEGER AS (c0+1) STORED")
			default:
				defs = append(defs, name+" INTEGER")
				plain = append(plain, name)
			}
		}
		if len(plain) == 0 {
			continue // nothing assignable: that shape is covered by the corpus above
		}
		tbl := fmt.Sprintf("f%d", i)
		stmts := []string{fmt.Sprintf("CREATE TABLE %s(%s)", tbl, strings.Join(defs, ", "))}
		if rng.Intn(2) == 0 {
			stmts = append(stmts, fmt.Sprintf("CREATE INDEX %sx ON %s(%s)", tbl, tbl, plain[rng.Intn(len(plain))]))
		}
		cols := strings.Join(plain, ",")
		vals := make([]string, len(plain))
		for k := range vals {
			vals[k] = fmt.Sprint(rng.Intn(50))
		}
		stmts = append(stmts,
			fmt.Sprintf("INSERT INTO %s(c0,%s) VALUES(1,%s),(2,%s)", tbl, cols, strings.Join(vals, ","), strings.Join(vals, ",")),
			fmt.Sprintf("SELECT * FROM %s ORDER BY c0", tbl))
		tgt := plain[rng.Intn(len(plain))]
		switch rng.Intn(4) {
		case 0:
			stmts = append(stmts, fmt.Sprintf("UPDATE %s SET %s=99 WHERE c0=1", tbl, tgt))
		case 1:
			stmts = append(stmts, fmt.Sprintf("UPDATE %s SET %s=%s+1 WHERE c0=1", tbl, tgt, tgt))
		case 2:
			stmts = append(stmts, fmt.Sprintf("DELETE FROM %s WHERE c0=1", tbl))
		default:
			stmts = append(stmts, fmt.Sprintf("INSERT INTO %s(c0,%s) VALUES(1,7) ON CONFLICT(c0) DO UPDATE SET %s=excluded.%s+1", tbl, tgt, tgt, tgt))
		}
		stmts = append(stmts,
			fmt.Sprintf("SELECT * FROM %s ORDER BY c0", tbl),
			fmt.Sprintf("SELECT c0 FROM %s WHERE %s>=0 ORDER BY c0", tbl, tgt),
			"PRAGMA integrity_check")
		if !differ(t, fmt.Sprintf("gencolwrite/%d", i), stmts) {
			t.Fatalf("stopping at first divergence (iteration %d): %v", i, stmts)
		}
	}
}

func TestGeneratedColumnsMatchCSQLite(t *testing.T) {
	wrong := 0
	for i, stmts := range generatedColumnCorpus {
		c := run(t, "cgo", stmts)
		m := run(t, "musql", stmts)
		if len(c) != len(m) {
			wrong++
			t.Errorf("[%d] %v: statement count differs: cgo=%d musql=%d", i, stmts, len(c), len(m))
			continue
		}
		for j := range c {
			if fmt.Sprint(c[j]) != fmt.Sprint(m[j]) {
				wrong++
				t.Errorf("[%d] %v\n  statement %d (%s) DIVERGES\n  cgo:    %v\n  musql: %v",
					i, stmts, j, stmts[j], c[j], m[j])
			}
		}
	}
	if wrong != 0 {
		t.Fatalf("generated-column parity FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestGeneratedColumnsFileFormat verifies on-disk record layout:
// VIRTUAL columns omitted, STORED columns carried, readable by C SQLite.
func TestGeneratedColumnsFileFormat(t *testing.T) {
	for i, c := range []struct {
		ddl, ins, sel string
		want          string
	}{
		{`CREATE TABLE gc(a INTEGER, b INTEGER GENERATED ALWAYS AS (a*2) STORED, c INTEGER GENERATED ALWAYS AS (a+1) VIRTUAL)`,
			`INSERT INTO gc(a) VALUES(5),(10)`, `SELECT a,b,c FROM gc ORDER BY a`, "[5 10 6] [10 20 11]"},
		{`CREATE TABLE v1(a INTEGER, c INTEGER GENERATED ALWAYS AS (a+1) VIRTUAL, d INTEGER)`,
			`INSERT INTO v1(a,d) VALUES(5,7)`, `SELECT a,c,d FROM v1`, "[5 6 7]"},
		{`CREATE TABLE s1(a INTEGER, b TEXT AS (a||'x') STORED)`,
			`INSERT INTO s1(a) VALUES(3)`, `SELECT a,b FROM s1`, "[3 3x]"},
	} {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("gen%d.db", i))
		mdb, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{c.ddl, c.ins} {
			if _, err := mdb.Exec(s); err != nil {
				t.Fatalf("[%d] musql %s: %v", i, s, err)
			}
		}
		if err := mdb.Close(); err != nil {
			t.Fatalf("[%d] musql close: %v", i, err)
		}

		cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
		if err != nil {
			t.Fatal(err)
		}
		var ic string
		if err := cdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil {
			t.Fatalf("[%d] integrity_check: %v", i, err)
		}
		if ic != "ok" {
			t.Errorf("[%d] C SQLite integrity_check = %q, want \"ok\"", i, ic)
		}
		rows, err := cdb.Query(c.sel)
		if err != nil {
			t.Fatalf("[%d] C SQLite read: %v", i, err)
		}
		got := ""
		cols, _ := rows.Columns()
		for rows.Next() {
			v := make([]any, len(cols))
			pt := make([]any, len(cols))
			for k := range v {
				pt[k] = &v[k]
			}
			if err := rows.Scan(pt...); err != nil {
				t.Fatal(err)
			}
			if got != "" {
				got += " "
			}
			got += fmt.Sprint(v)
		}
		rows.Close()
		cdb.Close()
		if got != c.want {
			t.Errorf("[%d] C SQLite read back %s, want %s (on-disk record layout wrong?)", i, got, c.want)
		}
	}
}
