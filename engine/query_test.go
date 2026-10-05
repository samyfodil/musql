package engine_test

import (
	"github.com/samyfodil/musql/engine"

	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// buildQueryTestDB creates a test database with various data types and storage classes.
func buildQueryTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "query.sqlite")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	exec(`CREATE TABLE items (
		id INTEGER PRIMARY KEY,
		name TEXT,
		price REAL,
		qty INTEGER,
		descr TEXT,
		data BLOB,
		tag TEXT,
		mixed
	)`)

	type row struct {
		id    int64
		name  any
		price any
		qty   any
		descr any
		data  any
		tag   any
		mixed any
	}
	rows := []row{
		{1, "Widget", 9.99, int64(10), "a widget", []byte{0x01, 0x02}, "red", int64(1)},
		{2, "Gadget", 19.5, int64(-5), "", []byte{}, nil, 3.5},
		{3, "Sprocket", nil, int64(1000000), "unicode: 日本語 café", []byte{0xde, 0xad, 0xbe, 0xef}, "blue", "hello"},
		{4, "Widget", 9.99, int64(0), "dup name test", nil, "red", nil},
		{5, nil, -3.14, nil, "null name", []byte{0xff}, "green", []byte{0x10, 0x20}},
		{6, "apple", 100.0, int64(42), "fruit", []byte{0x00}, "RED", int64(-7)},
		{7, "Banana", 2.5, int64(-32768), "b_underscore%percent", []byte{0xaa, 0xbb, 0xcc}, nil, 2.0},
		{8, "Cherry", 1234567.891, int64(math.MaxInt64), "max int64", []byte{0x01}, "blue", "42"},
		{9, "Date", 0.0, int64(math.MinInt64), "min int64", []byte{0x02}, "green", nil},
		{10, "Eggplant", 3.0, int64(7), "e", []byte{0x03, 0x04}, "Red", int64(99)},
	}
	st, err := db.Prepare(`INSERT INTO items (id,name,price,qty,descr,data,tag,mixed)
		VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := st.Exec(r.id, r.name, r.price, r.qty, r.descr, r.data, r.tag, r.mixed); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	exec(`CREATE TABLE plain_items (label TEXT, amount REAL)`)
	st, err = db.Prepare(`INSERT INTO plain_items (rowid, label, amount) VALUES (?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	type prow struct {
		rowid  int64
		label  any
		amount any
	}
	for _, r := range []prow{
		{100, "p1", 1.1},
		{105, "p2", 2.2},
		{90, nil, nil},
		{200, "p4", -5.5},
	} {
		if _, err := st.Exec(r.rowid, r.label, r.amount); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	return path, db
}

// refRowsFor runs sqlText through the reference database/sql path and
// returns its column names and values, normalized via refValue (defined in
// btree_test.go) into this package's engine.Value type.
func refRowsFor(t *testing.T, db *sql.DB, sqlText string) (cols []string, rows [][]engine.Value) {
	t.Helper()
	rr, err := db.Query(sqlText)
	if err != nil {
		t.Fatalf("reference query %q: %v", sqlText, err)
	}
	defer rr.Close()
	cols, err = rr.Columns()
	if err != nil {
		t.Fatal(err)
	}
	for rr.Next() {
		dest := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err := rr.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		vals := make([]engine.Value, len(cols))
		for i, d := range dest {
			vals[i] = refValue(d)
		}
		rows = append(rows, vals)
	}
	if err := rr.Err(); err != nil {
		t.Fatal(err)
	}
	return cols, rows
}

// mustMatch runs sqlText through both the pure-Go engine and the reference
// database/sql path and fails the test if they disagree on column names or
// row values (allowing the documented int/real storage-optimization slack).
func mustMatch(t *testing.T, p *engine.ReadOnlyPager, db *sql.DB, sqlText string) {
	t.Helper()
	gotCols, gotRows, err := p.Query(sqlText)
	if err != nil {
		t.Fatalf("engine Query(%q): %v", sqlText, err)
	}
	wantCols, wantRows := refRowsFor(t, db, sqlText)

	if len(gotCols) != len(wantCols) {
		t.Fatalf("%q: got %d columns %v, want %d columns %v", sqlText, len(gotCols), gotCols, len(wantCols), wantCols)
	}
	for i := range gotCols {
		if gotCols[i] != wantCols[i] {
			t.Errorf("%q: column %d name = %q, want %q", sqlText, i, gotCols[i], wantCols[i])
		}
	}
	if len(gotRows) != len(wantRows) {
		t.Fatalf("%q: got %d rows, want %d", sqlText, len(gotRows), len(wantRows))
	}
	for r := range gotRows {
		if len(gotRows[r]) != len(wantRows[r]) {
			t.Fatalf("%q: row %d: got %d values, want %d", sqlText, r, len(gotRows[r]), len(wantRows[r]))
		}
		for c := range gotRows[r] {
			if !valuesEqualAllowingRealStorageOptimization(gotRows[r][c], wantRows[r][c]) {
				t.Errorf("%q: row %d col %d (%s): got %+v, want %+v",
					sqlText, r, c, gotCols[c], gotRows[r][c], wantRows[r][c])
			}
		}
	}
}

// mustError asserts that the pure-Go engine rejects sqlText (parse error or
// evaluation error -- either is acceptable per the package's documented
// scope); reason documents why the query is out of scope.
func mustError(t *testing.T, p *engine.ReadOnlyPager, sqlText, reason string) {
	t.Helper()
	_, _, err := p.Query(sqlText)
	if err == nil {
		t.Errorf("%q: expected an error (%s), got none", sqlText, reason)
	}
}

func TestQueryMatchesReference(t *testing.T) {
	path, db := buildQueryTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		// SELECT *, plain projections, aliases.
		`SELECT * FROM items`,
		`SELECT * FROM plain_items`,
		`SELECT id, name, tag FROM items`,
		`SELECT id AS identifier, name AS nm FROM items`,
		`SELECT id, name nm FROM items`, // alias without AS
		`SELECT price * 2 FROM items`,   // unaliased computed expr: verbatim source text as name
		`SELECT price*2 FROM items`,     // same, different spacing -> different verbatim name

		// WHERE: comparisons, AND/OR, IS NULL/NOT NULL, ISNULL/NOTNULL.
		`SELECT * FROM items WHERE id = 3`,
		`SELECT * FROM items WHERE qty > 0`,
		`SELECT * FROM items WHERE qty >= 0`,
		`SELECT * FROM items WHERE price < 10.0`,
		`SELECT * FROM items WHERE price <= 9.99`,
		`SELECT * FROM items WHERE id != 3`,
		`SELECT * FROM items WHERE id <> 3`,
		`SELECT * FROM items WHERE tag = 'red' AND qty > 0`,
		`SELECT * FROM items WHERE tag = 'red' OR tag = 'blue'`,
		`SELECT * FROM items WHERE price IS NULL`,
		`SELECT * FROM items WHERE price IS NOT NULL`,
		`SELECT * FROM items WHERE name IS NULL`,
		`SELECT * FROM items WHERE qty ISNULL`,
		`SELECT * FROM items WHERE qty NOTNULL`,
		`SELECT * FROM items WHERE qty NOT NULL`,  // postfix "NOT NULL", equivalent to NOTNULL/IS NOT NULL
		`SELECT * FROM items WHERE name NOT NULL`, // same, false case (name has a NULL row)
		`SELECT * FROM items WHERE tag IS NULL AND price IS NULL`,

		// NOT INDEXED: a query-planner-only hint, ignored by this engine's
		// always-full-scan executor; results (including row order, since no
		// index exists here to make C SQLite's own plan diverge from a
		// sequential scan) must be identical to the same query without the
		// hint. (INDEXED BY <name> is covered separately, in
		// TestIndexedByHintMatchesReference, against a dedicated fixture that
		// actually has the named index -- adding one here would change real
		// SQLite's own query plan/row order for every other ORDER-BY-less
		// query above.)
		`SELECT * FROM items NOT INDEXED WHERE qty > 0`,
		`SELECT * FROM items AS it NOT INDEXED WHERE it.qty > 0`,
		`SELECT * FROM items WHERE NOT (qty > 0)`,
		`SELECT * FROM items WHERE NOT tag = 'red'`,
		`SELECT * FROM items WHERE id = id`, // trivially true incl. for the row where other cols are NULL

		// Affinity: numeric-affinity column vs TEXT literal, and vice versa.
		`SELECT * FROM items WHERE qty = '10'`,
		`SELECT * FROM items WHERE id = '3'`,
		`SELECT * FROM items WHERE price = '9.99'`,
		`SELECT * FROM items WHERE name = 6`,
		`SELECT * FROM items WHERE tag = 42`,

		// BETWEEN / IN / LIKE.
		`SELECT * FROM items WHERE qty BETWEEN 0 AND 100`,
		`SELECT * FROM items WHERE qty NOT BETWEEN 0 AND 100`,
		`SELECT * FROM items WHERE tag IN ('red', 'blue')`,
		`SELECT * FROM items WHERE tag NOT IN ('red', 'blue')`,
		`SELECT * FROM items WHERE id IN (1, 3, 5, 100)`,
		`SELECT * FROM items WHERE name LIKE 'a%'`,
		`SELECT * FROM items WHERE name LIKE '_a%'`,
		`SELECT * FROM items WHERE tag LIKE 'RED'`, // ASCII case-insensitive match
		`SELECT * FROM items WHERE descr LIKE '%underscore%percent%'`,
		`SELECT * FROM items WHERE name NOT LIKE 'a%'`,

		// Arithmetic + concatenation (WHERE qty IS NOT NULL avoids NULL rows
		// so the arithmetic itself, not NULL propagation, is being checked).
		`SELECT id, qty + 1, qty - 1, qty * 2 FROM items WHERE qty IS NOT NULL`,
		`SELECT id, qty / 2, qty % 3 FROM items WHERE qty IS NOT NULL`,
		`SELECT id, price + 1.5, price - 1.5 FROM items WHERE price IS NOT NULL`,
		`SELECT id, -qty, +qty FROM items WHERE qty IS NOT NULL`,
		`SELECT id, name || '-' || tag FROM items`,
		`SELECT id, name || qty FROM items WHERE name IS NOT NULL AND qty IS NOT NULL`,
		// Constant-expression arithmetic: our grammar requires FROM, so these
		// select from a real table and use LIMIT 1 to pin down a single row.
		`SELECT 1 + 2, 7 / 2, 7 % 2, 7.0 / 2 FROM items LIMIT 1`,
		`SELECT 1.5 || 'x' FROM items LIMIT 1`,
		`SELECT 5 / 0, 5 % 0, 5.0 / 0 FROM items LIMIT 1`,

		// Functions.
		// abs(-9223372036854775808) is excluded: C SQLite itself raises
		// "integer overflow" for it (abs() cannot represent -MinInt64 as an
		// Int64), so it can't be part of a must-match comparison; this
		// package's abs() instead promotes that one case to REAL (documented
		//) rather than erroring.
		`SELECT id, abs(qty) FROM items WHERE qty IS NOT NULL AND qty != -9223372036854775808`,
		`SELECT id, length(descr) FROM items`,
		`SELECT id, lower(name), upper(name) FROM items`,
		`SELECT id, substr(name, 1, 3) FROM items WHERE name IS NOT NULL`,
		`SELECT id, substr(name, 2) FROM items WHERE name IS NOT NULL`,
		`SELECT id, substr(name, -3) FROM items WHERE name IS NOT NULL`,
		`SELECT id, substr(name, -3, 2) FROM items WHERE name IS NOT NULL`,
		`SELECT id, substr(name, 0, 2) FROM items WHERE name IS NOT NULL`,
		`SELECT id, coalesce(tag, 'none') FROM items`,
		`SELECT id, coalesce(price, qty, 0) FROM items`,
		`SELECT id, ifnull(price, 0) FROM items`,
		`SELECT id, nullif(qty, 0) FROM items WHERE qty IS NOT NULL`,
		`SELECT id, typeof(price), typeof(name), typeof(data), typeof(qty) FROM items`,
		`SELECT id, hex(data) FROM items`,
		`SELECT id, hex(name) FROM items WHERE name IS NOT NULL`,
		`SELECT id, hex(qty) FROM items WHERE qty IS NOT NULL`,

		// CAST.
		`SELECT id, CAST(qty AS REAL) FROM items WHERE qty IS NOT NULL`,
		`SELECT id, CAST(price AS INTEGER) FROM items WHERE price IS NOT NULL`,
		`SELECT id, CAST(name AS BLOB) FROM items WHERE name IS NOT NULL`,
		`SELECT id, CAST(qty AS TEXT) FROM items WHERE qty IS NOT NULL`,
		`SELECT id, CAST(name AS NUMERIC) FROM items WHERE name IS NOT NULL`,
		`SELECT CAST('123abc' AS INTEGER) FROM items LIMIT 1`,
		`SELECT CAST('  42  ' AS INTEGER) FROM items LIMIT 1`,
		`SELECT CAST('3.5abc' AS REAL) FROM items LIMIT 1`,
		`SELECT CAST('abc' AS INTEGER) FROM items LIMIT 1`,
		`SELECT CAST(data AS TEXT) FROM items WHERE data IS NOT NULL`,

		// ORDER BY (with a tiebreaker where the primary key has duplicates).
		`SELECT id, price FROM items ORDER BY price ASC`,
		`SELECT id, price FROM items ORDER BY price DESC`,
		`SELECT id, name FROM items ORDER BY name ASC, id ASC`,
		`SELECT id, name FROM items ORDER BY name DESC, id ASC`,
		`SELECT id, tag FROM items ORDER BY tag ASC, id ASC`,
		`SELECT id, qty FROM items ORDER BY qty DESC, id ASC`,
		`SELECT id, mixed FROM items ORDER BY mixed ASC`,
		`SELECT id, mixed FROM items ORDER BY mixed DESC`,
		`SELECT id FROM items ORDER BY tag ASC, price DESC, id ASC`,
		`SELECT label, amount FROM plain_items ORDER BY amount ASC`,

		// ORDER BY on a whole-table (no-GROUP-BY) aggregate: exactly one result
		// row, so the ORDER BY is a semantic no-op -- validated but not applied
		// (sql_agg.go's queryAggregate). The term may reference a bare source
		// column not in the select list, an ordinal, or a select-list alias.
		`SELECT COUNT(*) FROM items ORDER BY name`,
		`SELECT MAX(price) FROM items ORDER BY qty`,
		`SELECT MAX(price) FROM items ORDER BY 1`,
		`SELECT COUNT(qty) AS s FROM items ORDER BY s`,
		`SELECT COUNT(*) FROM items ORDER BY price DESC, id ASC`,

		// LIMIT / OFFSET.
		`SELECT id FROM items ORDER BY id LIMIT 3`,
		`SELECT id FROM items ORDER BY id LIMIT 3 OFFSET 2`,
		`SELECT id FROM items ORDER BY id LIMIT 100`,
		`SELECT id FROM items ORDER BY id LIMIT 0`,
		`SELECT id FROM items ORDER BY id LIMIT -1`,
		`SELECT id FROM items ORDER BY id LIMIT 5 OFFSET 1000`,
		`SELECT id FROM items ORDER BY id LIMIT 2, 3`, // "offset, count" alt syntax

		// rowid/oid/_rowid_ pseudo-column (resolveColumn):
		// plain_items has no INTEGER PRIMARY KEY, so rowid/oid/_rowid_ here
		// are genuinely pseudo (not aliasing any declared column) with
		// explicit, non-contiguous values (90, 100, 105, 200 -- see
		// buildQueryTestDB); items' "id" IS the INTEGER PRIMARY KEY, so its
		// rowid must equal id everywhere.
		`SELECT rowid, label, amount FROM plain_items`,
		`SELECT oid, label FROM plain_items`,
		`SELECT _rowid_, label FROM plain_items`,
		`SELECT label FROM plain_items WHERE rowid = 105`,
		`SELECT label FROM plain_items WHERE _rowid_ >= 100`,
		`SELECT label FROM plain_items ORDER BY rowid DESC`,
		`SELECT max(rowid), min(rowid), count(rowid) FROM plain_items`,
		`SELECT rowid, count(*) FROM plain_items GROUP BY rowid`,
		`SELECT rowid, id FROM items`, // INTEGER PRIMARY KEY aliasing: rowid == id
		`SELECT id FROM items WHERE rowid = 5`,
		`SELECT id FROM items ORDER BY rowid DESC LIMIT 3`,
		// JOIN ON a pseudo-rowid, both sides; LEFT JOIN NULL-extends the
		// pseudo-column exactly like any other column of the unmatched side.
		`SELECT p.rowid, p.label, i.id FROM plain_items p JOIN items i ON p.rowid - 99 = i.id`,
		`SELECT p.rowid, p.label, i.rowid FROM plain_items p LEFT JOIN items i ON p.rowid = i.id + 1000`,
		// Correlated subquery referencing the enclosing row's rowid.
		`SELECT p.rowid, (SELECT i.name FROM items i WHERE i.id = p.rowid - 99) FROM plain_items p ORDER BY p.rowid`,
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatch(t, p, db, q)
		})
	}
}

