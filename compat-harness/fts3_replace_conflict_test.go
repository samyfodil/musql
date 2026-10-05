// Tests REPLACE and INSERT OR REPLACE against FTS3/FTS4 tables against C SQLite 3.53.3.
// FTS3 implements REPLACE by deleting then inserting in one segment. The tests
// read shadow table bytes directly to verify correct delete-marker handling and
// interchange files between engines.
package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// ftsReplaceShadowDump is what every case below reads back: the rows, then the
// raw bytes of every shadow table. The %_segdir root blobs are the assertion --
// they ARE the file format, and a delete marker written differently shows up
// here even when MATCH still answers correctly off %_content.
func ftsReplaceShadowDump(cols ...string) []string {
	sel := "docid"
	for _, c := range cols {
		sel += ", " + c
	}
	return []string{
		`SELECT ` + sel + ` FROM t ORDER BY docid`,
		`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
		`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
		`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
		`SELECT id, quote(value) FROM t_stat ORDER BY id`,
	}
}

// differAllAccepted is differ() plus the guard that makes the result mean
// something: a script whose CREATE or REPLACE both engines REJECTED agrees
// vacuously (a gate in this package once passed for exactly that reason), so
// every statement up to nAccepted must have SUCCEEDED on both sides.
func differAllAccepted(t *testing.T, name string, stmts []string, nAccepted int) {
	t.Helper()
	differ(t, name, stmts)
	for _, eng := range engineOrder {
		res := run(t, eng, stmts)
		for i := 0; i < nAccepted && i < len(res); i++ {
			if kind, _ := res[i]["kind"].(string); kind == "error" {
				t.Fatalf("[%s] %s REJECTED a statement this gate assumes it accepts (the comparison below would be vacuous): stmt #%d %s", name, eng, i, stmts[i])
			}
		}
	}
}

func TestFts3ReplaceShadowDiff(t *testing.T) {
	cases := []struct {
		name string
		// stmts is the write script; every one of them must succeed.
		stmts []string
		dump  []string
	}{
		{"REPLACE at a fresh docid is an ordinary INSERT", []string{
			`CREATE VIRTUAL TABLE t USING fts4`,
			`REPLACE INTO t(docid, content) VALUES (1, 'one two')`,
			`REPLACE INTO t(docid, content) VALUES (2, 'one two three four')`,
		}, ftsReplaceShadowDump("content")},

		// fts3conf.test 3.1-3.3, the mined statements this rule was closed for.
		{"REPLACE displacing an existing docid", []string{
			`CREATE VIRTUAL TABLE t USING fts4`,
			`REPLACE INTO t(docid, content) VALUES (1, 'one two')`,
			`REPLACE INTO t(docid, content) VALUES (2, 'one two three four')`,
			`REPLACE INTO t(docid, content) VALUES (1, 'one two three four five six')`,
			`SELECT quote(matchinfo(t, 'na')) FROM t WHERE t MATCH 'six'`,
		}, ftsReplaceShadowDump("content")},

		{"REPLACE writes the same bytes as the equivalent UPDATE", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t(docid,a,b) VALUES(1,'aa bb','cc')`,
			`INSERT INTO t(docid,a,b) VALUES(2,'dd','ee ff')`,
			`REPLACE INTO t(docid,a,b) VALUES(1,'gg','hh')`,
		}, ftsReplaceShadowDump("a", "b")},

		// fts3DeleteByRowid asks fts3IsEmpty first: displacing the ONLY row
		// runs fts3DeleteAll, so %_segdir restarts at idx 0 with no markers.
		{"REPLACE of the only row wipes every shadow table", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(7,'alpha beta')`,
			`REPLACE INTO t(docid,a) VALUES(7,'gamma')`,
		}, ftsReplaceShadowDump("a")},

		{"REPLACE of the only row of an fts3 table", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t(docid,a) VALUES(7,'alpha beta')`,
			`REPLACE INTO t(docid,a) VALUES(7,'gamma')`,
		}, []string{
			`SELECT docid, a FROM t ORDER BY docid`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
		}},

		{"multi-row REPLACE: one fresh docid and one displacement", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(5,'aa bb')`,
			`INSERT INTO t(docid,a) VALUES(9,'cc dd')`,
			`REPLACE INTO t(docid,a) VALUES(7,'xx'),(9,'yy')`,
		}, ftsReplaceShadowDump("a")},

		// The wipe and an auto-assigned docid in the SAME statement: %_content
		// is emptied, the explicit row goes back, and the next docid is one
		// past it -- 101, not 1.
		{"REPLACE wipes and then auto-assigns in one statement", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(100,'aa')`,
			`REPLACE INTO t(docid,a) VALUES(100,'bb'),(NULL,'cc')`,
			`SELECT last_insert_rowid()`,
		}, ftsReplaceShadowDump("a")},

		{"REPLACE with a NULL docid never displaces", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(3,'aa')`,
			`REPLACE INTO t(docid,a) VALUES(NULL,'bb')`,
			`REPLACE INTO t(a) VALUES('cc')`,
		}, ftsReplaceShadowDump("a")},

		{"REPLACE spelled INSERT OR REPLACE, and on the command channel", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'alpha')`,
			`INSERT INTO t(docid,a) VALUES(2,'beta')`,
			`INSERT OR REPLACE INTO t(docid,a) VALUES(1,'gamma')`,
			`INSERT OR REPLACE INTO t(t) VALUES('integrity-check')`,
		}, ftsReplaceShadowDump("a")},

		{"REPLACE with the rowid spelling of docid", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(rowid,a) VALUES(1,'alpha')`,
			`INSERT INTO t(rowid,a) VALUES(2,'beta')`,
			`REPLACE INTO t(rowid,a) VALUES(1,'gamma')`,
		}, ftsReplaceShadowDump("a")},

		// A prefix index takes its own delete markers: level 1024 gets the
		// truncated term retired and re-added exactly as level 0 does.
		{"REPLACE on a prefix= table", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, prefix="2")`,
			`INSERT INTO t(docid,a) VALUES(1,'alpha')`,
			`INSERT INTO t(docid,a) VALUES(2,'beta')`,
			`REPLACE INTO t(docid,a) VALUES(1,'gamma')`,
		}, ftsReplaceShadowDump("a")},

		{"REPLACE of the only row of a prefix= table wipes both indexes", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, prefix="2")`,
			`INSERT INTO t(docid,a) VALUES(7,'alpha')`,
			`REPLACE INTO t(docid,a) VALUES(7,'gamma')`,
		}, ftsReplaceShadowDump("a")},

		{"REPLACE on a notindexed= table", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, b, notindexed=b)`,
			`INSERT INTO t(docid,a,b) VALUES(1,'alpha','skipme')`,
			`INSERT INTO t(docid,a,b) VALUES(2,'beta','x')`,
			`REPLACE INTO t(docid,a,b) VALUES(1,'gamma','y')`,
		}, ftsReplaceShadowDump("a", "b")},

		{"REPLACE on a matchinfo=fts3 table (no %_docsize)", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, matchinfo=fts3)`,
			`INSERT INTO t(docid,a) VALUES(1,'alpha')`,
			`INSERT INTO t(docid,a) VALUES(2,'beta')`,
			`REPLACE INTO t(docid,a) VALUES(1,'gamma')`,
		}, []string{
			`SELECT docid, a FROM t ORDER BY docid`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
		}},

		{"REPLACE at a matching non-zero language id", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, languageid="lid")`,
			`INSERT INTO t(docid,a,lid) VALUES(1,'alpha',3)`,
			`INSERT INTO t(docid,a,lid) VALUES(2,'beta',3)`,
			`REPLACE INTO t(docid,a,lid) VALUES(1,'gamma',3)`,
		}, []string{
			`SELECT docid, a, lid FROM t ORDER BY docid`,
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
		}},

		{"REPLACE on an order=desc table", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, order=desc)`,
			`INSERT INTO t(docid,a) VALUES(1,'alpha')`,
			`INSERT INTO t(docid,a) VALUES(2,'beta')`,
			`REPLACE INTO t(docid,a) VALUES(1,'gamma')`,
		}, ftsReplaceShadowDump("a")},

		// sqlite3Fts3UpdateMethod guards the whole conflict branch with
		// "p->zContentTbl==0", so an external-content or contentless table
		// never displaces anything: the REPLACE is a plain insert that takes
		// %_stat's nDoc up regardless.
		{"REPLACE on a contentless table never displaces", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, content="")`,
			`INSERT INTO t(docid,a) VALUES(1,'alpha')`,
			`INSERT INTO t(docid,a) VALUES(2,'beta')`,
			`REPLACE INTO t(docid,a) VALUES(1,'gamma')`,
		}, []string{
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
		}},

		{"REPLACE on an external-content table never displaces", []string{
			`CREATE TABLE src(id INTEGER PRIMARY KEY, x)`,
			`INSERT INTO src VALUES(1,'alpha'),(2,'beta')`,
			`CREATE VIRTUAL TABLE t USING fts4(content="src", x)`,
			`INSERT INTO t(docid,x) VALUES(1,'alpha')`,
			`REPLACE INTO t(docid,x) VALUES(1,'gamma')`,
		}, []string{
			`SELECT level, idx, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM t_stat ORDER BY id`,
		}},

		{"REPLACE ... SELECT", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`CREATE TABLE s(d,v)`,
			`INSERT INTO s VALUES(1,'xx'),(3,'yy')`,
			`INSERT INTO t(docid,a) VALUES(1,'alpha')`,
			`INSERT INTO t(docid,a) VALUES(2,'beta')`,
			`REPLACE INTO t(docid,a) SELECT d,v FROM s`,
		}, ftsReplaceShadowDump("a")},

		{"two REPLACEs in one transaction fold into ONE segment", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'aa')`,
			`INSERT INTO t(docid,a) VALUES(2,'bb')`,
			`BEGIN`,
			`REPLACE INTO t(docid,a) VALUES(1,'cc')`,
			`REPLACE INTO t(docid,a) VALUES(2,'dd')`,
			`COMMIT`,
		}, ftsReplaceShadowDump("a")},

		{"a REPLACE whose docid goes BACKWARDS seals the transaction's segment", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'alpha')`,
			`INSERT INTO t(docid,a) VALUES(2,'beta')`,
			`BEGIN`,
			`REPLACE INTO t(docid,a) VALUES(2,'cc')`,
			`REPLACE INTO t(docid,a) VALUES(1,'dd')`,
			`COMMIT`,
		}, ftsReplaceShadowDump("a")},

		// OR ABORT is the DEFAULT conflict mode, not a mode of its own, so it
		// takes the ordinary INSERT path and must behave exactly like one.
		{"INSERT OR ABORT is the plain INSERT path", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT OR ABORT INTO t(docid,a) VALUES(1,'alpha')`,
			`INSERT OR ABORT INTO t(docid,a) VALUES(2,'beta')`,
		}, ftsReplaceShadowDump("a")},

		// The wipe reached from INSIDE a transaction: fts3DeleteAll discards
		// the pending terms, so the segment the transaction had been building
		// is gone too and only 'gamma' survives.
		{"a REPLACE that wipes inside a transaction discards its segment", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(7,'alpha')`,
			`BEGIN`,
			`INSERT INTO t(docid,a) VALUES(8,'beta')`,
			`DELETE FROM t WHERE docid=8`,
			`REPLACE INTO t(docid,a) VALUES(7,'gamma')`,
			`COMMIT`,
		}, ftsReplaceShadowDump("a")},

		{"REPLACE whose old row held NULLs", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
			`INSERT INTO t(docid,a,b) VALUES(1,'alpha',NULL)`,
			`INSERT INTO t(docid,a,b) VALUES(2,NULL,'beta')`,
			`REPLACE INTO t(docid,a) VALUES(1,'gamma')`,
		}, ftsReplaceShadowDump("a", "b")},

		{"REPLACE then DELETE then REPLACE again", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`REPLACE INTO t(docid,a) VALUES(1,'aa bb')`,
			`REPLACE INTO t(docid,a) VALUES(2,'bb cc')`,
			`REPLACE INTO t(docid,a) VALUES(2,'cc dd')`,
			`DELETE FROM t WHERE docid=1`,
			`REPLACE INTO t(docid,a) VALUES(2,'ee')`,
		}, ftsReplaceShadowDump("a")},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			differAllAccepted(t, c.name, append(append([]string{}, c.stmts...), c.dump...), len(c.stmts))
		})
	}
}

