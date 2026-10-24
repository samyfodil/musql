package engine

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// catchUpPair is a database two sessions hold, as two connections do: a
// table with a UNIQUE key, the replication clock's shape, and a plain one.
func catchUpPair(t *testing.T) (path string, a, b *Session) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "c.musq")
	stmts := []string{
		`CREATE TABLE c(tbl TEXT, pk BLOB, col TEXT, v INTEGER, PRIMARY KEY(tbl, pk, col))`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`,
		`CREATE INDEX p_k ON p(k)`,
	}
	for i := range 300 {
		stmts = append(stmts,
			fmt.Sprintf(`INSERT INTO c VALUES('t', x'%04x', 'a', %d)`, i, i),
			fmt.Sprintf(`INSERT INTO p(k, s) VALUES(%d, 'r%d')`, i%17, i))
	}
	buildDB(t, path, stmts...)
	open := func() *Session {
		s, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Discard() })
		return s
	}
	return path, open(), open()
}

func mustCommit(t *testing.T, s *Session, stmts ...string) {
	t.Helper()
	if _, err := s.RefreshIfStale(); err != nil {
		t.Fatal(err)
	}
	for _, q := range stmts {
		if err := s.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := s.Commit(); err != nil {
		t.Fatal(err)
	}
}

func render(t *testing.T, s *Session, q string) string {
	t.Helper()
	_, rows, err := s.Query(q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	var b strings.Builder
	for _, r := range rows {
		for _, c := range r {
			fmt.Fprintf(&b, "%d:%d:%v:%q|", c.Typ, c.I, c.F, string(c.S))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// indexesExact checks every index s's stores hold against one built from
// scratch over a session opened on the file now.
func indexesExact(t *testing.T, path string, s *Session) {
	t.Helper()
	f, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Discard()
	for _, tb := range s.tables {
		st := tb.rows
		if st == nil || tb.isTemp {
			continue
		}
		ft := f.tableNamed(tb.name)
		if err := f.ensureTableLoaded(ft); err != nil {
			t.Fatal(err)
		}
		for idx, ix := range st.uqIdx {
			colls, _ := uniqCollations(idx)
			want := ft.rows.buildUniqIndex(ft, f.uniqueIndexLike(idx, ft), colls)
			if !sameRowidLists(ix.keys, want.keys) || !slices.Equal(sortedCopy(ix.loose), sortedCopy(want.loose)) {
				t.Fatalf("%s: the carried UNIQUE probe for %s is not the index of the rows the file holds", tb.name, idx.name)
			}
		}
		for col, ix := range st.eqIdx {
			if ix == nil {
				continue
			}
			ft.rows.eqIdx = nil
			want, ok := ft.rows.eqIndexFor(col)
			if !ok || !sameRowidLists(ix.rows, want.rows) || !sameRowidLists(ix.bytes, want.bytes) || !slices.Equal(ix.short, want.short) {
				t.Fatalf("%s: the carried equality index on column %d is not the index of the rows the file holds", tb.name, col)
			}
		}
	}
}

func sameRowidLists[K comparable](a, b map[K][]uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, l := range a {
		if !slices.Equal(sortedCopy(l), sortedCopy(b[k])) {
			return false
		}
	}
	return true
}

func sortedCopy(l []uint64) []uint64 {
	l = slices.Clone(l)
	slices.Sort(l)
	return l
}

// buildIndexes has s build the indexes a replicated write builds: the UNIQUE
// probe of an upsert, and an equality index on p.k.
func buildIndexes(t *testing.T, s *Session) {
	t.Helper()
	mustCommit(t, s, `INSERT INTO c VALUES('t', x'0001', 'a', -1) ON CONFLICT(tbl, pk, col) DO UPDATE SET v = excluded.v`)
	p := s.tableNamed("p")
	if err := s.ensureTableLoaded(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.rows.eqIndexFor(1); !ok {
		t.Fatal("no equality index on p.k")
	}
}

// TestCatchUpFoldsInTheNewBatches: a session that another connection's commits
// left behind folds those batches into the delta state it has -- not a fresh
// replay of the whole log -- and then answers exactly what a session opened on
// the file answers: puts, kills, rows put again after a kill, UPDATEs written
// in place (patch records) and over.
func TestCatchUpFoldsInTheNewBatches(t *testing.T) {
	_, a, b := catchUpPair(t)
	render(t, b, `SELECT count(*) FROM c`)
	mustCommit(t, b, `INSERT INTO p(k, s) VALUES(1, 'b0')`)
	st := deltaStateOf(b.src)
	rounds := [][]string{
		{`INSERT INTO c VALUES('t', x'ffff', 'a', 1)`, `DELETE FROM p WHERE id % 7 = 0`},
		{`UPDATE p SET s = s || '!' WHERE k = 3`, `UPDATE c SET v = v + 1000 WHERE pk < x'0010'`},
		{`INSERT INTO p(id, k, s) VALUES(7, 99, 'again')`, `DELETE FROM c WHERE pk = x'0005'`},
		{`UPDATE p SET k = k + 1 WHERE id < 50`, `INSERT INTO c VALUES('t', x'0005', 'a', 5)`},
	}
	for i, r := range rounds {
		mustCommit(t, a, r...)
		render(t, b, `SELECT count(*) FROM p WHERE k < 5`)
		if b.segColumns == nil || b.segColumnsStale {
			t.Fatalf("round %d: a read left no committed column view", i)
		}
		if refreshed, err := b.RefreshIfStale(); err != nil || !refreshed {
			t.Fatalf("round %d: refresh %v, %v", i, refreshed, err)
		}
		if deltaStateOf(b.src) != st {
			t.Fatalf("round %d: the refresh replayed the delta whole instead of folding in the new batches", i)
		}
		if b.segColumns == nil || b.segColumnsStale {
			t.Fatalf("round %d: the refresh dropped the committed column view instead of moving it past the new batches", i)
		}
		for _, q := range []string{`SELECT rowid, * FROM c ORDER BY rowid`, `SELECT rowid, * FROM p ORDER BY rowid`, `SELECT k, count(*) FROM p WHERE k = 4 GROUP BY k`,
			`SELECT count(*), sum(k) FROM p WHERE k < 5`, `SELECT count(*) FROM c WHERE v > 100`} {
			if got, want := render(t, b, q), render(t, a, q); got != want {
				t.Fatalf("round %d: %s\ncaught up: %s\nwriter:    %s", i, q, got, want)
			}
		}
		mustCommit(t, b, fmt.Sprintf(`INSERT INTO p(k, s) VALUES(%d, 'b')`, i))
		if refreshed, err := a.RefreshIfStale(); err != nil || !refreshed {
			t.Fatalf("round %d: the writer's refresh %v, %v", i, refreshed, err)
		}
	}
}

// TestCatchUpCarriesExactIndexes: the indexes a session built move into the
// session its refresh rebuilds, each moved for exactly the rows the other
// connection's batches wrote -- a key deleted there conflicts with nothing
// here, a key added there conflicts, a key moved conflicts only where it went.
func TestCatchUpCarriesExactIndexes(t *testing.T) {
	path, a, b := catchUpPair(t)
	buildIndexes(t, b)
	ix := b.tableNamed("c").rows.uqIdx
	if len(ix) != 1 {
		t.Fatalf("%d UNIQUE probes built, want 1", len(ix))
	}
	var carried *rsUniqIndex
	for _, v := range ix {
		carried = v
	}
	mustCommit(t, a,
		`DELETE FROM c WHERE pk = x'0002'`,
		`INSERT INTO c VALUES('t', x'1000', 'a', 1)`,
		`UPDATE c SET pk = x'2000' WHERE pk = x'0003'`,
		`UPDATE p SET k = 1000 WHERE id < 20`,
		`DELETE FROM p WHERE id > 290`)
	if _, err := b.RefreshIfStale(); err != nil {
		t.Fatal(err)
	}
	for _, v := range b.tableNamed("c").rows.uqIdx {
		if v != carried {
			t.Fatal("the refresh built the UNIQUE probe again instead of carrying it")
		}
	}
	indexesExact(t, path, b)
	for _, c := range []struct {
		pk       string
		conflict bool
	}{{"0002", false}, {"1000", true}, {"0003", false}, {"2000", true}, {"0004", true}} {
		err := b.Exec(fmt.Sprintf(`INSERT INTO c VALUES('t', x'%s', 'a', 0)`, c.pk))
		if got := err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed"); got != c.conflict {
			t.Fatalf("x'%s': conflict %v (%v), want %v", c.pk, got, err, c.conflict)
		}
		if err == nil {
			if err := b.Exec(fmt.Sprintf(`DELETE FROM c WHERE pk = x'%s'`, c.pk)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got, want := render(t, b, `SELECT id FROM p WHERE k = 1000 ORDER BY id`), render(t, a, `SELECT id FROM p WHERE k = 1000 ORDER BY id`); got != want {
		t.Fatalf("p.k = 1000\ncaught up: %s\nwriter:    %s", got, want)
	}
}

// TestCatchUpTakesTurns: two sessions writing in turn, each catching up on the
// other's commit, keep exact indexes and the same rows throughout -- the
// shape of a replicated node's ingest beside its local writes.
func TestCatchUpTakesTurns(t *testing.T) {
	path, a, b := catchUpPair(t)
	buildIndexes(t, a)
	buildIndexes(t, b)
	for i := range 30 {
		w, other := a, b
		if i%2 == 1 {
			w, other = b, a
		}
		var stmts []string
		for j := range 20 {
			k := i*20 + j
			stmts = append(stmts,
				fmt.Sprintf(`INSERT INTO c VALUES('t', x'%04x', 'b', %d) ON CONFLICT(tbl, pk, col) DO UPDATE SET v = excluded.v`, k%250, k),
				fmt.Sprintf(`UPDATE p SET k = %d WHERE id = %d`, k%13, k%300+1))
		}
		stmts = append(stmts, fmt.Sprintf(`DELETE FROM c WHERE col = 'b' AND v %% 7 = %d`, i%7))
		mustCommit(t, w, stmts...)
		if _, err := other.RefreshIfStale(); err != nil {
			t.Fatal(err)
		}
		indexesExact(t, path, other)
		for _, q := range []string{`SELECT rowid, * FROM c ORDER BY rowid`, `SELECT rowid, * FROM p ORDER BY rowid`} {
			if got, want := render(t, other, q), render(t, w, q); got != want {
				t.Fatalf("turn %d: %s differs after the catch-up", i, q)
			}
		}
	}
}

// TestCatchUpAfterARewrite: a commit that rewrote the file (a catalog change)
// is no batch to fold in: the refresh reads the new file whole, and the
// indexes are built again from it.
func TestCatchUpAfterARewrite(t *testing.T) {
	path, a, b := catchUpPair(t)
	buildIndexes(t, b)
	st := deltaStateOf(b.src)
	mustCommit(t, a, `CREATE TABLE z(x)`, `INSERT INTO c VALUES('t', x'3000', 'a', 1)`)
	if _, err := b.RefreshIfStale(); err != nil {
		t.Fatal(err)
	}
	if deltaStateOf(b.src) == st {
		t.Fatal("the refresh folded batches into the state of a file that was rewritten")
	}
	indexesExact(t, path, b)
	if err := b.Exec(`INSERT INTO c VALUES('t', x'3000', 'a', 2)`); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("a key committed with the rewrite: %v, want a UNIQUE conflict", err)
	}
	if got, want := render(t, b, `SELECT name FROM sqlite_master ORDER BY name`), render(t, a, `SELECT name FROM sqlite_master ORDER BY name`); got != want {
		t.Fatalf("the rewritten file's catalog\ncaught up: %s\nwriter:    %s", got, want)
	}
}

// TestCatchUpKeepsIndexesThroughItsOwnCompaction: a commit that rewrites the
// file to fold the delta in moves no row, so the committing session's stores
// over the new file keep the indexes they had -- and its view of the new
// delta is one the next catch-up starts from.
func TestCatchUpKeepsIndexesThroughItsOwnCompaction(t *testing.T) {
	path, a, b := catchUpPair(t)
	buildIndexes(t, a)
	var before []*rsUniqIndex
	for _, v := range a.tableNamed("c").rows.uqIdx {
		before = append(before, v)
	}
	mustCommit(t, a, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 99999)
		INSERT INTO c SELECT 'u', zeroblob(0) || i, 'a', i FROM n`)
	if a.deltaEnd != segDeltaHdrSize {
		t.Fatal("a load past the compaction threshold was appended to the delta, not folded in by a rewrite")
	}
	after := slices.Collect(maps.Values(a.tableNamed("c").rows.uqIdx))
	if len(after) != 1 || len(before) != 1 || after[0] != before[0] {
		t.Fatal("the rewrite dropped the UNIQUE probe its stores had built")
	}
	indexesExact(t, path, a)
	mustCommit(t, b, `INSERT INTO c VALUES('t', x'6000', 'a', 1)`, `DELETE FROM c WHERE pk = x'0009'`)
	st := deltaStateOf(a.src)
	if _, err := a.RefreshIfStale(); err != nil {
		t.Fatal(err)
	}
	if deltaStateOf(a.src) != st {
		t.Fatal("the catch-up after the rewrite replayed the delta whole")
	}
	if now := slices.Collect(maps.Values(a.tableNamed("c").rows.uqIdx)); len(now) != 1 || now[0] != before[0] {
		t.Fatal("the catch-up after the rewrite built the UNIQUE probe again")
	}
	indexesExact(t, path, a)
}

