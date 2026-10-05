// Tests ON CONFLICT vs join-constraint grammar ambiguity in INSERT ... SELECT.
package compat

import "testing"

// r34xUpsertSetup is the schema for upsert tests.
var r34xUpsertSetup = []string{
	`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
	`CREATE TABLE src(a,b)`,
	`CREATE TABLE src2(a,c)`,
	`CREATE INDEX i1 ON src(a)`,
	`INSERT INTO t1 VALUES(1,'x')`,
	`INSERT INTO src VALUES(1,'p'),(2,'q')`,
	`INSERT INTO src2 VALUES(1,'m'),(3,'n')`,
}

// r34xUpsertCase runs the setup, the statement under test, and then READS THE
// TABLE BACK. The readback is the load-bearing half: a statement that is
// rejected at prepare time and one that is accepted-and-applied differ in the
// rows afterwards, and only the readback sees that -- the worker reports both a
// refusal and a successful DML as a single opaque result kind.
func r34xUpsertCase(t *testing.T, name, stmt string) {
	t.Helper()
	differ(t, name, append(append([]string{}, r34xUpsertSetup...),
		stmt,
		`SELECT a,b FROM t1 ORDER BY a`,
	))
}

// TestR34XUpsertAmbiguousON covers the spellings where the ON sits directly
// against a FROM element, so the C shifts it into that element's join
// constraint and the statement dies at DO ("near \"DO\": syntax error",
// measured against the 3.53.3 oracle).
func TestR34XUpsertAmbiguousON(t *testing.T) {
	r34xUpsertCase(t, "r34x leading table",
		`INSERT INTO t1 SELECT a,b FROM src ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x leading table do nothing",
		`INSERT INTO t1 SELECT a,b FROM src ON CONFLICT DO NOTHING`)
	r34xUpsertCase(t, "r34x leading table targeted do nothing",
		`INSERT INTO t1 SELECT a,b FROM src ON CONFLICT(a) DO NOTHING`)
	r34xUpsertCase(t, "r34x aliased table",
		`INSERT INTO t1 SELECT a,b FROM src AS s ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x indexed by",
		`INSERT INTO t1 SELECT a,b FROM src INDEXED BY i1 ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x derived table",
		`INSERT INTO t1 SELECT * FROM (SELECT a,b FROM src) ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x comma join",
		`INSERT INTO t1 SELECT src.a,src.b FROM src, src2 ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x natural join",
		`INSERT INTO t1 SELECT src.a,src.b FROM src NATURAL JOIN src2 ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x compound arm",
		`INSERT INTO t1 SELECT a,b FROM src UNION ALL SELECT a,c FROM src2 ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x cte source",
		`WITH c(a,b) AS (SELECT 7,'z') INSERT INTO t1 SELECT a,b FROM c ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x with returning",
		`INSERT INTO t1 SELECT a,b FROM src ON CONFLICT(a) DO UPDATE SET b=excluded.b RETURNING a`)
	// A TRIGGER BODY takes the same "insert_cmd INTO xfullname idlist_opt select
	// upsert" production (parse.y:1810), so the ambiguity reaches CREATE TRIGGER
	// too and the whole CREATE is rejected.
	differ(t, "r34x trigger body", append(append([]string{}, r34xUpsertSetup...),
		`CREATE TABLE z(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON z BEGIN
		   INSERT INTO t1 SELECT a,b FROM src ON CONFLICT(a) DO UPDATE SET b=excluded.b;
		 END`,
		`INSERT INTO z VALUES(1)`,
		`SELECT a,b FROM t1 ORDER BY a`,
	))
}

// TestR34XUpsertUnambiguousON is the other half: every spelling that puts
// something between the FROM element and the ON, so the empty on_using has
// already been reduced and the upsert clause is reachable. These must keep
// WORKING -- the fix must not turn a legal upsert into a syntax error.
func TestR34XUpsertUnambiguousON(t *testing.T) {
	r34xUpsertCase(t, "r34x where true",
		`INSERT INTO t1 SELECT a,b FROM src WHERE true ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x where predicate",
		`INSERT INTO t1 SELECT a,b FROM src WHERE a<9 ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x group by",
		`INSERT INTO t1 SELECT a,b FROM src GROUP BY a,b ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x order by",
		`INSERT INTO t1 SELECT a,b FROM src ORDER BY a ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x limit",
		`INSERT INTO t1 SELECT a,b FROM src LIMIT 5 ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x no from clause",
		`INSERT INTO t1 SELECT 1,'k' ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x join fills the slot",
		`INSERT INTO t1 SELECT src.a,src.b FROM src JOIN src2 ON src.a=src2.a ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x using fills the slot",
		`INSERT INTO t1 SELECT src.a,src.b FROM src JOIN src2 USING(a) ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	r34xUpsertCase(t, "r34x values source",
		`INSERT INTO t1 VALUES(1,'v') ON CONFLICT(a) DO UPDATE SET b=excluded.b`)
	// The same trigger body with the WHERE the grammar needs: the CREATE is
	// accepted and the upsert really runs when the trigger fires.
	differ(t, "r34x trigger body with where", append(append([]string{}, r34xUpsertSetup...),
		`CREATE TABLE z(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON z BEGIN
		   INSERT INTO t1 SELECT a,b FROM src WHERE true ON CONFLICT(a) DO UPDATE SET b=excluded.b;
		 END`,
		`INSERT INTO z VALUES(1)`,
		`SELECT a,b FROM t1 ORDER BY a`,
	))
}

// TestR34XLeadingJoinConstraint is the rule the fix rests on, stated without an
// upsert anywhere: an ON or USING against the LEADING FROM element parses (the
// slot exists) and is then rejected, "a JOIN clause is required before ON" /
// "... before USING" -- measured against the 3.53.3 oracle. Every FROM shape
// that can be leading is here, because the fix consumes the constraint in
// exactly one place and this is what says it consumed it everywhere.
func TestR34XLeadingJoinConstraint(t *testing.T) {
	for _, c := range []struct{ name, stmt string }{
		{"r34x bare table", `SELECT a FROM src ON 1`},
		{"r34x bare table using", `SELECT a FROM src USING(a)`},
		{"r34x aliased", `SELECT a FROM src AS x ON 1`},
		{"r34x parenthesized", `SELECT a FROM (src) ON 1`},
		{"r34x parenthesized group", `SELECT src.a FROM (src JOIN src2 ON 1) ON 1`},
		{"r34x derived", `SELECT a FROM (SELECT a FROM src) ON 1`},
		{"r34x subquery in where", `SELECT a FROM src WHERE a IN (SELECT a FROM src2 ON 1)`},
		{"r34x update from", `UPDATE t1 SET b=1 FROM src ON 1`},
		{"r34x delete subquery", `DELETE FROM t1 WHERE a IN (SELECT a FROM src ON 1)`},
	} {
		differ(t, c.name, append(append([]string{}, r34xUpsertSetup...),
			c.stmt,
			`SELECT a,b FROM t1 ORDER BY a`,
		))
	}
}
