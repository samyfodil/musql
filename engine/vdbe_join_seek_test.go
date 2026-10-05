package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// buildJoinSeekTestDB builds an outer table and a larger inner table with a
// secondary index on its join column (fk) plus an INTEGER PRIMARY KEY (for the
// rowid-seek path), exercising duplicate join keys, unmatched outer keys, NULL
// keys, a NOCASE-indexed text join column, and enough inner rows that the index
// b-tree spans interior pages -- so the correlated inner seek is genuinely
// exercised, not just a single-page lookup.
func buildJoinSeekTestDB(t *testing.T) *ReadOnlyPager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "joinseek.sqlite")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	must := func(s string) {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	must(`CREATE TABLE o (id INTEGER PRIMARY KEY, k INTEGER, name TEXT, tag TEXT COLLATE NOCASE)`)
	must(`CREATE TABLE i (id INTEGER PRIMARY KEY, fk INTEGER, tag TEXT COLLATE NOCASE, payload TEXT)`)
	must(`CREATE INDEX ifk ON i(fk)`)   // INTEGER-affinity join column
	must(`CREATE INDEX itag ON i(tag)`) // NOCASE join column (collation-sensitive)

	// Outer: keys 0..9 plus an unmatched key (777) and a NULL key.
	outer := []struct {
		id, k int64
		kNull bool
		tag   string
	}{
		{1, 0, false, "A"},
		{2, 3, false, "b"},
		{3, 3, false, "B"}, // dup outer key 3, NOCASE tag variant
		{4, 7, false, "c"},
		{5, 777, false, "z"}, // no inner match
		{6, 0, true, "A"},    // NULL join key
		{7, 5, false, "d"},
	}
	for _, r := range outer {
		kv := Value{Typ: Int, I: r.k}
		if r.kNull {
			kv = Value{Typ: Null}
		}
		if _, _, err := db.ExecArgs(`INSERT INTO o(id,k,name,tag) VALUES(?,?,?,?)`,
			[]Value{{Typ: Int, I: r.id}, kv, {Typ: Text, S: []byte(fmt.Sprintf("o%d", r.id))}, {Typ: Text, S: []byte(r.tag)}}); err != nil {
			t.Fatal(err)
		}
	}

	// Inner: 3000 rows, fk = id%10 (so keys 0..9 each have ~300 duplicates,
	// forcing multi-row seek ranges and interior index pages); tag cycles over
	// case variants of a/b/c so the NOCASE join has real fold cases.
	const n = 3000
	tags := []string{"a", "A", "b", "B", "c"}
	for id := 1; id <= n; id++ {
		if _, _, err := db.ExecArgs(`INSERT INTO i(id,fk,tag,payload) VALUES(?,?,?,?)`,
			[]Value{{Typ: Int, I: int64(id)}, {Typ: Int, I: int64(id % 10)},
				{Typ: Text, S: []byte(tags[id%len(tags)])},
				{Typ: Text, S: []byte(fmt.Sprintf("payload-%d", id))}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// TestJoinSeekResultNeutral is the core guarantee: for a spread of join shapes,
// running with the correlated inner seek ENGAGED and with it DISABLED
// (joinSeekDisabled, the same seam the benchmarks use) must return byte-for-byte
// identical rows in identical order. A reorder or a dropped/duplicated row
// caused by the optimization fails here.
func TestJoinSeekResultNeutral(t *testing.T) {
	p := buildJoinSeekTestDB(t)

	cases := []struct {
		name string
		sql  string
	}{
		{"inner-on-index", `SELECT o.id, i.id FROM o JOIN i ON i.fk = o.k`},
		{"inner-on-index-rev", `SELECT o.id, i.id FROM o JOIN i ON o.k = i.fk`},
		{"where-equi", `SELECT o.id, i.id FROM o, i WHERE i.fk = o.k`},
		{"rowid-seek", `SELECT o.id, i.id FROM o JOIN i ON i.id = o.k`},
		{"nocase-join", `SELECT o.id, i.id FROM o JOIN i ON i.tag = o.tag`},
		{"ordered-by-inner", `SELECT o.id, i.id FROM o JOIN i ON i.fk = o.k ORDER BY i.id`},
		{"ordered-by-outer", `SELECT o.id, i.id FROM o JOIN i ON i.fk = o.k ORDER BY o.id, i.id`},
		{"no-order-natural", `SELECT o.id, i.id, i.payload FROM o JOIN i ON i.fk = o.k`},
		{"extra-conjunct", `SELECT o.id, i.id FROM o JOIN i ON i.fk = o.k AND i.id < 100`},
		{"where-plus-on", `SELECT o.id, i.id FROM o JOIN i ON i.fk = o.k WHERE i.payload LIKE 'payload-1%'`},
		{"left-join", `SELECT o.id, i.id FROM o LEFT JOIN i ON i.fk = o.k`},
		{"left-join-anti", `SELECT o.id FROM o LEFT JOIN i ON i.fk = o.k WHERE i.id IS NULL`},
		// The LEFT shapes the seek must not disturb: an outer key that matches
		// nothing (777) and a NULL one both have to NULL-extend, and both the
		// rowid and the NOCASE-collated index paths have to agree with the full
		// scan row for row. See emitJoinLevel's LEFT branch.
		{"left-join-rowid", `SELECT o.id, i.id FROM o LEFT JOIN i ON i.id = o.k`},
		{"left-join-nocase", `SELECT o.id, i.id FROM o LEFT JOIN i ON i.tag = o.tag AND i.id < 40`},
		{"left-join-extra-conjunct", `SELECT o.id, i.id FROM o LEFT JOIN i ON i.fk = o.k AND i.id < 100`},
		{"left-join-where", `SELECT o.id, i.id FROM o LEFT JOIN i ON i.fk = o.k WHERE o.k IS NOT NULL`},
		{"left-join-gc", `SELECT o.id, group_concat(i.id) FROM o LEFT JOIN i ON i.fk = o.k AND i.id < 60 GROUP BY o.id`},
		{"left-join-inner-chain", `SELECT a.id, b.id, c.id FROM o a JOIN i b ON b.fk = a.k LEFT JOIN i c ON c.fk = b.fk AND c.id < 30`},
		{"right-join", `SELECT o.id, i.id FROM o RIGHT JOIN i ON i.fk = o.k`},
		{"three-table-chain", `SELECT a.id, b.id, c.id FROM o a JOIN i b ON b.fk = a.k JOIN i c ON c.fk = a.k AND c.id < 50`},
		{"count-agg", `SELECT o.id, COUNT(*) FROM o JOIN i ON i.fk = o.k GROUP BY o.id`},
		{"group-concat-order", `SELECT o.id, group_concat(i.id) FROM o JOIN i ON i.fk = o.k AND i.id < 60 GROUP BY o.id`},
		{"limited", `SELECT o.id, i.id FROM o JOIN i ON i.fk = o.k LIMIT 25`},
	}

	run := func(sql string, seekOn bool) ([][]Value, error) {
		saved := joinSeekDisabled
		joinSeekDisabled = !seekOn
		defer func() { joinSeekDisabled = saved }()
		_, rows, err := p.QueryArgs(sql, nil)
		return rows, err
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seekRows, seekErr := run(tc.sql, true)
			scanRows, scanErr := run(tc.sql, false)
			if (seekErr == nil) != (scanErr == nil) {
				t.Fatalf("error mismatch: seek=%v scan=%v", seekErr, scanErr)
			}
			if seekErr != nil {
				return
			}
			if !rowsEqual(seekRows, scanRows) {
				t.Fatalf("join-seek changed the result:\n seek=%v\n scan=%v", seekRows, scanRows)
			}
		})
	}
}