// TestCatchUpAfterAFailedCommit: a commit refused because another connection
// committed first leaves the statement's rows in the stores, and the rebuild
// the refusal asks for (Poison) still folds in only the new batches -- the
// delta state holds nothing of the failed statement -- but builds its indexes
// again: a row the failed statement replaced was one this session had put
// itself, and its drop left no trace in the store to move it back by.
func TestCatchUpAfterAFailedCommit(t *testing.T) {
	path, a, b := catchUpPair(t)
	buildIndexes(t, b)
	mustCommit(t, b, `INSERT INTO c VALUES('t', x'7000', 'a', 1)`)
	st := deltaStateOf(b.src)
	for _, q := range []string{
		`INSERT INTO c VALUES('t', x'4000', 'a', 1)`,
		`DELETE FROM c WHERE pk = x'0007'`,
		`INSERT OR REPLACE INTO c VALUES('t', x'7000', 'a', 2)`,
		`UPDATE p SET k = 777 WHERE id = 3`,
	} {
		if err := b.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	mustCommit(t, a, `INSERT INTO c VALUES('t', x'5000', 'a', 1)`, `UPDATE p SET k = 778 WHERE id = 4`)
	if _, err := b.Commit(); err == nil {
		t.Fatal("a commit over a state another connection had moved was accepted")
	}
	b.Poison()
	if refreshed, err := b.RefreshIfStale(); err != nil || !refreshed {
		t.Fatalf("refresh %v, %v", refreshed, err)
	}
	if deltaStateOf(b.src) != st {
		t.Fatal("the rebuild after the failed commit replayed the delta whole")
	}
	indexesExact(t, path, b)
	for _, c := range []struct {
		pk       string
		conflict bool
	}{{"4000", false}, {"0007", true}, {"5000", true}, {"7000", true}} {
		err := b.Exec(fmt.Sprintf(`INSERT INTO c VALUES('t', x'%s', 'a', 0)`, c.pk))
		if got := err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed"); got != c.conflict {
			t.Fatalf("x'%s': conflict %v (%v), want %v", c.pk, got, err, c.conflict)
		}
	}
	for _, q := range []string{`SELECT id FROM p WHERE k = 777`, `SELECT id FROM p WHERE k = 778`, `SELECT rowid, v FROM c WHERE pk = x'7000'`} {
		if got, want := render(t, b, q), render(t, a, q); got != want {
			t.Fatalf("%s\nrebuilt: %s\nwriter:  %s", q, got, want)
		}
	}
}
