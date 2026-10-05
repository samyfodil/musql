package engine_test

import (
	"github.com/samyfodil/musql/engine"

	"path/filepath"
	"testing"
)

// fts5 is opt-in (see engine.RegisterFTS5); enable it for the whole engine test binary.
func init() { engine.RegisterFTS5() }

func newFts5DB(t *testing.T) *engine.Session {
	t.Helper()
	db, err := engine.Create(filepath.Join(t.TempDir(), "fts5.musq"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return db
}

// queryRowids runs a query and returns the first column values as int64s.
func queryRowids(t *testing.T, db snapshotter, q string) []int64 {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, rows, err := p.Query(q)
	if err != nil {
		t.Fatalf("Query %q: %v", q, err)
	}
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r[0].I
	}
	return out
}

func fts5EqInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFts5Tokenizer checks unicode61 tokenization.
func TestFts5Tokenizer(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Hello, World!", []string{"hello", "world"}},
		{"café résumé", []string{"cafe", "resume"}},
		{"DÉJÀ VU", []string{"deja", "vu"}},
		{"one_two-three.four", []string{"one", "two", "three", "four"}},
		{"MixedCASE123", []string{"mixedcase123"}},
		{"   ", nil},
		{"ΓΕΙΑ σου", []string{"γεια", "σου"}},
	}
	for _, c := range cases {
		got := engine.FTS5TokenizeForTest(c.in)
		if len(got) != len(c.want) {
			t.Errorf("tokenize(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("tokenize(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

// TestFts5MatchCore checks MATCH grammar and rowid ordering.
func TestFts5MatchCore(t *testing.T) {
	db := newFts5DB(t)
	mustExec(t, db, "CREATE VIRTUAL TABLE t USING fts5(a, b)")
	mustExec(t, db, "INSERT INTO t(a,b) VALUES('one two three','alpha')") // 1
	mustExec(t, db, "INSERT INTO t(a,b) VALUES('one three two','beta')")  // 2
	mustExec(t, db, "INSERT INTO t(a,b) VALUES('three one two','gamma')") // 3
	mustExec(t, db, "INSERT INTO t(a,b) VALUES('café RESUME','prefixw')") // 4
	mustExec(t, db, "INSERT INTO t(a,b) VALUES('hello world','one two')") // 5

	cases := []struct {
		q    string
		want []int64
	}{
		{"SELECT rowid FROM t WHERE t MATCH 'one two'", []int64{1, 2, 3, 5}},         // implicit AND
		{"SELECT rowid FROM t WHERE t MATCH '\"one two\"'", []int64{1, 3, 5}},        // phrase
		{"SELECT rowid FROM t WHERE t MATCH 'one AND two'", []int64{1, 2, 3, 5}},     // explicit AND
		{"SELECT rowid FROM t WHERE t MATCH 'one OR three'", []int64{1, 2, 3, 5}},    // OR
		{"SELECT rowid FROM t WHERE t MATCH 'one NOT three'", []int64{5}},            // NOT
		{"SELECT rowid FROM t WHERE t MATCH 'a:one'", []int64{1, 2, 3}},              // column filter
		{"SELECT rowid FROM t WHERE t MATCH 'b:one'", []int64{5}},                    // column filter
		{"SELECT rowid FROM t WHERE t MATCH '{a b}:alpha'", []int64{1}},              // column set
		{"SELECT rowid FROM t WHERE t MATCH 'pre*'", []int64{4}},                     // prefix
		{"SELECT rowid FROM t WHERE t MATCH 'cafe'", []int64{4}},                     // diacritic fold
		{"SELECT rowid FROM t WHERE t MATCH 'resume'", []int64{4}},                   // case fold
		{"SELECT rowid FROM t WHERE t MATCH 'one + two'", []int64{1, 3, 5}},          // '+' phrase
		{"SELECT rowid FROM t WHERE a MATCH 'one'", []int64{1, 2, 3}},                // col MATCH
		{"SELECT rowid FROM t WHERE t.a MATCH 'one'", []int64{1, 2, 3}},              // qual col MATCH
		{"SELECT rowid FROM t WHERE t MATCH '(one OR hello) AND world'", []int64{5}}, // parens
		{"SELECT rowid FROM t WHERE t MATCH 'nomatch'", nil},                         // no rows
	}
	for _, c := range cases {
		got := queryRowids(t, db, c.q)
		if !fts5EqInts(got, c.want) {
			t.Errorf("%s => %v, want %v", c.q, got, c.want)
		}
	}
}

// TestFts5StarSelect verifies "SELECT *" returns only the text columns (the
// hidden rowid slot is not exposed), matching C fts5.
func TestFts5StarSelect(t *testing.T) {
	db := newFts5DB(t)
	mustExec(t, db, "CREATE VIRTUAL TABLE t USING fts5(a, b)")
	mustExec(t, db, "INSERT INTO t VALUES('x','y')")
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	cols, rows, err := p.Query("SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 2 || cols[0] != "a" || cols[1] != "b" {
		t.Errorf("SELECT * columns = %v, want [a b]", cols)
	}
	if len(rows) != 1 || string(rows[0][0].S) != "x" || string(rows[0][1].S) != "y" {
		t.Errorf("SELECT * rows = %v", rows)
	}
}

// TestFts5WriteOps exercises INSERT (explicit + implicit + rowid), UPDATE, and
// DELETE through xUpdate.
func TestFts5WriteOps(t *testing.T) {
	db := newFts5DB(t)
	mustExec(t, db, "CREATE VIRTUAL TABLE t USING fts5(a)")
	mustExec(t, db, "INSERT INTO t(rowid, a) VALUES(10, 'apple')")
	mustExec(t, db, "INSERT INTO t VALUES('banana')") // auto rowid 11
	mustExec(t, db, "INSERT INTO t(a) VALUES('cherry')")
	if got := queryRowids(t, db, "SELECT rowid FROM t WHERE t MATCH 'banana'"); !fts5EqInts(got, []int64{11}) {
		t.Errorf("banana rowid = %v, want [11]", got)
	}
	mustExec(t, db, "UPDATE t SET a='blueberry' WHERE rowid=11")
	if got := queryRowids(t, db, "SELECT rowid FROM t WHERE t MATCH 'banana'"); len(got) != 0 {
		t.Errorf("after update, banana should be gone, got %v", got)
	}
	if got := queryRowids(t, db, "SELECT rowid FROM t WHERE t MATCH 'blueberry'"); !fts5EqInts(got, []int64{11}) {
		t.Errorf("blueberry rowid = %v, want [11]", got)
	}
	mustExec(t, db, "DELETE FROM t WHERE t MATCH 'apple'")
	if got := queryRowids(t, db, "SELECT rowid FROM t ORDER BY rowid"); !fts5EqInts(got, []int64{11, 12}) {
		t.Errorf("after delete, rowids = %v, want [11 12]", got)
	}
}

// TestFts5DeclinedOptions locks in that unsupported fts5 options and columns are
// declined at CREATE time (never silently accepted).
func TestFts5DeclinedOptions(t *testing.T) {
	declined := []string{
		// tokenize='porter' used to head this list; it is now PORTED
		// (fts5_tokenizers.go's fts5PorterStem), gated against C fts5 by
		// compat-harness/fts5_r30_porter_test.go.
		"CREATE VIRTUAL TABLE t USING fts5(a, tokenize='nosuchtokenizer')",
		"CREATE VIRTUAL TABLE t USING fts5(a, prefix=0)",
		"CREATE VIRTUAL TABLE t USING fts5(a, prefix='1,')",
		// content=<table> used to head this list too; EXTERNAL content is now
		// IMPLEMENTED (fts5_extcontent.go), gated against C fts5 by
		// compat-harness/fts5_r32p_extcontent_test.go. What is still declined
		// is the CONTENTLESS spelling -- an empty value, which fts5 reads as
		// FTS5_CONTENT_NONE. That one is now IMPLEMENTED, by reading %_data's
		// inverted index back into documents (fts5_contentless.go); what is
		// still declined of it is the shapes that file names, of which these
		// are the ones a CREATE can carry on its own. contentless_delete=1 used
		// to be here too and is now served (the V2 structure record, the
		// %_docsize origin column and the tombstone reader) --
		// compat-harness/fts5_r34w_contentless_delete_test.go is its gate.
		// columnsize=0 with content='' used to be here as well; it is now served
		// (fts5_contentless.go reconstructs the documents from the postings, and
		// fts5ContentlessScanGuard makes the "table does not support scanning"
		// refusal fts5_main.c:1623 makes for it), gated by
		// compat-harness/fts5_r38b_columnsize0_test.go.
		// detail=none with content='' used to be here too. CREATING and
		// WRITING such a table is now served -- nothing about INDEXING needs
		// positions -- and only a READ, which would have to rebuild the
		// documents out of the index, still declines. See
		// TestFts5ContentlessDetailDeclinesOnReadNotCreate below.
		// contentless_unindexed=1 over a table with an UNINDEXED column used to
		// be here as well; it is FTS5_CONTENT_UNINDEXED and is now served
		// (fts5_contentless.go), gated by
		// compat-harness/fts5_r35e_test.go. It remains an error on a table
		// that is not contentless at all, which the next line keeps.
		"CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, contentless_unindexed=1)",
		// ...and content_rowid= without an external content= (C fts5 stores
		// it and then fails every read, because %_content has no such column).
		"CREATE VIRTUAL TABLE t USING fts5(a, content_rowid=x)",
		// detail=none/columns used to head this list too; both are now
		// IMPLEMENTED (fts5_detail.go), gated against C fts5 by
		// compat-harness/fts5_r31_detail_test.go. What is still declined is a
		// value fts5ConfigSetEnum itself refuses: one that prefixes no name,
		// and one that prefixes MORE than one (the empty string).
		"CREATE VIRTUAL TABLE t USING fts5(a, detail=nonex)",
		"CREATE VIRTUAL TABLE t USING fts5(a, detail=)",
		"CREATE VIRTUAL TABLE t USING fts5(a, columnsize=2)",
		"CREATE VIRTUAL TABLE t USING fts5(a, nosuchopt=1)",
		"CREATE VIRTUAL TABLE t USING fts5(a, a)",
		"CREATE VIRTUAL TABLE t USING fts5()",
		// UNINDEXED is supported, but it is the ONLY column option, and it is
		// separated from the name by SPACES only -- C fts5 answers "parse
		// error" for both of these (see fts5ParseColumnSpec).
		"CREATE VIRTUAL TABLE t USING fts5(a, b NOT NULL)",
		"CREATE VIRTUAL TABLE t USING fts5(a\tUNINDEXED, b)",
	}
	for _, sql := range declined {
		db := newFts5DB(t)
		if err := db.Exec(sql); err == nil {
			t.Errorf("%s: expected decline, got nil", sql)
		}
	}
}

// TestFts5Unindexed locks in that an UNINDEXED column is STORED and selectable
// but contributes no tokens, so nothing in it can be MATCHed -- the whole point
// of the modifier, and gated byte-for-byte against C fts5 in
// compat-harness/fts5_options_test.go.
func TestFts5Unindexed(t *testing.T) {
	db := newFts5DB(t)
	mustExec(t, db, "CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED)")
	mustExec(t, db, "INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')")
	mustExec(t, db, "INSERT INTO t(rowid,a,b) VALUES(2,'bar baz','hello')")
	if got := queryRowids(t, db, "SELECT rowid FROM t WHERE t MATCH 'hello'"); !fts5EqInts(got, []int64{1}) {
		t.Errorf("MATCH 'hello' = %v, want [1] (row 2 has it only in the UNINDEXED column)", got)
	}
	if got := queryRowids(t, db, "SELECT rowid FROM t WHERE t MATCH 'foo'"); len(got) != 0 {
		t.Errorf("MATCH 'foo' = %v, want none: it appears only in the UNINDEXED column", got)
	}
	if got := queryRowids(t, db, "SELECT rowid FROM t WHERE t MATCH 'b:bar'"); len(got) != 0 {
		t.Errorf("MATCH 'b:bar' = %v, want none: column b is UNINDEXED", got)
	}
	if got := queryRowids(t, db, "SELECT rowid FROM t WHERE t MATCH 'a:bar'"); !fts5EqInts(got, []int64{2}) {
		t.Errorf("MATCH 'a:bar' = %v, want [2]", got)
	}
}

// TestFts5ColumnsizeZero locks in columnsize=0's one and only effect: the
// %_docsize shadow table is not created, and the table is otherwise an
// ordinary fts5 table. Its BYTES are gated against C fts5 in
// compat-harness/fts5_options_test.go.
func TestFts5ColumnsizeZero(t *testing.T) {
	db := newFts5DB(t)
	mustExec(t, db, "CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=0)")
	mustExec(t, db, "INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')")
	mustExec(t, db, "INSERT INTO t(rowid,a,b) VALUES(2,'goodbye','world')")
	if got := queryRowids(t, db, "SELECT rowid FROM t WHERE t MATCH 'world' ORDER BY rowid"); !fts5EqInts(got, []int64{1, 2}) {
		t.Errorf("MATCH 'world' = %v, want [1 2]", got)
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, qerr := p.Query("SELECT * FROM t_docsize"); qerr == nil {
		t.Error("columnsize=0 left a %_docsize shadow table behind; C fts5 creates none")
	}
	if _, _, qerr := p.Query("SELECT * FROM t_content"); qerr != nil {
		t.Errorf("columnsize=0 must still have %%_content: %v", qerr)
	}
}

// TestFts5PrefixIndexIsQueryNeutral locks in that a prefix= index changes only
// the FILE, never an answer: it is a seek accelerator, and this engine answers
// MATCH by re-tokenizing %_content either way. Its BYTES are gated against real
// fts5 in compat-harness/fts5_options_test.go.
func TestFts5PrefixIndexIsQueryNeutral(t *testing.T) {
	db := newFts5DB(t)
	mustExec(t, db, "CREATE VIRTUAL TABLE t USING fts5(a, b, prefix='2 3')")
	mustExec(t, db, "INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo')")
	mustExec(t, db, "INSERT INTO t(rowid,a,b) VALUES(2,'help','hello')")
	for _, c := range []struct {
		q    string
		want []int64
	}{
		{"SELECT rowid FROM t WHERE t MATCH 'hel*' ORDER BY rowid", []int64{1, 2}},
		{"SELECT rowid FROM t WHERE t MATCH 'he*' ORDER BY rowid", []int64{1, 2}},
		{"SELECT rowid FROM t WHERE t MATCH 'hello' ORDER BY rowid", []int64{1, 2}},
		{"SELECT rowid FROM t WHERE t MATCH 'b:hello' ORDER BY rowid", []int64{2}},
		{`SELECT rowid FROM t WHERE t MATCH '"hello world"' ORDER BY rowid`, []int64{1}},
	} {
		if got := queryRowids(t, db, c.q); !fts5EqInts(got, c.want) {
			t.Errorf("%s = %v, want %v", c.q, got, c.want)
		}
	}
}

// TestFts5DeclinedQueries locks in that NEAR and MATCH on non-fts5 tables are
// declined (an error), never answered wrong.
func TestFts5DeclinedQueries(t *testing.T) {
	db := newFts5DB(t)
	mustExec(t, db, "CREATE VIRTUAL TABLE t USING fts5(a)")
	mustExec(t, db, "INSERT INTO t VALUES('one two')")
	mustExec(t, db, "CREATE TABLE plain(a)")
	mustExec(t, db, "INSERT INTO plain VALUES('one two')")

	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"SELECT rowid FROM plain WHERE plain MATCH 'one'",
		"SELECT rowid FROM plain WHERE a MATCH 'one'",
	} {
		if _, _, err := p.Query(q); err == nil {
			t.Errorf("%s: expected decline error, got nil", q)
		}
	}
}

// TestFts5ContentlessDetailDeclinesOnReadNotCreate pins where the contentless
// detail= restriction lives now.
//
// A contentless table's index IS its only document store, and rebuilding the
// documents out of it relies on detail=full numbering a column's tokens
// 0..szCol-1 with no gaps (fts5_contentless.go). detail=none records no
// positions at all, so the REBUILD would be a guess -- but nothing about
// CREATING or INDEXING needs them, and the oracle accepts both. So the refusal
// moved from CREATE to the read that would have to guess.
//
// fts5misc.test declined on the CREATE alone, which is why this matters: the
// statement after it is an ordinary INSERT.
func TestFts5ContentlessDetailDeclinesOnReadNotCreate(t *testing.T) {
	db := newFts5DB(t)
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t1 USING fts5(a, detail='none', content='')`,
		`INSERT INTO t1(a) VALUES('a b c')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v (the oracle accepts this)", s, err)
		}
	}
	// detail=full stays fully served, create through read.
	db2 := newFts5DB(t)
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t2 USING fts5(a, content='')`,
		`INSERT INTO t2(a) VALUES('a b c')`,
	} {
		if err := db2.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// TestFts5ExternalContentMissingStillAnswersAMatch gates fts5ExtIndexOnlyMatch
// (fts5_extcontent.go): C fts5 answers a MATCH from the index and reads the
// content table only to fetch a matching row's column, so a content source that
// cannot be read is an error for a SCAN and not for a MATCH that selects
// nothing. Probed against the 3.53.3 oracle with a "content=t1" table whose t1
// does not exist -- "SELECT rowid FROM t2 WHERE t2 MATCH 'deh'" answers zero
// rows, "SELECT * FROM t2" fails with "no such table: main.t1".
func TestFts5ExternalContentMissingStillAnswersAMatch(t *testing.T) {
	db := newFts5DB(t)
	defer db.Close()
	if err := db.Exec(`CREATE VIRTUAL TABLE t2 USING fts5(content="t1", c)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	for _, q := range []string{
		`SELECT rowid FROM t2 WHERE t2 MATCH 'deh'`,
		`SELECT rowid, c FROM t2 WHERE t2 MATCH 'deh'`,
		`SELECT rowid FROM t2('deh')`,
	} {
		_, rows, err := p.Query(q)
		if err != nil || len(rows) != 0 {
			t.Errorf("%s: rows=%d err=%v; want 0 rows and no error", q, len(rows), err)
		}
	}
	// ...and every read whose row source IS the content table still fails, as
	// it does in C fts5. The OR form is deliberate: under an OR the MATCH is
	// not a constraint xBestIndex can take, so C scans the content table too.
	for _, q := range []string{
		`SELECT * FROM t2`,
		`SELECT rowid FROM t2`,
		`SELECT rowid FROM t2 WHERE t2 MATCH 'deh' OR rowid=1`,
	} {
		if _, _, err := p.Query(q); err == nil {
			t.Errorf("%s: answered; want the missing-content-table error", q)
		}
	}
}
