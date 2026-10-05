package compat

// This file tests unqualified rowid references in index-order port scenarios.
// An unqualified rowid must resolve correctly and must be ambiguous with
// multiple tables in the FROM clause.

import (
	"encoding/json"
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestR35FUnqualifiedRowidGate(t *testing.T) {
	setup := []string{
		"CREATE TABLE t(a INTEGER, b TEXT, c INTEGER)",
		"INSERT INTO t VALUES(3,'x',9),(1,'y',8),(2,'z',7),(5,'w',6),(4,'v',5)",
		"CREATE INDEX ta ON t(a)",
		"CREATE INDEX tb ON t(b)",
		"CREATE TABLE u(k INTEGER, m TEXT)",
		"INSERT INTO u VALUES(2,'p'),(1,'q'),(3,'r')",
		"CREATE INDEX uk ON u(k)",
	}
	// Every spelling of the pseudo-column, and the qualified form as the
	// control: it already worked, so a failure there is a different bug.
	names := []string{"rowid", "oid", "_rowid_", "ROWID", "t.rowid"}
	var queries []string
	for _, n := range names {
		queries = append(queries,
			fmt.Sprintf("SELECT %s,a FROM t WHERE a>=2", n),
			fmt.Sprintf("SELECT %s,a FROM t WHERE a BETWEEN 2 AND 4", n),
			fmt.Sprintf("SELECT %s FROM t WHERE b>'v'", n),
			fmt.Sprintf("SELECT group_concat(a) FROM t WHERE %s>0", n),
			fmt.Sprintf("SELECT a FROM t WHERE a>1 ORDER BY %s", n),
			fmt.Sprintf("SELECT %s,b FROM t WHERE a>0 LIMIT 3", n),
			fmt.Sprintf("SELECT typeof(%s), %s*2 FROM t WHERE a<4", n, n),
		)
	}
	queries = append(queries,
		"SELECT group_concat(a) FROM t,u WHERE t.a=u.k",
		"SELECT group_concat(t.rowid) FROM t,u WHERE t.a=u.k",
		"SELECT group_concat(rowid) FROM t,u WHERE t.a=u.k", // ambiguous in the oracle
		"SELECT count(*) FROM t WHERE rowid IN (1,2)",
	)

	stmts := append([]string{}, setup...)
	for _, q := range queries {
		stmts = append(stmts, q)
	}
	cgo := run(t, "cgo", stmts)
	mush := run(t, "musql", stmts)
	bad := 0
	for i := len(setup); i < len(stmts); i++ {
		cb, _ := json.Marshal(cgo[i])
		mb, _ := json.Marshal(mush[i])
		if string(cb) == string(mb) {
			continue
		}
		bad++
		t.Errorf("R35F rowid gate: %s\n  cgo:    %s\n  musql: %s", stmts[i], cb, mb)
	}
	if bad > 0 {
		t.Fatalf("R35F rowid gate: %d/%d statements diverge.\n"+
			"  An unqualified rowid resolves through wherePlanColumnRef's\n"+
			"  single-FROM-item fallback (engine/where_plan.go, ported from\n"+
			"  resolve.c:471 and :623). Read it against the C before touching this\n"+
			"  test; if a shape is genuinely outside the port, make the port\n"+
			"  DECLINE it -- never relax the assertion.", bad, len(queries))
	}
	t.Logf("R35F rowid gate: %d/%d statements agree with the oracle", len(queries), len(queries))
}
