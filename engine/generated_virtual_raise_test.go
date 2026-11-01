package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A VIRTUAL column added by ADD COLUMN can raise over an older row. C computes
// it only where it is read, so only that read raises, and no rewrite of the
// file loses the row.

// genRaiseFixture is i(value, j, g AS json_extract(j,'$')) holding (1,'bad'),
// whose g raises, and (7,'1'). sameTxn adds the column in the transaction
// that wrote the rows.
func genRaiseFixture(t *testing.T, sameTxn bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "g.musq")
	stmts := []string{`CREATE TABLE i(value INT, j TEXT)`, `INSERT INTO i VALUES(1,'bad'),(7,'1')`}
	alter := `ALTER TABLE i ADD COLUMN g AS (json_extract(j,'$')) VIRTUAL`
	if sameTxn {
		buildDB(t, path, append(stmts, alter)...)
	} else {
		buildDB(t, path, stmts...)
		execDB(t, path, alter)
	}
	return path
}

// genRows renders q's rows as "a,b;c,d", or returns the error.
func genRows(t *testing.T, path, q string) (string, error) {
	t.Helper()
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	_, rows, err := n.Query(q, nil)
	if err != nil {
		return "", err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		cells := make([]string, len(r))
		for k, v := range r {
			switch v.Typ {
			case Null:
				cells[k] = "NULL"
			case Int:
				cells[k] = fmt.Sprint(v.I)
			case Float:
				cells[k] = fmt.Sprint(v.F)
			default:
				cells[k] = string(v.S)
			}
		}
		out = append(out, strings.Join(cells, ","))
	}
	return strings.Join(out, ";"), nil
}

func wantGenRows(t *testing.T, path, q, want string) {
	t.Helper()
	got, err := genRows(t, path, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if got != want {
		t.Fatalf("%s:\n got %q\nwant %q", q, got, want)
	}
}

func wantGenRaise(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "malformed JSON") {
		t.Fatalf("%s: err = %v, want malformed JSON", what, err)
	}
}

// execOne runs one statement in its own transaction and returns its error.
func execOne(t *testing.T, path, stmt string) error {
	t.Helper()
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Exec(stmt); err != nil {
		n.Discard()
		return err
	}
	return n.Close()
}

func TestVirtualGeneratedRaiseKeepsItsRow(t *testing.T) {
	for _, sameTxn := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameTxn=%v", sameTxn), func(t *testing.T) {
			path := genRaiseFixture(t, sameTxn)
			wantGenRows(t, path, `SELECT value, j FROM i`, "1,bad;7,1")
			wantGenRows(t, path, `SELECT count(*) FROM i`, "2")
			wantGenRows(t, path, `SELECT sum(value) FROM i`, "8")
			wantGenRows(t, path, `SELECT value FROM i WHERE value = 7 AND g > 0`, "7")
			wantGenRows(t, path, `SELECT g FROM i WHERE value = 7`, "1")
			wantGenRows(t, path, `SELECT j FROM i ORDER BY j`, "1;bad")
			for _, q := range []string{
				`SELECT g FROM i`,
				`SELECT * FROM i`,
				`SELECT value FROM i WHERE g > 0`,
				`SELECT value FROM i ORDER BY g`,
				`SELECT count(g) FROM i`,
			} {
				_, err := genRows(t, path, q)
				wantGenRaise(t, err, q)
			}
		})
	}
}

func TestVirtualGeneratedRaiseWrites(t *testing.T) {
	path := genRaiseFixture(t, false)
	// The new row's g is computed, and over the bad row it raises.
	wantGenRaise(t, execOne(t, path, `UPDATE i SET value = value + 100`), "UPDATE every row")
	wantGenRows(t, path, `SELECT value, j FROM i`, "1,bad;7,1")
	if err := execOne(t, path, `UPDATE i SET value = value + 100 WHERE value = 7`); err != nil {
		t.Fatal(err)
	}
	wantGenRows(t, path, `SELECT value, j, g FROM i WHERE value > 1`, "107,1,1")
	// RETURNING reads the whole old row, g included.
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, rerr := n.ExecReturning(`DELETE FROM i WHERE value = 1 RETURNING value`, nil)
	n.Discard()
	wantGenRaise(t, rerr, "DELETE ... RETURNING")
	wantGenRows(t, path, `SELECT value, j FROM i`, "1,bad;107,1")
	if err := execOne(t, path, `DELETE FROM i WHERE value = 1`); err != nil {
		t.Fatal(err)
	}
	wantGenRows(t, path, `SELECT value, j FROM i`, "107,1")
}

