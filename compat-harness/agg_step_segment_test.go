// This file gates aggregate step segment ordering, verifying that errors from
// earlier aggregates precede errors from later ones.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// aggStepSegmentCases: each SQL raises in two different places on the same
// single row, and wantErr names the one that must win.
//
//   - json('zz') is "malformed JSON"; abs(-9223372036854775808) is "integer
//     overflow"; json_group_array(x'ff') raises inside its step BODY, on a BLOB
//     that is not valid JSONB.
//   - the fixture is ONE row, and it is the row that overflows, so the losing
//     error is genuinely available on the very first row the scan produces --
//     without that the winner would be decided by which row came first rather
//     than by the coding order.
var aggStepSegmentCases = []struct {
	name, sql, wantErr, loser string
}{
	// An argument musql could not lower is evaluated by aggItem.rowValue
	// INSIDE its own step body, so it raises where that body does -- and the
	// NEXT aggregate's lowered argument must not be hoisted in front of it.
	// It was a LIVE WRONG ERROR: musql reported the integer overflow, which
	// is the LATER aggregate's.
	//
	// The shape that still reaches that arm is the SORTED DRAIN's own refusal
	// -- a SUBQUERY cannot be compiled against the sorter record, because its
	// sub-Program would reach back into cursors the scan has closed
	// (aggDrainRow.lowerable). The grouped-subtype refusals that used to reach
	// it are gone: the subtype loss is taken at the LEAF on both grouped routes
	// now, so an expression above one is never refused for it.
	{"unlowered-arg-drain-subquery", `SELECT k, sum(json((SELECT s))), sum(abs(v)) FROM t GROUP BY k ORDER BY k`,
		"malformed JSON", "integer overflow"},
	{"unlowered-arg-hash", `SELECT k, sum(json(s)), sum(abs(v)) FROM t GROUP BY k`,
		"malformed JSON", "integer overflow"},
	{"unlowered-arg-drain", `SELECT k, sum(json(s)), sum(abs(v)) FROM t GROUP BY k ORDER BY k`,
		"malformed JSON", "integer overflow"},
	// The whole-table twin keeps the subtype and lowers BOTH arguments, so the
	// same error wins for the other reason: json() is argument list 0.
	{"unlowered-arg-whole", `SELECT sum(json(s)), sum(abs(v)) FROM t`,
		"malformed JSON", "integer overflow"},

	// A raising step BODY, which is what the old rule covered by refusing to
	// lower anything after it. Both accumulators lower now and the CUT carries
	// the ordering instead.
	{"raising-body-whole", `SELECT json_group_array(x'ff'), sum(abs(v)) FROM t`,
		"JSON cannot hold BLOB", "integer overflow"},
	{"raising-body-hash", `SELECT k, json_group_array(x'ff'), sum(abs(v)) FROM t GROUP BY k`,
		"JSON cannot hold BLOB", "integer overflow"},
	{"raising-body-drain", `SELECT k, json_group_array(x'ff'), sum(abs(v)) FROM t GROUP BY k ORDER BY k`,
		"JSON cannot hold BLOB", "integer overflow"},

	// The mirror: with the overflowing argument FIRST, the overflow is correct
	// on both engines -- C codes sum's argument list before json_group_array
	// exists. This is the direction a cut must NOT introduce, and without it
	// "always report the first accumulator's error" would pass everything above
	// while being wrong here.
	{"raising-body-reversed", `SELECT sum(abs(v)), json_group_array(x'ff') FROM t`,
		"integer overflow", "JSON cannot hold BLOB"},
	{"unlowered-arg-reversed", `SELECT k, sum(abs(v)), sum(json(s)) FROM t GROUP BY k`,
		"integer overflow", "malformed JSON"},
}

const aggStepSegmentFixture = `CREATE TABLE t(k INTEGER, s TEXT, v INTEGER)`
const aggStepSegmentInsert = `INSERT INTO t VALUES(1,'zz',-9223372036854775808)`

