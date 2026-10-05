package compat

// Randomized tests for join order optimization over unindexed tables.
// These test that the planner produces correct scan orders, which are observable
// through group_concat, bare columns in grouped aggregates, and LIMIT.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r27Case is one generated shape: the DDL/DML that builds it, the single
// statement whose result is compared, and a coarse label for bucketing.
type r27Case struct {
	setup []string
	query string
	shape string
}

func r27Env(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// r27Val yields a literal from a pool wide enough that group_concat output
// distinguishes visit orders, including the NULL and REAL/INTEGER pairs that
// make min/max and the reported GROUP BY key value order-sensitive.
func r27Val(rng *rand.Rand, i int) string {
	switch rng.Intn(8) {
	case 0:
		return "NULL"
	case 1:
		return strconv.Itoa(rng.Intn(4))
	case 2:
		return fmt.Sprintf("%d.0", rng.Intn(3))
	case 3:
		return "'" + string(rune('a'+rng.Intn(6))) + strconv.Itoa(i) + "'"
	default:
		return "'" + string(rune('A'+i%26)) + "'"
	}
}

// r27Table emits one CREATE TABLE + INSERT pair. nCol columns named <p>1..<p>n,
// with the join column at position joinAt (0-based) so the sweep does not
// silently hold "the shared column is declared first" fixed. The column at
// obsAt carries a value unique to (table, row): an observer column whose values
// repeat cannot distinguish two visit orders, and that is how a divergence
// hides.
//
// Those unique values are a random PERMUTATION of 0..nRow-1, not r itself.
// Ascending-with-rowid tags made the whole `eq` half of this battery report 0
// divergences on the first run: SQLite's automatic index is keyed on the
// constrained column followed by the other columns the query uses, so its scan
// of one key's matches comes out in observer-value order -- which, with an
// ascending tag, is byte-identical to the rowid order musql scans in. The
// divergence was real and the fixture hid it.
// R27_INDEXED=1 additionally declares an index on the join column and one on
// the observer column of every table. It is a MEASUREMENT knob, not a gate:
// markWherePlanEligibility declines any multi-table FROM clause carrying an
// index (whereLoopAddBtreeIndex is ported only for the SINGLE-table scan --
// engine/where_plan_index.go), so what it reads is the size of that remaining
// hole: 315/600 at the commit that added it, against 0/600 unindexed. It is
// therefore EXEMPT from the assertion below -- the number is the deliverable.
func r27Indexed() bool { return os.Getenv("R27_INDEXED") != "" }

func r27Table(rng *rand.Rand, name, p string, nCol, nRow, joinAt, obsAt int, keyPool []string) []string {
	cols := make([]string, nCol)
	for i := range cols {
		cols[i] = p + strconv.Itoa(i+1)
	}
	stmts := []string{"CREATE TABLE " + name + "(" + strings.Join(cols, ",") + ")"}
	if r27Indexed() {
		stmts = append(stmts,
			"CREATE INDEX x"+name+"j ON "+name+"("+cols[joinAt]+")",
			"CREATE INDEX x"+name+"o ON "+name+"("+cols[obsAt]+")")
	}
	if nRow == 0 {
		return stmts
	}
	tag := rng.Perm(nRow)
	rows := make([]string, nRow)
	for r := 0; r < nRow; r++ {
		vals := make([]string, nCol)
		for c := range vals {
			switch c {
			case joinAt:
				vals[c] = keyPool[rng.Intn(len(keyPool))]
			case obsAt:
				vals[c] = "'" + p + strconv.Itoa(tag[r]) + "'"
			default:
				vals[c] = r27Val(rng, r)
			}
		}
		rows[r] = "(" + strings.Join(vals, ",") + ")"
	}
	stmts = append(stmts, "INSERT INTO "+name+" VALUES"+strings.Join(rows, ","))
	return stmts
}

func r27Gen(rng *rand.Rand, id int) r27Case {
	sfx := strconv.Itoa(id)
	t1, t2 := "ja"+sfx, "jb"+sfx
	n1, n2 := 2+rng.Intn(3), 2+rng.Intn(3)
	// One of the two tables is empty roughly one case in twelve; an empty
	// inner table is the shape that hides a loop-order difference entirely,
	// and an empty OUTER one is the shape that hides it only for INNER joins.
	r1, r2 := 3+rng.Intn(5), 3+rng.Intn(5)
	switch rng.Intn(12) {
	case 0:
		r1 = 0
	case 1:
		r2 = 0
	}
	j1, j2 := rng.Intn(n1), rng.Intn(n2)
	o1, o2 := (j1+1)%n1, (j2+1)%n2
	keyPool := []string{"1", "2", "3", "'1'", "NULL"}

	c := r27Case{}
	c.setup = append(c.setup, r27Table(rng, t1, "a", n1, r1, j1, o1, keyPool)...)
	c.setup = append(c.setup, r27Table(rng, t2, "b", n2, r2, j2, o2, keyPool)...)

	ka := t1 + ".a" + strconv.Itoa(j1+1)
	kb := t2 + ".b" + strconv.Itoa(j2+1)
	// The per-row-unique observer column on each side.
	va := t1 + ".a" + strconv.Itoa(o1+1)
	vb := t2 + ".b" + strconv.Itoa(o2+1)

	// Connector. musql's AST cannot represent CROSS (JoinCross is JoinKind's
	// zero value and a plain comma join gets it), so "CROSS JOIN" here is a
	// C SQLite-side reorder barrier and a no-op on the musql side -- which
	// is exactly what makes it worth sampling.
	var from, conn string
	switch rng.Intn(5) {
	case 0:
		from, conn = t1+","+t2, "comma"
	case 1:
		from, conn = t1+" CROSS JOIN "+t2, "cross"
	case 2:
		from, conn = t1+" JOIN "+t2+" ON "+ka+"="+kb, "on-eq"
	case 3:
		from, conn = t1+" LEFT JOIN "+t2+" ON "+ka+"="+kb, "left-eq"
	default:
		from, conn = t2+","+t1, "comma-rev"
	}

	// WHERE. The single-table restriction is the case SQLite hoists into the
	// OUTER loop and musql's component rule pushes to the INNER one.
	var where, wlab string
	switch rng.Intn(6) {
	case 0:
		where, wlab = "", "none"
	case 1:
		where, wlab = " WHERE "+ka+"="+kb, "eq"
	case 2:
		where, wlab = " WHERE "+kb+" IS NOT NULL", "filt-b"
	case 3:
		where, wlab = " WHERE "+ka+" IS NOT NULL", "filt-a"
	case 4:
		where, wlab = " WHERE "+ka+"="+kb+" AND "+va+" IS NOT NULL", "eq+filt-a"
	default:
		where, wlab = " WHERE "+kb+"="+ka, "eq-rev"
	}
	if strings.HasPrefix(conn, "left") || conn == "on-eq" {
		// Keep the ON join's own equality out of the WHERE half the time so
		// the two halves are sampled independently.
		if rng.Intn(2) == 0 {
			where, wlab = "", "none"
		}
	}

	var q, qlab string
	switch rng.Intn(6) {
	case 0:
		q, qlab = "SELECT "+ka+", count(*), group_concat("+vb+") FROM "+from+where+" GROUP BY "+ka+" ORDER BY 1", "gc-grouped"
	case 1:
		q, qlab = "SELECT "+ka+", count(*), "+vb+" FROM "+from+where+" GROUP BY "+ka+" ORDER BY 1", "bare-grouped"
	case 2:
		q, qlab = "SELECT group_concat("+va+"||'-'||"+vb+") FROM "+from+where, "gc-flat"
	case 3:
		q, qlab = "SELECT "+va+", "+vb+" FROM "+from+where+" LIMIT 3", "limit"
	case 4:
		q, qlab = "SELECT "+kb+", count(*), group_concat("+va+") FROM "+from+where+" GROUP BY "+kb+" ORDER BY 1", "gc-grouped-b"
	default:
		q, qlab = "SELECT "+kb+", count(*), "+va+" FROM "+from+where+" GROUP BY "+kb+" ORDER BY 1", "bare-grouped-b"
	}
	c.query = q
	c.shape = conn + "/" + wlab + "/" + qlab
	return c
}

// r27Batch runs a slate of independent cases through one worker process each
// side. Every case has its own table names, so batching cannot make one case
// visible to another; it only keeps 600 shapes from costing 1200 process
// spawns.
const r27Batch = 25

func TestR27JoinOrderBattery(t *testing.T) {
	nCase := r27Env("R27_CASES", 600)
	rng := rand.New(rand.NewSource(int64(r27Env("R27_SEED", 27))))

	cases := make([]r27Case, nCase)
	for i := range cases {
		cases[i] = r27Gen(rng, i)
	}
	if os.Getenv("R27_DUMP") != "" {
		for i, c := range cases {
			t.Logf("R27CASE %d %s | %s | %s", i, c.shape, strings.Join(c.setup, "; "), c.query)
		}
		return
	}

	type tally struct{ n, diverged int }
	byShape := map[string]*tally{}
	example := map[string]string{}
	total := 0

	for start := 0; start < nCase; start += r27Batch {
		end := start + r27Batch
		if end > nCase {
			end = nCase
		}
		var stmts []string
		if os.Getenv("R27_AUTOIDX_OFF") != "" {
			// Runs the WHOLE battery with the transient automatic index switched
			// off on BOTH sides, which is the only order-observing channel that
			// gates "PRAGMA automatic_index" at all: the TCL corpus routes every
			// pragma to the exec side (tclIsQuery), where rows are never compared.
			// One statement per worker process, so it applies to all r27Batch cases
			// that follow it on that connection -- which is also what proves the
			// driver CARRIES the flag across its one-session-per-statement model.
			//
			// It began as a pure MEASUREMENT knob and its meaning inverted twice.
			// Before the automatic index was ported this engine behaved as though
			// the pragma were permanently OFF, so the knob read 4/600 while the
			// DEFAULT oracle read 76; once the index landed it behaved as though
			// permanently ON, so the default read 0 and the knob read 75. Both
			// numbers measured the same unported thing -- whereLoopAddBtree's
			// "(pParse->db->flags & SQLITE_AutoIndex)!=0" guard. That guard is now
			// ported (engine/where_plan_gate.go's sqliteExecOrder), so BOTH
			// settings assert 0 and the knob is a gate rather than a measurement.
			stmts = append(stmts, "PRAGMA automatic_index=off")
		}
		qAt := make([]int, 0, r27Batch)
		for _, c := range cases[start:end] {
			stmts = append(stmts, c.setup...)
			qAt = append(qAt, len(stmts))
			stmts = append(stmts, c.query)
		}
		cgo := run(t, "cgo", stmts)
		mush := run(t, "musql", stmts)
		for k, c := range cases[start:end] {
			i := qAt[k]
			cb, _ := json.Marshal(cgo[i])
			mb, _ := json.Marshal(mush[i])
			tl := byShape[c.shape]
			if tl == nil {
				tl = &tally{}
				byShape[c.shape] = tl
			}
			tl.n++
			if string(cb) != string(mb) {
				tl.diverged++
				total++
				if example[c.shape] == "" {
					example[c.shape] = fmt.Sprintf("%s\n      %s\n      cgo:    %s\n      musql: %s",
						strings.Join(c.setup, "; "), c.query, cb, mb)
				}
			}
		}
	}

	keys := make([]string, 0, len(byShape))
	for k := range byShape {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if byShape[keys[i]].diverged != byShape[keys[j]].diverged {
			return byShape[keys[i]].diverged > byShape[keys[j]].diverged
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		tl := byShape[k]
		if tl.diverged == 0 {
			continue
		}
		t.Logf("R27 %-34s %3d/%-3d diverge", k, tl.diverged, tl.n)
		if os.Getenv("R27_EXAMPLES") != "" {
			t.Logf("    %s", example[k])
		}
	}
	if os.Getenv("R27_MEASURE") != "" {
		t.Logf("R27 BATTERY TOTAL: %d/%d diverge", total, nCase)
	}
	if total > 0 && r27Indexed() {
		t.Logf("R27 INDEXED (measurement only): %d/%d shapes diverge -- the size of "+
			"the multi-table half of whereLoopAddBtreeIndex, which is ported only "+
			"for a SINGLE-table scan (engine/where_plan_index.go)", total, nCase)
	} else if total > 0 {
		// Asserted under R27_AUTOIDX_OFF too, which is the point of that knob now:
		// both pragma settings read 0/600. It used to be exempt because only the
		// oracle honoured the pragma; see the knob's own comment above.
		t.Errorf("join order / automatic index: %d/%d shapes diverge "+
			"(re-run with R27_EXAMPLES=1 for the first case per shape)", total, nCase)
	}
}
