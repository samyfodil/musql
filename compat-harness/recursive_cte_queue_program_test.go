// Gate for recursive CTE queue program ordering, compared cell-by-cell against C SQLite.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var recQueueProgramSetup = []string{
	`CREATE TABLE edge(xfrom, xto, seq)`,
	`INSERT INTO edge VALUES(0,1,10),(0,2,20),(0,3,30),(1,4,40),(2,5,50),(3,6,60),(3,7,70),(4,8,80),(5,9,90)`,
	`CREATE TABLE tie(parent, k, tag)`,
	`INSERT INTO tie VALUES('s',1,'A'),('s',1,'B'),('s',0,'C'),('A',0,'D'),('B',0,'E'),('C',1,'F')`,
	`CREATE TABLE tt(k TEXT COLLATE NOCASE)`,
	`INSERT INTO tt VALUES('a'),('A'),('b'),('B')`,
}

var recQueueProgramQueries = []string{
	// The three loop divergences.
	`WITH c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<5 LIMIT -1) SELECT group_concat(x) FROM c`,
	`WITH c(x) AS (SELECT abs(-9223372036854775807-1) UNION ALL SELECT x+1 FROM c LIMIT 0) SELECT count(*) FROM c`,
	`WITH c(x) AS (VALUES(1) UNION ALL SELECT CASE WHEN x=2 THEN abs(-9223372036854775807-1) ELSE x+1 END FROM c LIMIT 2) SELECT group_concat(x) FROM c`,
	// ORDER BY ties break by insertion order.
	`WITH RECURSIVE r(k,tag) AS (VALUES(0,'s') UNION ALL SELECT tie.k, tie.tag FROM r, tie WHERE tie.parent=r.tag ORDER BY 1) SELECT group_concat(tag) FROM r`,
	`WITH RECURSIVE r(k,tag) AS (VALUES(0,'s') UNION ALL SELECT tie.k, tie.tag FROM r, tie WHERE tie.parent=r.tag ORDER BY 1 DESC) SELECT group_concat(tag) FROM r`,
	// Queue discipline, LIMIT/OFFSET, negative OFFSET.
	`WITH RECURSIVE a(id,m) AS (VALUES(0,0) UNION ALL SELECT edge.xto, edge.seq FROM edge, a WHERE edge.xfrom=a.id ORDER BY 2 DESC) SELECT group_concat(id) FROM a`,
	`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n<20 ORDER BY 1 DESC LIMIT 5 OFFSET 3) SELECT group_concat(n) FROM c`,
	`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n<20 LIMIT 3 OFFSET -2) SELECT group_concat(n) FROM c`,
	`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n<20 LIMIT -1 OFFSET 17) SELECT group_concat(n) FROM c`,
	// Several recursive arms expand the same popped row in written order.
	`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x*2 FROM c WHERE x<10 UNION ALL SELECT x*3 FROM c WHERE x<10) SELECT group_concat(x) FROM c`,
	// UNION dedups the setup rows too, under the column's collation, first wins.
	`WITH RECURSIVE c(s) AS (SELECT k FROM tt UNION SELECT s||'x' FROM c WHERE length(s)<3) SELECT group_concat(s) FROM c`,
	// The self-reference under an alias, joined.
	`WITH RECURSIVE a(id) AS (VALUES(0) UNION ALL SELECT edge.xto FROM a AS p JOIN edge ON edge.xfrom=p.id) SELECT group_concat(id) FROM a`,
	// A non-terminating recursion bounded only by its consumer.
	`WITH RECURSIVE i(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM i) SELECT group_concat(x) FROM (SELECT x FROM i LIMIT 7)`,
	// Two references, each its own queue.
	`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<3) SELECT (SELECT group_concat(x) FROM c), (SELECT count(*) FROM c)`,
	// A recursive CTE inside a recursive arm's WHERE.
	`WITH RECURSIVE a(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM a WHERE x < (WITH RECURSIVE b(y) AS (VALUES(1) UNION ALL SELECT y+1 FROM b WHERE y<4) SELECT max(y) FROM b)) SELECT group_concat(x) FROM a`,
	// A queue row carries no subtype (it is a record in C).
	`WITH RECURSIVE c(j,n) AS (SELECT json_array(1), 1 UNION ALL SELECT j, n+1 FROM c WHERE n<2) SELECT group_concat(json_quote(j)) FROM c`,
	// A recursive CTE in a scalar subquery, evaluated per enclosing row.
	`SELECT column1, (WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<3) SELECT group_concat(x) FROM c) FROM (VALUES(1),(3))`,
}