// ftsReplaceWrite is arranged so the resulting index cannot be read correctly
// by an engine that mishandles a displacement: docid 2 is displaced twice (so
// one term is retired in a segment newer than the one that introduced it),
// docid 4's replacement re-uses a term the old row also had (which must fold
// into ONE doclist entry, not a marker plus a posting), and docid 1 keeps a
// term the displaced rows also carried.
var ftsReplaceWrite = []string{
	`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
	`INSERT INTO t(docid,a,b) VALUES(1,'alpha beta','gamma')`,
	`INSERT INTO t(docid,a,b) VALUES(2,'beta gamma','delta')`,
	`INSERT INTO t(docid,a,b) VALUES(3,'gamma delta','epsilon')`,
	`INSERT INTO t(docid,a,b) VALUES(4,'zeta eta','theta')`,
	`REPLACE INTO t(docid,a,b) VALUES(2,'beta iota','kappa')`,
	`REPLACE INTO t(docid,a,b) VALUES(2,'lambda','mu')`,
	`REPLACE INTO t(docid,a,b) VALUES(4,'zeta nu','theta')`,
	`REPLACE INTO t(docid,a,b) VALUES(5,'alpha xi','omicron')`,
}

var ftsReplaceRead = []string{
	`SELECT docid, a, b FROM t ORDER BY docid`,
	`SELECT docid, c0a, c1b FROM t_content ORDER BY docid`,
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
	`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
	`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
	`SELECT id, quote(value) FROM t_stat ORDER BY id`,
}

// ftsReplaceMatch is what the index itself must answer -- only a reader that
// resolves the displacement's delete markers newest-segment-first gets these.
var ftsReplaceMatch = []struct{ query, want string }{
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'alpha' ORDER BY docid)`, "1,5"},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'beta' ORDER BY docid)`, "1"},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'gamma' ORDER BY docid)`, "1,3"},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'iota' ORDER BY docid)`, ""},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'kappa' ORDER BY docid)`, ""},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'lambda' ORDER BY docid)`, "2"},
	// 'zeta' was in docid 4's OLD row and in its replacement: retired and
	// re-added by the same statement, so it must survive.
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'zeta' ORDER BY docid)`, "4"},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'eta' ORDER BY docid)`, ""},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'nu' ORDER BY docid)`, "4"},
	{`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH '"zeta nu"' ORDER BY docid)`, "4"},
	{`SELECT offsets(t) FROM t WHERE t MATCH 'nu'`, "0 0 5 2"},
	{`SELECT quote(matchinfo(t,'na')) FROM t WHERE t MATCH 'alpha' LIMIT 1`, "X'050000000200000001000000'"},
}

