package compat

// r38c -- SQLite's query FLATTENER reaching a CTE reference, and a transparent
// derived table that carries its own LIMIT.
//
// flattenSubquery (select.c) merges a FROM-clause subquery into the query that
// reads it. Two of its effects are observable here, and both are WRONG ANSWERS
// when they are missed rather than missed optimizations:
//
//   - the PLAN. A merged subquery is planned as part of its parent, so the
//     parent's WHERE (or a covering index over the columns it reads) drives an
//     index on the BASE TABLE and the rows arrive in that index's key order.
//     This package materializes a derived table and scans it in arrival order.
//   - the LIMIT, which select.c:4695 hands to the PARENT, so it applies after
//     the parent's ORDER BY rather than before it.
//
// Round 35 ported the LIMIT half for an inline subquery, round 37 the plan half
// for a transparent one plus a stored VIEW's and a compound's LIMIT. What this
// file pins is round 38's two additions -- withExpand's CTE reference, and a
// transparent body that also carries a LIMIT -- TOGETHER WITH the boundary,
// because that boundary is not a guess: "AS MATERIALIZED" is an outright
// optimization fence (select.c:7797, restriction (28)) and a recursive CTE is
// restriction (22), so both keep the pre-flattening answer and the two spellings
// must be told apart.
//
// Every case here asserts the ORACLE's own answer via differ(), never "this
// declines": a case that starts being served must stay agreeing, not turn red.

import (
	"encoding/json"
	"strings"
	"testing"
)

// r38cKnownDivergent names the cells that diverge TODAY for a reason this
// rewrite neither causes nor can fix, each verified to diverge at main
// (071e983) with every r38c commit reverted -- so none of them is a regression
// this file's subject introduced. They are pinned the other way round (the cell
// must still diverge) rather than deleted, so the day the named fix lands the
// pin FAILS and says so: that is the instruction to delete the entry, not a
// "this declines" assertion that would read as a regression.
//
// BOTH are one bug class: an ORDER BY key TIE. The fixture holds INTEGER 1 and
// REAL 1.0, which compare equal, so which one comes first is decided entirely by
// the access path -- arrival order here, the walked index there.
var r38cKnownDivergent = map[string]string{
	// multiSelect (select.c:2986) takes multiSelectByMerge whenever the compound
	// has an ORDER BY: it imposes that ORDER BY on EACH ARM separately, so an arm
	// may satisfy it from an index, and merges the sorted streams. This package
	// runs every arm unordered, concatenates and stable-sorts
	// (execCompoundProgram, engine/vdbe_compound_codegen.go), so ties keep
	// arrival order. Measured fix: give each arm the compound's ORDER BY as the
	// ordinal SQLite resolves it to (iOrderByCol) in compileCompound, after
	// cp.orderByIdx is known -- a stable sort over a concatenation of sorted
	// streams IS the merge, so nothing else has to change. With that plus lifting
	// r38cFlattenCompoundArms' sequential-only guard, this cell and idx-cover's
	// twin both agree and the -short gate gains no divergence.
	"idx-a/compound-arm-order": "port multiSelectByMerge's per-arm ORDER BY (engine/vdbe_compound_codegen.go)",
	// SQLite flattens this all the way down to "SELECT a FROM t ORDER BY 1 DESC"
	// and walks i1 backwards. This package declines the rewrite -- correctly, and
	// for a reason it must keep: c's body resolves at ITS OWN defining depth
	// (r38cExpandCTERef's scopeDepth check), because the derived table that reads
	// c redefines t, and inlining would resolve c's body against the WRONG t.
	// Both engines already return the right ROWS; only the tie order differs.
	"idx-a/cte-inner-shadow": "flatten a CTE reference whose body resolves at an outer scope depth",
}

// r38cAgrees reports whether the two engines answer stmts identically. It never
// fails the test itself -- r38cKnownDivergent needs the answer, not a verdict.
func r38cAgrees(t *testing.T, stmts []string) bool {
	t.Helper()
	m, _ := json.Marshal(run(t, "musql", stmts))
	c, _ := json.Marshal(run(t, "cgo", stmts))
	return string(m) == string(c)
}