func TestRecursiveCTEQueueProgramOrdered(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	for _, s := range recQueueProgramSetup {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %q: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo %q: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range recQueueProgramQueries {
		ecols, ev, eerr := p.QueryArgs(q, nil)
		ccols, cr, cerr := cgoSelect(t, cdb, q, nil)
		if cerr != nil {
			t.Errorf("[%s] the oracle rejected a query this gate expects to be answered: %v", q, cerr)
			continue
		}
		if eerr != nil {
			t.Errorf("[%s] engine error where C SQLite answers %v: %v", q, cr, eerr)
			continue
		}
		eRows := engineRowsToStrings(ev)
		if ok, reason := queryResultsMatch(ecols, eRows, ccols, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
}

// TestInsertSelectCoroutineErrorOrder pins WHEN an INSERT ... SELECT source
// runs relative to the rows it inserts, which only an error can show.
// sqlite3Insert runs the source as a co-routine (insert.c:1140-1156), so a
// source row that raises is computed AFTER every earlier row was inserted. It
// materializes the source into a temp table first only for a trigger or a
// source that reads the target (insert.c:1167-1169), and then a source error
// comes before any insert.
//
// The observable case is a CONSTRAINT failure under OR FAIL that the co-routine
// reaches before a later source row's error: OP_Halt records OE_Fail
// (vdbe.c:1330) and the statement's earlier rows are released
// (vdbeaux.c:3445). A non-constraint error leaves errorAction at OE_Abort
// (vdbeaux.c:2615) and rolls the statement back under any conflict clause.
func TestInsertSelectCoroutineErrorOrder(t *testing.T) {
	type step struct {
		sql      string
		wantRows string // a SELECT whose ordered rows are compared afterwards; "" for none
	}
	steps := []step{
		{`CREATE TABLE u(a UNIQUE)`, ""},
		{`INSERT INTO u VALUES(2)`, ""},
		// The UNIQUE clash at row 2 comes first; row 1 stays under OR FAIL.
		{`WITH c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<3) INSERT OR FAIL INTO u SELECT CASE WHEN x=3 THEN abs(-9223372036854775807-1) ELSE x END FROM c`, `SELECT rowid, a FROM u ORDER BY rowid`},
		// The overflow at row 3 comes after two inserted rows, and rolls both
		// back even under OR FAIL -- the streamed rows' undo.
		{`CREATE TABLE u2(a UNIQUE)`, ""},
		{`WITH c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<5) INSERT OR FAIL INTO u2 SELECT CASE WHEN x=3 THEN abs(-9223372036854775807-1) ELSE x END FROM c`, `SELECT rowid, a FROM u2 ORDER BY rowid`},
		{`WITH c(x) AS (VALUES(10) UNION ALL SELECT x+1 FROM c WHERE x<15) INSERT INTO u2 SELECT CASE WHEN x=13 THEN abs(-9223372036854775807-1) ELSE x END FROM c`, `SELECT rowid, a FROM u2 ORDER BY rowid`},
		// A source reading the target is materialized: its error at the third
		// row precedes the insert of the first and the clash of the second.
		{`CREATE TABLE v(a UNIQUE)`, ""},
		{`INSERT INTO v VALUES(5),(6),(7)`, ""},
		{`INSERT OR FAIL INTO v SELECT CASE a WHEN 5 THEN 15 WHEN 6 THEN 6 ELSE abs(-9223372036854775807-1) END FROM v`, `SELECT rowid, a FROM v ORDER BY rowid`},
		// So is a triggered target's.
		{`CREATE TABLE w(a UNIQUE)`, ""},
		{`INSERT INTO w VALUES(2)`, ""},
		{`CREATE TABLE wlog(x)`, ""},
		{`CREATE TRIGGER wt AFTER INSERT ON w BEGIN INSERT INTO wlog VALUES(new.a); END`, ""},
		{`WITH c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<3) INSERT OR FAIL INTO w SELECT CASE WHEN x=3 THEN abs(-9223372036854775807-1) ELSE x END FROM c`, `SELECT (SELECT group_concat(a) FROM w), (SELECT group_concat(x) FROM wlog)`},
	}
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	for _, st := range steps {
		eerr := edb.Exec(st.sql)
		_, cerr := cdb.Exec(st.sql)
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("[%s] engine err=%v, cgo err=%v", st.sql, eerr, cerr)
		}
		if st.wantRows == "" {
			continue
		}
		p, perr := edb.SnapshotPager()
		if perr != nil {
			t.Fatal(perr)
		}
		ecols, ev, qerr := p.QueryArgs(st.wantRows, nil)
		if qerr != nil {
			t.Fatalf("[%s] engine: %v", st.wantRows, qerr)
		}
		ccols, cr, qcerr := cgoSelect(t, cdb, st.wantRows, nil)
		if qcerr != nil {
			t.Fatalf("[%s] cgo: %v", st.wantRows, qcerr)
		}
		eRows := engineRowsToStrings(ev)
		if ok, reason := queryResultsMatch(ecols, eRows, ccols, cr, true); !ok {
			t.Errorf("after [%s]: DIVERGES: %s\n  engine: %v\n  cgo:    %v", st.sql, reason, eRows, cr)
		}
	}
}

// TestRecursiveCTEQueueProgramInsertRowids pins the order an INSERT ... SELECT
// assigns rowids in when its source is a recursive CTE: the queue order, row
// for row. The corpus cannot see this -- it compares the table's rows as a set.
func TestRecursiveCTEQueueProgramInsertRowids(t *testing.T) {
	stmts := append(append([]string(nil), recQueueProgramSetup...),
		`CREATE TABLE ins(tag)`,
		`WITH RECURSIVE r(k,tag) AS (VALUES(0,'s') UNION ALL SELECT tie.k, tie.tag FROM r, tie WHERE tie.parent=r.tag ORDER BY 1) INSERT INTO ins SELECT tag FROM r`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x*2 FROM c WHERE x<10 UNION ALL SELECT x*3 FROM c WHERE x<10) INSERT INTO ins SELECT x FROM c`,
		`WITH RECURSIVE a(id,m) AS (VALUES(0,0) UNION ALL SELECT edge.xto, edge.seq FROM edge, a WHERE edge.xfrom=a.id ORDER BY 2 DESC LIMIT 6 OFFSET 2) INSERT INTO ins SELECT id FROM a`,
	)
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	for _, s := range stmts {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %q: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo %q: %v", s, err)
		}
	}
	const q = `SELECT rowid, tag FROM ins ORDER BY rowid`
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	ecols, ev, eerr := p.QueryArgs(q, nil)
	if eerr != nil {
		t.Fatal(eerr)
	}
	ccols, cr, cerr := cgoSelect(t, cdb, q, nil)
	if cerr != nil {
		t.Fatal(cerr)
	}
	eRows := engineRowsToStrings(ev)
	if ok, reason := queryResultsMatch(ecols, eRows, ccols, cr, true); !ok {
		t.Errorf("DIVERGES: %s\n  engine: %v\n  cgo:    %v", reason, eRows, cr)
	}
}
