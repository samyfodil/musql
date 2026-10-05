// Tests MATCH operand evaluation in fts3/fts4/fts5 virtual table constraints.
// The pattern argument to MATCH must not reference columns from the matched
// table; if it does, that is an error, not a wrong answer.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// matchOperandPair is one differential session over two independent files.
type matchOperandPair struct {
	t   *testing.T
	go_ *sql.DB
	cgo *sql.DB
}

func matchOperandOpen(t *testing.T, setup ...string) *matchOperandPair {
	t.Helper()
	dir := t.TempDir()
	godb, err := sql.Open("sqlite", filepath.Join(dir, "go.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite): %v", err)
	}
	t.Cleanup(func() { godb.Close() })
	godb.SetMaxOpenConns(1)
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	t.Cleanup(func() { cgodb.Close() })
	cgodb.SetMaxOpenConns(1)
	p := &matchOperandPair{t: t, go_: godb, cgo: cgodb}
	for _, s := range setup {
		_, gerr := godb.Exec(s)
		_, cerr := cgodb.Exec(s)
		if (gerr == nil) != (cerr == nil) {
			t.Fatalf("setup %s disagrees\n  musql: %v\n  cgo: %v", s, gerr, cerr)
		}
	}
	return p
}

// bothReject requires the statement to be REFUSED by both engines. It is not
// enough that musql errors: the table must be untouched afterwards, which is
// the half a "returns an error somewhere" check would have missed while a
// partial write had already landed.
func (p *matchOperandPair) bothReject(stmt, verify string) {
	p.t.Helper()
	_, gerr := p.go_.Exec(stmt)
	_, cerr := p.cgo.Exec(stmt)
	if cerr == nil {
		p.t.Fatalf("%s: the ORACLE accepted it -- this test's premise is wrong", stmt)
	}
	if gerr == nil {
		p.t.Errorf("%s: musql ACCEPTED a statement C SQLite refuses (%v)", stmt, cerr)
	}
	p.agreeQuery(verify)
}

// agreeQuery requires identical rows (or a matching accept/reject verdict).
func (p *matchOperandPair) agreeQuery(q string) {
	p.t.Helper()
	g, gerr := matchOperandRows(p.go_, q)
	c, cerr := matchOperandRows(p.cgo, q)
	if (gerr == nil) != (cerr == nil) {
		p.t.Errorf("%s: one side errored\n  musql: %v\n  cgo: %v", q, gerr, cerr)
		return
	}
	if gerr != nil {
		return
	}
	if g != c {
		p.t.Errorf("%s DIVERGES\n  musql: %s\n  cgo:    %s", q, g, c)
	}
}

// agreeExec requires the same accept/reject verdict and the same table state.
func (p *matchOperandPair) agreeExec(stmt, verify string) {
	p.t.Helper()
	_, gerr := p.go_.Exec(stmt)
	_, cerr := p.cgo.Exec(stmt)
	if (gerr == nil) != (cerr == nil) {
		p.t.Errorf("%s: one side errored\n  musql: %v\n  cgo: %v", stmt, gerr, cerr)
		return
	}
	p.agreeQuery(verify)
}

func matchOperandRows(db *sql.DB, q string) (string, error) {
	rows, err := db.Query(q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, cerr := rows.Columns()
	if cerr != nil {
		return "", cerr
	}
	var b strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if serr := rows.Scan(ptrs...); serr != nil {
			return "", serr
		}
		for i, v := range vals {
			if bs, ok := v.([]byte); ok {
				vals[i] = string(bs)
			}
		}
		fmt.Fprintf(&b, "%v|", vals)
	}
	return b.String(), rows.Err()
}

// matchOperandModules is every fts module this file runs each case against.
// fts5 is behind the harness's own build tag (see fts5_diff_test.go), so the
// list is assembled from harnessFTS5 rather than written twice.
func matchOperandModules() []string {
	if harnessFTS5 {
		return []string{"fts5", "fts4"}
	}
	return []string{"fts4"}
}