// TestFts3ReplacedFileInterchange: a database each engine wrote AND THEN
// REPLACED into must read back identically under both -- down to the segment
// blobs, which is the only place a displacement's delete markers live.
func TestFts3ReplacedFileInterchange(t *testing.T) {
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "fts.db")
			wrote := runWithDSN(t, writer, dsn, ftsReplaceWrite)
			for i, r := range wrote {
				if kind, _ := r["kind"].(string); kind == "error" {
					t.Fatalf("%s could not run the write script (the comparison would be vacuous): stmt #%d %s", writer, i, ftsReplaceWrite[i])
				}
			}
			var baseline string
			// Each engine over its OWN format, with the converter in between where the
			// writer was the other one (convert_for_oracle_test.go).
			goPath, cgoPath := pathsForBothEngines(t, writerEngineName(writer), dsn)
			for _, reader := range engineOrder {
				readPath := goPath
				if reader == "cgo" {
					readPath = cgoPath
				}
				got := fmt.Sprintf("%v", runWithDSN(t, reader, readPath, ftsReplaceRead))
				if baseline == "" {
					baseline = got
					continue
				}
				if got != baseline {
					t.Errorf("[%s writes+replaces] readers disagree\n  %s\n  %s reads: %s", writer, baseline, reader, got)
				}
			}
		})
	}
}

