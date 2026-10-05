package compat

// Tests the bare-column anchor escape hatch when the GROUP BY key contains a
// ROWID or INTEGER PRIMARY KEY: that item contributes one row per group, so its
// columns are independent of scan order.

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestR28AnchorPinnedByGroupKey pins the rule case by case, each with the
// control that must NOT be served beside it.
func TestR28AnchorPinnedByGroupKey(t *testing.T) {
	schema := []string{
		"CREATE TABLE p(k INTEGER PRIMARY KEY, a, b)",
		"INSERT INTO p VALUES(1,7,'x'),(2,7,'y'),(3,9,'z')",
		"CREATE UNIQUE INDEX pab ON p(a,b)", // covering, so SQLite prefers it
		"CREATE TABLE q(qk INTEGER PRIMARY KEY, pk, c)",
		"INSERT INTO q VALUES(1,1,90),(2,1,10),(3,2,12),(4,3,13)",
		"CREATE INDEX qpk ON q(pk,c)",
		"CREATE TABLE r(x,y)", // no rowid alias declared, no index
		"INSERT INTO r VALUES(1,'m'),(1,'n'),(2,'o')",
	}
	// SERVED: every FROM item the anchor is read from is pinned by the key.
	for _, q := range []string{
		// The single-table forms. An INTEGER PRIMARY KEY in the key...
		"SELECT k, count(*), a, b FROM p GROUP BY k ORDER BY k",
		"SELECT k, max(a), a, b FROM p GROUP BY k ORDER BY k",
		// ...the rowid pseudo-column, on a table that declares no alias for it --
		// affinity2.test's own "SELECT * FROM ttt ... GROUP BY rowid" shape...
		"SELECT count(*), x, y FROM r GROUP BY rowid ORDER BY rowid",
		"SELECT * FROM r GROUP BY rowid ORDER BY rowid",
		// ...and reached through a qualifier or one of its other spellings.
		"SELECT count(*), x, y FROM r GROUP BY r._rowid_ ORDER BY r._rowid_",
		// A correlated select-list subquery reads the same anchor, and with the
		// only FROM item pinned its value cannot depend on which row won.
		"SELECT k, count(*), (SELECT b) FROM p GROUP BY k ORDER BY k",
		// A HAVING bare column, likewise.
		"SELECT k, count(*) FROM p GROUP BY k HAVING b IS NOT NULL ORDER BY k",
		// The JOIN form -- TestCorpus's own joins-subquery shape: the bare column
		// belongs to the pinned outer table while the aggregate ranges over the
		// unpinned inner one, which the anchor never reads.
		"SELECT p.k, sum(q.c), p.a, p.b FROM p LEFT JOIN q ON q.pk=p.k GROUP BY p.k ORDER BY p.k",
		"SELECT p.k, count(*), p.a FROM p, q WHERE q.pk=p.k GROUP BY p.k ORDER BY p.k",
		// ...including a NULL-extended group, where the pin holds because a NULL
		// rowid means the whole item was NULL-extended.
		"SELECT q.qk, count(*), q.c FROM p LEFT JOIN q ON q.pk=p.k GROUP BY q.qk ORDER BY q.qk",
		// q.c is read bare while only p is pinned -- a CONTROL until the
		// multi-table whereLoopAddBtreeIndex port decided this loop order too.
		"SELECT p.k, count(*), q.c FROM p, q WHERE q.pk=p.k GROUP BY p.k ORDER BY p.k",
		// These two were CONTROLS -- declined because the pin cannot speak for
		// them (the key is a plain column, and a whole-table aggregate has no
		// key at all). They are served now, and byte-identical to the oracle,
		// because the anchor's ACCESS-PATH half no longer asks "does any table
		// carry an index" but asks the ported planner whether the scan order is
		// decided (wherePlanIndexOrderDecided, engine/where_plan_gate.go). The
		// pin did not widen; the other half of the question got a real answer.
		"SELECT a, count(*), b FROM p GROUP BY a ORDER BY a",
		"SELECT count(*), a, b FROM p",
		// This was the LAST control below ("a correlated subquery can name any
		// column of any item, so pinning p alone is not enough"). It is served
		// now, and byte-identical to the oracle, for the same reason the two
		// above are: the access-path half got a real answer. Round 36 made
		// SrcItem.colUsed computable THROUGH a subquery (r36dSubColUsed,
		// engine/where_plan_subcolused_r36d.go), and colUsed is what
		// wherePlanIndexOrderDecided needs in order to say whether the scan
		// order is decided -- so "(SELECT q.c)" no longer blinds the guard. The
		// pin itself did not widen.
		"SELECT p.k, count(*), (SELECT q.c) FROM p, q WHERE q.pk=p.k GROUP BY p.k ORDER BY p.k",
	} {
		stmts := append(append([]string(nil), schema...), q)
		res := run(t, "musql", stmts)
		if res[len(res)-1]["kind"] == "error" {
			t.Errorf("the GROUP BY key pins every item the anchor is read from; this must be served: %s\n  got: %v",
				q, res[len(res)-1])
			continue
		}
		if !differ(t, "r28pinned", stmts) {
			t.Errorf("diverged on: %s", q)
		}
	}

	// Order-sensitive aggregates over unpinned tables: may decline or agree.
	for _, q := range []string{
		"SELECT p.k, group_concat(q.c), p.a FROM p, q WHERE q.pk=p.k GROUP BY p.k ORDER BY p.k",
		"SELECT p.k, group_concat(q.c), p.a FROM p LEFT JOIN q ON q.pk=p.k GROUP BY p.k ORDER BY p.k",
	} {
		stmts := append(append([]string(nil), schema...), q)
		if run(t, "musql", stmts)[len(stmts)-1]["kind"] == "error" {
			continue
		}
		if !differ(t, "r28pinned", stmts) {
			t.Errorf("diverged on: %s", q)
		}
	}

}

