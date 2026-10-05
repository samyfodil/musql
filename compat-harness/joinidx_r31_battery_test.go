package compat

// The INDEXED-join battery tests two-table join queries with various index
// placements (join column, observer column, covering, desc, etc.), detected
// via scan order (group_concat, grouped aggregates, LIMIT).

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

type r31JoinCase struct {
	setup []string
	query string
	idx   string // index placement family
	conn  string // join connector
	shape string // conn/where/query
}

func r31Env(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// r31Val generates values for group_concat ordering detection.
func r31Val(rng *rand.Rand, i int) string {
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

var r31Placements = []string{"none", "join", "obs", "dead", "join+dead", "cover", "multi", "desc"}

// r31Table generates CREATE TABLE, indexes, and INSERT statements with
// varying declared types.
func r31Table(rng *rand.Rand, name, p string, nCol, nRow, joinAt, obsAt, deadAt int, placement string, keyPool []string) []string {
	types := []string{"", " INT", " TEXT", " REAL", " BLOB"}
	cols := make([]string, nCol)
	bare := make([]string, nCol)
	for i := range cols {
		bare[i] = p + strconv.Itoa(i+1)
		cols[i] = bare[i] + types[rng.Intn(len(types))]
	}
	stmts := []string{"CREATE TABLE " + name + "(" + strings.Join(cols, ",") + ")"}
	add := func(sfx, body string) {
		stmts = append(stmts, "CREATE INDEX x"+name+sfx+" ON "+name+"("+body+")")
	}
	switch placement {
	case "join":
		add("j", bare[joinAt])
	case "obs":
		add("o", bare[obsAt])
	case "dead":
		add("d", bare[deadAt])
	case "join+dead":
		add("j", bare[joinAt])
		add("d", bare[deadAt])
	case "cover":
		add("c", strings.Join(bare, ","))
	case "multi":
		add("m", bare[joinAt]+","+bare[(joinAt+1)%nCol])
	case "desc":
		add("s", bare[joinAt]+" DESC")
	}
	if nRow == 0 {
		return stmts
	}
	// A random PERMUTATION for the observer tag, not the row number: an
	// ascending tag makes an index walk of one key's matches byte-identical to
	// the rowid order musql scans in, which is how r27's first run hid a real
	// divergence in its whole `eq` half.
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
				vals[c] = r31Val(rng, r)
			}
		}
		rows[r] = "(" + strings.Join(vals, ",") + ")"
	}
	return append(stmts, "INSERT INTO "+name+" VALUES"+strings.Join(rows, ","))
}

