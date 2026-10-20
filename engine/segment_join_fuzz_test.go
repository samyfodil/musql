package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

// TestSegJoinFuzzMatchesLoop holds the join kernel to the loop it replaces
// over random tables and random join aggregates: the same statements against
// two databases built the same way, one with the kernel's recognisers off,
// every answer compared exactly -- rows, their order, bare columns, errors.
// The tables mix in what the kernel must decline or handle: NULLs, a TEXT in
// an INTEGER column, keys naming no row, negative and far-apart rowids,
// deletes and updates, rows still in the log and rows compacted by VACUUM.
func TestSegJoinFuzzMatchesLoop(t *testing.T) {
	seeds := 24
	if testing.Short() {
		seeds = 8
	}
	served := int64(0)
	for seed := range seeds {
		served += segJoinFuzzSeed(t, int64(seed))
	}
	if served == 0 {
		t.Fatal("the join kernel answered nothing")
	}
	t.Logf("kernel answered %d statements", served)
}

func segJoinFuzzSeed(t *testing.T, seed int64) int64 {
	r := rand.New(rand.NewSource(seed))
	clean := seed%3 != 0 // most seeds keep the keys clean, so the kernel serves
	var setup []string
	setup = append(setup,
		`CREATE TABLE d1(id INTEGER PRIMARY KEY, a INTEGER, b TEXT, c REAL)`,
		`CREATE TABLE d2(id INTEGER PRIMARY KEY, d1_id INTEGER, x INTEGER, y REAL)`,
		`CREATE TABLE f(id INTEGER PRIMARY KEY, d2_id INTEGER, d1_id INTEGER, v INTEGER, w REAL, s TEXT)`,
	)
	if r.Intn(2) == 0 {
		setup = append(setup, `CREATE INDEX f_d2 ON f(d2_id)`)
	}
	if r.Intn(2) == 0 {
		setup = append(setup, `CREATE INDEX d2_d1 ON d2(d1_id)`)
	}
	// An index the outer level can be scanned by, so the loop visits it in
	// the index's order rather than by rowid.
	switch r.Intn(5) {
	case 0:
		setup = append(setup, `CREATE INDEX d2_x ON d2(x)`)
	case 1:
		setup = append(setup, `CREATE INDEX d2_x ON d2(x DESC, y)`)
	case 2:
		setup = append(setup, `CREATE INDEX d2_e ON d2(x * 2 - y)`)
	case 3:
		// Two indexes an OR over the level can take one sub-scan each.
		setup = append(setup, `CREATE INDEX d2_x ON d2(x)`, `CREATE INDEX d2_y ON d2(y)`)
	}
	id := func(i int) int {
		switch r.Intn(10) {
		case 0:
			return -i - 1
		case 1:
			return i * 1000003
		}
		return i + 1
	}
	val := func(max int, nulls bool) string {
		if nulls && r.Intn(12) == 0 {
			return "NULL"
		}
		if !clean && r.Intn(25) == 0 {
			return "'t" + fmt.Sprint(r.Intn(5)) + "'"
		}
		return fmt.Sprint(r.Intn(max) - max/4)
	}
	real := func() string {
		if r.Intn(15) == 0 {
			return "NULL"
		}
		return fmt.Sprintf("%d.%d", r.Intn(40)-10, r.Intn(4)*25)
	}
	words := []string{"'a'", "'B'", "'b'", "'c'", "NULL", "''"}
	n1, n2, nf := 3+r.Intn(12), 5+r.Intn(30), 20+r.Intn(400)
	var d1ids, d2ids []int
	for i := range n1 {
		d1ids = append(d1ids, id(i))
		setup = append(setup, fmt.Sprintf(`INSERT INTO d1 VALUES (%d, %s, %s, %s)`, d1ids[i], val(6, true), words[r.Intn(len(words))], real()))
	}
	ref := func(ids []int) string {
		if !clean && r.Intn(15) == 0 {
			return "NULL"
		}
		if r.Intn(12) == 0 {
			return fmt.Sprint(r.Intn(1000) + 5000) // names no row
		}
		return fmt.Sprint(ids[r.Intn(len(ids))])
	}
	for i := range n2 {
		d2ids = append(d2ids, id(i+100))
		setup = append(setup, fmt.Sprintf(`INSERT INTO d2 VALUES (%d, %s, %s, %s)`, d2ids[i], ref(d1ids), val(9, !clean), real()))
	}
	for i := range nf {
		setup = append(setup, fmt.Sprintf(`INSERT INTO f VALUES (%d, %s, %s, %s, %s, %s)`,
			i+1, ref(d2ids), ref(d1ids), val(100, !clean), real(), words[r.Intn(len(words))]))
	}
	if r.Intn(2) == 0 {
		setup = append(setup, fmt.Sprintf(`DELETE FROM f WHERE id %% %d = 0`, 3+r.Intn(5)))
		setup = append(setup, fmt.Sprintf(`UPDATE f SET v = v + 1 WHERE id %% %d = 0`, 2+r.Intn(5)))
	}
	vacuum := r.Intn(2) == 0
	if r.Intn(3) == 0 {
		// Statistics, under which the planner may take a MULTI_OR level.
		setup = append(setup, `ANALYZE`)
	}

	queries := make([]string, 0, 60)
	aggs := []string{"count(*)", "count(f.v)", "sum(f.v)", "total(f.w)", "avg(f.v)", "min(f.v)", "max(f.v)",
		"sum(f.w)", "count(DISTINCT f.v)", "sum(DISTINCT d2.x)", "count(d2.x)", "max(d1.a)", "avg(f.w)",
		"sum(f.v * 3 + 1)", "count(1)", "sum(length(f.s))", "max(f.v % 7)", "total(f.w * f.v)", "sum(d2.x * d2.x)",
		"count(DISTINCT f.v / 10)", "avg(abs(f.v))", "sum(coalesce(f.v, 0))", "min(f.s || 'x')",
		"sum(CASE WHEN f.v > 10 THEN 1 ELSE 0 END)", "max(CASE f.s WHEN 'a' THEN f.v END)", "total(ifnull(f.w, 2))",
		"sum(CASE WHEN d2.x IS NULL THEN 0 WHEN d2.x > 2 THEN d2.x ELSE -1 END)",
		"group_concat(f.s)", "group_concat(f.v, '-')", "min(f.s)", "max(f.w)", "json_group_array(f.v)",
		"count(DISTINCT f.s)", "group_concat(DISTINCT f.s)", "group_concat(f.id ORDER BY f.w DESC)", "max(d1.b)",
		"count(*) FILTER (WHERE f.v > 3)", "sum(f.v) FILTER (WHERE f.s = 'a')", "avg(f.w) FILTER (WHERE d2.x IS NOT NULL)",
		"group_concat(f.s, '|' ORDER BY f.v, f.id)", "json_group_array(f.v ORDER BY f.id DESC)",
		"sum(-f.v)", "sum(f.v * d2.x)", "total(f.w + d2.y)", "group_concat(f.s || d2.x)", "max(coalesce(d2.x, f.v))", "sum(f.v & 7)", "max(~f.v)", "sum(f.v << 2 | 1)", "sum(CASE WHEN f.v > 0 THEN -f.v ELSE f.v >> 1 END)"}
	joins := []string{
		"FROM f JOIN d2 ON d2.id = f.d2_id",
		"FROM d2 JOIN f ON f.d2_id = d2.id",
		"FROM d2 LEFT JOIN f ON f.d2_id = d2.id",
		"FROM f LEFT JOIN d2 ON d2.id = f.d2_id",
		"FROM f JOIN d2 ON d2.id = f.d2_id JOIN d1 ON d1.id = d2.d1_id",
		"FROM d1 JOIN d2 ON d2.d1_id = d1.id JOIN f ON f.d2_id = d2.id",
		"FROM d2 CROSS JOIN f ON f.d2_id = d2.id",
		"FROM f JOIN d1 ON d1.id = f.d1_id",
		"FROM d2 LEFT JOIN f ON f.d2_id = d2.id AND f.v > 5",
		"FROM d2 JOIN f ON f.v = d2.x",
		"FROM f JOIN d2 ON d2.x = f.v % 4",
		"FROM d2 LEFT JOIN f ON f.d1_id = d2.d1_id",
		"FROM d2 JOIN f ON f.v = d2.x * 2",
		"FROM f JOIN d2 ON d2.id = f.d2_id + 0",
		"FROM d2 LEFT JOIN f ON f.v % 4 = d2.x AND f.w > 0",
		"FROM f JOIN d2 ON abs(d2.x) = f.v / 10",
		"FROM f LEFT JOIN d2 ON d2.id = f.d2_id AND d2.x < 4 AND d2.y >= 0",
		"FROM f LEFT JOIN d2 ON d2.id = f.d2_id JOIN d1 ON d1.id = f.d1_id",
		"FROM d2 LEFT JOIN f ON f.d2_id = d2.id JOIN d1 ON d1.id = d2.d1_id",
		"FROM d2 LEFT JOIN f ON f.d2_id = d2.id AND f.v > 0 JOIN d1 ON d1.id = d2.d1_id AND d1.a <> 1",
		"FROM d1 LEFT JOIN d2 ON d2.d1_id = d1.id JOIN f ON f.d1_id = d1.id",
		"FROM d2 LEFT JOIN f ON f.d2_id = d2.id LEFT JOIN d1 ON d1.id = f.d1_id",
		"FROM f LEFT JOIN d2 ON d2.id = f.d2_id LEFT JOIN d1 ON d1.id = f.d1_id",
		"FROM d1 LEFT JOIN d2 ON d2.d1_id = d1.id LEFT JOIN f ON f.d2_id = d2.id AND f.v % 3 = 1",
		"FROM f LEFT JOIN d2 ON d2.id = f.d2_id + 0 LEFT JOIN d1 ON d1.id = coalesce(d2.d1_id, f.d1_id)",
	}
	groups := []string{"", "GROUP BY d2.id", "GROUP BY f.d2_id", "GROUP BY d2.x", "GROUP BY d1.id", "GROUP BY d1.b", "GROUP BY f.s",
		"GROUP BY d2.x / 2", "GROUP BY f.v % 3 + d2.x", "GROUP BY upper(f.s)", "GROUP BY f.v % 5", "GROUP BY CASE WHEN f.v > 0 THEN 1 ELSE 2 END"}
	wheres := []string{"", "WHERE f.v > 10", "WHERE d2.x <= 3 AND f.w > 0", "WHERE f.id % 2 = 0", "WHERE d1.a <> 2", "WHERE f.v >= ?",
		"WHERE f.s LIKE 'b%'", "WHERE abs(f.v) > 3 AND length(f.s) = 1", "WHERE d2.y", "WHERE f.v + d2.x > 4", "WHERE coalesce(f.w, 1) > 0",
		"WHERE f.v > 50 OR f.s = 'a'", "WHERE CASE WHEN f.w > 0 THEN 1 END",
		"WHERE f.v IN (1, 2, 3, 40)", "WHERE d2.x IN (1, 2) AND f.s IN ('a', 'b')", "WHERE f.v NOT IN (1, 2, NULL)",
		"WHERE f.v IN (5, NULL)", "WHERE f.s NOT IN ('B', '')", "WHERE d2.x > 1", "WHERE d2.x BETWEEN 0 AND 5 AND f.v > 0",
		"WHERE d2.x * 2 - d2.y > 1", "WHERE d2.x = 1 OR d2.y > 3", "WHERE d2.x IN (2, 3) OR d2.y < 0",
		"WHERE f.v > d2.x", "WHERE f.w < d2.y OR f.s = 'a'", "WHERE d2.x = 1 OR d2.y = f.w", "WHERE d2.x = f.v % 5 OR d2.y > 3",
		"WHERE coalesce(f.v, d2.x) > 2", "WHERE -f.v < 3 AND f.v & 1 = 0", "WHERE f.v BETWEEN 5 AND 60", "WHERE CASE WHEN f.v > d2.x THEN f.w ELSE d2.y END > 0"}
	for range 50 {
		join := joins[r.Intn(len(joins))]
		hasD1 := strings.Contains(join, "d1 ")
		var sel []string
		for range 1 + r.Intn(3) {
			a := aggs[r.Intn(len(aggs))]
			if strings.Contains(a, "d1.") && !hasD1 {
				a = "count(*)"
			}
			sel = append(sel, a)
		}
		g := groups[r.Intn(len(groups))]
		if strings.Contains(g, "d1.") && !hasD1 {
			g = ""
		}
		if g != "" {
			sel = append([]string{strings.TrimPrefix(g, "GROUP BY ")}, sel...)
			if r.Intn(3) == 0 {
				sel = append(sel, "d2.y") // a bare column: the group's first row
			}
		}
		w := wheres[r.Intn(len(wheres))]
		if strings.Contains(w, "d1.") && !hasD1 {
			w = ""
		}
		q := "SELECT " + strings.Join(sel, ", ") + " " + join + " " + w + " " + g
		if g != "" && r.Intn(3) == 0 {
			// A HAVING over aggregates the select list does not name: their
			// own steps.
			q += []string{" HAVING count(*) > 1", " HAVING max(f.id) % 2 = 0", " HAVING sum(f.v) > 10 AND min(f.w) < 3",
				" HAVING substr(group_concat(f.id), 1, 2) <> '1,'", " HAVING total(f.w) > 7.25"}[r.Intn(5)]
		}
		if g != "" && r.Intn(3) == 0 {
			q += " ORDER BY 2 DESC, 1"
		}
		queries = append(queries, q)
	}
	for range 10 {
		join := joins[r.Intn(len(joins))]
		keys := []string{"f.v", "f.v DESC", "f.v + d2.x, f.id", "coalesce(d2.y, f.w) DESC, f.id", "d2.x", "f.w DESC", "f.s", "d2.id DESC, f.id", "f.v % 9 DESC", "length(f.s), f.id", "coalesce(d2.x, 7), f.id", "d2.y DESC, f.id DESC", "d2.x IS NULL, f.v"}
		topWheres := []string{"", "", "WHERE d2.x * 2 - d2.y > 1", "WHERE d2.x = 1 OR d2.y > 3", "WHERE d2.x > 1",
			"WHERE d2.x = 1 OR d2.y = f.w", "WHERE f.v > d2.x"}
		queries = append(queries, fmt.Sprintf("SELECT f.id, d2.id, f.v %s %s ORDER BY %s LIMIT %d OFFSET %d",
			join, topWheres[r.Intn(len(topWheres))], keys[r.Intn(len(keys))], 1+r.Intn(12), r.Intn(4)))
	}

	run := func(off bool) ([]string, int64) {
		segPeepholesOffForTest = off
		defer func() { segPeepholesOffForTest = false }()
		db, err := OpenOrCreate(filepath.Join(t.TempDir(), "f.musq"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, s := range setup {
			if _, _, err := db.ExecArgs(s, nil); err != nil {
				t.Fatalf("seed %d: %s: %v", seed, s, err)
			}
			db.CommitIfAutocommit()
		}
		if vacuum {
			if _, _, err := db.ExecArgs("VACUUM", nil); err != nil {
				t.Fatal(err)
			}
		}
		SegJoinHitsForTest()
		out := make([]string, len(queries))
		for i, q := range queries {
			var params []Value
			if strings.Contains(q, "?") {
				params = []Value{{Typ: Int, I: 20}}
			}
			_, rows, err := db.Query(q, params)
			out[i] = fmt.Sprint(rows, err)
		}
		return out, SegJoinHitsForTest()
	}
	want, refHits := run(true)
	if refHits != 0 {
		t.Fatalf("seed %d: the reference run used the join kernel", seed)
	}
	got, hits := run(false)
	for i := range queries {
		if want[i] != got[i] {
			t.Errorf("seed %d (clean %v, vacuum %v): %s\n  loop   %.400s\n  kernel %.400s", seed, clean, vacuum, queries[i], want[i], got[i])
		}
	}
	return hits
}