// r38cSchemas are the three index shapes that make flattening observable at all.
// idx-cover is the sharpest: with no WHERE and no ORDER BY at all, an index that
// covers every column the select list reads is still SCANNED IN KEY ORDER, which
// is how a plan difference shows through a statement that names no ordering.
var r38cSchemas = []struct{ name, ddl string }{
	{"noidx", ``},
	{"idx-a", `CREATE INDEX i1 ON t(a);`},
	{"idx-cover", `CREATE INDEX i1 ON t(a,b,c);`},
}

const r38cBase = `
CREATE TABLE t(a, b, c);
INSERT INTO t VALUES(3,'x',10);
INSERT INTO t VALUES(1,'Y',20);
INSERT INTO t VALUES(2,'x',30);
INSERT INTO t VALUES(2,'z',40);
INSERT INTO t VALUES(NULL,'w',50);
INSERT INTO t VALUES(1.0,'X',60);
CREATE TABLE u(b);
INSERT INTO u VALUES(10),(20);
`

// r38cCases are run once per schema. Each is one SELECT; the fixture and the
// schema's DDL precede it. Cases whose comment says "fenced" or names a
// restriction are the ones that must NOT be rewritten -- they are in the same
// list precisely so that widening the rewrite past its boundary turns them red.
var r38cCases = []struct{ name, sql string }{
	// --- the CTE reference, plan half -----------------------------------------
	{"cte-transparent-in",
		`WITH c AS (SELECT * FROM t) SELECT a, typeof(a) FROM c WHERE a IN (1,2,3) LIMIT 2`},
	{"cte-transparent-bare",
		`WITH c AS (SELECT * FROM t) SELECT a, typeof(a) FROM c`},
	{"cte-transparent-aliased",
		`WITH c AS (SELECT * FROM t) SELECT x.a, typeof(x.a) FROM c AS x`},
	{"cte-transparent-eq",
		`WITH c AS (SELECT * FROM t) SELECT a, b FROM c WHERE a = 2`},
	{"cte-transparent-join",
		`WITH c AS (SELECT * FROM t) SELECT c.a, z.b FROM c, t AS z WHERE c.a = z.a`},
	{"cte-transparent-order",
		`WITH c AS (SELECT * FROM t) SELECT a, typeof(a) FROM c ORDER BY a DESC`},
	{"cte-transparent-group",
		`WITH c AS (SELECT * FROM t) SELECT a, count(*) FROM c GROUP BY a`},
	// (28): AS MATERIALIZED is an optimization fence, so this keeps the
	// PRE-flattening answer -- the one a materialized derived table gives.
	{"cte-materialized-fenced",
		`WITH c AS MATERIALIZED (SELECT * FROM t) SELECT a, typeof(a) FROM c WHERE a IN (1,2,3) LIMIT 2`},
	// NOT MATERIALIZED flattens exactly like the unwritten default.
	{"cte-not-materialized",
		`WITH c AS NOT MATERIALIZED (SELECT * FROM t) SELECT a, typeof(a) FROM c WHERE a IN (1,2,3) LIMIT 2`},

	// --- the CTE reference, LIMIT half ----------------------------------------
	{"cte-limit-body-cols",
		`WITH v(a) AS (SELECT a FROM t LIMIT 2) SELECT * FROM v ORDER BY 1 DESC`},
	{"cte-limit-body-nocols",
		`WITH v AS (SELECT a FROM t LIMIT 2) SELECT * FROM v ORDER BY 1 DESC`},
	{"cte-limit-body-renamed",
		`WITH v(z) AS (SELECT a FROM t LIMIT 2) SELECT z FROM v ORDER BY 1 DESC`},
	{"cte-limit-materialized-fenced",
		`WITH v(a) AS MATERIALIZED (SELECT a FROM t LIMIT 2) SELECT * FROM v ORDER BY 1 DESC`},
	// (13) outer LIMIT, (19) outer WHERE, (21) outer DISTINCT, (9) outer
	// aggregate, (14) sub OFFSET, (11) sub ORDER BY: each blocks the transfer.
	{"cte-limit-outer-limit",
		`WITH v(a) AS (SELECT a FROM t LIMIT 2) SELECT * FROM v ORDER BY 1 DESC LIMIT 9`},
	{"cte-limit-outer-where",
		`WITH v(a) AS (SELECT a FROM t LIMIT 2) SELECT * FROM v WHERE a > 0 ORDER BY 1 DESC`},
	{"cte-limit-outer-distinct",
		`WITH v(a) AS (SELECT a FROM t LIMIT 2) SELECT DISTINCT a FROM v ORDER BY 1 DESC`},
	{"cte-limit-outer-agg",
		`WITH v(a) AS (SELECT a FROM t LIMIT 2) SELECT max(a) FROM v ORDER BY 1 DESC`},
	{"cte-limit-sub-offset",
		`WITH v(a) AS (SELECT a FROM t LIMIT 2 OFFSET 1) SELECT * FROM v ORDER BY 1 DESC`},
	{"cte-limit-sub-order",
		`WITH v(a) AS (SELECT a FROM t ORDER BY a LIMIT 2) SELECT * FROM v ORDER BY 1 DESC`},
	// A column-count mismatch is an error on both engines, and the expansion
	// must not swallow it ("table v has 1 values for 2 columns").
	{"cte-limit-colcount-mismatch",
		`WITH v(x,y) AS (SELECT a FROM t LIMIT 2) SELECT * FROM v ORDER BY 1 DESC`},

	// --- (22): a recursive CTE is never flattened -----------------------------
	{"cte-recursive",
		`WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n<5) SELECT * FROM r ORDER BY 1 DESC`},
	{"cte-recursive-limit",
		`WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n<5 LIMIT 3) SELECT * FROM r ORDER BY 1 DESC`},

	// --- the CTE reference must not be resolved in the WRONG scope ------------
	// c's body names t; the derived table that reads c redefines t. c's body
	// resolves at ITS OWN defining depth, so it must still see the real table.
	{"cte-inner-shadow",
		`WITH c AS (SELECT * FROM t) SELECT * FROM (WITH t AS (SELECT 99 AS a) SELECT a FROM c) AS d ORDER BY 1 DESC`},
	// A CTE SHADOWS a same-named view, so this must read t and not u.
	{"cte-shadows-view",
		`WITH c AS (SELECT * FROM t) SELECT a FROM c ORDER BY 1`},
	// A qualified name always means the TABLE, never a same-named CTE.
	{"cte-qualified-is-table",
		`WITH t AS (SELECT 99 AS a, 'q' AS b, 0 AS c) SELECT a FROM main.t ORDER BY 1`},
	// A derived table has no rowid pseudo-column: "no such column" on both.
	{"cte-rowid-rejected",
		`WITH c AS (SELECT * FROM t) SELECT rowid, a FROM c`},

	// --- a TRANSPARENT body that carries its own LIMIT ------------------------
	{"derived-limit-bare",
		`SELECT a, typeof(a) FROM (SELECT * FROM t LIMIT 3) AS d`},
	{"derived-limit-order",
		`SELECT a, typeof(a) FROM (SELECT * FROM t LIMIT 3) AS d ORDER BY a DESC`},
	{"derived-limit-star",
		`SELECT * FROM (SELECT * FROM t LIMIT 3) AS d`},
	{"derived-limit-zero",
		`SELECT a FROM (SELECT * FROM t LIMIT 0) AS d`},
	{"derived-limit-negative",
		`SELECT a FROM (SELECT * FROM t LIMIT -1) AS d`},
	{"derived-limit-outer-where",
		`SELECT a FROM (SELECT * FROM t LIMIT 3) AS d WHERE a > 1`},
	{"derived-limit-outer-limit",
		`SELECT a FROM (SELECT * FROM t LIMIT 3) AS d LIMIT 2`},
	{"derived-limit-outer-distinct",
		`SELECT DISTINCT a FROM (SELECT * FROM t LIMIT 3) AS d`},
	{"derived-limit-outer-agg",
		`SELECT max(a), count(*) FROM (SELECT * FROM t LIMIT 3) AS d`},
	{"derived-limit-outer-group",
		`SELECT a, count(*) FROM (SELECT * FROM t LIMIT 3) AS d GROUP BY a`},
	{"derived-limit-sub-offset",
		`SELECT a FROM (SELECT * FROM t LIMIT 3 OFFSET 1) AS d`},
	{"derived-limit-join",
		`SELECT d.a, z.b FROM (SELECT * FROM t LIMIT 3) AS d, u AS z`},
	{"derived-limit-cte",
		`WITH c AS (SELECT * FROM t LIMIT 3) SELECT a, typeof(a) FROM c`},
	{"derived-limit-view",
		`SELECT a, typeof(a) FROM vwl`},

	// --- the same rewrite inside a COMPOUND ARM -------------------------------
	// A compound is prepared arm by arm, so flattening reaches a FROM-clause
	// subquery inside an arm too. The compound's own LIMIT must NOT move into an
	// arm, which is restriction (15).
	{"compound-arm-cte",
		`WITH c AS (SELECT * FROM t) SELECT a, typeof(a) FROM c WHERE a IN (1,2,3)
		   UNION ALL SELECT a, typeof(a) FROM c WHERE a IN (1,2,3) LIMIT 2`},
	{"compound-arm-derived",
		`SELECT a, typeof(a) FROM (SELECT * FROM t) AS d WHERE a IN (1,2,3)
		   UNION ALL SELECT b, typeof(b) FROM u`},
	{"compound-arm-second-only",
		`SELECT b, typeof(b) FROM u
		   UNION ALL SELECT a, typeof(a) FROM (SELECT * FROM t) AS d WHERE a IN (1,2,3)`},
	{"compound-arm-derived-limit-15",
		`SELECT a, typeof(a) FROM (SELECT * FROM t LIMIT 3) AS d
		   UNION ALL SELECT b, typeof(b) FROM u`},
	{"compound-arm-view",
		`SELECT a, typeof(a) FROM vwt UNION ALL SELECT b, typeof(b) FROM u`},
	{"compound-arm-order",
		`SELECT a FROM (SELECT * FROM t) AS d WHERE a IN (1,2,3)
		   UNION ALL SELECT b FROM u ORDER BY 1 DESC`},
	{"compound-arm-except",
		`SELECT a, typeof(a) FROM (SELECT * FROM t) AS d
		   EXCEPT SELECT b, typeof(b) FROM u`},
	{"compound-arm-materialized-fenced",
		`WITH c AS MATERIALIZED (SELECT * FROM t) SELECT a, typeof(a) FROM c WHERE a IN (1,2,3)
		   UNION ALL SELECT a, typeof(a) FROM c WHERE a IN (1,2,3) LIMIT 2`},
}