// TestR28AnchorPinnedFuzz fuzzes the anchor pin rule with random schemas and
// aggregates. Non-order-sensitive aggregates (count, sum, max) must always be
// answered and agree with C SQLite. Order-sensitive ones (group_concat) may
// decline or agree depending on scan order.
func TestR28AnchorPinnedFuzz(t *testing.T) {
	if testing.Short() {
		t.Skip("randomized anchor pin sweep")
	}
	rng := rand.New(rand.NewSource(r26EnvInt("R28_SEED", 0x28b2)))
	declined := 0
	for iter := 0; iter < int(r26EnvInt("R28_ITERS", 300)); iter++ {
		stmts := r28PinnedCase(rng)
		res := run(t, "musql", stmts)
		if res[len(res)-1]["kind"] == "error" {
			declined++
			if !strings.Contains(stmts[len(stmts)-1], "group_concat(") {
				t.Errorf("[r28pin] the key pins every item the anchor reads; this must be served\n  sql: %v\n  got: %v",
					stmts, res[len(res)-1])
			}
			continue
		}
		if !differ(t, "r28pin", stmts) {
			t.Fatalf("diverged on: %v", stmts)
		}
	}
	t.Logf("%d declined", declined)
}

// r28PinnedCase builds one grouped aggregate whose GROUP BY key pins every FROM
// item its bare columns come from.
func r28PinnedCase(rng *rand.Rand) []string {
	cell := func(n int) string {
		if rng.Intn(6) == 0 {
			return "NULL"
		}
		return fmt.Sprintf("%d", rng.Intn(n))
	}
	pRows := make([]string, 3+rng.Intn(4))
	for i := range pRows {
		pRows[i] = fmt.Sprintf("(%d,%s,%s)", i+1, cell(3), cell(4))
	}
	// q's join key ranges over only 3 values while p's is a dense primary key, so
	// a group holds SEVERAL q rows -- without that, most groups are one row and
	// no aggregate in them can be order-sensitive at all.
	qRows := make([]string, 5+rng.Intn(5))
	for i := range qRows {
		qRows[i] = fmt.Sprintf("(%d,%s,%s)", i+1, cell(3), cell(9))
	}
	stmts := []string{
		"CREATE TABLE p(k INTEGER PRIMARY KEY, a, b)",
		"INSERT INTO p VALUES" + strings.Join(pRows, ","),
		"CREATE TABLE q(qk INTEGER PRIMARY KEY, pk, c)",
		"INSERT INTO q VALUES" + strings.Join(qRows, ","),
	}
	// Randomly add various index shapes.
	if idx := []string{
		"", "CREATE INDEX pa ON p(a)", "CREATE INDEX pab ON p(a,b)",
		"CREATE UNIQUE INDEX pab ON p(a,b)", "CREATE INDEX pad ON p(a DESC)",
	}[rng.Intn(5)]; idx != "" {
		stmts = append(stmts, idx)
	}
	if rng.Intn(2) == 0 {
		stmts = append(stmts, []string{"CREATE INDEX qc ON q(c)", "CREATE INDEX qpk ON q(pk,c)"}[rng.Intn(2)])
	}
	// group_concat is included to test order-sensitive aggregates.
	agg := []string{"count(*)", "sum(q.c)", "max(q.c)", "group_concat(q.c)",
		"group_concat(q.c)", "group_concat(q.c,'|')"}[rng.Intn(6)]
	switch rng.Intn(4) {
	case 0: // single table, grouped by its INTEGER PRIMARY KEY
		stmts = append(stmts, "SELECT k, count(*), a, b FROM p GROUP BY k ORDER BY k")
	case 1: // single table, grouped by the rowid pseudo-column
		stmts = append(stmts, "SELECT rowid, count(*), a, b FROM p GROUP BY rowid ORDER BY rowid")
	case 2: // a join, with every bare column on the pinned side
		stmts = append(stmts, "SELECT p.k, "+agg+", p.a, p.b FROM p LEFT JOIN q ON q.pk=p.k GROUP BY p.k ORDER BY p.k")
	default: // a comma join, same rule
		stmts = append(stmts, "SELECT p.k, "+agg+", p.a, p.b FROM p, q WHERE q.pk=p.k GROUP BY p.k ORDER BY p.k")
	}
	return stmts
}