// TestAggStepSegmentErrorOrder pins, per engine, WHICH of the two available
// errors a statement reports. The oracle arm is the specification; the musql
// arm is what must match it.
//
// The messages are compared by SUBSTRING because the two engines word their
// errors differently in general -- what is being asserted is which FAILURE was
// reached, not how it is spelled -- and the loser substring is asserted absent
// so a message that happened to contain both could not pass.
func TestAggStepSegmentErrorOrder(t *testing.T) {
	dir := t.TempDir()
	for _, eng := range []struct{ driver, dsn string }{
		{"sqlite3", filepath.Join(dir, "cgo.db")},
		{"sqlite", filepath.Join(dir, "musql.db")},
	} {
		db, err := sql.Open(eng.driver, eng.dsn)
		if err != nil {
			t.Fatalf("%s: open: %v", eng.driver, err)
		}
		for _, s := range []string{aggStepSegmentFixture, aggStepSegmentInsert} {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("%s: %s: %v", eng.driver, s, err)
			}
		}
		for _, c := range aggStepSegmentCases {
			t.Run(eng.driver+"/"+c.name, func(t *testing.T) {
				rows, err := db.Query(c.sql)
				if err == nil {
					// database/sql defers some errors to the first Next().
					for rows.Next() {
					}
					err = rows.Err()
					rows.Close()
				}
				if err == nil {
					t.Fatalf("%s: answered, want an error mentioning %q", c.sql, c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("%s: %v -- want %q (the EARLIER aggregate's failure)", c.sql, err, c.wantErr)
				}
				if strings.Contains(err.Error(), c.loser) {
					t.Errorf("%s: %v -- reports %q, which belongs to a LATER aggregate", c.sql, err, c.loser)
				}
			})
		}
		db.Close()
	}
}

// TestAggCensusSegmentAnswers is the ANSWER half of the segmentation, and the
// only half a differential can see. The min()/max() CENSUS decides which row a
// bare column reads (aggAccumulators.magnetWalk, vdbe_agg.go), and magnetWalk
// runs as ONE unit inside the first step opcode -- so a census site stamped for
// a LATER segment would be stepped off a register this row has not filled yet.
//
// Each case pairs a census site whose argument a GROUPED plan refuses (json()
// and subtype() both, for the two different reasons in engine/agg_subtype.go)
// with a second census site that DOES lower, and reads a bare column. Measured
// by mutation -- planAggArgRegs' "i >= nMagnet" replaced by an always-true test,
// which lets the cut land between the two sites:
//
//	SELECT k, max(json(s)), min(v), v FROM t GROUP BY k
//	    3.53.3 and musql: 1|[3]|2|2      mutated musql: 1|[3]|2|9
func TestAggCensusSegmentAnswers(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(k, v, s)`,
		`INSERT INTO t VALUES(1,5,'[1]'),(1,9,'[2]'),(1,2,'[3]'),(2,7,'[4]'),(2,1,'[5]'),(2,8,'[6]')`,
	}
	for _, c := range []struct{ name, sql string }{
		{"json-max-then-min", `SELECT k, max(json(s)), min(v), v FROM t GROUP BY k ORDER BY k`},
		{"json-min-then-max", `SELECT k, min(json(s)), max(v), v FROM t GROUP BY k ORDER BY k`},
		{"json-three-sites", `SELECT k, max(json(s)), min(v), max(v), v FROM t GROUP BY k ORDER BY k`},
		{"json-and-groupconcat", `SELECT k, max(json(s)), min(v), group_concat(s), v FROM t GROUP BY k ORDER BY k`},
		{"subtype-max-then-min", `SELECT k, max(subtype(v)), min(v), v FROM t GROUP BY k ORDER BY k`},
		{"json-whole-table", `SELECT max(json(s)), min(v), v FROM t`},
		// The control: two census sites that BOTH lower, so no cut is even
		// available inside the block. Without it the cases above would pass with
		// the census refused wholesale.
		{"both-lower", `SELECT k, max(v), min(v), group_concat(s), v FROM t GROUP BY k ORDER BY k`},
	} {
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, append(append([]string{}, setup...), c.sql)) })
	}
}