// TestFts3ReplacedIndexIsSearchableByCSQLite closes the loop: this engine
// does every REPLACE, and C SQLite -- which never saw any of it happen --
// has to integrity_check the file AND the inverted index, resolve every
// displacement marker, and go on writing.
func TestFts3ReplacedIndexIsSearchableByCSQLite(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "fts.db")
	wrote := runWithDSN(t, "musql", dsn, ftsReplaceWrite)
	for i, r := range wrote {
		if kind, _ := r["kind"].(string); kind == "error" {
			t.Fatalf("musql could not run the write script: stmt #%d %s", i, ftsReplaceWrite[i])
		}
	}

	sdb, err := sql.Open("sqlite3", exportedForOracle(t, dsn))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()

	var ic string
	if err := sdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if ic != "ok" {
		t.Fatalf("C SQLite reports integrity_check = %q on a database this engine REPLACEd into", ic)
	}
	// fts3's OWN checker: it re-tokenizes %_content and compares the result
	// against the segments, so it fails on an index that merely looks like a
	// valid b-tree. PRAGMA integrity_check alone would not catch that.
	if _, err := sdb.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
		t.Fatalf("fts3's own 'integrity-check' rejected an index this engine REPLACEd into: %v", err)
	}
	for _, c := range ftsReplaceMatch {
		var got string
		if err := sdb.QueryRow(c.query).Scan(&got); err != nil {
			t.Errorf("C SQLite failed on %q over a database this engine REPLACEd into: %v", c.query, err)
			continue
		}
		if got != c.want {
			t.Errorf("C SQLite disagrees about this engine's own displacement markers\n  query: %s\n  got:   %q\n  want:  %q", c.query, got, c.want)
		}
	}

	// And C SQLite must be able to keep REPLACING: its own displacement
	// reads this engine's segments to tokenize the row it is retiring.
	if _, err := sdb.Exec(`REPLACE INTO t(docid,a,b) VALUES(1,'pi rho','sigma')`); err != nil {
		t.Fatalf("C SQLite could not REPLACE into a table this engine REPLACEd into: %v", err)
	}
	var got string
	if err := sdb.QueryRow(`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'alpha' ORDER BY docid)`).Scan(&got); err != nil {
		t.Fatalf("MATCH after a real-SQLite REPLACE: %v", err)
	}
	if got != "5" {
		t.Errorf("after C SQLite displaced docid 1, MATCH 'alpha' returned %q, want \"5\"", got)
	}
	if _, err := sdb.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
		t.Errorf("fts3 'integrity-check' after a real-SQLite REPLACE: %v", err)
	}
}