// TestRowidPseudoColumnShadowing exercises the one rule
// TestQueryMatchesReference's shared fixture can't (neither items nor
// plain_items has a real column literally named rowid/oid/_rowid_): a real
// column of that name always wins over the pseudo-column, per table and per
// name independently -- see sql_eval.go's resolveColumn doc comment. Verified
// against the reference engine (C SQLite has exactly this shadowing
// rule).
func TestRowidPseudoColumnShadowing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shadow.sqlite")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// oid is a real (shadowing) column here; rowid/_rowid_ are not declared,
	// so they still resolve to the pseudo rowid.
	exec(`CREATE TABLE shadow_oid (oid TEXT, val INTEGER)`)
	exec(`INSERT INTO shadow_oid (oid, val) VALUES ('alpha', 1), ('beta', 2)`)
	// rowid is a real (shadowing) column here; oid/_rowid_ are not declared.
	exec(`CREATE TABLE shadow_rowid (rowid TEXT, val INTEGER)`)
	exec(`INSERT INTO shadow_rowid (rowid, val) VALUES ('r1', 10), ('r2', 20)`)

	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, q := range []string{
		`SELECT oid, val FROM shadow_oid`,     // real column: the TEXT values
		`SELECT rowid, val FROM shadow_oid`,   // pseudo rowid: oid is shadowed here, rowid isn't
		`SELECT _rowid_, val FROM shadow_oid`, // pseudo rowid too
		`SELECT rowid, oid, val FROM shadow_oid WHERE rowid = 1`,
		`SELECT rowid, val FROM shadow_rowid`,   // real column: rowid itself is shadowed here
		`SELECT oid, val FROM shadow_rowid`,     // pseudo rowid: rowid is shadowed here, oid isn't
		`SELECT _rowid_, val FROM shadow_rowid`, // pseudo rowid too
	} {
		t.Run(q, func(t *testing.T) {
			mustMatch(t, p, db, q)
		})
	}
}

