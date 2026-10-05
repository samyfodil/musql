package engine_test

import (
	"github.com/samyfodil/musql/engine"

	"bytes"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// buildReaderTestDB builds a test database covering all column types
// and exercises large TEXT/BLOB values and interior pages.
func buildReaderTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reader.musq")
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

	// types_ipk: an INTEGER PRIMARY KEY table exercising every serial-type family.
	exec(`CREATE TABLE types_ipk (
		id INTEGER PRIMARY KEY,
		i0 INTEGER,
		i1 INTEGER,
		ismall INTEGER,
		imed INTEGER,
		ibig INTEGER,
		ineg INTEGER,
		r REAL,
		rneg REAL,
		t TEXT,
		temoji TEXT,
		tempty TEXT,
		b BLOB,
		bempty BLOB,
		n
	)`)
	type row struct {
		i0, i1, ismall, imed, ibig, ineg any
		r, rneg                          any
		t, temoji, tempty                any
		b, bempty                        any
		n                                any
	}
	rows := []row{
		{0, 1, int64(42), int64(70000), int64(math.MaxInt64), int64(math.MinInt64),
			3.14159, -2.5,
			"hello, world", "unicode: 日本語 \U0001F600 café", "",
			[]byte{0x00, 0x01, 0x02, 0xff, 0xfe}, []byte{}, nil},
		{0, 1, int64(-1), int64(-70000), int64(1), int64(-1),
			0.0, -0.0,
			"", "éèê", "x",
			[]byte{}, []byte{0xde, 0xad, 0xbe, 0xef}, nil},
		{nil, nil, nil, nil, nil, nil,
			nil, nil,
			nil, nil, nil,
			nil, nil, nil},
		{int64(127), int64(-128), int64(32767), int64(-32768), int64(16777215), int64(-16777216),
			1.0 / 3.0, math.Pi,
			"padding-" + strings.Repeat("z", 100), "\U0001F4A9\U0001F680", "y",
			bytes.Repeat([]byte{0xab, 0xcd}, 50), []byte{0x00}, nil},
	}
	st, err := db.Prepare(`INSERT INTO types_ipk
		(i0,i1,ismall,imed,ibig,ineg,r,rneg,t,temoji,tempty,b,bempty,n)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := st.Exec(r.i0, r.i1, r.ismall, r.imed, r.ibig, r.ineg, r.r, r.rneg, r.t, r.temoji, r.tempty, r.b, r.bempty, r.n); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	// plain_rowid: no INTEGER PRIMARY KEY column; non-contiguous rowids.
	exec(`CREATE TABLE plain_rowid (name TEXT, val REAL)`)
	st, err = db.Prepare(`INSERT INTO plain_rowid(rowid, name, val) VALUES (?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	rid := int64(10)
	for i := 0; i < 30; i++ {
		if _, err := st.Exec(rid, fmt.Sprintf("plain-%d", i), float64(i)*0.25); err != nil {
			t.Fatal(err)
		}
		rid += int64(7 + i%5) // irregular gaps
	}
	st.Close()

	// many_rows: enough rows to force multiple b-tree levels.
	exec(`CREATE TABLE many_rows (id INTEGER PRIMARY KEY, name TEXT, val REAL)`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	st, err = tx.Prepare(`INSERT INTO many_rows(id, name, val) VALUES (?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	const nMany = 2500
	for i := 1; i <= nMany; i++ {
		if _, err := st.Exec(i, fmt.Sprintf("row-%d-with-some-padding-text-to-bulk-up-the-cell", i), float64(i)*1.5); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// big_blobs: multi-KB values forcing overflow chains.
	exec(`CREATE TABLE big_blobs (id INTEGER PRIMARY KEY, data BLOB, note TEXT)`)
	st, err = db.Prepare(`INSERT INTO big_blobs(id, data, note) VALUES (?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	sizesKB := []int{1, 8, 40}
	for i, kb := range sizesKB {
		blob := make([]byte, kb*1024)
		for j := range blob {
			blob[j] = byte((j*31 + i) % 256)
		}
		note := strings.Repeat(fmt.Sprintf("note-%d-中文-", i), 400) // multi-KB unicode text
		if _, err := st.Exec(i+1, blob, note); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	return path, db
}

// ipkColumnIndex maps table name to the zero-based index (within the
// non-rowid columns, i.e. within decodeRecord's output / the columns after
// "rowid" in "SELECT rowid, * FROM t") of its INTEGER PRIMARY KEY rowid-alias
// column, or -1 if the table has none.
var ipkColumnIndex = map[string]int{
	"types_ipk":   0,
	"plain_rowid": -1,
	"many_rows":   0,
	"big_blobs":   0,
}

// refValue mirrors what database/sql (via this repo's driver) hands back for
// each SQLite storage class when Scanning into `any`, normalized into our
// engine.Value type for comparison against the pure-Go decoder's output.
func refValue(v any) engine.Value {
	switch x := v.(type) {
	case nil:
		return engine.Value{Typ: engine.Null}
	case int64:
		return engine.Value{Typ: engine.Int, I: x}
	case float64:
		return engine.Value{Typ: engine.Float, F: x}
	case string:
		return engine.Value{Typ: engine.Text, S: []byte(x)}
	case []byte:
		return engine.Value{Typ: engine.Blob, S: append([]byte(nil), x...)}
	default:
		panic(fmt.Sprintf("refValue: unexpected type %T", v))
	}
}

// valuesEqual compares a pure-Go decoded engine.Value against a reference engine.Value,
// both already normalized by refValue/decodeRecord.
func valuesEqual(got, want engine.Value) bool {
	if got.Typ != want.Typ {
		return false
	}
	switch got.Typ {
	case engine.Null:
		return true
	case engine.Int:
		return got.I == want.I
	case engine.Float:
		// Bit-for-bit: both paths ultimately read the same 8 stored bytes.
		return math.Float64bits(got.F) == math.Float64bits(want.F)
	case engine.Text, engine.Blob:
		return bytes.Equal(got.S, want.S)
	default:
		return false
	}
}

// valuesEqualAllowingRealStorageOptimization is valuesEqual, plus tolerance
// for one well-documented, intentional SQLite behavior: "small floating
// point values with no fractional component and stored in columns with REAL
// affinity are written to disk as integers in order to take up less space
// and are automatically converted back into floating point as the value is
// read out of the database" (sqlite.org/datatype3.html). So a REAL column
// holding e.g. 0.0 or 3576.0 is genuinely stored on disk with an INTEGER
// serial type; decodeRecord correctly reports that raw storage class, while
// database/sql's column-affinity-aware read reports it as a float. Both
// sides are right about their own layer; only the numeric value need agree.
func valuesEqualAllowingRealStorageOptimization(got, want engine.Value) bool {
	if valuesEqual(got, want) {
		return true
	}
	if got.Typ == engine.Int && want.Typ == engine.Float {
		return float64(got.I) == want.F
	}
	if got.Typ == engine.Float && want.Typ == engine.Int {
		return got.F == float64(want.I)
	}
	return false
}

// compareTable reads tableName through the pure-Go engine and through plain
// SQL on the same database file, and asserts
// they agree row for row, column for column, including substituting the
// rowid for the INTEGER PRIMARY KEY column the pure-Go layer necessarily
// leaves as NULL in the raw record.
func compareTable(t *testing.T, p *engine.ReadOnlyPager, db *sql.DB, tableName string) {
	t.Helper()
	ipkIdx := ipkColumnIndex[tableName]

	gotRowids, gotRows, err := p.Rows(tableName)
	if err != nil {
		t.Fatalf("engine Rows(%s): %v", tableName, err)
	}

	refRows, err := db.Query(fmt.Sprintf("SELECT rowid, * FROM %s ORDER BY rowid", tableName))
	if err != nil {
		t.Fatalf("reference query on %s: %v", tableName, err)
	}
	defer refRows.Close()
	cols, err := refRows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	nCol := len(cols) - 1 // minus the leading rowid

	var wantRowids []int64
	var wantRows [][]any
	for refRows.Next() {
		dest := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err := refRows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		rid, ok := dest[0].(int64)
		if !ok {
			t.Fatalf("reference rowid column has type %T", dest[0])
		}
		wantRowids = append(wantRowids, rid)
		wantRows = append(wantRows, dest[1:])
	}
	if err := refRows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(gotRowids) != len(wantRowids) {
		t.Fatalf("%s: got %d rows, want %d", tableName, len(gotRowids), len(wantRowids))
	}
	for i := range gotRowids {
		if gotRowids[i] != uint64(wantRowids[i]) {
			t.Errorf("%s: row %d: rowid = %d, want %d", tableName, i, gotRowids[i], wantRowids[i])
		}
		gotVals := gotRows[i]
		if len(gotVals) != nCol {
			t.Fatalf("%s: row %d: got %d columns, want %d", tableName, i, len(gotVals), nCol)
		}
		for c := 0; c < nCol; c++ {
			got := gotVals[c]
			if c == ipkIdx && got.Typ == engine.Null {
				// INTEGER PRIMARY KEY rowid aliasing: the record stores NULL;
				// the true value is the rowid.
				got = engine.Value{Typ: engine.Int, I: int64(gotRowids[i])}
			}
			want := refValue(wantRows[i][c])
			if !valuesEqualAllowingRealStorageOptimization(got, want) {
				t.Errorf("%s: row %d (rowid %d) col %d (%s): got %+v, want %+v",
					tableName, i, gotRowids[i], c, cols[c+1], got, want)
			}
		}
	}
}

// TestReaderMatchesReference: every value the writer stored reads back through
// the segment reader exactly as the driver's own SELECT reports it.
//
// It ran twice, once per page size, which is why it was called a b-tree reader
// test. One run now: see buildReaderTestDB.
func TestReaderMatchesReference(t *testing.T) {
	path, db := buildReaderTestDB(t)

	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for tbl := range ipkColumnIndex {
		t.Run(tbl, func(t *testing.T) {
			compareTable(t, p, db, tbl)
		})
	}
}

func TestSchemaAndTableRoots(t *testing.T) {
	path, db := buildReaderTestDB(t)
	defer db.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	got, err := p.Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	// Every table this fixture created must appear in the parsed schema with
	// a usable root page. (Cross-engine parity lives in compat-harness,
	// against C SQLite.)
	byName := map[string]engine.SchemaRow{}
	for _, row := range got {
		byName[row.Name] = row
	}
	for tbl := range ipkColumnIndex {
		row, ok := byName[tbl]
		if !ok {
			t.Errorf("Schema() has no row for table %s", tbl)
			continue
		}
		if row.Type != "table" || row.TblName != tbl || row.SQL == "" {
			t.Errorf("Schema() row for %s = %+v", tbl, row)
		}
		root, err := p.TableRoot(tbl)
		if err != nil {
			t.Errorf("TableRoot(%s): %v", tbl, err)
			continue
		}
		if root == 0 || root != row.RootPage {
			t.Errorf("TableRoot(%s) = %d, schema says %d", tbl, root, row.RootPage)
		}
	}
	if _, err := p.TableRoot("no_such_table"); err == nil {
		t.Error("TableRoot(no_such_table) should error")
	}
}

// TestLargeBlobsRoundTrip checks that the largest blob in big_blobs (tens of KB)
// round-trips byte for byte.
//
// It was TestOverflowChainsSpanManyPages, built at a 512-byte page size so that a
// 40KB blob HAD to span an overflow chain, and it proved readOverflow walked one.
// This format has neither pages nor overflow chains, so what is left to
// prove is the part that still exists: a value far larger than any block boundary
// comes back whole.
func TestLargeBlobsRoundTrip(t *testing.T) {
	path, db := buildReaderTestDB(t)

	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	_, rows, err := p.Rows("big_blobs")
	if err != nil {
		t.Fatalf("Rows(big_blobs): %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	// The third row (id=3) has a 40KB blob: confirm its length and content
	// independently via the reference path.
	var want []byte
	if err := db.QueryRow(`SELECT data FROM big_blobs WHERE id = 3`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	got := rows[2][1] // columns: data, note (data is index 1 after id/rowid substitution... )
	if got.Typ != engine.Blob {
		t.Fatalf("data column has Typ %v, want engine.Blob", got.Typ)
	}
	if len(want) < 20*1024 {
		t.Fatalf("test setup bug: want blob only %d bytes", len(want))
	}
	if !bytes.Equal(got.S, want) {
		t.Fatalf("overflow blob mismatch: got %d bytes, want %d bytes", len(got.S), len(want))
	}
}