func TestVirtualGeneratedRaiseSurvivesRewrites(t *testing.T) {
	t.Run("vacuum", func(t *testing.T) {
		path := genRaiseFixture(t, false)
		execDB(t, path, `VACUUM`)
		wantGenRows(t, path, `SELECT value, j FROM i`, "1,bad;7,1")
		_, err := genRows(t, path, `SELECT g FROM i`)
		wantGenRaise(t, err, "SELECT g after VACUUM")
	})
	t.Run("compact", func(t *testing.T) {
		path := genRaiseFixture(t, false)
		execDB(t, path, `INSERT INTO i(value, j) VALUES(8, '8')`, `DELETE FROM i WHERE value = 7`)
		if did, err := CompactSegmentFile(path); err != nil || !did {
			t.Fatalf("CompactSegmentFile = %v, %v", did, err)
		}
		wantGenRows(t, path, `SELECT value, j, typeof(j) FROM i`, "1,bad,text;8,8,text")
	})
	// Enough commits to take the delta past the compaction threshold, written to
	// the table itself and to another one; each commit's own compaction rewrites
	// the whole file.
	for _, target := range []string{"i", "other"} {
		t.Run("writes to "+target, func(t *testing.T) {
			path := genRaiseFixture(t, false)
			execDB(t, path, `CREATE TABLE other(v TEXT)`)
			stmt := `INSERT INTO other VALUES('"' || hex(zeroblob(2000)) || '"')`
			if target == "i" {
				stmt = `INSERT INTO i(value, j) VALUES(100, '"' || hex(zeroblob(2000)) || '"')`
			}
			compacted, prev, k := false, int64(0), 0
			for ; k < 2000 && !compacted; k++ {
				execDB(t, path, stmt)
				st, err := os.Stat(segDeltaPath(path))
				size := int64(0)
				if err == nil {
					size = st.Size()
				}
				compacted = k > 0 && size < prev
				prev = size
			}
			if !compacted {
				t.Fatalf("no compaction after %d commits", k)
			}
			want := "2"
			if target == "i" {
				want = fmt.Sprint(2 + k)
			}
			wantGenRows(t, path, `SELECT count(*) FROM i`, want)
			wantGenRows(t, path, `SELECT value, j FROM i WHERE value < 100`, "1,bad;7,1")
		})
	}
}

// genManyFixture is i(value, j) holding 1..500 as JSON, plus (1000,'bad') when
// bad, with g added after; compact moves the rows into a segment, where g's
// stored cells are not its values.
func genManyFixture(t *testing.T, bad, compact bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "g.musq")
	stmts := []string{`CREATE TABLE i(value INT, j TEXT)`,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x<500) INSERT INTO i SELECT x, CAST(x AS TEXT) FROM c`}
	if bad {
		stmts = append(stmts, `INSERT INTO i VALUES(1000,'bad')`)
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `ALTER TABLE i ADD COLUMN g AS (json_extract(j,'$')) VIRTUAL`)
	if compact {
		if _, err := CompactSegmentFile(path); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestVirtualGeneratedReadsTakeTheComputedValue(t *testing.T) {
	for _, compact := range []bool{false, true} {
		path := genManyFixture(t, false, compact)
		for range 3 {
			wantGenRows(t, path, `SELECT count(*) FROM i WHERE g IS NULL`, "0")
			wantGenRows(t, path, `SELECT count(*) FROM i WHERE g IS NOT NULL`, "500")
			wantGenRows(t, path, `SELECT count(*), sum(g) FROM i WHERE g > 250`, "250,93875")
			if err := execOne(t, path, `DELETE FROM i WHERE g IS NULL`); err != nil {
				t.Fatal(err)
			}
			if err := execOne(t, path, `UPDATE i SET value = value + 1 WHERE g IS NULL`); err != nil {
				t.Fatal(err)
			}
			wantGenRows(t, path, `SELECT count(*), sum(value) FROM i`, "500,125250")
		}
	}
}

// A write whose WHERE reads g reads it for the row whose g raises, so the
// statement fails and changes nothing, as in C, however many times it runs.
func TestVirtualGeneratedRaiseFailsWritesThatReadIt(t *testing.T) {
	for _, compact := range []bool{false, true} {
		path := genManyFixture(t, true, compact)
		for range 3 {
			for _, w := range []string{`DELETE FROM i WHERE g = 5`, `DELETE FROM i WHERE g IS NULL`, `DELETE FROM i WHERE g IS NOT NULL`,
				`UPDATE i SET value = value + 1 WHERE g > 499`, `UPDATE i SET value = value + 1 WHERE g IS NULL`} {
				wantGenRaise(t, execOne(t, path, w), w)
				wantGenRows(t, path, `SELECT count(*), sum(value) FROM i`, "501,126250")
			}
			_, err := genRows(t, path, `SELECT count(*) FROM i WHERE g IS NULL`)
			wantGenRaise(t, err, "count where g is null")
		}
	}
}