// TestR38CFlattenCTE runs every case above under every schema and fails on any
// divergence from the C-SQLite oracle.
func TestR38CFlattenCTE(t *testing.T) {
	for _, sc := range r38cSchemas {
		for _, tc := range r38cCases {
			t.Run(sc.name+"/"+tc.name, func(t *testing.T) {
				var stmts []string
				setup := r38cBase +
					`CREATE VIEW c AS SELECT b AS a FROM u;` + // shadowed by the CTE named c
					`CREATE VIEW vwl AS SELECT * FROM t LIMIT 3;` +
					`CREATE VIEW vwt AS SELECT * FROM t;` +
					sc.ddl
				for _, s := range strings.Split(setup, ";") {
					if s = strings.TrimSpace(s); s != "" {
						stmts = append(stmts, s)
					}
				}
				key := sc.name + "/" + tc.name
				if why, known := r38cKnownDivergent[key]; known {
					if r38cAgrees(t, append(stmts, tc.sql)) {
						t.Fatalf("[r38c/%s] now AGREES with the oracle. Delete its entry from "+
							"r38cKnownDivergent -- the fix it was waiting on (%s) has landed.", key, why)
					}
					return
				}
				differ(t, "r38c/"+key, append(stmts, tc.sql))
			})
		}
	}
}