// TestFts3ReplaceFuzzDiff is the part hand-picked cases cannot do. Whether a
// displacement's delete markers fold correctly into the segment depends on the
// VALUES -- which terms the old and new rows share, whether the displaced row
// was the last one, and how the docids interleave with the ones already there
// -- so agreement on a fixed script proves only that script. This generates
// random write histories and compares the SHADOW BYTES after each.
func TestFts3ReplaceFuzzDiff(t *testing.T) {
	// -short REDUCES the history count rather than skipping, the way
	// TestWriteFuzz does: -short is the gate this project actually runs, and a
	// fuzz that never runs there is not a gate at all.
	nIter := 300
	if testing.Short() {
		nIter = 60
	}
	words := []string{"aa", "bb", "cc", "dd", "ee", "alpha", "beta", "gamma"}
	schemas := []string{
		`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
		`CREATE VIRTUAL TABLE t USING fts4(a,b, prefix="2")`,
		`CREATE VIRTUAL TABLE t USING fts4(a,b, notindexed=b)`,
		`CREATE VIRTUAL TABLE t USING fts3(a,b)`,
	}
	rng := rand.New(rand.NewSource(20260802))
	text := func() string {
		n := 1 + rng.Intn(3)
		var parts []string
		for i := 0; i < n; i++ {
			parts = append(parts, words[rng.Intn(len(words))])
		}
		return strings.Join(parts, " ")
	}
	for iter := 0; iter < nIter; iter++ {
		schema := schemas[rng.Intn(len(schemas))]
		fts3Only := strings.Contains(schema, "fts3(")
		stmts := []string{schema}
		// Bounded well under fts3MergeCount (16) level-0 segments: a level
		// MERGE is declined here, and hitting it would report a divergence
		// that is a known decline rather than a REPLACE bug.
		nDML := 2 + rng.Intn(9)
		for i := 0; i < nDML; i++ {
			docid := 1 + rng.Intn(4)
			switch rng.Intn(6) {
			case 0:
				stmts = append(stmts, fmt.Sprintf(`DELETE FROM t WHERE docid=%d`, docid))
			case 1:
				stmts = append(stmts, fmt.Sprintf(`UPDATE t SET a='%s' WHERE docid=%d`, text(), docid))
			case 2:
				stmts = append(stmts, fmt.Sprintf(`INSERT INTO t(docid,a,b) VALUES(%d,'%s','%s')`, docid, text(), text()))
			default:
				stmts = append(stmts, fmt.Sprintf(`REPLACE INTO t(docid,a,b) VALUES(%d,'%s','%s')`, docid, text(), text()))
			}
		}
		stmts = append(stmts,
			`SELECT docid, a, b FROM t ORDER BY docid`,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
		)
		if !fts3Only {
			stmts = append(stmts,
				`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
				`SELECT id, quote(value) FROM t_stat ORDER BY id`,
			)
		}
		for _, w := range words {
			stmts = append(stmts, fmt.Sprintf(`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH '%s' ORDER BY docid)`, w))
		}
		if !differ(t, fmt.Sprintf("replace-fuzz/%d", iter), stmts) {
			t.Fatalf("stopping at the first divergent history (iteration %d)", iter)
		}
	}
}

