// Gates the converter: a database C SQLite wrote must come through Import
// intact and back out through Export as a .db C accepts identically. Each
// case is a source database with an awkward property (reserved-per-page,
// auto_vacuum, UTF-16) that a converter could silently get wrong.
package compat

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	musqlengine "github.com/samyfodil/musql/engine"
)

// buildCgoReservedFreelistFixture creates a fresh database with reserved bytes
// applied and n pages on the freelist. Reserved bytes on an existing database
// are deferred until the next VACUUM.
func buildCgoReservedFreelistFixture(t *testing.T, path string, pageSize, reserved uint32, n int) {
	t.Helper()
	db, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("cgo open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // one physical connection: file control + PRAGMAs must land on the SAME session

	if _, err := db.Exec(fmt.Sprintf(`PRAGMA page_size=%d`, pageSize)); err != nil {
		t.Fatalf("cgo page_size: %v", err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("cgo conn: %v", err)
	}
	if err := conn.Raw(func(dc any) error {
		sc := dc.(*sqlite3.SQLiteConn)
		return sc.SetFileControlInt("main", sqlite3.SQLITE_FCNTL_RESERVE_BYTES, int(reserved))
	}); err != nil {
		t.Fatalf("cgo set reserve bytes: %v", err)
	}
	conn.Close()

	if _, err := db.Exec(`CREATE TABLE seed(a)`); err != nil {
		t.Fatalf("cgo seed table: %v", err)
	}
	// ROWS, so a conversion of this file has content to be faithful about. The
	// two tests this fixture was built for only ever read the HEADER (reserved
	// bytes, freelist_count), so an empty table was enough for them; the
	// round-trip that replaced them compares rows through both engines.
	for i := 1; i <= 64; i++ {
		if _, err := db.Exec(`INSERT INTO seed(a) VALUES(?)`, i*7); err != nil {
			t.Fatalf("cgo seed rows: %v", err)
		}
	}
	if reserved > 0 {
		// Applies the deferred page_size/reserve-bytes request -- see this
		// function's own doc comment.
		if _, err := db.Exec(`VACUUM`); err != nil {
			t.Fatalf("cgo vacuum: %v", err)
		}
	}

	// Generate n free pages: n tables, each occupying exactly one page (no
	// rows), then drop them all -- C SQLite chains every one onto its own
	// freelist (freelist_test.go's own "DROP TABLE frees a page" finding,
	// pinned cross-engine there for the ordinary, non-reserved-bytes case).
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("cgo begin: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(fmt.Sprintf(`CREATE TABLE f%d(a)`, i)); err != nil {
			t.Fatalf("cgo create f%d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(fmt.Sprintf(`DROP TABLE f%d`, i)); err != nil {
			t.Fatalf("cgo drop f%d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("cgo commit: %v", err)
	}
}

// header32At and header8At read the raw on-disk header at the given offset.
func header32At(t *testing.T, path string, offset int64) uint32 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return binary.BigEndian.Uint32(b[offset : offset+4])
}

func header8At(t *testing.T, path string, offset int64) uint8 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b[offset]
}

func cgoIntegrityCheckOK(t *testing.T, path string) (string, bool) {
	t.Helper()
	db, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("cgo reopen: %v", err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&got); err != nil {
		t.Fatalf("cgo integrity_check: %v", err)
	}
	return got, got == "ok"
}

// TestReservedBytesFileRoundTripsThroughTheConverter verifies that a database
// C wrote with nonzero reserved-per-page and a large freelist converts both ways
// intact, because a naive converter would read cells at wrong offsets.
func TestReservedBytesFileRoundTripsThroughTheConverter(t *testing.T) {
	const pageSize = 4096
	const reserved = 32
	const n = 1100 // more free pages than one trunk holds at usable = 4064

	dir := t.TempDir()
	src := filepath.Join(dir, "resv.db")
	buildCgoReservedFreelistFixture(t, src, pageSize, reserved, n)

	// The fixture really is awkward, or the rest of this proves nothing.
	if got := header8At(t, src, 20); got != reserved {
		t.Fatalf("fixture setup: header ReservedPerPage = %d, want %d", got, reserved)
	}
	if got := header32At(t, src, 36); got < n {
		t.Fatalf("fixture setup: only %d freelist pages, want >= %d", got, n)
	}
	if ic, ok := cgoIntegrityCheckOK(t, src); !ok {
		t.Fatalf("fixture setup: cgo's own integrity_check on the fixture says %q, want ok", ic)
	}
	wantRows := cgoRowDump(t, src, `SELECT a FROM seed ORDER BY a`)
	if len(wantRows) == 0 {
		t.Fatal("fixture setup: the fixture has no rows to compare")
	}

	// IN: C's file becomes a segment database.
	segPath := filepath.Join(dir, "resv.musq")
	if err := sqliteconv.Import(src, segPath, sqliteconv.ImportOptions{}); err != nil {
		t.Fatalf("ImportSQLite of a reserved-bytes file: %v", err)
	}
	rp, oerr := musqlengine.Open(segPath)
	if oerr != nil {
		t.Fatalf("Open: %v", oerr)
	}
	_, rows, qerr := rp.Query(`SELECT a FROM seed ORDER BY a`)
	rp.Close()
	if qerr != nil {
		t.Fatalf("reading the imported database: %v", qerr)
	}
	if len(rows) != len(wantRows) {
		t.Fatalf("the import has %d rows, the source has %d", len(rows), len(wantRows))
	}
	for i, r := range rows {
		if got := fmt.Sprint(r[0].I); got != wantRows[i] {
			t.Fatalf("row %d = %q, source says %q", i, got, wantRows[i])
		}
	}

	// OUT: and back to a .db that C accepts and reads identically.
	back := filepath.Join(dir, "back.db")
	if err := sqliteconv.Export(segPath, back, pageSize); err != nil {
		t.Fatalf("ExportSQLite: %v", err)
	}
	if ic, ok := cgoIntegrityCheckOK(t, back); !ok {
		t.Errorf("cgo's integrity_check on the export says %q, want ok", ic)
	}
	gotRows := cgoRowDump(t, back, `SELECT a FROM seed ORDER BY a`)
	if len(gotRows) != len(wantRows) {
		t.Fatalf("the export has %d rows, the source had %d", len(gotRows), len(wantRows))
	}
	for i := range gotRows {
		if gotRows[i] != wantRows[i] {
			t.Fatalf("export row %d = %q, source says %q", i, gotRows[i], wantRows[i])
		}
	}
}

// cgoRowDump reads rows through C SQLite.
func cgoRowDump(t *testing.T, path, query string) []string {
	t.Helper()
	db, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("cgo open %s: %v", path, err)
	}
	defer db.Close()
	rows, qerr := db.QueryContext(context.Background(), query)
	if qerr != nil {
		t.Fatalf("cgo read %s: %v", path, qerr)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if serr := rows.Scan(&v); serr != nil {
			t.Fatalf("cgo scan: %v", serr)
		}
		out = append(out, v)
	}
	return out
}

// TestAutoVacuumFileRoundTripsThroughTheConverter verifies that a database C
// wrote with auto_vacuum set converts intact, with the setting preserved.
func TestAutoVacuumFileRoundTripsThroughTheConverter(t *testing.T) {
	for _, mode := range []int{1, 2} {
		t.Run(fmt.Sprintf("auto_vacuum=%d", mode), func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "av.db")
			cdb, err := sql.Open("sqlite3", src)
			if err != nil {
				t.Fatal(err)
			}
			exec := func(db *sql.DB, q string, args ...any) {
				t.Helper()
				if _, eerr := db.Exec(q, args...); eerr != nil {
					t.Fatalf("%s: %v", q, eerr)
				}
			}
			exec(cdb, fmt.Sprintf("PRAGMA auto_vacuum=%d", mode))
			exec(cdb, `CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`)
			for i := 1; i <= 200; i++ {
				exec(cdb, `INSERT INTO t VALUES(?,?)`, i, fmt.Sprintf("row-%d", i))
			}
			// DROP frees pages, which is what makes the pointer map do something.
			exec(cdb, `CREATE TABLE junk(a)`)
			for i := 0; i < 200; i++ {
				exec(cdb, `INSERT INTO junk VALUES(randomblob(400))`)
			}
			exec(cdb, `DROP TABLE junk`)
			var got int
			if qerr := cdb.QueryRow(`PRAGMA auto_vacuum`).Scan(&got); qerr != nil || got != mode {
				t.Fatalf("fixture setup: auto_vacuum reads %d (err %v), want %d", got, qerr, mode)
			}
			if cerr := cdb.Close(); cerr != nil {
				t.Fatal(cerr)
			}
			wantRows := cgoRowDump(t, src, `SELECT v FROM t ORDER BY id`)
			if len(wantRows) != 200 {
				t.Fatalf("fixture setup: %d rows", len(wantRows))
			}

			segPath := filepath.Join(dir, "av.musq")
			if ierr := sqliteconv.Import(src, segPath, sqliteconv.ImportOptions{}); ierr != nil {
				t.Fatalf("ImportSQLite of an auto_vacuum=%d database: %v", mode, ierr)
			}
			rp, oerr := musqlengine.Open(segPath)
			if oerr != nil {
				t.Fatal(oerr)
			}
			_, rows, qerr := rp.Query(`SELECT v FROM t ORDER BY id`)
			rp.Close()
			if qerr != nil {
				t.Fatalf("reading the import: %v", qerr)
			}
			if len(rows) != len(wantRows) {
				t.Fatalf("the import has %d rows, the source has %d", len(rows), len(wantRows))
			}
			for i, r := range rows {
				if string(r[0].S) != wantRows[i] {
					t.Fatalf("row %d = %q, source says %q", i, string(r[0].S), wantRows[i])
				}
			}

			back := filepath.Join(dir, "back.db")
			if eerr := sqliteconv.Export(segPath, back, 0); eerr != nil {
				t.Fatalf("ExportSQLite: %v", eerr)
			}
			if ic, ok := cgoIntegrityCheckOK(t, back); !ok {
				t.Errorf("cgo's integrity_check on the export says %q, want ok", ic)
			}
			gotRows := cgoRowDump(t, back, `SELECT v FROM t ORDER BY id`)
			if len(gotRows) != len(wantRows) {
				t.Fatalf("the export has %d rows, the source had %d", len(gotRows), len(wantRows))
			}
			for i := range gotRows {
				if gotRows[i] != wantRows[i] {
					t.Fatalf("export row %d = %q, source says %q", i, gotRows[i], wantRows[i])
				}
			}
			// ...and the SETTING survives: the import records it in the catalog and
			// the export writes it back, so C reads the mode it wrote.
			ecdb, _ := sql.Open("sqlite3", back)
			defer ecdb.Close()
			var exported int
			if serr := ecdb.QueryRow(`PRAGMA auto_vacuum`).Scan(&exported); serr != nil {
				t.Fatal(serr)
			}
			if exported != mode {
				t.Errorf("the export reports auto_vacuum=%d, the source was %d", exported, mode)
			}
		})
	}
}

