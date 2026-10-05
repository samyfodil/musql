// Tests FTS3/FTS4 segment merge operations against the raw segment contents.
// bottom makes C SQLite search a merged index this engine built.
//
// The rules being held down (each derived from the oracle, see
// engine/fts3_merge.go for the probes):
//
//   - the merge fires on the ALLOCATION that would reach idx 16, and the new
//     statement's own segment then takes level-0 idx 0;
//   - it CASCADES: a full level-1 is merged into level-2 first;
//   - delete markers are dropped only when the output level is above every
//     level present, and kept otherwise;
//   - the output's %_segments blocks are numbered before the inputs' are freed.
package compat

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// segdirDump is the raw shadow-table read every case below ends with: these are
// ordinary tables in the file, so their bytes ARE the file format.
var segdirDump = []string{
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
	`SELECT blockid, length(block), quote(block) FROM t_segments ORDER BY blockid`,
	`SELECT docid, quote(c0a) FROM t_content ORDER BY docid`,
}

func fts3Inserts(from, to int, word string) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf(`INSERT INTO t(docid,a) VALUES(%d,'w%d %s')`, i, i, word))
	}
	return out
}

// fts3Fat is a value large enough to spill a segment past a single root node
// (page size 4096 minus fts3's 35 bytes of overhead).
func fts3Fat(i int) string {
	var b strings.Builder
	for j := 0; j < 900; j++ {
		fmt.Fprintf(&b, "t%dword%d ", i, j)
	}
	return b.String()
}

func TestFts3LevelMergeMatchesCSQLite(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"the 17th segment merges level 0 into level 1", concat(
			[]string{`CREATE VIRTUAL TABLE t USING fts3(a)`},
			fts3Inserts(1, 16, "common"),
			segdirDump,
			fts3Inserts(17, 17, "common"),
			segdirDump,
			[]string{
				`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'common' ORDER BY docid)`,
				`SELECT docid FROM t WHERE t MATCH 'w9'`,
				`SELECT count(*) FROM t`,
			},
		)},
		// Nothing older survives this merge, so the markers a DELETE and an
		// UPDATE left are dropped -- docid 2 and 3 leave no trace in the
		// level-1 segment at all.
		{"a merge with nothing older DROPS delete markers", concat(
			[]string{`CREATE VIRTUAL TABLE t USING fts3(a)`},
			fts3Inserts(1, 5, "common"),
			[]string{
				`DELETE FROM t WHERE docid=3`,
				`UPDATE t SET a='w2 other' WHERE docid=2`,
			},
			fts3Inserts(6, 14, "common"),
			segdirDump,
			[]string{`INSERT INTO t(docid,a) VALUES(99,'zz')`},
			segdirDump,
			[]string{
				`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'common' ORDER BY docid)`,
				`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'other' ORDER BY docid)`,
				`SELECT group_concat(docid) FROM (SELECT docid FROM t ORDER BY docid)`,
			},
		)},
		// Here a level-1 segment already exists, so it is OLDER than the merge's
		// output and still holds docid 5: the marker must survive, or docid 5
		// comes back from the dead.
		{"a merge above an older segment KEEPS delete markers", concat(
			[]string{`CREATE VIRTUAL TABLE t USING fts3(a)`},
			fts3Inserts(1, 17, "common"),
			segdirDump,
			[]string{`DELETE FROM t WHERE docid=5`},
			fts3Inserts(20, 34, "common"),
			segdirDump,
			[]string{`INSERT INTO t(docid,a) VALUES(99,'zz')`},
			segdirDump,
			[]string{
				`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'common' ORDER BY docid)`,
				`SELECT count(*) FROM t WHERE docid=5`,
			},
		)},
		{"a spilling merge numbers its blocks above the inputs it frees", concat(
			[]string{`CREATE VIRTUAL TABLE t USING fts3(a)`},
			fatInserts(1, 16),
			segdirDump,
			[]string{`INSERT INTO t(docid,a) VALUES(999,'tiny')`},
			segdirDump,
			// ...and the NEXT spilling segment continues from the output's last
			// block rather than reusing the freed ones.
			fatInserts(1000, 1000),
			segdirDump,
			[]string{
				`SELECT count(*) FROM t WHERE t MATCH 't7word899'`,
				`SELECT docid FROM t WHERE t MATCH 'tiny'`,
			},
		)},
		{"a DELETE allocates and merges exactly like an INSERT", concat(
			[]string{`CREATE VIRTUAL TABLE t USING fts3(a)`},
			fts3Inserts(1, 16, "common"),
			segdirDump,
			[]string{`DELETE FROM t WHERE docid=4`},
			segdirDump,
			[]string{
				`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'common' ORDER BY docid)`,
			},
		)},
		{"an UPDATE allocates and merges exactly like an INSERT", concat(
			[]string{`CREATE VIRTUAL TABLE t USING fts4(a)`},
			fts4Inserts(1, 16),
			[]string{`UPDATE t SET a='replaced text' WHERE docid=4`},
			[]string{
				`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
				`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
				`SELECT id, quote(value) FROM t_stat ORDER BY id`,
				`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'common' ORDER BY docid)`,
				`SELECT docid FROM t WHERE t MATCH 'replaced'`,
			},
		)},
		// The whole cascade: level-0 full AND level-1 full, so level-1 merges
		// into a level-2 segment and level-0's own output lands at level-1 idx 0
		// -- the state in which a dropped marker would resurrect a row the
		// level-2 segment still holds.
		{"a full level-1 cascades into level 2", concat(
			[]string{`CREATE VIRTUAL TABLE t USING fts3(a)`},
			fts3Inserts(1, 271, "common"),
			[]string{
				`SELECT level, count(*) FROM t_segdir GROUP BY level ORDER BY level`,
				`DELETE FROM t WHERE docid=7`,
				`INSERT INTO t(docid,a) VALUES(9999,'zz')`,
			},
			segdirDump,
			[]string{
				`SELECT count(*) FROM t WHERE t MATCH 'common' AND docid=7`,
				`SELECT count(*) FROM t WHERE docid=7`,
				`SELECT count(*) FROM t WHERE t MATCH 'common'`,
				`SELECT docid FROM t WHERE t MATCH 'w200'`,
			},
		)},
		// A transaction accumulates ONE segment, so two statements inside one do
		// NOT each allocate -- but the transaction's own segment can be the
		// allocation that merges, and after COMMIT the bytes must be identical.
		//
		// %_segdir is deliberately NOT read mid-transaction here. Real fts3 holds
		// the transaction's terms in memory and leaves %_segdir showing the
		// pre-transaction segments until COMMIT (verified: the two INSERTs below
		// leave the sixteen level-0 rows untouched and add nothing), while this
		// engine writes the transaction's segment eagerly so that a MATCH inside
		// the transaction can be answered from the file at all -- the trade-off
		// engine/fts3_txn.go documents. That difference predates the merge and is
		// not what this case is about.
		{"a transaction's single segment still allocates through the merge", concat(
			[]string{`CREATE VIRTUAL TABLE t USING fts3(a)`},
			fts3Inserts(1, 16, "common"),
			[]string{
				`BEGIN`,
				`INSERT INTO t(docid,a) VALUES(50,'w50 common')`,
				`INSERT INTO t(docid,a) VALUES(51,'w51 common')`,
				`COMMIT`,
			},
			segdirDump,
			[]string{
				`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'common' ORDER BY docid)`,
			},
		)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "fts3merge/"+tc.name, tc.stmts) })
	}
}