// TestFts3ReplaceDeclined pins what REPLACE still leaves out. C SQLite
// ACCEPTS every one of these, so an engine that quietly started answering one
// would be WRONG rather than incomplete.
func TestFts3ReplaceDeclined(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		bad   string
	}{
		// The delete half lands under the OLD row's language and the insert
		// half under the new one, so C fts3 flushes between them: a
		// marker-only segment at level 3072 and the insert at level 5120.
		{"REPLACE that moves a row to another language id", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, languageid="lid")`,
			`INSERT INTO t(docid,a,lid) VALUES(1,'alpha',3)`,
			`INSERT INTO t(docid,a,lid) VALUES(2,'beta',3)`,
		}, `REPLACE INTO t(docid,a,lid) VALUES(1,'gamma',5)`},

		// Two segments on the oracle, for the same reason a plain descending
		// multi-row INSERT writes two.
		{"multi-row REPLACE with descending docids", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'alpha')`,
			`INSERT INTO t(docid,a) VALUES(9,'beta')`,
		}, `REPLACE INTO t(docid,a) VALUES(9,'xx'),(1,'yy')`},

		// The insert half arrives at a docid the previous row's insert already
		// used, with bPrevDelete==0, which is fts3PendingTermsDocid's flush.
		{"multi-row REPLACE repeating one docid", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'alpha')`,
		}, `REPLACE INTO t(docid,a) VALUES(1,'xx'),(1,'yy')`},

		// fts3 implements only REPLACE itself; every other mode goes through
		// fts3InsertData's duplicate-rowid SQLITE_CONSTRAINT and the vtab
		// layer's own resolution, which has no reproduction here.
		{"INSERT OR IGNORE", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'alpha')`,
		}, `INSERT OR IGNORE INTO t(docid,a) VALUES(1,'beta')`},
		{"INSERT OR ROLLBACK", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
		}, `INSERT OR ROLLBACK INTO t(docid,a) VALUES(1,'beta')`},
		{"INSERT OR FAIL", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
		}, `INSERT OR FAIL INTO t(docid,a) VALUES(1,'beta')`},
		// OR ABORT is deliberately absent: it is the DEFAULT mode, so it takes
		// the ordinary INSERT path -- TestFts3ReplaceShadowDiff gates it as an
		// accepted shape instead.

		// UPDATE OR REPLACE used to be here, and is now SERVED: it deletes the
		// row at the NEW docid, then the old row (which can flush, and can
		// even empty the table and throw the flushed segment away), then
		// inserts. All three halves are reproduced -- see
		// fts_r25_update_or_replace_test.go, which gates the wipe rule the
		// OR clause alone decides. The one shape that stays declined is a
		// MULTI-ROW OR REPLACE that actually displaces, below.
		{"multi-row UPDATE OR REPLACE that displaces", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t(docid,a) VALUES(1,'aa')`,
			`INSERT INTO t(docid,a) VALUES(2,'bb')`,
			`INSERT INTO t(docid,a) VALUES(3,'cc')`,
		}, `UPDATE OR REPLACE t SET docid=docid+1`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			db := openMusqlFts(t)
			for _, s := range c.setup {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if _, err := db.Exec(c.bad); err == nil {
				t.Fatalf("engine ACCEPTED an out-of-scope fts3/fts4 statement (C SQLite answers it differently): %s", c.bad)
			}
		})
	}
}