// TestMatchOperandMayNotReadTheMatchedTable is the wrong-answer gate:
// whereexpr.c:1543 refuses to lift out a MATCH whose query reads the MATCHed
// table's own columns, so the surviving match() call raises. Every statement
// here must be REFUSED and must leave the table exactly as it was.
func TestMatchOperandMayNotReadTheMatchedTable(t *testing.T) {
	for _, mod := range matchOperandModules() {
		t.Run(mod, func(t *testing.T) {
			p := matchOperandOpen(t,
				`CREATE VIRTUAL TABLE ft USING `+mod+`(a)`,
				`INSERT INTO ft(rowid,a) VALUES(1,'alpha'),(2,'beta'),(3,'gamma')`,
			)
			verify := `SELECT rowid, a FROM ft ORDER BY rowid`
			p.agreeQuery(verify)
			// A SELECT reaches it through the compiled route...
			p.agreeQuery(`SELECT rowid FROM ft WHERE ft MATCH a`)
			// ...and these two through the write path, which is where the
			// silent destruction lived.
			p.bothReject(`DELETE FROM ft WHERE ft MATCH a`, verify)
			p.bothReject(`UPDATE ft SET a='x' WHERE ft MATCH a`, verify)
		})
	}
}

// TestMatchOperandEvaluatedBeforeTheTarget pins the coding ORDER: the pattern
// is match()'s argument 0 (parse.y:1363-1371), so its own error surfaces
// rather than "unable to use function MATCH". Both engines must report the
// SAME KIND of failure -- the assertion is on the operand's error text, which
// is what tells the two orders apart.
//
// fts5 only. The fts3/fts4 arm refuses an unusable MATCH at COMPILE time
// (checkFts3Match, engine/fts3_search.go) -- which it must, because that is
// the only moment at which a malformed query over an EMPTY fts3 table can be
// rejected the way C fts3's per-outer-row xFilter rejects it -- so its
// refusal precedes the operand's own error and "SELECT f3 MATCH
// abs(<overflow>) FROM f3" reports the MATCH error where 3.53.3 reports
// "integer overflow". Both engines REJECT, so it is an error-text difference
// rather than a divergence, and reordering it would trade this text for the
// empty-table one. Recorded, deliberately not asserted.
func TestMatchOperandEvaluatedBeforeTheTarget(t *testing.T) {
	if !harnessFTS5 {
		t.Skip("fts5 only -- see this test's doc comment for why the fts3/fts4 arm refuses earlier")
	}
	for _, mod := range []string{"fts5"} {
		t.Run(mod, func(t *testing.T) {
			p := matchOperandOpen(t,
				`CREATE VIRTUAL TABLE ft USING `+mod+`(a)`,
				`INSERT INTO ft(rowid,a) VALUES(1,'alpha')`,
			)
			for _, q := range []string{
				`SELECT 'lit' MATCH abs(-9223372036854775807-1)`,
				`SELECT ft MATCH abs(-9223372036854775807-1) FROM ft`,
			} {
				_, gerr := matchOperandRows(p.go_, q)
				_, cerr := matchOperandRows(p.cgo, q)
				if cerr == nil || !strings.Contains(cerr.Error(), "integer overflow") {
					t.Fatalf("%s: the ORACLE did not report the operand's own error (%v) -- premise is wrong", q, cerr)
				}
				if gerr == nil {
					t.Errorf("%s: musql accepted it, want %v", q, cerr)
					continue
				}
				if !strings.Contains(gerr.Error(), "integer overflow") {
					t.Errorf("%s: musql reports %v, want the operand's own \"integer overflow\" -- "+
						"the MATCH target is being resolved before its pattern is evaluated", q, gerr)
				}
			}
		})
	}
}

// TestMatchOperandIsPerRow is fts3join.test's own 1.1 shape: the query string
// comes from ANOTHER table's column, so each outer row must be answered with
// ITS OWN query. An operand evaluated once -- hoisted out of the loop, cached
// on the opcode's payload, or folded at compile time -- collapses this to a
// single repeated answer.
func TestMatchOperandIsPerRow(t *testing.T) {
	for _, mod := range matchOperandModules() {
		t.Run(mod, func(t *testing.T) {
			p := matchOperandOpen(t,
				`CREATE VIRTUAL TABLE ft USING `+mod+`(a)`,
				`INSERT INTO ft(rowid,a) VALUES(1,'one'),(2,'two'),(3,'three')`,
				`CREATE TABLE q(id INTEGER PRIMARY KEY, y)`,
				`INSERT INTO q VALUES(1,'one'),(2,'two'),(3,'three')`,
			)
			p.agreeQuery(`SELECT q.id, ft.rowid FROM ft, q WHERE ft MATCH q.y ORDER BY q.id`)
			p.agreeQuery(`SELECT q.id, ft.rowid FROM q, ft WHERE ft MATCH q.y ORDER BY q.id`)
		})
	}
}

