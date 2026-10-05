package replication

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

const usersDDL = `CREATE TABLE users(id TEXT PRIMARY KEY, name TEXT NOT NULL, age INTEGER)`

func newTestStore(t *testing.T, site string) *Store {
	t.Helper()
	// The bare "site" here is a shared-cache in-memory database NAME, not a
	// path: driver backs it with a temp file under os.TempDir()
	// (memdb.go). Before that interception existed these DSNs were opened as
	// RELATIVE PATHS and this helper littered crdt/ with files called n1,
	// gate, syncA... -- "make test" now fails on any such stray
	// (check-no-stray-dbs), and driver's TestMemoryDSNsCreateNoFilesInCWD
	// pins the interception itself.
	db, err := openApplyDB("file:" + site + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // keep the shared in-memory db alive for the test
	t.Cleanup(func() { db.Close() })
	s, err := OpenStore(context.Background(), db, site)
	if err != nil {
		t.Fatal(err)
	}
	installUsers(t, s)
	return s
}

// usersID and ucol are the IDs of usersDDL's table and columns: content IDs,
// so any node that creates it from this text agrees on them.
var usersID = objectID("table", "users", usersDDL, 0)

func ucol(name string) string { return initialColID(usersID, name) }

// installUsers creates the users table the way a peer would: as a schema op
// from another site.
func installUsers(t *testing.T, s *Store) {
	t.Helper()
	op := Op{Site: "schema", Seq: 1, HLC: 1, Kind: OpSchema,
		Cells: schemaOp{Act: actCreate, ID: usersID, Type: "table", Name: "users", SQL: usersDDL}.cells()}
	if err := s.Ingest(context.Background(), op, false); err != nil {
		t.Fatal(err)
	}
}

func insertOp(site string, seq uint64, hlc HLC, id, name string, age int64) Op {
	return Op{Site: site, Seq: seq, HLC: hlc, Tbl: usersID, PK: EncodePK(id), Kind: OpInsert,
		Cells: []Cell{CellFromValue(ucol("name"), name), CellFromValue(ucol("age"), age)}}
}

func updateOp(site string, seq uint64, hlc HLC, id string, cells ...Cell) Op {
	return Op{Site: site, Seq: seq, HLC: hlc, Tbl: usersID, PK: EncodePK(id), Kind: OpUpdate, Cells: cells}
}

func deleteOp(site string, seq uint64, hlc HLC, id string) Op {
	return Op{Site: site, Seq: seq, HLC: hlc, Tbl: usersID, PK: EncodePK(id), Kind: OpDelete}
}

func dumpUsers(t *testing.T, s *Store) string {
	t.Helper()
	rows, err := s.db.Query(`SELECT id, name, age FROM users ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, name string
		var age sql.NullInt64
		if err := rows.Scan(&id, &name, &age); err != nil {
			t.Fatal(err)
		}
		ageStr := "NULL"
		if age.Valid {
			ageStr = fmt.Sprintf("%d", age.Int64)
		}
		fmt.Fprintf(&b, "%s|%s|%s\n", id, name, ageStr)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func dumpClock(t *testing.T, s *Store) string {
	t.Helper()
	rows, err := s.db.Query(`SELECT tbl, quote(pk), col, hlc, site, quote(val) FROM _repl_clock ORDER BY tbl, pk, col`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var tbl, pk, col, site, val string
		var hlc int64
		if err := rows.Scan(&tbl, &pk, &col, &hlc, &site, &val); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s|%s|%s|%d|%s|%s\n", tbl, pk, col, hlc, site, val)
	}
	return b.String()
}

// scenario returns a fixed set of ops from two sites that collectively exercise:
// concurrent per-column LWW (u1 name vs age), delete-beats-insert (u2), and
// insert-resurrects-after-delete (u3).
func scenario() []Op {
	return []Op{
		insertOp("A", 1, 100, "u1", "alice", 30),
		updateOp("A", 2, 300, "u1", CellFromValue(ucol("age"), int64(31))),
		insertOp("A", 3, 600, "u3", "carol", 40),

		updateOp("B", 1, 200, "u1", CellFromValue(ucol("name"), "alison")),
		insertOp("B", 2, 400, "u2", "bob", 25),
		deleteOp("B", 3, 500, "u2"),
		deleteOp("B", 4, 550, "u3"),
	}
}

const wantUsers = "u1|alison|31\nu3|carol|40\n"

func ingestAll(t *testing.T, s *Store, ops []Op) {
	t.Helper()
	for _, op := range ops {
		if err := s.Ingest(context.Background(), op, false); err != nil {
			t.Fatalf("ingest %+v: %v", op, err)
		}
	}
}

func reversed(ops []Op) []Op {
	out := make([]Op, len(ops))
	for i, op := range ops {
		out[len(ops)-1-i] = op
	}
	return out
}

// TestConvergence feeds the same ops to independent stores in different orders,
// with duplicates, and requires identical user tables and clocks — plus the one
// correct converged value.
func TestConvergence(t *testing.T) {
	ops := scenario()

	// order 1: natural
	s1 := newTestStore(t, "n1")
	ingestAll(t, s1, ops)

	// order 2: fully reversed (every op arrives before its causal predecessors)
	s2 := newTestStore(t, "n2")
	ingestAll(t, s2, reversed(ops))

	// order 3: hand-picked interleave with duplicates (idempotency)
	s3 := newTestStore(t, "n3")
	idx := []int{1, 6, 0, 3, 1, 5, 2, 4, 6, 3, 0, 2, 5, 4}
	shuffled := make([]Op, 0, len(idx))
	for _, i := range idx {
		shuffled = append(shuffled, ops[i])
	}
	ingestAll(t, s3, shuffled)

	d1, d2, d3 := dumpUsers(t, s1), dumpUsers(t, s2), dumpUsers(t, s3)
	if d1 != wantUsers {
		t.Fatalf("natural-order users =\n%s\nwant\n%s", d1, wantUsers)
	}
	if d1 != d2 || d1 != d3 {
		t.Fatalf("user tables diverged:\n-- n1 --\n%s-- n2 --\n%s-- n3 --\n%s", d1, d2, d3)
	}

	c1, c2, c3 := dumpClock(t, s1), dumpClock(t, s2), dumpClock(t, s3)
	if c1 != c2 || c1 != c3 {
		t.Fatalf("clocks diverged:\n-- n1 --\n%s-- n2 --\n%s-- n3 --\n%s", c1, c2, c3)
	}
}

// TestUpdateBeforeInsert isolates the completeness gate: an UPDATE that lands
// before the INSERT that first populates the NOT NULL columns must not
// materialize a torn/NULL row — the row stays absent until the INSERT arrives.
func TestUpdateBeforeInsert(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, "gate")

	// UPDATE age arrives first; name (NOT NULL) has no value yet.
	if err := s.Ingest(ctx, updateOp("A", 2, 300, "u1", CellFromValue(ucol("age"), int64(9))), false); err != nil {
		t.Fatal(err)
	}
	if got := dumpUsers(t, s); got != "" {
		t.Fatalf("row materialized before its insert (would be a NULL name): %q", got)
	}

	// INSERT arrives; now name is present -> row materializes with LWW age.
	if err := s.Ingest(ctx, insertOp("A", 1, 100, "u1", "alice", 30), false); err != nil {
		t.Fatal(err)
	}
	if got, want := dumpUsers(t, s), "u1|alice|9\n"; got != want {
		t.Fatalf("after insert users = %q, want %q", got, want)
	}
}

// TestDeleteVsUpdate pins the presence LWW both ways round.
func TestDeleteVsUpdate(t *testing.T) {
	// delete (hlc 500) beats a later-arriving but older update (hlc 300).
	s := newTestStore(t, "dvu1")
	ingestAll(t, s, []Op{
		insertOp("A", 1, 100, "u1", "alice", 30),
		deleteOp("B", 1, 500, "u1"),
		updateOp("A", 2, 300, "u1", CellFromValue(ucol("name"), "aly")),
	})
	if got := dumpUsers(t, s); got != "" {
		t.Fatalf("delete(500) should win over update(300); got %q", got)
	}

	// update (hlc 700) beats an earlier delete (hlc 500): row resurrects.
	s2 := newTestStore(t, "dvu2")
	ingestAll(t, s2, []Op{
		insertOp("A", 1, 100, "u1", "alice", 30),
		deleteOp("B", 1, 500, "u1"),
		updateOp("A", 2, 700, "u1", CellFromValue(ucol("name"), "aly")),
	})
	if got, want := dumpUsers(t, s2), "u1|aly|30\n"; got != want {
		t.Fatalf("update(700) should resurrect over delete(500); got %q want %q", got, want)
	}
}

// TestIngestRefusesASharedRowidRange: two sites whose rowid ranges are one would
// hand out the same rowids and merge different rows as one -- on either of them,
// and on any third node that admitted both. Each node admits the first it sees
// and refuses the other. s94558 and s113311 hash to one range.
func TestIngestRefusesASharedRowidRange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, "s94558")
	err := s.Ingest(ctx, insertOp("s113311", 1, 100, "u1", "alice", 30), false)
	if err == nil || !strings.Contains(err.Error(), "shares a rowid range") {
		t.Fatalf("got %v, want the shared-range refusal", err)
	}
	if err := s.Ingest(ctx, insertOp("other", 1, 100, "u1", "alice", 30), false); err != nil {
		t.Fatal(err)
	}

	// A third node: whichever it sees first is the one it keeps.
	for _, order := range [][2]string{{"s94558", "s113311"}, {"s113311", "s94558"}} {
		c := newTestStore(t, "third-"+order[0])
		if err := c.Ingest(ctx, insertOp(order[0], 1, 100, "u1", "first", 1), false); err != nil {
			t.Fatal(err)
		}
		err := c.Ingest(ctx, insertOp(order[1], 1, 200, "u2", "second", 2), false)
		if err == nil || !strings.Contains(err.Error(), "shares a rowid range") {
			t.Fatalf("%v: got %v, want the shared-range refusal", order, err)
		}
		if err := c.Ingest(ctx, insertOp(order[0], 2, 300, "u3", "first again", 3), false); err != nil {
			t.Fatal(err)
		}
		if got, want := dumpUsers(t, c), "u1|first|1\nu3|first again|3\n"; got != want {
			t.Fatalf("%v: got %q want %q", order, got, want)
		}
	}
}

// TestIngestRefusesASecondWriterOfOneSite: two nodes writing under one site id
// (a copied database file) put different ops at one seq; the second is an error,
// not a duplicate quietly dropped. The same op again is a duplicate.
func TestIngestRefusesASecondWriterOfOneSite(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, "dupsite")
	if err := s.Ingest(ctx, insertOp("x", 1, 100, "u1", "alice", 30), false); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, insertOp("x", 1, 100, "u1", "alice", 30), false); err != nil {
		t.Fatalf("a redelivered op: %v", err)
	}
	err := s.Ingest(ctx, insertOp("x", 1, 100, "u1", "bob", 30), false)
	if err == nil || !strings.Contains(err.Error(), "two different ops") {
		t.Fatalf("got %v, want the two-writers refusal", err)
	}
}