// TestRowidPseudoColumnAmbiguous checks that an unqualified rowid/oid/
// _rowid_ reference across more than one joined table is rejected as
// "ambiguous column name", exactly like an ordinary same-named column
// present in more than one joined table -- every table in scope always
// contributes exactly one candidate for these three names (its real column
// if it has one, else its own pseudo rowid), so this is never a genuinely
// resolvable reference. Verified against the reference engine: C SQLite
// rejects "SELECT rowid FROM items, plain_items" the same way.
func TestRowidPseudoColumnAmbiguous(t *testing.T) {
	path, _ := buildQueryTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, q := range []string{
		`SELECT rowid FROM items, plain_items`,
		`SELECT oid FROM items JOIN plain_items ON items.id = plain_items.rowid - 99`,
		`SELECT _rowid_ FROM items, plain_items`,
	} {
		t.Run(q, func(t *testing.T) {
			mustError(t, p, q, "an unqualified rowid/oid/_rowid_ reference across more than one joined table is ambiguous")
		})
	}
}

// TestIndexedByHintMatchesReference exercises "FROM t INDEXED BY
// <index-name>" (parsed and discarded in parseTableRef, sql_parser.go) with
// an index that actually exists, on a small fixture of its own -- adding a
// real index to buildQueryTestDB's shared items table would change real
// SQLite's own query plan (and, for the many pre-existing ORDER-BY-less
// queries above, its row order) for every other test sharing that fixture.
// ORDER BY here makes the comparison itself independent of either engine's
// scan order, whether or not the index actually gets used.
func TestIndexedByHintMatchesReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "indexed_by.sqlite")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE TABLE idxt (id INTEGER PRIMARY KEY, v INTEGER)`)
	exec(`CREATE INDEX idxt_v ON idxt (v)`)
	for _, v := range []int{5, 3, 8, 1, 9, 3} {
		if _, err := db.Exec(`INSERT INTO idxt (v) VALUES (?)`, v); err != nil {
			t.Fatal(err)
		}
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		`SELECT * FROM idxt INDEXED BY idxt_v WHERE v > 2 ORDER BY id`,
		`SELECT * FROM idxt AS it INDEXED BY idxt_v WHERE it.v > 2 ORDER BY it.id`,
		`SELECT * FROM idxt NOT INDEXED WHERE v > 2 ORDER BY id`,
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatch(t, p, db, q)
		})
	}
}

// TestQueryKnownUnsupported documents grammar this package deliberately does
// not implement (out of scope per query.go's header comment): each query
// must be rejected (parse or evaluation error), never silently mishandled.
func TestQueryKnownUnsupported(t *testing.T) {
	path, db := buildQueryTestDB(t)
	_ = db
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	cases := []struct {
		sql    string
		reason string
	}{
		{`SELECT SUM(COUNT(*)) FROM items`,
			"an aggregate nested inside another aggregate's argument is out of scope"},
		{`SELECT *, COUNT(*) FROM items`,
			"'*' combined with an aggregate is out of scope (no GROUP BY support)"},
		{`SELECT * FROM items JOIN plain_items USING (id)`,
			"USING(...) requires the named column on both sides; plain_items has no 'id' column at all"},
		{`SELECT * FROM items NATURAL JOIN plain_items USING (tag)`,
			"NATURAL combined with USING is a parse error, exactly like C SQLite"},
		{`SELECT * FROM items NATURAL JOIN plain_items ON 1 = 1`,
			"NATURAL combined with ON is a parse error, exactly like C SQLite"},
		{`SELECT * FROM items UNION SELECT * FROM plain_items`,
			"compound SELECT (UNION/INTERSECT/EXCEPT) is out of scope"},
		{`INSERT INTO items (id) VALUES (999)`,
			"only SELECT is supported; this is not even a SELECT statement"},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			mustError(t, p, c.sql, c.reason)
		})
	}
}

// TestQueryErrorsCleanly checks a few specific error-shape expectations that
// aren't about grammar scope: an unknown table, an unknown column, and a
// syntactically-empty statement should all fail as errors, not panics.
func TestQueryErrorsCleanly(t *testing.T) {
	path, _ := buildQueryTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, q := range []string{
		`SELECT * FROM no_such_table`,
		`SELECT no_such_column FROM items`,
		``,
		`SELECT`,
		`SELECT * FROM items WHERE`,
	} {
		t.Run(fmt.Sprintf("%q", q), func(t *testing.T) {
			if _, _, err := p.Query(q); err == nil {
				t.Errorf("expected an error for %q, got none", q)
			}
		})
	}
}
