package compat

// Gates on aliased join groups with USING/NATURAL joins that coalesce column
// names. Two corrections verify the model: a bare "*" over the group omits
// NOEXPAND columns (correction 1), and member-qualified "*" fails when the
// group is the FROM clause's only item because selectExpander emits the
// ephemeral table name unqualified, which resolution cannot find (correction 2).

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// mustCgoDB opens one ORACLE database over setup. r26Pair already opens an
// engine/oracle pair, but the assertions below need the oracle's own COLUMN LIST
// and its exact ERROR TEXT, which that helper folds away.
func mustCgoDB(t *testing.T, setup []string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range setup {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
	}
	return db
}

// r28nNFSchema. f1 declares the shared name FIRST and kf/kg likewise, which is
// the ONLY arrangement rule (A)'s front-move decline does not already cover --
// i.e. the arrangement that isolates rule (B). m1t/m2t put it in the MIDDLE of
// three columns, so the ":N" positions vary too.
var r28nNFSchema = []string{
	`CREATE TABLE f1(b,a)`, `CREATE TABLE f2(b,g)`, `CREATE TABLE fc(z)`,
	`CREATE TABLE kf(k,x)`, `CREATE TABLE kg(k,y)`,
	`CREATE TABLE m1t(p,b,q)`, `CREATE TABLE m2t(r,b,s)`,
	`INSERT INTO f1 VALUES(2,1),(4,3),(NULL,5)`,
	`INSERT INTO f2 VALUES(2,'x'),(4,'y'),(6,'w')`,
	`INSERT INTO fc VALUES('p')`,
	`INSERT INTO kf VALUES(1,'x1')`, `INSERT INTO kg VALUES(1,'y1')`,
	`INSERT INTO m1t VALUES('p1',2,'q1')`, `INSERT INTO m2t VALUES('r1',2,'s1')`,
}

// TestR28NNestedFromBareStar verifies that a bare "*" over an aliased group
// skips NOEXPAND and ROWID columns, matching the oracle's column list.
func TestR28NNestedFromBareStar(t *testing.T) {
	cmp := r26Pair(t, r28nNFSchema)
	for _, tc := range []struct{ q, wantOracleCols string }{
		{`SELECT * FROM (f1 JOIN f2 USING(b)) AS gq`, "b,a,g"},
		{`SELECT * FROM (f1 NATURAL JOIN f2) AS gq`, "b,a,g"},
		{`SELECT * FROM (kf JOIN kg USING(k)) AS g`, "k,x,y"},
		{`SELECT * FROM (kf LEFT JOIN kg USING(k)) AS g`, "k,x,y"},
		{`SELECT * FROM (f1 JOIN f2 USING(b)) AS ""`, "b,a,g"},
		{`SELECT * FROM fc, (f1 JOIN f2 USING(b)) AS gq`, "z,b,a,g"},
	} {
		// The oracle's own column list first: the whole point of correction 1 is
		// that it has no ":N" column in it at all.
		cCols, _, cErr := cgoSelect(t, mustCgoDB(t, r28nNFSchema), tc.q, nil)
		if cErr != nil {
			t.Errorf("[%s] the oracle now REJECTS a bare star over the group: %v -- re-measure", tc.q, cErr)
			continue
		}
		if got := strings.Join(cCols, ","); got != tc.wantOracleCols {
			t.Errorf("[%s] the ORACLE's column list changed: got %q, want %q -- the NOEXPAND suppression is the whole model, re-derive it before trusting anything here", tc.q, got, tc.wantOracleCols)
			continue
		}
		if strings.Contains(strings.Join(cCols, ","), ":") {
			t.Errorf("[%s] the oracle's bare star now EXPOSES a \":N\" column (%v) -- correction 1 is dead", tc.q, cCols)
		}
		declined, agrees, _, detail := cmp(tc.q)
		if declined {
			continue
		}
		if !agrees {
			t.Errorf("[%s] the engine now ANSWERS it and DIVERGES: %s", tc.q, detail)
			continue
		}
		t.Logf("[%s] the engine now answers it and AGREES -- rule (B) must have been narrowed; move this line into an asserting gate", tc.q)
	}
}

