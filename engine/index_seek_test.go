package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// buildIndexSeekTestDB builds a table with various index forms: BINARY, NOCASE,
// RTRIM, collation mismatches, and NULLs, with duplicate and variant keys.
func buildIndexSeekTestDB(t *testing.T) *ReadOnlyPager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "idxseek.sqlite")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE t (
			id INTEGER PRIMARY KEY,
			a TEXT,
			b INTEGER,
			c TEXT COLLATE NOCASE,
			d TEXT COLLATE RTRIM,
			e TEXT COLLATE NOCASE,
			u TEXT UNIQUE,
			f INTEGER,
			g INTEGER
		)`,
		`CREATE INDEX ia ON t(a)`,                // BINARY (inherited)
		`CREATE INDEX ib ON t(b)`,                // INTEGER affinity column
		`CREATE INDEX ic ON t(c)`,                // NOCASE (inherited)
		`CREATE INDEX id ON t(d)`,                // RTRIM (inherited)
		`CREATE INDEX ie ON t(e COLLATE BINARY)`, // MISMATCH: BINARY index on a NOCASE column
		`CREATE INDEX ifdesc ON t(f DESC)`,       // DESC LEADING -> declined
		`CREATE INDEX igmix ON t(g, f DESC)`,     // ASC leading, non-leading DESC -> usable on g
	}
	for _, s := range stmts {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// Rows chosen so several indexed columns have duplicate keys, case/space
	// variants, NULLs, and numeric/text affinity mismatches.
	type row struct {
		a, c, d, e, u string
		b, f, g       int64
		aNull, uNull  bool
	}
	rows := []row{
		{a: "apple", b: 14, c: "foo", d: "x", e: "foo", u: "u1", f: 100, g: 5},
		{a: "apple", b: 14, c: "FOO", d: "x ", e: "FOO", u: "u2", f: 200, g: 5}, // dup a, dup b, dup g, NOCASE/RTRIM variants
		{a: "banana", b: 7, c: "Foo", d: "x  ", e: "Foo", u: "u3", f: 100, g: 8},
		{a: "cherry", b: 99, c: "bar", d: "y", e: "bar", u: "u4", f: 300, g: 8},
		{aNull: true, b: 14, c: "baz", d: "z ", e: "baz", u: "u5", f: 200, g: 5},  // NULL a, dup b, dup g
		{a: "date", b: 7, c: "qux", d: "y ", e: "qux", uNull: true, f: 100, g: 9}, // NULL u
	}
	for i, r := range rows {
		id := int64(i + 1)
		av := Value{Typ: Text, S: []byte(r.a)}
		if r.aNull {
			av = Value{Typ: Null}
		}
		uv := Value{Typ: Text, S: []byte(r.u)}
		if r.uNull {
			uv = Value{Typ: Null}
		}
		if _, _, err := db.ExecArgs(`INSERT INTO t(id,a,b,c,d,e,u,f,g) VALUES(?,?,?,?,?,?,?,?,?)`,
			[]Value{{Typ: Int, I: id}, av, {Typ: Int, I: r.b},
				{Typ: Text, S: []byte(r.c)}, {Typ: Text, S: []byte(r.d)},
				{Typ: Text, S: []byte(r.e)}, uv,
				{Typ: Int, I: r.f}, {Typ: Int, I: r.g}}); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
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

// TestIndexSeekResultNeutral verifies that index-seek optimization returns
// exactly the correct rows in the correct order for various WHERE shapes.
func TestIndexSeekResultNeutral(t *testing.T) {
	p := buildIndexSeekTestDB(t)

	cases := []struct {
		name string
		sql  string
		args []Value
		want []int64 // matching ids, ascending; nil means zero rows
	}{
		{"binary-dup", "SELECT id FROM t WHERE a = 'apple'", nil, []int64{1, 2}},
		{"binary-reversed", "SELECT id FROM t WHERE 'banana' = a", nil, []int64{3}},
		{"binary-param", "SELECT id FROM t WHERE a = ?", []Value{{Typ: Text, S: []byte("cherry")}}, []int64{4}},
		{"binary-nomatch", "SELECT id FROM t WHERE a = 'zzz'", nil, nil},
		{"binary-null-key", "SELECT id FROM t WHERE a = ?", []Value{{Typ: Null}}, nil},
		{"binary-case-sensitive", "SELECT id FROM t WHERE a = 'APPLE'", nil, nil}, // BINARY: no case fold
		{"numeric-dup", "SELECT id FROM t WHERE b = 14", nil, []int64{1, 2, 5}},
		{"numeric-textkey", "SELECT id FROM t WHERE b = '14'", nil, []int64{1, 2, 5}}, // affinity: text key -> numeric
		{"numeric-7", "SELECT id FROM t WHERE b = 7", nil, []int64{3, 6}},
		{"nocase-fold", "SELECT id FROM t WHERE c = 'foo'", nil, []int64{1, 2, 3}}, // foo/FOO/Foo
		{"nocase-upperkey", "SELECT id FROM t WHERE c = 'FOO'", nil, []int64{1, 2, 3}},
		{"nocase-bar", "SELECT id FROM t WHERE c = 'BAR'", nil, []int64{4}},
		{"rtrim-fold", "SELECT id FROM t WHERE d = 'x'", nil, []int64{1, 2, 3}}, // x/x /x
		{"rtrim-keyspace", "SELECT id FROM t WHERE d = 'x  '", nil, []int64{1, 2, 3}},
		{"rtrim-y", "SELECT id FROM t WHERE d = 'y'", nil, []int64{4, 6}},
		// Seek is declined (collation mismatch), but the planner scans the covering
		// index anyway, returning rows in index key order.
		{"mismatch-index-nocase-col", "SELECT id FROM t WHERE e = 'FOO'", nil, []int64{2, 3, 1}},
		{"unique-auto", "SELECT id FROM t WHERE u = 'u3'", nil, []int64{3}}, // auto-index declined -> full scan, still correct
		{"unique-null", "SELECT id FROM t WHERE u = ?", []Value{{Typ: Null}}, nil},
		{"desc-leading-declined", "SELECT id FROM t WHERE f = 100", nil, []int64{1, 3, 6}}, // DESC-leading index declined -> full scan
		{"desc-leading-none", "SELECT id FROM t WHERE f = 999", nil, nil},
		// Usable: seeks on g (ASC leading). The rows then come out in igmix's
		// own key order (g, f DESC, rowid) rather than rowid order, because the
		// ported planner scans that index -- f=200 before f=100. Verified
		// against the oracle (compat-harness's TestR29IndexOrderRules).
		{"asc-leading-nonleading-desc", "SELECT id FROM t WHERE g = 5", nil, []int64{2, 5, 1}},
		{"asc-leading-g8", "SELECT id FROM t WHERE g = 8", nil, []int64{4, 3}},
		{"extra-conjunct", "SELECT id FROM t WHERE a = 'apple' AND b = 14", nil, []int64{1, 2}},
		{"extra-conjunct-prune", "SELECT id FROM t WHERE b = 14 AND a = 'apple'", nil, []int64{1, 2}},
		{"or-not-seekable", "SELECT id FROM t WHERE a = 'apple' OR b = 99", nil, []int64{1, 2, 4}},
	}

	run := func(sql string, args []Value) [][]Value {
		_, rows, err := p.QueryArgs(sql, args)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return rows
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vrows := run(tc.sql, tc.args)
			if len(vrows) != len(tc.want) {
				t.Fatalf("row count: got %d want %d (%v)", len(vrows), len(tc.want), vrows)
			}
			for i, w := range tc.want {
				if len(vrows[i]) != 1 || vrows[i][0].Typ != Int || vrows[i][0].I != w {
					t.Fatalf("row %d: got %v want id=%d", i, vrows[i], w)
				}
			}
		})
	}
}

// TestIndexSeekFiresWhenEligible verifies the OpSeekIndexHint is present for
// eligible WHERE shapes and absent for declined shapes.
func TestIndexSeekFiresWhenEligible(t *testing.T) {
	p := buildIndexSeekTestDB(t)

	fires := []string{
		"SELECT id FROM t WHERE a = 'apple'", // BINARY index
		"SELECT id FROM t WHERE b = 14",      // numeric index
		"SELECT id FROM t WHERE c = 'foo'",   // NOCASE index
		"SELECT id FROM t WHERE d = 'x'",     // RTRIM index
		"SELECT id FROM t WHERE g = 5",       // ASC leading of a multi-column index (non-leading DESC is harmless)
		"SELECT id FROM t WHERE 'apple' = a", // reversed
		"SELECT id FROM t WHERE a = ?",       // parameter key
		"SELECT id FROM t WHERE b = 14 AND a = 'x'",
	}
	for _, sql := range fires {
		d, err := DisassembleScan(p, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if !strings.Contains(d, "SeekIndexHint") {
			t.Errorf("expected SeekIndexHint for %q, got:\n%s", sql, d)
		}
	}

	// These three decline their SQL index -- a b-tree walk would need its
	// collation and direction -- and now seek the segments' own per-column
	// equality index instead (automaticSeekCandidates), which has no order to
	// get wrong; a NOCASE TEXT probe still declines at run time.
	for _, sql := range []string{
		"SELECT id FROM t WHERE e = 'FOO'",
		"SELECT id FROM t WHERE u = 'u1'",
		"SELECT id FROM t WHERE f = 100",
	} {
		if d, err := DisassembleScan(p, sql); err == nil && !strings.Contains(d, "SeekIndexHint") {
			t.Errorf("expected an automatic SeekIndexHint for %q, got:\n%s", sql, d)
		}
	}
	noFire := []string{
		"SELECT id FROM t WHERE id = 3",                 // rowid: rowid-seek path, not index-seek
		"SELECT id FROM t WHERE a > 'm'",                // not equality
		"SELECT id FROM t WHERE a = b",                  // key references a column
		"SELECT id FROM t WHERE a = 'x' OR b = 1",       // under OR
		"SELECT id FROM t WHERE a COLLATE NOCASE = 'x'", // explicit COLLATE on the column operand
	}
	for _, sql := range noFire {
		d, err := DisassembleScan(p, sql)
		if err != nil {
			// Statement declined compilation, so no index seek.
			continue
		}
		if strings.Contains(d, "SeekIndexHint") {
			t.Errorf("did NOT expect SeekIndexHint for %q, got:\n%s", sql, d)
		}
	}
}

// TestIndexSeekAcrossBtreeLevels verifies index-seek correctness on a large
// table with a multi-level index, testing interior-node pruning.
func TestIndexSeekAcrossBtreeLevels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idxbig.sqlite")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE big (id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE INDEX bk ON big(k)`); err != nil {
		t.Fatal(err)
	}
	const n = 6000
	for i := 1; i <= n; i++ {
		// k has ~6 duplicates each (n/1000 buckets), forcing multi-row equality
		// ranges; s pads the row so the index/table span many pages.
		if _, _, err := db.ExecArgs(`INSERT INTO big(id,k,s) VALUES(?,?,?)`,
			[]Value{{Typ: Int, I: int64(i)}, {Typ: Int, I: int64(i % 1000)},
				{Typ: Text, S: []byte(fmt.Sprintf("padding-value-%d", i))}}); err != nil {
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

	// Verify the seek engages.
	d, err := DisassembleScan(p, "SELECT id FROM big WHERE k = 5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d, "SeekIndexHint") {
		t.Fatalf("expected SeekIndexHint for the large-table query, got:\n%s", d)
	}

	run := func(k int64) [][]Value {
		_, rows, err := p.QueryArgs("SELECT id FROM big WHERE k = ?", []Value{{Typ: Int, I: k}})
		if err != nil {
			t.Fatalf("k=%d: %v", k, err)
		}
		return rows
	}
	for _, k := range []int64{0, 1, 5, 250, 499, 500, 999, 1000, -1, 123456} {
		v := run(k)
		// Check expected count based on modulo distribution.
		want := 0
		for i := 1; i <= n; i++ {
			if int64(i%1000) == k {
				want++
			}
		}
		if len(v) != want {
			t.Fatalf("k=%d: got %d rows want %d", k, len(v), want)
		}
	}
}