// TestUtf16FileRoundTripsThroughTheConverter verifies that a UTF-16 database
// converts both ways with encoding preserved, requiring transcoding not byte-copying.
func TestUtf16FileRoundTripsThroughTheConverter(t *testing.T) {
	for _, enc := range []string{"UTF-16le", "UTF-16be"} {
		t.Run(enc, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "u16.db")
			cdb, err := sql.Open("sqlite3", src)
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{
				`PRAGMA encoding='` + enc + `'`,
				`CREATE TABLE t(a TEXT)`,
				`INSERT INTO t VALUES('hello'),('héllo'),('')`,
			} {
				if _, eerr := cdb.Exec(q); eerr != nil {
					t.Fatalf("%s: %v", q, eerr)
				}
			}
			var have string
			if serr := cdb.QueryRow(`PRAGMA encoding`).Scan(&have); serr != nil {
				t.Fatal(serr)
			}
			if have != enc {
				t.Fatalf("fixture setup: the source reports encoding %q, want %q", have, enc)
			}
			wantRows := cgoRowDump(t, src, `SELECT a FROM t ORDER BY a`)
			cdb.Close()

			segPath := filepath.Join(dir, "u16.musq")
			if ierr := sqliteconv.Import(src, segPath, sqliteconv.ImportOptions{}); ierr != nil {
				t.Fatalf("ImportSQLite of a %s database: %v", enc, ierr)
			}
			rp, oerr := musqlengine.Open(segPath)
			if oerr != nil {
				t.Fatal(oerr)
			}
			_, rows, qerr := rp.Query(`SELECT a, hex(CAST(a AS BLOB)) FROM t ORDER BY a`)
			rp.Close()
			if qerr != nil {
				t.Fatalf("reading the import: %v", qerr)
			}
			if len(rows) != len(wantRows) {
				t.Fatalf("the import has %d rows, the source has %d", len(rows), len(wantRows))
			}
			// THE ENCODING IS CARRIED, not normalised. It used to be transcoded to
			// UTF-8 here, because the segment format had no field to record another
			// encoding -- it has one now (ConvertedCatalog.Encoding), so an imported
			// UTF-16 database IS a UTF-16 database and the bytes behind its text are
			// the source's own. That is what C does with its own file, and it is the
			// stronger promise: a value's storage survives the conversion, not only
			// its meaning.
			//
			// The comparison is against the ORACLE's own hex() of the same rows, so
			// this checks agreement rather than a hand-written expectation.
			wantHex := cgoRowDump(t, src, `SELECT hex(CAST(a AS BLOB)) FROM t ORDER BY a`)
			for i, r := range rows {
				if got := string(r[0].S); got != wantRows[i] {
					t.Errorf("row %d = %q, the source says %q", i, got, wantRows[i])
				}
				if i < len(wantHex) {
					if got := string(r[1].S); got != wantHex[i] {
						t.Errorf("row %d hex = %q, the source says %q -- the import did not carry the encoding", i, got, wantHex[i])
					}
				}
			}

			back := filepath.Join(dir, "back.db")
			if eerr := sqliteconv.Export(segPath, back, 0); eerr != nil {
				t.Fatalf("ExportSQLite: %v", eerr)
			}
			if ic, ok := cgoIntegrityCheckOK(t, back); !ok {
				t.Errorf("cgo's integrity_check on the export says %q, want ok", ic)
			}
			gotRows := cgoRowDump(t, back, `SELECT a FROM t ORDER BY a`)
			for i := range gotRows {
				if i < len(wantRows) && gotRows[i] != wantRows[i] {
					t.Errorf("export row %d = %q, source %q", i, gotRows[i], wantRows[i])
				}
			}
			if got := cgoRowDump(t, back, `PRAGMA encoding`); len(got) != 1 || got[0] != enc {
				t.Errorf("the export reports encoding %v, the source was %s", got, enc)
			}
		})
	}
}