// TestR28NNestedFromQualifiedStar verifies that "<member>.*" over a rebuilt group
// fails when the group is the FROM clause's only item, and succeeds when another
// FROM item exists, confirming the nSrc-driven rule rather than alias-driven.
func TestR28NNestedFromQualifiedStar(t *testing.T) {
	cdb := mustCgoDB(t, r28nNFSchema)
	cmp := r26Pair(t, r28nNFSchema)

	// The group is the whole FROM clause: REJECTED, and the error names the
	// uniquified column so a change in SQLite's uniquifier is visible.
	for _, tc := range []struct{ q, wantErr string }{
		{`SELECT f1.* FROM (f1 JOIN f2 USING(b)) AS gq`, "no such column: b:1"},
		{`SELECT f2.* FROM (f1 JOIN f2 USING(b)) AS gq`, "no such column: b:2"},
		{`SELECT f1.* FROM (f1 NATURAL JOIN f2) AS gq`, "no such column: b:1"},
		{`SELECT f2.* FROM (f1 NATURAL FULL JOIN f2) AS gq`, "no such column: b:2"},
		{`SELECT f1.* FROM (f1 JOIN f2 USING(b)) AS ""`, "no such column: b:1"},
		{`SELECT kf.* FROM (kf JOIN kg USING(k)) AS g`, "no such column: k:1"},
		{`SELECT kg.* FROM (kf JOIN kg USING(k)) AS g`, "no such column: k:2"},
		{`SELECT m1t.* FROM (m1t JOIN m2t USING(b)) AS g`, "no such column: b:1"},
		{`SELECT m2t.* FROM (m1t JOIN m2t USING(b)) AS g`, "no such column: b:2"},
	} {
		_, _, cErr := cgoSelect(t, cdb, tc.q, nil)
		if cErr == nil {
			t.Errorf("[%s] the ORACLE now ANSWERS it -- correction 2 is dead, re-measure the whole model", tc.q)
			continue
		}
		if !strings.Contains(cErr.Error(), tc.wantErr) {
			t.Errorf("[%s] the oracle's rejection changed: got %q, want it to contain %q", tc.q, cErr, tc.wantErr)
		}
		if declined, _, _, _ := cmp(tc.q); !declined {
			t.Errorf("[%s] the engine ANSWERS what the oracle rejects -- rule (B) was narrowed without emitting the suppressed \":N\" copy", tc.q)
		}
	}

	// One more FROM item, so the SAME expansion is emitted QUALIFIED and the
	// oracle ANSWERS it, naming the column "b:1"/"b:2". This is the half that
	// proves the rule is nSrc-driven and not alias-driven.
	for _, tc := range []struct{ q, wantOracleCols string }{
		{`SELECT f1.* FROM fc, (f1 JOIN f2 USING(b)) AS gq`, "b:1,a"},
		{`SELECT f2.* FROM fc, (f1 JOIN f2 USING(b)) AS gq`, "b:2,g"},
		{`SELECT f1.* FROM fc, (f1 JOIN f2 USING(b)) g2`, "b:1,a"},
		{`SELECT f1.* FROM fc, (f1 NATURAL JOIN f2) AS gq`, "b:1,a"},
		{`SELECT kf.* FROM fc, (kf JOIN kg USING(k)) AS g`, "k:1,x"},
	} {
		cCols, _, cErr := cgoSelect(t, cdb, tc.q, nil)
		if cErr != nil {
			t.Errorf("[%s] the ORACLE now REJECTS it (%v) -- correction 2 said a non-leading group answers; re-measure", tc.q, cErr)
			continue
		}
		if got := strings.Join(cCols, ","); got != tc.wantOracleCols {
			t.Errorf("[%s] the oracle's column list changed: got %q, want %q", tc.q, got, tc.wantOracleCols)
		}
	}

	// ...and the GROUP-ALIAS-qualified star is rejected in BOTH engines, for the
	// same reason in the oracle (sqlite3MatchEName never sees the alias, only the
	// members' names) -- kept so a future "make gq.* work" change has to notice.
	for _, q := range []string{
		`SELECT gq.* FROM (f1 JOIN f2 USING(b)) AS gq`,
		`SELECT g.* FROM (kf JOIN kg USING(k)) AS g`,
	} {
		if _, _, cErr := cgoSelect(t, cdb, q, nil); cErr == nil {
			t.Errorf("[%s] the oracle now expands a GROUP-ALIAS-qualified star -- re-measure", q)
		}
		if declined, _, _, _ := cmp(q); !declined {
			t.Errorf("[%s] the engine ANSWERED a group-alias-qualified star the oracle rejects", q)
		}
	}
}

// TestR28NNestedFromNamedRefs verifies that NAMED references into an aliased
// group ignore NOEXPAND entirely, answering on the oracle where currently declined.
func TestR28NNestedFromNamedRefs(t *testing.T) {
	cdb := mustCgoDB(t, r28nNFSchema)
	cmp := r26Pair(t, r28nNFSchema)
	for _, tc := range []struct{ q, wantOracleCols string }{
		{`SELECT b FROM (f1 JOIN f2 USING(b)) AS gq`, "b"},
		{`SELECT gq.b FROM (f1 JOIN f2 USING(b)) AS gq`, "b"},
		{`SELECT gq.a FROM (f1 JOIN f2 USING(b)) AS gq`, "a"},
		{`SELECT f1.b FROM (f1 JOIN f2 USING(b)) AS gq`, "b:1"},
		{`SELECT f2.b FROM (f1 JOIN f2 USING(b)) AS gq`, "b:2"},
		{`SELECT a, b, g FROM (f1 JOIN f2 USING(b)) AS gq`, "a,b,g"},
		{`SELECT k, x, y FROM (kf JOIN kg USING(k)) AS g`, "k,x,y"},
		// A double-quoted ":N" spelling is NOT a column: nothing matches it, so
		// SQLite's double-quoted-string misfeature makes it the TEXT 'b:1'.
		{`SELECT "b:1" FROM (f1 JOIN f2 USING(b)) AS gq`, `"b:1"`},
		// ...but the GROUP-ALIAS-qualified spelling IS one, resolved through the
		// ephemeral table's own name index. This engine declines it even with
		// rule (B) narrowed, which is a clean decline and not part of the 28.
		{`SELECT gq."b:1" FROM (f1 JOIN f2 USING(b)) AS gq`, "b:1"},
	} {
		cCols, _, cErr := cgoSelect(t, cdb, tc.q, nil)
		if cErr != nil {
			t.Errorf("[%s] the ORACLE now rejects it: %v -- re-measure", tc.q, cErr)
			continue
		}
		if got := strings.Join(cCols, ","); got != tc.wantOracleCols {
			t.Errorf("[%s] the oracle's column list changed: got %q, want %q", tc.q, got, tc.wantOracleCols)
		}
		if declined, agrees, _, detail := cmp(tc.q); !declined && !agrees {
			t.Errorf("[%s] the engine ANSWERS it and DIVERGES: %s", tc.q, detail)
		}
	}
}
