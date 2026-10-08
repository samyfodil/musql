package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestAutomaticSeekMatchesTheScan: an equality on a column with no SQL index
// seeks the segments' own per-column equality index (automaticSeekCandidates)
// -- C's automatic index without building one. Each query is compared with the
// same SQL planned without automatic candidates, across the shapes where
// "equal" is subtle: affinity between classes, a NOCASE column, NULLs, a
// column of no affinity holding every class, constant and correlated keys,
// and rows the log holds.
func TestAutomaticSeekMatchesTheScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.musq")
	stmts := []string{
		`CREATE TABLE b (id INTEGER PRIMARY KEY, txt TEXT, r REAL, n NUMERIC, any_, nc TEXT COLLATE NOCASE)`,
		`CREATE TABLE t (id INTEGER PRIMARY KEY, k, s TEXT)`,
	}
	vals := []string{"1", "'1'", "1.0", "1.5", "'1.0'", "'abc'", "'ABC'", "NULL", "x'31'", "2", "'2'", "-3", "' 4'", "4"}
	for i := 0; i < 600; i++ {
		v := vals[i%len(vals)]
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO b VALUES (%d, %s, %s, %s, %s, %s)`, i+1, v, v, v, v, v))
	}
	for i := 0; i < 300; i++ {
		v := vals[(i*5)%len(vals)]
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES (%d, %s, %s)`, i+1, v, v))
	}
	// Homogeneous columns, which the segments CAN index, probed with keys of
	// another class: TEXT holding only digit strings against integer keys
	// (TEXT affinity turns the key into text), REAL against integers, NOCASE
	// text against mixed case.
	stmts = append(stmts, `CREATE TABLE h (id INTEGER PRIMARY KEY, dt TEXT, rr REAL, ci TEXT COLLATE NOCASE)`,
		`CREATE TABLE hk (id INTEGER PRIMARY KEY, ik INTEGER, sk TEXT, dk TEXT)`)
	for i := 0; i < 500; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO h VALUES (%d, '%d', %d.0, 'Name%d')`, i+1, i%40, i%40, i%40))
	}
	for i := 0; i < 200; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO hk VALUES (%d, %d, 'NAME%d', '%d')`, i+1, i%50, i%50, i%50))
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)
	queries := []string{
		`SELECT count(*) FROM hk WHERE EXISTS (SELECT 1 FROM h WHERE h.dt = hk.ik)`,
		`SELECT count(*) FROM hk WHERE EXISTS (SELECT 1 FROM h WHERE h.rr = hk.ik)`,
		`SELECT count(*) FROM hk WHERE EXISTS (SELECT 1 FROM h WHERE h.ci = hk.sk)`,
		`SELECT hk.id, (SELECT count(*) FROM h WHERE h.dt = hk.ik) FROM hk ORDER BY hk.id`,
		// Same class on both sides: served.
		`SELECT count(*) FROM hk WHERE EXISTS (SELECT 1 FROM h WHERE h.dt = hk.dk)`,
		`SELECT hk.id, (SELECT count(*) FROM h WHERE h.dt = hk.dk) FROM hk ORDER BY hk.id`,
		`SELECT id FROM h WHERE dt = 7`,
		`SELECT id FROM h WHERE dt = '7'`,
		`SELECT id FROM h WHERE rr = 7`,
		`SELECT id FROM h WHERE rr = 7.0`,
		`SELECT id FROM h WHERE ci = 'NAME7'`,
	}
	for _, col := range []string{"txt", "r", "n", "any_", "nc"} {
		queries = append(queries,
			fmt.Sprintf(`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.%s = t.k)`, col),
			fmt.Sprintf(`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE t.s = b.%s)`, col),
			fmt.Sprintf(`SELECT t.id, (SELECT count(*) FROM b WHERE b.%s = t.k) FROM t ORDER BY t.id`, col))
		for _, v := range vals {
			queries = append(queries, fmt.Sprintf(`SELECT id FROM b WHERE %s = %s`, col, v))
		}
	}
	run := func(stage string) {
		segIndexSeeksServed = 0
		defer func() {
			if segIndexSeeksServed == 0 {
				t.Errorf("%s: no seek was served, so every query compared a scan with a scan", stage)
			}
		}()
		for _, q := range queries {
			got, err := queryDB(t, path, q)
			if err != nil {
				t.Fatalf("%s %s: %v", stage, q, err)
			}
			automaticSeeksOffForTest = true
			want, err := queryDB(t, path, q)
			automaticSeeksOffForTest = false
			if err != nil {
				t.Fatalf("%s %s without automatic seeks: %v", stage, q, err)
			}
			if got != want {
				t.Errorf("%s %s:\n seek %.200q\n scan %.200q", stage, q, got, want)
			}
		}
	}
	run("segments")
	execDB(t, path, `INSERT INTO b VALUES (9001, 1, 1, 1, 1, 'abc')`, `DELETE FROM b WHERE id = 3`, `UPDATE t SET k = 'ABC' WHERE id = 2`)
	run("with log")
}

// TestAutomaticSeekIsPlanned is the gate's non-vacuity check: with automatic
// candidates, an equality on an unindexed column compiles to an index seek,
// and without them it does not.
func TestAutomaticSeekIsPlanned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.musq")
	buildDB(t, path, `CREATE TABLE b (id INTEGER PRIMARY KEY, txt TEXT)`, `INSERT INTO b VALUES (1, 'a')`)
	execDB(t, path, `VACUUM`)
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	rp, err := n.ReadPager()
	if err != nil {
		t.Fatal(err)
	}
	seeks := func() bool {
		stmt, err := ParseSelect(`SELECT id FROM b WHERE txt = 'a'`)
		if err != nil {
			t.Fatal(err)
		}
		prog, err := compileSelectScan(rp, stmt, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range prog.Insns {
			if in.Op == OpSeekIndexHint {
				return true
			}
		}
		return false
	}
	if !seeks() {
		t.Fatal("an equality on an unindexed column compiled to no seek")
	}
	automaticSeeksOffForTest = true
	defer func() { automaticSeeksOffForTest = false }()
	if seeks() {
		t.Fatal("the reference plan seeks too, so the differential compares a seek with itself")
	}
}
