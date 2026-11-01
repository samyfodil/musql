package engine

import (
	"errors"
	"fmt"
	"iter"
	"math"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// integrityPrep is integrityRowFindings's own preparation of a rowid table.
func integrityPrep(t *testing.T, p *ReadOnlyPager, name string) (*SchemaRow, *tableMeta, []*indexMeta, []checkConstraint) {
	t.Helper()
	rows, err := p.Schema()
	if err != nil {
		t.Fatal(err)
	}
	var tr *SchemaRow
	for i := range rows {
		if rows[i].Type == "table" && rows[i].Name == name {
			tr = &rows[i]
		}
	}
	if tr == nil {
		t.Fatalf("no table %s", name)
	}
	cols, checks, specs, err := parseCreateTableColumnsAndAutoIndexes(tr.SQL, false)
	if err != nil {
		t.Fatal(err)
	}
	ipk := -1
	for i, c := range cols {
		if c.IsRowidAlias {
			ipk = i
			break
		}
	}
	tm := &tableMeta{name: tr.Name, sql: tr.SQL, cols: cols, ipkIndex: ipk, rootPage: tr.RootPage}
	return tr, tm, p.integrityIndexesOf(tr, rows, tm, cols, specs), checks
}

type integrityAnswer struct {
	counts, out []string
	n           int
	err         error
}

func (a integrityAnswer) String() string {
	return fmt.Sprintf("counts=%q n=%d out=%q err=%v", a.counts, a.n, a.out, a.err)
}

func equalAnswers(a, b integrityAnswer) bool {
	return slices.Equal(a.counts, b.counts) && slices.Equal(a.out, b.out) && a.n == b.n && (a.err == nil) == (b.err == nil)
}

// bothWays runs the streamed and the in-memory per-row check on one table.
func bothWays(t *testing.T, p *ReadOnlyPager, name string, isQuick bool, limit int) (streamed, inMemory integrityAnswer) {
	t.Helper()
	tr, tm, indexes, checks := integrityPrep(t, p, name)
	var s, m integrityAnswer
	s.counts, s.out, s.n, s.err = p.integrityRowFindingsStreamed(tr, tm, indexes, checks, false, isQuick, limit)
	m.counts, m.out, m.n, m.err = p.integrityRowFindingsInMemory(tr, tm, indexes, checks, false, isQuick, limit)
	return s, m
}

// setTinyStreamBudget makes every sort in the streamed check spill.
func setTinyStreamBudget(t *testing.T) {
	old := integrityStreamBudgetVar
	integrityStreamBudgetVar = 1
	t.Cleanup(func() { integrityStreamBudgetVar = old })
}

func execAll(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// integrityFixture is a table that breaks every per-row check.
func integrityFixture(t *testing.T, n int) string {
	path := filepath.Join(t.TempDir(), "fx.musq")
	execAll(t, path,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c INT, d TEXT COLLATE NOCASE, e REAL, f INT)`,
		`CREATE INDEX t_b ON t(b)`,
		`CREATE INDEX t_cd ON t(c DESC, d)`,
		`CREATE INDEX t_e ON t(e, b COLLATE RTRIM)`,
		fmt.Sprintf(`INSERT INTO t(b,c,d,e,f) SELECT
			CASE WHEN i%%13=0 THEN NULL ELSE 'b'||(i%%97)||substr('    ',1,i%%3) END,
			CASE WHEN i%%11=0 THEN NULL ELSE i%%(%d) END,
			CASE i%%3 WHEN 0 THEN 'Ab'||(i%%41) WHEN 1 THEN 'aB'||(i%%41) ELSE NULL END,
			CASE WHEN i%%5=0 THEN NULL ELSE (i%%7)*1.5 END,
			i%%9
			FROM (WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<%d) SELECT i FROM s)`, n/2+1, n),
		`DELETE FROM t WHERE a%17=0`,
		`CREATE TABLE w(k TEXT PRIMARY KEY, v INT) WITHOUT ROWID`,
		`INSERT INTO w SELECT 'k'||value, value FROM generate_series(1, 300)`,
		`CREATE TABLE s(rowid TEXT, x INT)`,
		`INSERT INTO s SELECT 'r'||value, value%7 - 2 FROM generate_series(1, 400)`,
		`CREATE TABLE z(rowid, _rowid_, oid, x INT)`,
		`INSERT INTO z SELECT value, value, value, value%5 - 1 FROM generate_series(1, 200)`,
		`PRAGMA writable_schema=ON`,
		`INSERT INTO sqlite_master VALUES('index','u_c','t',0,'CREATE UNIQUE INDEX u_c ON t(c)')`,
		`INSERT INTO sqlite_master VALUES('index','u_d','t',0,'CREATE UNIQUE INDEX u_d ON t(d)')`,
		`INSERT INTO sqlite_master VALUES('index','u_bc','t',0,'CREATE UNIQUE INDEX u_bc ON t(b, c DESC)')`,
		`UPDATE sqlite_master SET sql='CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT NOT NULL, c INT, d TEXT COLLATE NOCASE, e REAL, f TEXT, CHECK (c >= 3), CHECK (length(b) < 4))' WHERE name='t'`,
		`UPDATE sqlite_master SET sql='CREATE TABLE w(k TEXT PRIMARY KEY, v INT CHECK (v % 3 <> 0)) WITHOUT ROWID' WHERE name='w'`,
		`UPDATE sqlite_master SET sql='CREATE TABLE s(rowid TEXT, x INT CHECK (x > 0))' WHERE name='s'`,
		`UPDATE sqlite_master SET sql='CREATE TABLE z(rowid, _rowid_, oid, x INT CHECK (x > 0))' WHERE name='z'`,
	)
	return path
}

var integrityLimits = []int{1, 2, 7, 100, 1 << 30}

// TestIntegrityStreamedMatchesInMemory: the streamed check answers as the
// in-memory one, at several caps, with every sort spilling.
func TestIntegrityStreamedMatchesInMemory(t *testing.T) {
	setTinyStreamBudget(t)
	p, err := Open(integrityFixture(t, 3000))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, table := range []string{"t", "s"} {
		for _, quick := range []bool{false, true} {
			for _, limit := range integrityLimits {
				s, m := bothWays(t, p, table, quick, limit)
				if m.err != nil {
					t.Fatalf("%s quick=%v limit=%d: in-memory: %v", table, quick, limit, m.err)
				}
				if !equalAnswers(s, m) {
					t.Fatalf("%s quick=%v limit=%d:\nstreamed  %v\nin-memory %v", table, quick, limit, s, m)
				}
			}
		}
	}
	// The check found what the fixture planted.
	s, _ := bothWays(t, p, "t", false, 1<<30)
	for _, want := range []string{"non-unique entry in index u_c", "non-unique entry in index u_d", "NULL value in t.b", "CHECK constraint failed in t", "NUMERIC value in t.f"} {
		if !slices.Contains(s.out, want) {
			t.Errorf("no %q among %d findings", want, s.n)
		}
	}
}

// TestIntegrityStreamedPragmaAnswers runs the whole pragma and holds the
// tables the streamed form hands back (z: every rowid name taken) and the
// count-shaped ones (w) to what they answered before.
func TestIntegrityStreamedPragmaAnswers(t *testing.T) {
	setTinyStreamBudget(t)
	p, err := Open(integrityFixture(t, 600))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	tr, tm, indexes, checks := integrityPrep(t, p, "z")
	if _, _, _, err := p.integrityRowFindingsStreamed(tr, tm, indexes, checks, false, false, 100); !errors.Is(err, errIntegrityInMemory) {
		t.Fatalf("z: streamed = %v, want the in-memory hand-back", err)
	}
	for _, q := range []string{`PRAGMA integrity_check(5000)`, `PRAGMA quick_check(5000)`, `PRAGMA integrity_check(z)`, `PRAGMA integrity_check(w)`, `PRAGMA integrity_check(3)`} {
		_, got, err := p.Query(q)
		if err != nil && !errors.Is(err, errVDBEUnsupported) {
			t.Fatalf("%s: %v", q, err)
		}
		t.Logf("%s: %d rows, err=%v", q, len(got), err)
	}
	_, got, err := p.Query(`PRAGMA integrity_check(w)`)
	if err != nil || len(got) != 100 || string(got[0][0].S) != "CHECK constraint failed in w" {
		t.Fatalf("integrity_check(w) = %d rows, %v; want 100 CHECK findings", len(got), err)
	}
}

// storedCorruption is one way to damage a stored copy of an index.
type storedCorruption func(r *rand.Rand, recs [][]Value) [][]Value

var storedCorruptions = map[string]storedCorruption{
	"exact": func(_ *rand.Rand, recs [][]Value) [][]Value { return recs },
	"drop": func(r *rand.Rand, recs [][]Value) [][]Value {
		var out [][]Value
		for _, rec := range recs {
			if r.IntN(9) != 0 {
				out = append(out, rec)
			}
		}
		return out
	},
	"dup": func(r *rand.Rand, recs [][]Value) [][]Value {
		var out [][]Value
		for _, rec := range recs {
			out = append(out, rec)
			if r.IntN(15) == 0 {
				out = append(out, rec)
			}
		}
		return out
	},
	"swap": func(r *rand.Rand, recs [][]Value) [][]Value {
		out := slices.Clone(recs)
		for range len(out)/20 + 1 {
			if len(out) < 2 {
				break
			}
			i := r.IntN(len(out) - 1)
			out[i], out[i+1] = out[i+1], out[i]
		}
		return out
	},
	"shuffle": func(r *rand.Rand, recs [][]Value) [][]Value {
		out := slices.Clone(recs)
		r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	},
	"reverse": func(_ *rand.Rand, recs [][]Value) [][]Value {
		out := slices.Clone(recs)
		slices.Reverse(out)
		return out
	},
	"mutate": func(r *rand.Rand, recs [][]Value) [][]Value {
		out := make([][]Value, len(recs))
		for i, rec := range recs {
			rec = slices.Clone(rec)
			switch r.IntN(12) {
			case 0:
				// Another case of a NOCASE key: equal under the index, but
				// not to the row.
				for k := range rec {
					if rec[k].Typ == Text {
						rec[k].S = []byte(strings.ToUpper(string(rec[k].S)))
						break
					}
				}
			case 1:
				rec[len(rec)-1].I += 100000 // a rowid the table does not have
			case 2:
				rec = rec[:len(rec)-1] // a short entry
			}
			out[i] = rec
		}
		return out
	},
}

// TestIntegrityStoredCopyMatchesInMemory holds a stored copy of each index,
// damaged every way above, against the rows, both ways.
func TestIntegrityStoredCopyMatchesInMemory(t *testing.T) {
	setTinyStreamBudget(t)
	p, err := Open(integrityFixture(t, 2000))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, tm, indexes, _ := integrityPrep(t, p, "t")
	rows, _, err := readTableRowsWithin(p, tm, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	enc := p.encoding()
	derived := map[string][][]Value{}
	for _, idx := range indexes {
		var recs [][]Value
		for rowid, vals := range rows {
			recs = append(recs, indexEntryForNewRow(tm, idx, rowid, vals))
		}
		sort.SliceStable(recs, func(a, b int) bool {
			return compareIndexRec(recs[a], recs[b], idx.colCollation, idx.colDesc, enc) < 0
		})
		derived[idx.name] = recs
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for k := range storedCorruptions {
			if !yield(k) {
				return
			}
		}
	})
	for _, seed := range []uint64{0, 1, 3} {
		for _, how := range names {
			r := rand.New(rand.NewPCG(seed, 99))
			stored := map[string][][]Value{}
			failing := ""
			for i, idx := range indexes {
				stored[idx.name] = storedCorruptions[how](r, derived[idx.name])
				if seed == 3 && i == 1 {
					failing = idx.name // a copy whose walk fails: derived instead
				}
			}
			p.storedIndexEntries = func(name string) (iter.Seq[[]Value], func() error, bool) {
				recs, ok := stored[name]
				if !ok {
					return nil, nil, false
				}
				var werr error
				return func(yield func([]Value) bool) {
						for _, rec := range recs {
							if !yield(slices.Clone(rec)) {
								return
							}
						}
						if name == failing {
							werr = errors.New("walk failed")
						}
					}, func() error {
						return werr
					}, true
			}
			for _, quick := range []bool{false, true} {
				for _, limit := range []int{1, 7, 1 << 30} {
					s, m := bothWays(t, p, "t", quick, limit)
					if m.err != nil {
						t.Fatalf("%s/%d quick=%v limit=%d: in-memory: %v", how, seed, quick, limit, m.err)
					}
					if !equalAnswers(s, m) {
						t.Fatalf("%s/%d quick=%v limit=%d:\nstreamed  %v\nin-memory %v", how, seed, quick, limit, s, m)
					}
				}
			}
			if how != "exact" && seed == 0 {
				s, _ := bothWays(t, p, "t", false, 1<<30)
				t.Logf("%s: %d findings, %d count findings", how, s.n, len(s.counts))
			}
		}
	}
	p.storedIndexEntries = nil
}

// TestIntegrityHandsBackForgedRows: rowids out of order and a NaN key, which
// only a foreign file holds, are answered in memory and declined past its
// budget.
func TestIntegrityHandsBackForgedRows(t *testing.T) {
	setTinyStreamBudget(t)
	for _, shape := range []string{"order", "nan"} {
		path := filepath.Join(t.TempDir(), shape+".musq")
		b, err := NewBuilder(path)
		if err != nil {
			t.Fatal(err)
		}
		tb, err := b.Table("t", `CREATE TABLE t(a INTEGER PRIMARY KEY, x REAL, y INT)`, 1)
		if err != nil {
			t.Fatal(err)
		}
		for i := int64(1); i <= 400; i++ {
			rowid := i
			x := Value{Typ: Float, F: float64(i % 23)}
			if shape == "order" && i%50 == 0 {
				rowid = i - 30
			}
			if shape == "nan" && i%40 == 0 {
				x.F = math.NaN()
			}
			if err := tb.Add(rowid, []Value{{Typ: Null}, x, {Typ: Int, I: i % 31}}); err != nil {
				t.Fatal(err)
			}
		}
		b.Object("index", "u_x", "t", `CREATE UNIQUE INDEX u_x ON t(x)`, 2)
		b.Object("index", "i_y", "t", `CREATE INDEX i_y ON t(y)`, 3)
		if err := b.Finish(); err != nil {
			t.Fatal(err)
		}
		p, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		tr, tm, indexes, checks := integrityPrep(t, p, "t")
		if _, _, _, err := p.integrityRowFindingsStreamed(tr, tm, indexes, checks, false, false, 100); !errors.Is(err, errIntegrityInMemory) {
			t.Fatalf("%s: streamed = %v, want the in-memory hand-back", shape, err)
		}
		rows, _ := p.Schema()
		var got integrityAnswer
		got.counts, got.out, got.n, got.err = p.integrityRowFindings(tr, rows, false, false, 100)
		if got.err != nil || got.n == 0 {
			t.Fatalf("%s: %v", shape, got)
		}
		// With a NaN the in-memory answer itself turns on the map order its
		// sort starts from; only the out-of-order rows have one answer.
		if _, m := bothWays(t, p, "t", false, 100); shape == "order" && !equalAnswers(got, m) {
			t.Fatalf("%s:\ngot       %v\nin-memory %v", shape, got, m)
		}
		old := integrityInMemoryBudgetVar
		integrityInMemoryBudgetVar = 1000
		_, _, _, err = p.integrityRowFindings(tr, rows, false, false, 100)
		integrityInMemoryBudgetVar = old
		if !errors.Is(err, errVDBEUnsupported) {
			t.Fatalf("%s: past the budget = %v, want a decline", shape, err)
		}
		p.Close()
	}
}

// TestIntegritySearchIsSortSearch holds integritySearch to sort.Search over
// sequences in and out of order, keys ascending.
func TestIntegritySearchIsSortSearch(t *testing.T) {
	cmp := func(a, b []Value) int { return compareIndexRec(a, b, nil, nil, 1) }
	r := rand.New(rand.NewPCG(5, 6))
	for trial := range 300 {
		n := r.IntN(60)
		seq := make([][]Value, n)
		for i := range seq {
			seq[i] = []Value{{Typ: Int, I: int64(r.IntN(40))}}
		}
		if trial%3 == 0 {
			slices.SortFunc(seq, cmp)
		}
		keys := make([][]Value, r.IntN(80))
		for i := range keys {
			keys[i] = []Value{{Typ: Int, I: int64(r.IntN(44) - 2)}}
		}
		slices.SortFunc(keys, cmp)
		reads := 0
		s := &integritySearch{n: n, cmp: cmp, probe: func(h int) ([]Value, error) { reads++; return seq[h], nil }}
		for _, k := range keys {
			want := sort.Search(n, func(i int) bool { return cmp(seq[i], k) >= 0 })
			got, _ := s.search(k)
			if got != want {
				t.Fatalf("trial %d: search(%d) = %d, want %d over %v", trial, k[0].I, got, want, seq)
			}
		}
		if reads > n {
			t.Fatalf("trial %d: %d reads over %d entries", trial, reads, n)
		}
	}
}

// TestIntegrityValuesRoundTrip is the scratch encoding keeping every field.
func TestIntegrityValuesRoundTrip(t *testing.T) {
	in := []Value{
		{Typ: Null},
		{Typ: Int, I: -1 << 63},
		{Typ: Float, F: math.Inf(-1)},
		{Typ: Float, F: 3, intReal: true},
		{Typ: Text, S: []byte("héllo"), Subtype: 74},
		{Typ: Text, S: []byte{}},
		{Typ: Blob, S: []byte{0, 1, 2}},
		{Typ: Blob},
	}
	out, err := decodeIntegrityValues(appendIntegrityValues(nil, in))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("%d values back, want %d", len(out), len(in))
	}
	for i := range in {
		a, b := in[i], out[i]
		if a.Typ != b.Typ || a.Subtype != b.Subtype || a.intReal != b.intReal || a.I != b.I || a.F != b.F ||
			string(a.S) != string(b.S) || (a.S == nil) != (b.S == nil) {
			t.Fatalf("value %d: %+v back as %+v", i, a, b)
		}
	}
}
