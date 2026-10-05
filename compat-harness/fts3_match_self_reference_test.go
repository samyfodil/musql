// Tests that self-referential MATCH expressions (where the pattern reads from
// the same FTS table being matched) are rejected, matching C SQLite behavior.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/samyfodil/musql/engine"
)

// TestFts3MatchSelfReferenceDeclines verifies that all self-referencing MATCH
// shapes are properly declined.
func TestFts3MatchSelfReferenceDeclines(t *testing.T) {
	bad := []struct {
		name  string
		setup []string
		query string
	}{
		{"fts4, pattern reads the SAME column MATCH targets", []string{
			`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
			`INSERT INTO t10 VALUES ('apple'),('banana')`,
		}, `SELECT * FROM t10 WHERE t10 MATCH t10.value`},
		{"fts3 (not just fts4), same shape", []string{
			`CREATE VIRTUAL TABLE t10 USING fts3(value)`,
			`INSERT INTO t10 VALUES ('apple'),('banana')`,
		}, `SELECT * FROM t10 WHERE t10 MATCH t10.value`},
		{"pattern reads a DIFFERENT column of the SAME table", []string{
			`CREATE VIRTUAL TABLE t10 USING fts4(value, other)`,
			`INSERT INTO t10 VALUES ('apple','banana'),('banana','apple')`,
		}, `SELECT * FROM t10 WHERE t10 MATCH t10.other`},
		{"pattern reads the table's own hidden docid column", []string{
			`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
			`INSERT INTO t10 VALUES ('apple'),('banana')`,
		}, `SELECT * FROM t10 WHERE t10 MATCH t10.docid`},
		{"unqualified pattern, single-scope statement", []string{
			`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
			`INSERT INTO t10 VALUES ('apple'),('banana')`,
		}, `SELECT * FROM t10 WHERE t10 MATCH value`},
		{"self-reference inside a JOIN's WHERE clause", []string{
			`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
			`INSERT INTO t10 VALUES ('apple'),('banana')`,
			`CREATE TABLE tt(id)`,
			`INSERT INTO tt VALUES(1),(2)`,
		}, `SELECT * FROM tt, t10 WHERE t10 MATCH t10.value`},
		{"self-reference inside a JOIN's own ON clause", []string{
			`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
			`INSERT INTO t10 VALUES ('apple'),('banana')`,
			`CREATE TABLE tt(id)`,
			`INSERT INTO tt VALUES(1),(2)`,
		}, `SELECT * FROM tt JOIN t10 ON t10 MATCH t10.value`},
		{"OR of a self-referencing leaf with an otherwise-fine literal leaf", []string{
			`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
			`INSERT INTO t10 VALUES ('apple'),('banana')`,
		}, `SELECT * FROM t10 WHERE t10 MATCH t10.value OR t10 MATCH 'apple'`},
		{"a self-ref scope alongside an otherwise-well-bound different scope", []string{
			`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
			`INSERT INTO t10 VALUES ('apple'),('banana')`,
			`CREATE VIRTUAL TABLE t11 USING fts4(value)`,
			`INSERT INTO t11 VALUES ('apple'),('banana')`,
		}, `SELECT * FROM t10, t11 WHERE t10 MATCH t10.value AND t11 MATCH 'apple'`},
	}
	for _, c := range bad {
		c := c
		t.Run(c.name, func(t *testing.T) {
			db := openMusqlFts(t)
			for _, s := range c.setup {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			rows, err := db.Query(c.query)
			if err == nil {
				iterErr := rows.Err()
				for rows.Next() {
				}
				if iterErr == nil {
					iterErr = rows.Err()
				}
				rows.Close()
				err = iterErr
			}
			if err == nil {
				t.Fatalf("engine ACCEPTED a self-referencing MATCH the oracle refuses: %s", c.query)
			}
		})
	}
}

// TestFts3MatchSelfReferenceCrossTableStillServed is the regression half:
// the fix must not touch a pattern that genuinely reads a DIFFERENT table's
// column ("ft1 MATCH y"), which the per-row pattern register still
// correctly serves
// per row -- fts3join.test's own shape, already gated elsewhere, re-pinned
// here beside the negative cases above so the two live in one place.
func TestFts3MatchSelfReferenceCrossTableStillServed(t *testing.T) {
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatalf("open cgo: %v", err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1)
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer edb.Close()

	setup := []string{
		`CREATE VIRTUAL TABLE ft1 USING fts4(x)`,
		`INSERT INTO ft1 VALUES('abc'),('def')`,
		`CREATE TABLE t1(y)`,
		`INSERT INTO t1 VALUES('abc')`,
	}
	for _, s := range setup {
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine setup %q: %v", s, err)
		}
	}
	const q = `SELECT ft1.x FROM ft1, t1 WHERE ft1 MATCH y`

	crows, cerr := cdb.Query(q)
	if cerr != nil {
		t.Fatalf("oracle rejected a genuinely cross-table non-literal MATCH: %v", cerr)
	}
	var cgoCount int
	for crows.Next() {
		cgoCount++
	}
	crows.Close()

	p, perr := edb.SnapshotPager()
	if perr != nil {
		t.Fatalf("SnapshotPager: %v", perr)
	}
	defer p.Close()
	_, erows, eqerr := p.QueryArgs(q, nil)
	if eqerr != nil {
		t.Fatalf("engine regressed: a genuinely cross-table non-literal MATCH now errors: %v", eqerr)
	}
	if len(erows) != cgoCount {
		t.Fatalf("row count mismatch: engine=%d cgo=%d", len(erows), cgoCount)
	}
}

// TestFts3MatchSelfReferenceErrorText pins the exact error text -- not just
// accept/reject -- against the live oracle for the bug's own headline shape,
// since differ() (used by the rest of this package) only ever compares
// whether BOTH sides errored, never which text either side produced.
func TestFts3MatchSelfReferenceErrorText(t *testing.T) {
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatalf("open cgo: %v", err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1)
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer edb.Close()

	setup := []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 VALUES ('apple'),('banana')`,
	}
	for _, s := range setup {
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine setup %q: %v", s, err)
		}
	}
	const q = `SELECT * FROM t10 WHERE t10 MATCH t10.value`
	const wantText = "unable to use function MATCH in the requested context"

	crows, cerr := cdb.Query(q)
	if cerr == nil {
		for crows.Next() {
		}
		cerr = crows.Err()
		crows.Close()
	}
	if cerr == nil || !strings.Contains(cerr.Error(), wantText) {
		t.Fatalf("oracle's own error text changed, re-measure: %v", cerr)
	}

	p, perr := edb.SnapshotPager()
	if perr != nil {
		t.Fatalf("SnapshotPager: %v", perr)
	}
	defer p.Close()
	_, _, eqerr := p.QueryArgs(q, nil)
	if eqerr == nil {
		t.Fatalf("engine ACCEPTED the self-referencing MATCH -- the bug this file guards against")
	}
	if !strings.Contains(eqerr.Error(), wantText) {
		t.Fatalf("engine error text does not match the oracle's sqlite3InvalidFunction message:\n  got:  %v\n  want substring: %s", eqerr, wantText)
	}
}
