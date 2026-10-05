package compat

import (
	"encoding/json"
	"testing"
)

// Tests INSTEAD OF triggers on views whose bodies contain CTEs.
// View bodies see their own WITH and nothing else. Column metadata must be
// bound in the correct scope so INSTEAD OF trigger NEW references are correct.
func TestViewCTEInsteadOfScope(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		// The finding: the body's CTE shadows a same-named base table, and an
		// INSTEAD OF INSERT trigger names the CTE's columns.
		{"shadowing-cte-instead-of", []string{
			`CREATE TABLE t1(x, y)`,
			`INSERT INTO t1 VALUES(1, 2)`,
			`CREATE TABLE log(a, b)`,
			`CREATE VIEW v AS WITH t1(p, q) AS (SELECT 9, 9) SELECT * FROM t1`,
			`CREATE TRIGGER tr INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(NEW.p, NEW.q); END`,
			`INSERT INTO v VALUES(7, 8)`,
			`SELECT a, b FROM log`,
			`SELECT * FROM v`,
		}},
		// Same shape, CTE named so it shadows nothing. Intended as a control
		// isolating the SHADOWING spelling -- it turned out to be a second
		// finding: this failed identically before the fix, because with no
		// scope pushed at all the body's "FROM cc" resolved to NOTHING, so
		// "INSERT INTO v VALUES(7,8)" errored outright and the trigger body
		// wrote no row. The bug was every INSTEAD OF trigger over a view whose
		// body carries a WITH, not just a shadowing one.
		{"non-shadowing-cte-instead-of", []string{
			`CREATE TABLE t1(x, y)`,
			`CREATE TABLE log(a, b)`,
			`CREATE VIEW v AS WITH cc(p, q) AS (SELECT 9, 9) SELECT * FROM cc`,
			`CREATE TRIGGER tr INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(NEW.p, NEW.q); END`,
			`INSERT INTO v VALUES(7, 8)`,
			`SELECT a, b FROM log`,
		}},
		// The BARRIER half: an enclosing WITH must NOT reach into the view
		// body, so the body's "FROM t1" is the base table and the view's
		// columns are x,y -- not the outer CTE's p,q.
		{"outer-cte-must-not-reach-body", []string{
			`CREATE TABLE t1(x, y)`,
			`INSERT INTO t1 VALUES(1, 2)`,
			`CREATE VIEW v AS SELECT * FROM t1`,
			`WITH t1(p, q) AS (SELECT 9, 9) SELECT * FROM v`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mus := run(t, "musql", tc.stmts)
			cgo := run(t, "cgo", tc.stmts)
			for i, s := range tc.stmts {
				mb, _ := json.Marshal(mus[i])
				cb, _ := json.Marshal(cgo[i])
				if string(mb) != string(cb) {
					t.Errorf("%s\n  stmt:   %s\n  oracle: %s\n  engine: %s\n"+
						"  A view body sees its own WITH and nothing else (select.c:5991/:5627).\n"+
						"  Every site that re-derives a view's column metadata from the AST must\n"+
						"  re-enter that scope -- viewColumnInfos (engine/view_trigger.go) is the\n"+
						"  one this case covers.", tc.name, s, cb, mb)
				}
			}
		})
	}
}
