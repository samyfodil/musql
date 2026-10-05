package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// Error message text parity with C SQLite.
//
//	  parity claim can be made about at all;
//	- the STATEMENT CONTEXT this engine prepends to a write's errors
//	  ("INSERT into t: UNIQUE constraint failed: t.a" where C says just
//	  "UNIQUE constraint failed: t.a") -- 6 more;
//	- four individually wrong messages: EOF reported as an unexpected token
//	  rather than "incomplete input", trailing input not spelled as C's
//	  `near "X": syntax error`, the two-primary-key error not naming the
//	  table, and CREATE INDEX's missing table not schema-qualified.
//
// The last four shared one cause of their own: a genuine C-SQLite error
// classified errVDBEUnsupported rather than errVDBESemantic, so it reached
// the caller inside "VDBE-only: ... not compilable to bytecode ... (no
// fallback)" -- text C has no counterpart for, wrapped around the real
// diagnosis. Completing declineOrSemantic's migration (67 sites were still
// on the marker-dropping `fmt.Errorf("%w: %v", errVDBEUnsupported, err)`),
// classifying those errors as semantic, and giving the out-of-range ORDER BY
// term C's own "%r ORDER BY term out of range - should be between 1 and %d"
// wording closed three. The fourth was a wrong DIAGNOSIS rather than wrong
// wording: an aggregate reaching the SCALAR compiler means it was written
// where aggregates are not allowed, which C calls "misuse of aggregate:
// max()" and this called an arity error.
//
// The classes below were found by measuring shapes and fixing what differed:
//
//   - a syntax error at END OF INPUT is "incomplete input", whatever the
//     grammar was in the middle of (parse.y:44-51's %syntax_error action tests
//     the offending token's own text). This engine said what it EXPECTED
//     instead, which is more useful mid-statement and simply wrong at EOF:
//     "SELECT 1 FROM" was `expected table name, got "<end of input>"`.
//     Applied at the three entry points a caller can reach -- ParseSelect,
//     ParseParamInfo (the driver PREPARES through it) and ExecArgs.
//   - an AGGREGATE or WINDOW function in a context where that kind of call is
//     not allowed is resolve.c:1265-1277's "misuse of %s function %#T()", and
//     which WORD is its own test, "(pDef->funcFlags & SQLITE_FUNC_WINDOW) ||
//     pWin": a registered window function is a "window" function even with no
//     OVER clause, and an AGGREGATE written WITH one is too.
//   - four more errors C also makes at prepare time were still classified as
//     declines, so they reached the caller inside "VDBE-only: ... (no
//     fallback)": the USING column not in both tables (select.c:588-591), an
//     UPDATE SET target naming no column (update.c:494), a scalar subquery of
//     the wrong width (expr.c:3489-3501), and a width MISMATCH in a comparison,
//     which is the separate "row value misused" (expr.c:5617-5619).
//   - "?0" was "invalid parameter number"; expr.c:1349-1351 words it
//     "variable number must be between ?1 and ?32766" and applies the same
//     message to a number past the ceiling, which this engine accepted.
//
// A third round took the DDL half, which had its own two classes:
//
//   - the STATEMENT CONTEXT again, this time on the DDL verbs ("CREATE INDEX
//     i9: no such column: nosuch", "CREATE TABLE x AS SELECT: no such table:
//     nosuch"). C reports the resolver's message alone, so stripStatementContext
//     now strips those prefixes too -- " AS SELECT" included, since it is part
//     of the clause and not part of the name.
//   - and three messages reworded to C's: an index or table-constraint column
//     that names no column is resolve.c:785's "no such column: x" (C resolves
//     both by ordinary name resolution and names neither the index nor the
//     table); an unknown COLLATE is callback.c:230's "no such collation
//     sequence: x" (C's three built-ins are exactly the three this engine
//     accepts, so the two refuse the same names); and an EMPTY column list is a
//     plain syntax error at the first token inside the parens, since C's
//     grammar requires a column definition there.
//
// A savepoint-trio verb with its NAME missing joins the "incomplete input"
// class: parseTxnStmt reports those as simply not parsing and the write
// dispatch called them unsupported statements, where C's parser fails them at
// end of input.
//
// TWO of the 78 still diverge, both left rather than guessed at:
//
//   - "SELECT * FROM t, t" -- C's ambiguity names "main.t.a" because a
//     "*"-expanded reference in a multi-source FROM is built SCHEMA-qualified
//     (select.c:6269-6288) and resolve.c:787 prints all three parts. This
//     engine's scopes carry a dbIdx, not a schema NAME, and inventing "main"
//     for it would be wrong for a temp or attached source.
//   - "SELECT a AS z FROM t GROUP BY z HAVING nosuch" -- still a decline
//     ("resolves to no reachable scope"). Reporting C's "no such column" needs
//     bindAggOuterRefs to report WHICH reference failed, and that helper is
//     load-bearing for correlated aggregate binding; not worth touching for a
//     message.
func TestErrorTextParity(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT NOT NULL, c UNIQUE, d CHECK(d>0))`,
		`INSERT INTO t VALUES(1,'x',1,1)`,
		`CREATE TABLE s(x INT, y TEXT) STRICT`,
		`CREATE TABLE p(k INTEGER PRIMARY KEY)`,
		`CREATE VIEW v AS SELECT a FROM t`,
		`CREATE TABLE g(a, b AS (a*2))`,
		`CREATE INDEX ti ON t(b)`,
	}
	for _, q := range []string{
		`SELECT * FROM nosuch`,
		`SELECT nosuch FROM t`,
		`SELECT * FROM t WHERE`,
		`SELECT nosuchfn(1)`,
		`INSERT INTO t VALUES(1,'y',2,1)`,
		`INSERT INTO t(a,b,c,d) VALUES(2,NULL,3,1)`,
		`INSERT INTO t(a,b,c,d) VALUES(3,'z',1,1)`,
		`INSERT INTO t(a,b,c,d) VALUES(4,'z',4,-1)`,
		`INSERT INTO s VALUES('nope','y')`,
		`INSERT INTO t DEFAULT VALUES`,
		`INSERT INTO v VALUES(1)`,
		`UPDATE g SET b=1`,
		`DELETE FROM nosuch`,
		`DROP TABLE nosuch`,
		`DROP VIEW nosuch`,
		`DROP INDEX nosuch`,
		`CREATE TABLE t(z)`,
		`CREATE INDEX ti ON t(a)`,
		`CREATE INDEX i2 ON nosuch(a)`,
		`ALTER TABLE nosuch RENAME TO q`,
		`ALTER TABLE t RENAME COLUMN nosuch TO q`,
		`ALTER TABLE t ADD COLUMN a`,
		`ALTER TABLE t DROP COLUMN a`,
		`SELECT count(*) FROM t GROUP BY nosuch`,
		`SELECT * FROM t LIMIT 'x'`,
		`RELEASE nosuch`,
		`SELECT * FROM t INDEXED BY nosuch`,
		`CREATE TABLE q(a PRIMARY KEY, b PRIMARY KEY)`,
		`SELECT * FROM (SELECT 1) AS x(a,b)`,
		`SELECT abs(1,2)`,
		`SELECT * FROM t ORDER BY 99`,
		`SELECT 1 UNION SELECT 1,2`,
		`SELECT max(a) FROM t WHERE max(a)>1`,
		// A syntax error at end of input, in each of the three grammars whose
		// own "expected X" message used to win: a SELECT, an INSERT that never
		// reached its VALUES/SELECT, and one that never reached its row.
		`SELECT 1 FROM`,
		`INSERT INTO t`,
		`INSERT INTO t VALUES`,
		// ...and one mid-statement, which must NOT become "incomplete input".
		`SELECT * FROM t WHERE a = 1 nonsense`,
		`INSERT INTO t VALUES(1,2)`,
		`UPDATE t SET nosuch=1`,
		`SELECT a FROM t, s USING(a)`,
		`SELECT * FROM t JOIN s USING(nosuch)`,
		// resolve.c:1265-1277's two words, on both sides of its own test.
		`SELECT row_number() FROM t`,
		`SELECT a FROM t WHERE row_number() OVER ()>1`,
		`SELECT count(*) OVER () FROM t WHERE count(*) OVER ()>1`,
		`SELECT sum(sum(a)) FROM t`,
		`SELECT (SELECT 1,2)`,
		`SELECT 1 WHERE (SELECT 1,2) = 1`,
		`SELECT * FROM t WHERE ?1 AND ?0`,
		`SELECT * FROM t WHERE a = ?32767`,
		// The DDL half: the statement context, and C's own wording for a
		// column, a table, a collation and an empty column list.
		`CREATE TABLE q2(a, b, PRIMARY KEY(nosuch))`,
		`CREATE TABLE q2(a, b, UNIQUE(nosuch))`,
		`CREATE TABLE q2 AS SELECT * FROM nosuch`,
		`CREATE INDEX i9 ON t(nosuch)`,
		`CREATE UNIQUE INDEX i9 ON t(b) WHERE nosuch>0`,
		`CREATE INDEX i9 ON nosuch(a)`,
		`CREATE TABLE q2(a COLLATE nosuch)`,
		`CREATE TABLE q2(a, b, PRIMARY KEY(a COLLATE nosuch))`,
		`CREATE INDEX i9 ON t(b COLLATE nosuch)`,
		`ALTER TABLE t ADD COLUMN e COLLATE nosuch`,
		`CREATE TABLE q2()`,
		// A fourth round, over a different 58 shapes: 42 already matched and
		// these are the classes it moved. Each is C's own rule:
		//   - a GROUP BY ordinal out of range carries its ORDINAL and the range
		//     ("1st GROUP BY term out of range - should be between 1 and 1"),
		//     which the read path already produced and the VDBE group planner
		//     did not;
		//   - a wrong argument count is resolve.c:1293's "wrong number of
		//     arguments to function %#T()", not a per-function sentence;
		//   - an upsert's conflict target and SET list, and RETURNING's own
		//     expressions, are RESOLVED like any other names (upsert.c:119 runs
		//     sqlite3ResolveExprListNames before it looks at a single index), so
		//     an unknown one is "no such column: x" -- where this engine
		//     reported the clause's own complaint, or wrapped the right message
		//     as a decline;
		//   - a syntax error away from end of input is `near "X": syntax error`
		//     (parse.y:44-51's other half), where this said "unexpected token".
		`SELECT a FROM t GROUP BY 0`,
		`SELECT a FROM t GROUP BY 99`,
		`SELECT a, b FROM t GROUP BY 3`,
		`SELECT group_concat(a, b, c, d) FROM t`,
		`SELECT * FROM t WHERE a = (SELECT)`,
		`SELECT * FROM t WHERE )`,
		`SELECT 1 +`,
		`INSERT INTO t VALUES(1,'y',9,1) ON CONFLICT(nosuch) DO NOTHING`,
		`INSERT INTO t VALUES(1,'y',9,1) ON CONFLICT(a) DO UPDATE SET nosuch=1`,
		`INSERT INTO t VALUES(1,'y',9,1) RETURNING nosuch`,
		// A savepoint verb with its name missing ends at EOF.
		`SAVEPOINT`,
		`RELEASE`,
		`RELEASE SAVEPOINT`,
		`ROLLBACK TO`,
		// ...while a name that is a bad TOKEN is still a syntax error, not
		// incomplete input.
		`SAVEPOINT 123`,
	} {
		q := q
		t.Run(strings.NewReplacer(" ", "_", "(", "", ")", "", "'", "", "*", "star").Replace(q), func(t *testing.T) {
			var msg [2]string
			for i, drv := range []string{"sqlite3", "sqlite"} {
				p := filepath.Join(t.TempDir(), "x.db")
				db, err := sql.Open(drv, p)
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				for _, s := range setup {
					db.Exec(s)
				}
				_, e := db.Exec(q)
				if e == nil {
					if rows, qerr := db.Query(q); qerr != nil {
						e = qerr
					} else if rows != nil {
						rows.Close()
					}
				}
				if e != nil {
					msg[i] = e.Error()
				}
				db.Close()
			}
			if msg[0] == "" {
				t.Fatalf("the oracle did not reject %q -- the case no longer tests an error", q)
			}
			if msg[0] != msg[1] {
				t.Errorf("%s\n  cgo: %q\n  mus: %q", q, msg[0], msg[1])
			}
		})
	}
}
