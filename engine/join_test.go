package engine_test

import (
	"github.com/samyfodil/musql/engine"

	"database/sql"
	"path/filepath"
	"testing"
)

// buildJoinTestDB creates three small tables for join executor testing.
func buildJoinTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "join.sqlite")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	exec(`CREATE TABLE t1 (id INTEGER PRIMARY KEY, name TEXT, val INTEGER)`)
	for _, r := range []struct {
		id   int64
		name string
		val  int64
	}{
		{1, "Alice", 10},
		{2, "Bob", 20},
		{3, "Carol", 30},
		{4, "Dave", 40},
	} {
		exec(`INSERT INTO t1 (id, name, val) VALUES (?,?,?)`, r.id, r.name, r.val)
	}

	exec(`CREATE TABLE t2 (id INTEGER PRIMARY KEY, t1_id INTEGER, label TEXT, amount REAL)`)
	for _, r := range []struct {
		id     int64
		t1ID   any
		label  string
		amount float64
	}{
		{1, int64(1), "A1", 50.0},
		{2, int64(1), "A2", 150.0},
		{3, int64(2), "B1", 75.0},
		{4, nil, "Orphan", 5.0},
	} {
		exec(`INSERT INTO t2 (id, t1_id, label, amount) VALUES (?,?,?,?)`, r.id, r.t1ID, r.label, r.amount)
	}

	exec(`CREATE TABLE t3 (id INTEGER PRIMARY KEY, t1_id INTEGER, tag TEXT)`)
	for _, r := range []struct {
		id   int64
		t1ID int64
		tag  string
	}{
		{1, 1, "x"},
		{2, 2, "y"},
		{3, 3, "z"},
	} {
		exec(`INSERT INTO t3 (id, t1_id, tag) VALUES (?,?,?)`, r.id, r.t1ID, r.tag)
	}

	return path, db
}

// TestJoinMatchesReference is the join executor's differential gate: every
// query below is run through both the pure-Go engine (join.go/query.go) and
// a reference
// (mustMatchGroup, reused as-is from group_test.go -- its row-order-
// insensitive-unless-ORDER-BY comparison is exactly what an unordered join
// result needs too), and must match exactly.
func TestJoinMatchesReference(t *testing.T) {
	path, db := buildJoinTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		// Comma joins (== CROSS JOIN, filtered by WHERE): 2 and 3 tables.
		`SELECT t1.name, t2.label FROM t1, t2 WHERE t1.id = t2.t1_id`,
		`SELECT t1.name, t2.label, t3.tag FROM t1, t2, t3 WHERE t1.id = t2.t1_id AND t1.id = t3.t1_id`,

		// Explicit CROSS JOIN behaves identically to a comma join.
		`SELECT t1.name, t2.label FROM t1 CROSS JOIN t2 WHERE t1.id = t2.t1_id`,

		// [INNER] JOIN ... ON.
		`SELECT t1.name, t2.label FROM t1 JOIN t2 ON t1.id = t2.t1_id`,
		`SELECT t1.name, t2.label FROM t1 INNER JOIN t2 ON t1.id = t2.t1_id WHERE t2.amount > 60`,
		`SELECT t1.name, t2.label FROM t1 JOIN t2 ON t1.id = t2.t1_id ORDER BY t1.name, t2.label`,

		// LEFT JOIN ON: every t1 row appears; ids 3/4 (no matching t2 row at
		// all) NULL-extend every t2 column.
		`SELECT t1.id, t1.name, t2.label, t2.amount FROM t1 LEFT JOIN t2 ON t1.id = t2.t1_id ORDER BY t1.id, t2.label`,

		// ON-vs-WHERE distinction for LEFT JOIN, the classic trap:
		//   (a) condition inside ON: id=1 keeps only its amount>100 match
		//       (label 'A2'); id=2's only candidate (amount=75) fails ON, so
		//       it NULL-extends (label NULL) rather than disappearing; ids
		//       3/4 NULL-extend as always. Four rows total (one per t1 row).
		`SELECT t1.id, t2.label FROM t1 LEFT JOIN t2 ON t1.id = t2.t1_id AND t2.amount > 100 ORDER BY t1.id`,
		//   (b) the same condition moved to WHERE: a real join happens first
		//       (id=1 gets both candidate rows, id=2 gets its one row, ids
		//       3/4 NULL-extend), then WHERE amount>100 filters the result:
		//       id=1's amount=50 row and id=2's amount=75 row are dropped
		//       outright (not NULL-extended -- they were real matches that
		//       WHERE simply excluded), and ids 3/4's NULL amount fails
		//       "NULL > 100" (NULL, not true) so they're dropped too. Only
		//       id=1's amount=150 row survives.
		`SELECT t1.id, t2.label FROM t1 LEFT JOIN t2 ON t1.id = t2.t1_id WHERE t2.amount > 100`,
		// A WHERE clause that specifically targets the NULL-extended rows
		// (only reachable if NULL-extension genuinely happened, since t2.label
		// is never NULL in any real row): exactly ids 3 and 4.
		`SELECT t1.id FROM t1 LEFT JOIN t2 ON t1.id = t2.t1_id WHERE t2.label IS NULL ORDER BY t1.id`,

		// Join + aggregate (no GROUP BY) and join + GROUP BY.
		`SELECT count(*), sum(t2.amount) FROM t1 JOIN t2 ON t1.id = t2.t1_id`,
		`SELECT t1.name, count(*) FROM t1 JOIN t2 ON t1.id = t2.t1_id GROUP BY t1.name ORDER BY t1.name`,
		`SELECT t1.name, sum(t2.amount) FROM t1 JOIN t2 ON t1.id = t2.t1_id GROUP BY t1.name ORDER BY t1.name`,
		`SELECT t1.name, count(*) FROM t1 LEFT JOIN t2 ON t1.id = t2.t1_id GROUP BY t1.name ORDER BY t1.name`,

		// Qualified column resolution: both t1 and t2 have an "id" column;
		// qualifying disambiguates.
		`SELECT t1.id, t2.id FROM t1, t2 WHERE t1.id = t2.t1_id ORDER BY t1.id, t2.id`,

		// Self-join: every (a,b) pair of distinct t1 rows with a.val < b.val.
		`SELECT a.name, b.name FROM t1 AS a JOIN t1 AS b ON a.val < b.val ORDER BY a.name, b.name`,
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatchGroup(t, p, db, q)
		})
	}
}