// TestMatchOperandWritePathShapes covers the operand forms a write-path WHERE
// can carry over a MATCH, all of which go through a COMPILED FROM-less select
// today -- the seam that used to evaluate them per row (writeRowSelected,
// engine/vtab_write.go) now only declines.
// Each runs on its own pair so an earlier delete cannot colour a later one.
func TestMatchOperandWritePathShapes(t *testing.T) {
	if !harnessFTS5 {
		t.Skip("the fts4 write path declines a MATCH in WHERE outright; the fts5 one is the route this covers")
	}
	for _, stmt := range []string{
		`DELETE FROM ft WHERE ft MATCH 'alpha'`,
		`DELETE FROM ft WHERE ft MATCH 'al'||'pha'`,
		`DELETE FROM ft WHERE ft MATCH upper('ALPHA')`,
		`DELETE FROM ft WHERE ft MATCH (SELECT 'beta')`,
		`DELETE FROM ft WHERE ft MATCH CAST('gamma' AS TEXT)`,
		// A NULL query is NOT "matches nothing" for fts5: fts5FilterMethod
		// substitutes the empty string for it (fts5_main.c:1504,
		// "        if( zText==0 ) zText = \"\";") and the query parser then
		// rejects it, so BOTH engines must refuse this. It answered "deleted
		// nothing, success" here until this engine stopped special-casing a
		// NULL query.
		`DELETE FROM ft WHERE ft MATCH NULL`,
		`UPDATE ft SET a='x' WHERE ft MATCH 'beta'`,
	} {
		t.Run(stmt, func(t *testing.T) {
			p := matchOperandOpen(t,
				`CREATE VIRTUAL TABLE ft USING fts5(a)`,
				`INSERT INTO ft(rowid,a) VALUES(1,'alpha'),(2,'beta'),(3,'gamma')`,
			)
			p.agreeExec(stmt, `SELECT rowid, a FROM ft ORDER BY rowid`)
		})
	}
}

// TestNotMatchIsNotALiftableConstraint gates a wrong answer found beside the
// operand work, in both modules. "X NOT MATCH Y" is not a MATCH carrying a
// flag: parse.y's likeop rule marks the NOT on the token (parse.y:1362) and
// then WRAPS the whole call --
//
//	if( bNot ) A = sqlite3PExpr(pParse, TK_NOT, A, 0);   /* parse.y:1370 */
//
// -- producing the identical tree to "NOT (X MATCH Y)", which exprAnalyze's
// constraint-lifting arm never sees (whereexpr.c:1533 requires the conjunct
// itself to be the operator). The match() stub raises instead, per row.
//
// This engine parses the two spellings into DIFFERENT trees (one node with a
// flag vs a wrapping NOT), and only the second was refused: "SELECT rowid FROM
// ft WHERE ft NOT MATCH 'beta'" answered the two non-matching rows, over fts5
// AND fts4. Both spellings must agree with the oracle, which rejects both.
func TestNotMatchIsNotALiftableConstraint(t *testing.T) {
	for _, mod := range matchOperandModules() {
		t.Run(mod, func(t *testing.T) {
			p := matchOperandOpen(t,
				`CREATE VIRTUAL TABLE ft USING `+mod+`(a)`,
				`INSERT INTO ft(rowid,a) VALUES(1,'alpha'),(2,'beta'),(3,'gamma')`,
			)
			for _, q := range []string{
				`SELECT rowid FROM ft WHERE ft NOT MATCH 'beta'`,
				`SELECT rowid FROM ft WHERE NOT (ft MATCH 'beta')`,
				`SELECT ft NOT MATCH 'beta' FROM ft`,
				`SELECT rowid FROM ft WHERE ft MATCH 'beta' AND NOT (ft MATCH 'alpha')`,
			} {
				_, gerr := matchOperandRows(p.go_, q)
				_, cerr := matchOperandRows(p.cgo, q)
				if cerr == nil {
					t.Fatalf("%s: the ORACLE accepted it -- this test's premise is wrong", q)
				}
				if gerr == nil {
					t.Errorf("%s: musql answered rows where C SQLite reports %v", q, cerr)
				}
			}
		})
	}
}