func concat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func fatInserts(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf(`INSERT INTO t(docid,a) VALUES(%d,'%s')`, i, fts3Fat(i)))
	}
	return out
}

func fts4Inserts(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf(`INSERT INTO t(docid,a) VALUES(%d,'w%d common')`, i, i))
	}
	return out
}

// TestFts3MergedFileInterchange is the load-bearing half: a MERGED index this
// engine wrote must be searchable by C SQLite -- which never saw it being
// written -- and clean under integrity_check, and the reverse.
func TestFts3MergedFileInterchange(t *testing.T) {
	write := concat(
		[]string{`CREATE VIRTUAL TABLE t USING fts4(a)`},
		fts4Inserts(1, 10),
		[]string{`DELETE FROM t WHERE docid=3`},
		fts4Inserts(11, 17),
		// ...and one that spills, so the merged segment owns %_segments blocks.
		fatInserts(500, 500),
		fts4Inserts(20, 20),
	)
	read := []string{
		`PRAGMA integrity_check`,
		`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
		`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
		`SELECT docid, quote(size) FROM t_docsize ORDER BY docid`,
		`SELECT id, quote(value) FROM t_stat ORDER BY id`,
		`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'common' ORDER BY docid)`,
		`SELECT group_concat(docid) FROM (SELECT docid FROM t WHERE t MATCH 'w1*' ORDER BY docid)`,
		`SELECT count(*) FROM t WHERE t MATCH 't500word899'`,
		`SELECT count(*) FROM t WHERE docid=3`,
		`SELECT offsets(t) FROM t WHERE t MATCH 'w9'`,
	}
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "merged.db")
			runWithDSN(t, writer, dsn, write)
			var baseline string
			// Each engine over its OWN format, with the converter in between where the
			// writer was the other one (convert_for_oracle_test.go).
			goPath, cgoPath := pathsForBothEngines(t, writerEngineName(writer), dsn)
			for _, reader := range engineOrder {
				readPath := goPath
				if reader == "cgo" {
					readPath = cgoPath
				}
				got := fmt.Sprintf("%v", runWithDSN(t, reader, readPath, read))
				if baseline == "" {
					baseline = got
					continue
				}
				if got != baseline {
					t.Errorf("[%s writes] readers disagree\n  %s\n  %s reads: %s", writer, baseline, reader, got)
				}
			}
		})
	}
}