// TestJoinUsingNaturalMatchesReference is the differential gate for JOIN ...
// USING(...) and NATURAL JOIN (join.go's desugarJoinItem, query.go's
// expandSelectList/resolveColumn output-coalescing): every query below is
// run through both the pure-Go engine and a reference, and must match exactly -- including column NAMES and ORDER
// (mustMatchGroup checks both), which is where a coalescing bug would show
// up first (a duplicated or misplaced "id" column). t1/t2/t3 each have their
// OWN "id" INTEGER PRIMARY KEY (unrelated to each other business-logic-wise,
// but exactly what's needed to exercise USING/NATURAL mechanically); t2 and
// t3 additionally share a "t1_id" column name with EACH OTHER that is never
// named in a USING clause below, so it deliberately stays un-coalesced
// (appearing twice in a 3-way join's "*") -- proof that coalescing only ever
// hides the SPECIFIC named/common columns, never every same-named column.
func TestJoinUsingNaturalMatchesReference(t *testing.T) {
	path, db := buildJoinTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		// USING: coalesced "*" (id shown once, in t1's position; t2's other
		// columns follow) and an unqualified common-column reference.
		`SELECT * FROM t1 JOIN t2 USING(id)`,
		`SELECT id, name, t1_id, label FROM t1 JOIN t2 USING(id)`,
		`SELECT t1.id, t2.id FROM t1 JOIN t2 USING(id)`,
		`SELECT * FROM t1 JOIN t2 USING(id) ORDER BY id`,
		`SELECT * FROM t1 JOIN t2 USING(id) WHERE amount > 60 ORDER BY id`,
		`SELECT id, count(*) FROM t1 JOIN t2 USING(id) GROUP BY id ORDER BY id`,

		// LEFT JOIN USING: the common column is always the LEFT side's value.
		`SELECT * FROM t1 LEFT JOIN t2 USING(id) ORDER BY id`,

		// NATURAL JOIN: t1/t2's only common column is "id" -- identical
		// condition/coalescing to USING(id) above.
		`SELECT * FROM t1 NATURAL JOIN t2`,
		`SELECT * FROM t1 NATURAL JOIN t2 ORDER BY id`,
		`SELECT * FROM t1 NATURAL LEFT JOIN t2 ORDER BY id`,

		// 3-table chain: "id" coalesces across all three (t3.id hidden too,
		// t1.id remains the single representative); t2.t1_id/t3.t1_id are
		// NOT named in any USING clause, so both survive in "*" untouched.
		`SELECT * FROM t1 JOIN t2 USING(id) JOIN t3 USING(id) ORDER BY id`,
		`SELECT * FROM t1 NATURAL JOIN t2 NATURAL JOIN t3 ORDER BY id`,
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatchGroup(t, p, db, q)
		})
	}
}

// TestJoinUsingNaturalStillRejectsCombinations pins the parse-time rejections
// C SQLite also applies: NATURAL may never be combined with an explicit
// ON or USING clause on the same join.
func TestJoinUsingNaturalStillRejectsCombinations(t *testing.T) {
	path, _ := buildJoinTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, q := range []string{
		`SELECT * FROM t1 NATURAL JOIN t2 ON t1.id = t2.id`,
		`SELECT * FROM t1 NATURAL JOIN t2 USING (id)`,
		`SELECT * FROM t1 JOIN t2 USING (no_such_column)`,
	} {
		t.Run(q, func(t *testing.T) {
			mustError(t, p, q, "NATURAL+ON, NATURAL+USING, and a USING column absent from a side are all rejected")
		})
	}
}

// TestJoinAmbiguousColumnErrors asserts that an unqualified column present in
// more than one joined table is rejected as ambiguous -- never silently
// resolved to one of them by guessing -- exactly SQLite's own rule.
func TestJoinAmbiguousColumnErrors(t *testing.T) {
	path, _ := buildJoinTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, q := range []string{
		`SELECT id FROM t1, t2`,
		`SELECT id FROM t1 JOIN t2 ON t1.id = t2.t1_id`,
		`SELECT t1.name, id FROM t1, t2 WHERE t1.id = t2.t1_id`,
	} {
		t.Run(q, func(t *testing.T) {
			mustError(t, p, q, "id is ambiguous: both t1 and t2 have a column named id")
		})
	}
}