func r31Gen(rng *rand.Rand, id int) r31JoinCase {
	sfx := strconv.Itoa(id)
	t1, t2 := "ka"+sfx, "kb"+sfx
	// >=3 columns so join/observer/dead can be three DISTINCT positions.
	n1, n2 := 3+rng.Intn(3), 3+rng.Intn(3)
	r1, r2 := 3+rng.Intn(5), 3+rng.Intn(5)
	switch rng.Intn(12) {
	case 0:
		r1 = 0
	case 1:
		r2 = 0
	}
	j1, j2 := rng.Intn(n1), rng.Intn(n2)
	o1, o2 := (j1+1)%n1, (j2+1)%n2
	d1, d2 := (j1+2)%n1, (j2+2)%n2
	keyPool := []string{"1", "2", "3", "'1'", "NULL"}

	placement := r31Placements[rng.Intn(len(r31Placements))]
	// Which side carries it: both, left only, or right only. A one-sided index
	// is the shape that separates "the planner is off" from "this table's scan
	// order changed".
	side := rng.Intn(3)
	p1, p2 := placement, placement
	if side == 1 {
		p2 = "none"
	} else if side == 2 {
		p1 = "none"
	}

	c := r31JoinCase{idx: placement}
	c.setup = append(c.setup, r31Table(rng, t1, "a", n1, r1, j1, o1, d1, p1, keyPool)...)
	c.setup = append(c.setup, r31Table(rng, t2, "b", n2, r2, j2, o2, d2, p2, keyPool)...)

	ka := t1 + ".a" + strconv.Itoa(j1+1)
	kb := t2 + ".b" + strconv.Itoa(j2+1)
	va := t1 + ".a" + strconv.Itoa(o1+1)
	vb := t2 + ".b" + strconv.Itoa(o2+1)

	var from string
	switch rng.Intn(5) {
	case 0:
		from, c.conn = t1+","+t2, "comma"
	case 1:
		from, c.conn = t1+" CROSS JOIN "+t2, "cross"
	case 2:
		from, c.conn = t1+" JOIN "+t2+" ON "+ka+"="+kb, "on-eq"
	case 3:
		from, c.conn = t1+" LEFT JOIN "+t2+" ON "+ka+"="+kb, "left-eq"
	default:
		from, c.conn = t2+","+t1, "comma-rev"
	}

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
	if strings.HasPrefix(c.conn, "left") || c.conn == "on-eq" {
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
	c.shape = c.conn + "/" + wlab + "/" + qlab
	return c
}

const r31Batch = 25

// TestR31JoinIdxBattery reports the divergence count per index placement and
// per connector. It asserts only the "none" family (which is R27's own gate
// restated over this fixture, so a regression there is a real bug); every other
// family is the measurement.
func TestR31JoinIdxBattery(t *testing.T) {
	nCase := r31Env("R31_CASES", 800)
	rng := rand.New(rand.NewSource(int64(r31Env("R31_SEED", 31))))

	cases := make([]r31JoinCase, nCase)
	for i := range cases {
		cases[i] = r31Gen(rng, i)
	}
	if os.Getenv("R31_DUMP") != "" {
		for i, c := range cases {
			t.Logf("R31CASE %d %s %s | %s | %s", i, c.idx, c.shape, strings.Join(c.setup, "; "), c.query)
		}
		return
	}

	// wrong vs declined is the split that decides who owns the remainder: a
	// WRONG answer is this planner's, a DECLINE is whichever guard refused to
	// compile (for this shape, usually the aggregate anchor's access-path
	// superset, anchorPlanOrderProvable in vdbe_agg_codegen.go).
	type tally struct{ n, diverged, declined int }
	byIdx := map[string]*tally{}
	byConn := map[string]*tally{}
	byIdxConn := map[string]*tally{}
	example := map[string]string{}
	declExample := map[string]string{}
	total, totalDecl := 0, 0
	bump := func(m map[string]*tally, k string, bad, decl bool) {
		tl := m[k]
		if tl == nil {
			tl = &tally{}
			m[k] = tl
		}
		tl.n++
		if bad {
			tl.diverged++
		}
		if decl {
			tl.declined++
		}
	}

	for start := 0; start < nCase; start += r31Batch {
		end := start + r31Batch
		if end > nCase {
			end = nCase
		}
		var stmts []string
		qAt := make([]int, 0, r31Batch)
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
			bad := string(cb) != string(mb)
			// A DECLINE: musql refused the statement outright while the oracle
			// answered. Never a wrong answer, and not this planner's to fix.
			decl := bad && mush[i]["kind"] == "error" && cgo[i]["kind"] != "error"
			bump(byIdx, c.idx, bad, decl)
			bump(byConn, c.conn, bad, decl)
			bump(byIdxConn, c.idx+"/"+c.conn, bad, decl)
			if bad {
				total++
				if decl {
					totalDecl++
					if declExample[c.idx] == "" {
						declExample[c.idx] = fmt.Sprintf("%s\n      %s\n      cgo: %s",
							strings.Join(c.setup, "; "), c.query, cb)
					}
				} else if example[c.idx] == "" {
					example[c.idx] = fmt.Sprintf("%s\n      %s\n      cgo:    %s\n      musql: %s",
						strings.Join(c.setup, "; "), c.query, cb, mb)
				}
			}
		}
	}

	dump := func(label string, m map[string]*tally) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t.Logf("R31 %-6s %-22s %3d/%-3d diverge (%d wrong, %d declined)",
				label, k, m[k].diverged, m[k].n, m[k].diverged-m[k].declined, m[k].declined)
		}
	}
	dump("idx", byIdx)
	dump("conn", byConn)
	if os.Getenv("R31_FULL") != "" {
		dump("pair", byIdxConn)
	}
	if os.Getenv("R31_EXAMPLES") != "" {
		for k, v := range example {
			t.Logf("R31 example %s: %s", k, v)
		}
		for k, v := range declExample {
			t.Logf("R31 decline %s: %s", k, v)
		}
	}
	t.Logf("R31 BATTERY TOTAL: %d/%d diverge (%d wrong, %d declined)",
		total, nCase, total-totalDecl, totalDecl)

	// The one assertion: with no index declared anywhere the ported planner is
	// eligible and must be exact. Everything else is the measurement.
	if tl := byIdx["none"]; tl != nil && tl.diverged > 0 {
		t.Errorf("unindexed join order regressed: %d/%d shapes diverge "+
			"(re-run with R31_EXAMPLES=1)", tl.diverged, tl.n)
	}
}
