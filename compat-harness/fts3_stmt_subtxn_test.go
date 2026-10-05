// This file gates fts3/fts4's statement sub-transaction flush point.
// fts3 flushes pending terms whenever a statement opens a statement journal.
package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

// Full index image dump for verification.
var fts3SubTxnDump = []string{
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM ft_segdir ORDER BY level, idx`,
	`SELECT rowid, level, idx FROM ft_segdir ORDER BY rowid`,
	`SELECT blockid, quote(block) FROM ft_segments ORDER BY blockid`,
	`SELECT docid, quote(x) FROM ft_content ORDER BY docid`,
	`SELECT group_concat(docid) FROM (SELECT docid FROM ft WHERE ft MATCH 'a1 OR b2 OR c3 OR d4 OR d10 OR d20 OR d30 OR d40 OR d60' ORDER BY docid)`,
}

func TestFts3StatementSubTransactionFlush(t *testing.T) {
	cases := []struct {
		name string
		// nAccepted: first N statements must succeed on both engines.
		stmts     []string
		nAccepted int
	}{
		// Multi-row INSERTs split the transaction's segment.
		{"two multi-row INSERTs split the transaction's segment", append([]string{
			`CREATE VIRTUAL TABLE ft USING fts3(x)`,
			`BEGIN`,
			`INSERT INTO ft VALUES('a1'),('b2')`,
			`INSERT INTO ft VALUES('c3'),('d4')`,
			`COMMIT`,
		}, fts3SubTxnDump...), 5},

		// Single-row INSERTs stay in one segment (control case).
		{"the same four documents as single-row INSERTs stay in one", append([]string{
			`CREATE VIRTUAL TABLE ft USING fts3(x)`,
			`BEGIN`,
			`INSERT INTO ft VALUES('a1')`,
			`INSERT INTO ft VALUES('b2')`,
			`INSERT INTO ft VALUES('c3')`,
			`INSERT INTO ft VALUES('d4')`,
			`COMMIT`,
		}, fts3SubTxnDump...), 7},

		// First multi-row statement has nothing to flush.
		{"a multi-row INSERT first has nothing to flush", append([]string{
			`CREATE VIRTUAL TABLE ft USING fts3(x)`,
			`BEGIN`,
			`INSERT INTO ft VALUES('a1'),('b2')`,
			`INSERT INTO ft VALUES('c3')`,
			`COMMIT`,
		}, fts3SubTxnDump...), 5},

		// INSERT..SELECT of one row still flushes (not based on row count).
		{"INSERT..SELECT of one row flushes anyway", append([]string{
			`CREATE VIRTUAL TABLE ft USING fts3(x)`,
			`BEGIN`,
			`INSERT INTO ft VALUES('a1')`,
			`INSERT INTO ft(docid,x) SELECT 40,'d40'`,
			`INSERT INTO ft VALUES('b2')`,
			`COMMIT`,
		}, fts3SubTxnDump...), 6},

		// INSERT..SELECT of zero rows flushes (proves flush is not side-effect).
		{"INSERT..SELECT of zero rows flushes anyway", append([]string{
			`CREATE VIRTUAL TABLE ft USING fts3(x)`,
			`CREATE TABLE src(a,b)`,
			`BEGIN`,
			`INSERT INTO ft VALUES('a1')`,
			`INSERT INTO ft(docid,x) SELECT a,b FROM src`,
			`INSERT INTO ft VALUES('b2')`,
			`COMMIT`,
		}, fts3SubTxnDump...), 7},

		// Single-row VALUES with complex expressions does not flush.
		{"a one-row VALUES with a subquery in it does not flush", append([]string{
			`CREATE VIRTUAL TABLE ft USING fts3(x)`,
			`BEGIN`,
			`INSERT INTO ft VALUES('a1')`,
			`INSERT INTO ft(docid,x) VALUES((SELECT 40),'d40')`,
			`INSERT INTO ft VALUES('b2')`,
			`COMMIT`,
		}, fts3SubTxnDump...), 6},

		// REPLACE follows row count, not conflict clause; tests displacement path.
		{"REPLACE follows the row count, not the conflict clause", append([]string{
			`CREATE VIRTUAL TABLE ft USING fts3(x)`,
			`INSERT INTO ft(docid,x) VALUES(30,'d30')`,
			`BEGIN`,
			`INSERT INTO ft(docid,x) VALUES(10,'d10')`,
			`INSERT OR REPLACE INTO ft(docid,x) VALUES(30,'d40')`,
			`INSERT OR REPLACE INTO ft(docid,x) VALUES(31,'a1'),(32,'b2')`,
			`INSERT INTO ft(docid,x) VALUES(60,'d60')`,
			`COMMIT`,
		}, fts3SubTxnDump...), 8},

		// Backwards docid and multi-row statement rules both apply.
		{"a backwards docid and a multi-row statement in one transaction", append([]string{
			`CREATE VIRTUAL TABLE ft USING fts3(x)`,
			`BEGIN`,
			`INSERT INTO ft(docid,x) VALUES(30,'d30')`,
			`INSERT INTO ft(docid,x) VALUES(10,'d10')`,
			`INSERT INTO ft(docid,x) VALUES(40,'d40'),(41,'a1')`,
			`INSERT INTO ft(docid,x) VALUES(60,'d60')`,
			`COMMIT`,
		}, fts3SubTxnDump...), 7},

		// ROLLBACK discards flushed segments too (all inside transaction).
		{"ROLLBACK discards the flushed segments as well", append([]string{
			`CREATE VIRTUAL TABLE ft USING fts3(x)`,
			`INSERT INTO ft VALUES('a1')`,
			`BEGIN`,
			`INSERT INTO ft VALUES('b2'),('c3')`,
			`INSERT INTO ft VALUES('d4'),('d10')`,
			`ROLLBACK`,
		}, fts3SubTxnDump...), 6},

		// fts4 with prefix index seals all segments at same point.
		{"fts4 and a prefix index seal together", append([]string{
			`CREATE VIRTUAL TABLE ft USING fts4(x, prefix=2)`,
			`BEGIN`,
			`INSERT INTO ft VALUES('alpha'),('beta')`,
			`INSERT INTO ft VALUES('gamma'),('delta')`,
			`COMMIT`,
			`SELECT docid, quote(size) FROM ft_docsize ORDER BY docid`,
			`SELECT quote(value) FROM ft_stat`,
		}, fts3SubTxnDump...), 5},

		// Multi-row INSERT flushes all fts tables in transaction.
		{"the flush reaches every fts table in the transaction", []string{
			`CREATE VIRTUAL TABLE ft USING fts3(x)`,
			`CREATE VIRTUAL TABLE gt USING fts3(y)`,
			`BEGIN`,
			`INSERT INTO gt(docid,y) VALUES(1,'g1')`,
			`INSERT INTO ft(docid,x) VALUES(10,'d10')`,
			`INSERT INTO ft(docid,x) VALUES(20,'d20'),(21,'a1')`,
			`INSERT INTO gt(docid,y) VALUES(9,'g9')`,
			`COMMIT`,
			`SELECT level, idx, quote(root) FROM ft_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM gt_segdir ORDER BY level, idx`,
		}, 8},
		{"a single-row INSERT reaches no other fts table either", []string{
			`CREATE VIRTUAL TABLE ft USING fts3(x)`,
			`CREATE VIRTUAL TABLE gt USING fts3(y)`,
			`BEGIN`,
			`INSERT INTO gt(docid,y) VALUES(1,'g1')`,
			`INSERT INTO ft(docid,x) VALUES(10,'d10')`,
			`INSERT INTO ft(docid,x) VALUES(20,'d20')`,
			`INSERT INTO gt(docid,y) VALUES(9,'g9')`,
			`COMMIT`,
			`SELECT level, idx, quote(root) FROM ft_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM gt_segdir ORDER BY level, idx`,
		}, 8},

		// Autocommit has one segment per statement regardless of row count.
		{"autocommit is one segment per statement whatever the row count", append([]string{
			`CREATE VIRTUAL TABLE ft USING fts3(x)`,
			`INSERT INTO ft VALUES('a1'),('b2')`,
			`INSERT INTO ft VALUES('c3')`,
			`INSERT INTO ft VALUES('d4'),('d10')`,
		}, fts3SubTxnDump...), 4},

		// Extra segments cascade merge inside transaction.
		{"the extra segments still cascade the level merge", func() []string {
			s := []string{`CREATE VIRTUAL TABLE ft USING fts4(x)`, `BEGIN`}
			for i := 0; i < 12; i++ {
				s = append(s, fmt.Sprintf(`INSERT INTO ft(docid,x) VALUES(%d,'w%02d common'),(%d,'w%02d common')`,
					2*i+1, 2*i+1, 2*i+2, 2*i+2))
			}
			return append(append(s, `COMMIT`), fts3SubTxnDump...)
		}(), 15},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			differAllAccepted(t, c.name, c.stmts, c.nAccepted)
		})
	}
}

// DELETE/UPDATE cases where statement journal depends on query plan (docid search only).
// Includes near-misses that look like docid plans but aren't.
var fts3SubTxnMutationCases = []struct {
	name  string
	where string
	verb  string // "DELETE" or "UPDATE"
}{
	{"docid ==", "docid=20", "DELETE"},
	{"rowid ==", "rowid=20", "DELETE"},
	{"oid ==", "oid=20", "DELETE"},
	{"_rowid_ ==", "_rowid_=20", "DELETE"},
	{"qualified docid ==", "ft.docid=20", "DELETE"},
	{"schema-qualified docid ==", "main.ft.docid=20", "DELETE"},
	{"reversed ==", "20=docid", "DELETE"},
	{"the == spelling", "docid==20", "DELETE"},
	{"parenthesised", "((docid)=20)", "DELETE"},
	{"a computed constant", "docid=abs(-20)", "DELETE"},
	{"a CAST constant", "docid=CAST(20 AS INTEGER)", "DELETE"},
	{"NULL", "docid=NULL", "DELETE"},
	{"COLLATE on the column", "docid COLLATE BINARY = 20", "DELETE"},
	{"COLLATE on the value", "docid=20 COLLATE BINARY", "DELETE"},
	{"conjoined after", "docid=20 AND x<>'q'", "DELETE"},
	{"conjoined before", "x<>'q' AND docid=20", "DELETE"},
	{"two equalities", "docid=20 AND docid=30", "DELETE"},
	{"an inequality then an equality", "docid>=20 AND docid=20", "DELETE"},
	{"a one-element IN", "docid IN (20)", "DELETE"},
	{"matching no row", "docid=999", "DELETE"},
	// Non-docid plans.
	{"IS", "docid IS 20", "DELETE"},
	{"BETWEEN", "docid BETWEEN 20 AND 20", "DELETE"},
	{"a two-element IN", "docid IN (20,30)", "DELETE"},
	{"a repeated-element IN", "docid IN (20,20)", "DELETE"},
	{"NOT IN", "docid NOT IN (20)", "DELETE"},
	{"an OR of two equalities", "docid=20 OR docid=30", "DELETE"},
	{"a negated inequality", "NOT docid<>20", "DELETE"},
	{"an inequality", "docid>=20", "DELETE"},
	{"a unary plus on the column", "+docid=20", "DELETE"},
	{"a unary plus, reversed", "20=+docid", "DELETE"},
	{"likely() on the column", "likely(docid)=20", "DELETE"},
	{"unlikely() on the column", "unlikely(docid)=20", "DELETE"},
	{"likelihood() on the column", "likelihood(docid,0.5)=20", "DELETE"},
	{"a non-docid column", "x='d20'", "DELETE"},
	{"a constant false", "0", "DELETE"},
	{"an inequality matching no row", "docid>=999", "DELETE"},
	// Same rule drives UPDATE.
	{"docid ==", "docid=20", "UPDATE"},
	{"a one-element IN", "docid IN (20)", "UPDATE"},
	{"conjoined", "x<>'q' AND docid=20", "UPDATE"},
	{"matching no row", "docid=999", "UPDATE"},
	{"an inequality", "docid>=20", "UPDATE"},
	{"an inequality matching no row", "docid>=999", "UPDATE"},
	{"a constant true", "1", "UPDATE"},
	{"a constant false", "0", "UPDATE"},
	{"an OR of two equalities", "docid=20 OR docid=30", "UPDATE"},
}

func TestFts3StatementSubTransactionMutationPlan(t *testing.T) {
	for _, c := range fts3SubTxnMutationCases {
		c := c
		mutation := `DELETE FROM ft WHERE ` + c.where
		if c.verb == "UPDATE" {
			mutation = `UPDATE ft SET x='u' WHERE ` + c.where
		}
		t.Run(c.verb+"/"+c.name, func(t *testing.T) {
			stmts := append([]string{
				`CREATE VIRTUAL TABLE ft USING fts3(x)`,
				`INSERT INTO ft(docid,x) VALUES(20,'d20')`,
				`INSERT INTO ft(docid,x) VALUES(30,'d30')`,
				`BEGIN`,
				`INSERT INTO ft(docid,x) VALUES(10,'d10')`,
				mutation,
				`INSERT INTO ft(docid,x) VALUES(60,'d60')`,
				`COMMIT`,
			}, fts3SubTxnDump...)
			differAllAccepted(t, c.verb+"/"+c.name, stmts, 8)
		})
	}
}

// Build database with text to push segments into block numbering.
func fts3SubTxnInterchangeWrite() []string {
	s := []string{`CREATE VIRTUAL TABLE ft USING fts4(x)`, `BEGIN`}
	for i := 0; i < 8; i++ {
		pad := strings.Repeat("pad"+string(rune('a'+i))+" ", 10)
		s = append(s, fmt.Sprintf(
			`INSERT INTO ft(docid,x) VALUES(%d,'doc%02d %s common trailing words'),(%d,'doc%02d %s common trailing words')`,
			2*i+1, 2*i+1, pad, 2*i+2, 2*i+2, pad))
		s = append(s, fmt.Sprintf(`INSERT INTO ft(docid,x) VALUES(%d,'solo%02d common')`, 100+i, i))
	}
	return append(s, `DELETE FROM ft WHERE docid=3`, `COMMIT`)
}

var fts3SubTxnInterchangeRead = []string{
	`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM ft_segdir ORDER BY level, idx`,
	`SELECT blockid, quote(block) FROM ft_segments ORDER BY blockid`,
	`SELECT group_concat(docid) FROM (SELECT docid FROM ft WHERE ft MATCH 'common' ORDER BY docid)`,
	`SELECT group_concat(docid) FROM (SELECT docid FROM ft WHERE ft MATCH 'doc05' ORDER BY docid)`,
	`SELECT group_concat(docid) FROM (SELECT docid FROM ft WHERE ft MATCH 'solo03' ORDER BY docid)`,
	`SELECT count(*) FROM ft`,
	`SELECT docid, quote(size) FROM ft_docsize ORDER BY docid`,
	`SELECT quote(value) FROM ft_stat`,
}

// Database read back identically under both engines regardless of which wrote it.
func TestFts3SubTxnFileInterchange(t *testing.T) {
	write := fts3SubTxnInterchangeWrite()
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "fts.db")
			wrote := runWithDSN(t, writer, dsn, write)
			for i, r := range wrote {
				if kind, _ := r["kind"].(string); kind == "error" {
					t.Fatalf("%s could not run the write script (the comparison would be vacuous): stmt #%d %s", writer, i, write[i])
				}
			}
			var baseline string
			goPath, cgoPath := pathsForBothEngines(t, writerEngineName(writer), dsn)
			for _, reader := range engineOrder {
				readPath := goPath
				if reader == "cgo" {
					readPath = cgoPath
				}
				got := fmt.Sprintf("%v", runWithDSN(t, reader, readPath, fts3SubTxnInterchangeRead))
				if baseline == "" {
					baseline = got
					continue
				}
				if got != baseline {
					t.Errorf("[%s wrote it] readers disagree\n  cgo reads:    %s\n  %s reads: %s", writer, baseline, reader, got)
				}
			}
		})
	}
}

// C SQLite reads musql-written transaction, checks integrity, and continues writing.
func TestFts3SubTxnIsIntactToCSQLite(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "fts.db")
	write := fts3SubTxnInterchangeWrite()
	wrote := runWithDSN(t, "musql", dsn, write)
	for i, r := range wrote {
		if kind, _ := r["kind"].(string); kind == "error" {
			t.Fatalf("musql could not run the write script: stmt #%d %s", i, write[i])
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
		t.Fatalf("C SQLite reports the musql-written file as corrupt: %s", ic)
	}
	if _, err := sdb.Exec(`INSERT INTO ft(ft) VALUES('integrity-check')`); err != nil {
		t.Fatalf("fts3's own integrity-check rejects the musql-written index: %v", err)
	}
	if _, err := sdb.Exec(`INSERT INTO ft(ft) VALUES('optimize')`); err != nil {
		t.Fatalf("C SQLite cannot optimize the musql-written index: %v", err)
	}
	if _, err := sdb.Exec(`INSERT INTO ft(x) VALUES('written by real sqlite afterwards')`); err != nil {
		t.Fatalf("C SQLite cannot go on writing to the musql-written index: %v", err)
	}
	if _, err := sdb.Exec(`INSERT INTO ft(ft) VALUES('integrity-check')`); err != nil {
		t.Fatalf("fts3's integrity-check fails after C SQLite wrote to the musql-written index: %v", err)
	}
}

// Vocabulary with deliberate prefix overlap for fuzzing.
var fts3SubTxnFuzzWords = []string{
	"alpha", "alphabet", "alp", "beta", "betamax", "be",
	"gamma", "gam", "delta", "del", "epsilon", "eps",
}

// Pick DELETE/UPDATE constraint mixing docid plans and near-misses.
func fts3SubTxnFuzzWhere(rng *rand.Rand, next int64) string {
	d := 1 + rng.Int63n(next-1)
	switch rng.Intn(8) {
	case 0:
		return fmt.Sprintf(`docid IN (%d)`, d)
	case 1:
		return fmt.Sprintf(`docid IN (%d,%d)`, d, d+1)
	case 2:
		return fmt.Sprintf(`docid>=%d AND docid<%d`, d, d+2)
	case 3:
		return fmt.Sprintf(`docid=%d AND x IS NOT NULL`, d)
	case 4:
		return fmt.Sprintf(`docid BETWEEN %d AND %d`, d, d)
	case 5:
		return fmt.Sprintf(`docid IS %d`, d)
	case 6:
		return fmt.Sprintf(`%d=docid`, d)
	default:
		return fmt.Sprintf(`docid=%d`, d)
	}
}

// Randomize write history to test rule against varied segmentation.
func TestFts3SubTxnFlushFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260802))
	for i := 0; i < 60; i++ {
		i := i
		name := fmt.Sprintf("h%02d", i)
		t.Run(name, func(t *testing.T) {
			create := `CREATE VIRTUAL TABLE ft USING fts4(x)`
			switch rng.Intn(3) {
			case 1:
				create = `CREATE VIRTUAL TABLE ft USING fts4(x, prefix=3)`
			case 2:
				create = `CREATE VIRTUAL TABLE ft USING fts3(x)`
			}
			s := []string{create, `CREATE TABLE src(a, b)`}
			docid := int64(1)
			for k := 0; k < rng.Intn(3); k++ {
				s = append(s, fmt.Sprintf(`INSERT INTO ft(docid,x) VALUES(%d,'%s')`,
					docid, fts3SubTxnFuzzWords[rng.Intn(len(fts3SubTxnFuzzWords))]))
				docid++
			}
			s = append(s, `BEGIN`)
			for k := 0; k < 1+rng.Intn(7); k++ {
				switch rng.Intn(10) {
				case 0:
					// SELECT source: one, zero, or two rows.
					n := rng.Intn(3)
					var arms []string
					for j := 0; j < n; j++ {
						arms = append(arms, fmt.Sprintf(`SELECT %d,'%s'`, docid,
							fts3SubTxnFuzzWords[rng.Intn(len(fts3SubTxnFuzzWords))]))
						docid++
					}
					if len(arms) == 0 {
						s = append(s, `INSERT INTO ft(docid,x) SELECT a,b FROM src`)
					} else {
						s = append(s, `INSERT INTO ft(docid,x) `+strings.Join(arms, " UNION ALL "))
					}
				case 1:
					if docid > 2 {
						s = append(s, `DELETE FROM ft WHERE `+fts3SubTxnFuzzWhere(rng, docid))
					}
				case 2:
					if docid > 2 {
						s = append(s, fmt.Sprintf(`UPDATE ft SET x='%s' WHERE %s`,
							fts3SubTxnFuzzWords[rng.Intn(len(fts3SubTxnFuzzWords))],
							fts3SubTxnFuzzWhere(rng, docid)))
					}
				case 3:
					// Backward docid; tests both seal rules.
					if docid > 5 {
						back := docid - 1 - rng.Int63n(3)
						s = append(s, fmt.Sprintf(`INSERT OR REPLACE INTO ft(docid,x) VALUES(%d,'%s')`,
							back, fts3SubTxnFuzzWords[rng.Intn(len(fts3SubTxnFuzzWords))]))
					}
				default:
					// VALUES list of 1..3 rows.
					var rows []string
					for j := 0; j < 1+rng.Intn(3); j++ {
						rows = append(rows, fmt.Sprintf(`(%d,'%s %s')`, docid,
							fts3SubTxnFuzzWords[rng.Intn(len(fts3SubTxnFuzzWords))],
							fts3SubTxnFuzzWords[rng.Intn(len(fts3SubTxnFuzzWords))]))
						docid++
					}
					s = append(s, `INSERT INTO ft(docid,x) VALUES`+strings.Join(rows, ","))
				}
			}
			nSetup := len(s) + 1
			s = append(s, `COMMIT`,
				`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM ft_segdir ORDER BY level, idx`,
				`SELECT blockid, quote(block) FROM ft_segments ORDER BY blockid`,
				`SELECT docid, quote(x) FROM ft_content ORDER BY docid`,
				`SELECT group_concat(docid) FROM (SELECT docid FROM ft WHERE ft MATCH 'alpha' ORDER BY docid)`,
				`SELECT group_concat(docid) FROM (SELECT docid FROM ft WHERE ft MATCH 'be*' ORDER BY docid)`,
				`SELECT count(*) FROM ft`,
			)
			differAllAccepted(t, name, s, nSetup)
		})
	}
}
