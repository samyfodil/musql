package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// PRAGMA database_list reports one row per open database.
// The file column is normalized to its basename for cross-engine comparison.
func dbListRows(t *testing.T, db *sql.DB, label string) string {
	t.Helper()
	rows, err := db.Query(`PRAGMA database_list`)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var seq int
		var name, file sql.NullString
		if err := rows.Scan(&seq, &name, &file); err != nil {
			t.Fatalf("%s: scan: %v", label, err)
		}
		f := file.String
		if f != "" {
			if !filepath.IsAbs(f) {
				t.Errorf("%s: %q reports a RELATIVE file %q; C SQLite reports an absolute one", label, name.String, f)
			}
			f = filepath.Base(f)
		}
		out = append(out, fmt.Sprintf("[%d|%s|%s]", seq, name.String, f))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	return strings.Join(out, "")
}

func TestPragmaDatabaseListMatchesCSQLite(t *testing.T) {
	dir := t.TempDir()
	open := func(driver, name string) *sql.DB {
		db, err := sql.Open(driver, filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1) // the database list is CONNECTION state
		t.Cleanup(func() { db.Close() })
		return db
	}
	godb, cgodb := open("sqlite", "go.db"), open("sqlite3", "cgo.db")

	step := func(label, stmt string) {
		t.Helper()
		if stmt != "" {
			for _, d := range []struct {
				db   *sql.DB
				name string
			}{{godb, "musql"}, {cgodb, "cgo"}} {
				s := strings.ReplaceAll(stmt, "{dir}", dir)
				if d.name == "musql" {
					s = strings.ReplaceAll(s, "{aux}", "go-aux.db")
				} else {
					s = strings.ReplaceAll(s, "{aux}", "cgo-aux.db")
				}
				if _, err := d.db.Exec(s); err != nil {
					t.Fatalf("%s [%s]: %v", label, d.name, err)
				}
			}
		}
		got, want := dbListRows(t, godb, label+" (musql)"), dbListRows(t, cgodb, label+" (cgo)")
		// The two connections are on differently-named files, so main's own
		// basename differs by construction; compare it by POSITION instead.
		got = strings.Replace(got, "|go.db]", "|<main>]", 1)
		want = strings.Replace(want, "|cgo.db]", "|<main>]", 1)
		got = strings.ReplaceAll(got, "|go-aux.db]", "|<aux>]")
		want = strings.ReplaceAll(want, "|cgo-aux.db]", "|<aux>]")
		if got != want {
			t.Errorf("%s\n  musql: %s\n  cgo:    %s", label, got, want)
		}
	}

	step("fresh connection", "")
	step("after CREATE TABLE", `CREATE TABLE t(a)`)
	// An OPENER: it puts nothing in the temp database but makes C SQLite
	// open aDb[1], after which the temp row is listed.
	step("after reading sqlite_temp_master", `SELECT * FROM sqlite_temp_master`)
	step("after CREATE TEMP TABLE", `CREATE TEMP TABLE tt(z)`)
	step("after DROP of the temp table", `DROP TABLE tt`)
	step("after ATTACH aux", `ATTACH '{dir}/{aux}' AS aux`)
	step("after ATTACH ':memory:'", `ATTACH ':memory:' AS memx`)
	step("after DETACH aux", `DETACH aux`)
}

// TestPragmaDatabaseListAttachBeforeTemp pins the numbering rule on its own:
// aDb[1] is TEMP's slot whether or not temp is open, so the first attachment is
// seq 2 even on a connection that never touched temp -- and a DETACH renumbers
// the attachments after it while temp keeps index 1.
func TestPragmaDatabaseListAttachBeforeTemp(t *testing.T) {
	dir := t.TempDir()
	for _, eng := range []struct{ driver, main, aux, bee string }{
		{"sqlite", "go.db", "go-a.db", "go-b.db"},
		{"sqlite3", "cgo.db", "cgo-a.db", "cgo-b.db"},
	} {
		db, err := sql.Open(eng.driver, filepath.Join(dir, eng.main))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, s := range []string{
			`ATTACH '` + filepath.Join(dir, eng.aux) + `' AS aux`,
			`ATTACH '` + filepath.Join(dir, eng.bee) + `' AS bee`,
		} {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("[%s] %s: %v", eng.driver, s, err)
			}
		}
		if got, want := seqNames(t, db), "0:main 2:aux 3:bee"; got != want {
			t.Errorf("[%s] with temp never opened: %q, want %q", eng.driver, got, want)
		}
		if _, err := db.Exec(`CREATE TEMP TABLE tt(z)`); err != nil {
			t.Fatalf("[%s] CREATE TEMP TABLE: %v", eng.driver, err)
		}
		if got, want := seqNames(t, db), "0:main 1:temp 2:aux 3:bee"; got != want {
			t.Errorf("[%s] after opening temp: %q, want %q", eng.driver, got, want)
		}
		if _, err := db.Exec(`DETACH aux`); err != nil {
			t.Fatalf("[%s] DETACH: %v", eng.driver, err)
		}
		if got, want := seqNames(t, db), "0:main 1:temp 2:bee"; got != want {
			t.Errorf("[%s] after DETACH of the first attachment: %q, want %q", eng.driver, got, want)
		}
		db.Close()
	}
}

func seqNames(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`PRAGMA database_list`)
	if err != nil {
		t.Fatalf("database_list: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var seq int
		var name, file sql.NullString
		rows.Scan(&seq, &name, &file)
		out = append(out, fmt.Sprintf("%d:%s", seq, name.String))
	}
	return strings.Join(out, " ")
}

// TestPragmaDatabaseListSpellingsAndMemory covers the forms around the bare
// pragma: the table-valued wrapper, a schema qualifier (which C SQLite
// IGNORES -- the list is connection-wide, pragma.c walks db->aDb with no iDb
// filter), and a connection whose own main database has no file.
func TestPragmaDatabaseListSpellingsAndMemory(t *testing.T) {
	dir := t.TempDir()
	for _, q := range []string{
		`SELECT seq, name FROM pragma_database_list`,
		`SELECT count(*) FROM pragma_database_list`,
		`PRAGMA main.database_list`,
		`PRAGMA temp.database_list`,
	} {
		godb, _ := sql.Open("sqlite", filepath.Join(dir, "g1.db"))
		cgodb, _ := sql.Open("sqlite3", filepath.Join(dir, "c1.db"))
		godb.SetMaxOpenConns(1)
		cgodb.SetMaxOpenConns(1)
		for _, d := range []*sql.DB{godb, cgodb} {
			if _, err := d.Exec(`CREATE TEMP TABLE tt(z)`); err != nil {
				t.Fatalf("%s: setup: %v", q, err)
			}
		}
		got, gerr := oneCell(godb, q)
		want, cerr := oneCell(cgodb, q)
		// The two connections are on differently-named files by construction.
		got = strings.ReplaceAll(got, filepath.Join(dir, "g1.db"), "<main>")
		want = strings.ReplaceAll(want, filepath.Join(dir, "c1.db"), "<main>")
		if (gerr == nil) != (cerr == nil) {
			t.Errorf("%s: musql err=%v, cgo err=%v", q, gerr, cerr)
		} else if gerr == nil && got != want {
			t.Errorf("%s: musql=%q, cgo=%q", q, got, want)
		}
		godb.Close()
		cgodb.Close()
	}

	// A memory-backed main reports NO file, and its temp row likewise.
	gomem, _ := sql.Open("sqlite", ":memory:")
	cgomem, _ := sql.Open("sqlite3", ":memory:")
	gomem.SetMaxOpenConns(1)
	cgomem.SetMaxOpenConns(1)
	defer gomem.Close()
	defer cgomem.Close()
	for _, d := range []*sql.DB{gomem, cgomem} {
		if _, err := d.Exec(`CREATE TEMP TABLE tt(z)`); err != nil {
			t.Fatalf(":memory: setup: %v", err)
		}
	}
	if got, want := dbListRows(t, gomem, ":memory: musql"), dbListRows(t, cgomem, ":memory: cgo"); got != want {
		t.Errorf(":memory: main\n  musql: %s\n  cgo:    %s", got, want)
	}
}

// oneCell renders a whole result set as one comparable string.
func oneCell(db *sql.DB, q string) (string, error) {
	rows, err := db.Query(q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := strings.Join(cols, ",") + "|"
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		for _, c := range cells {
			if b, ok := c.([]byte); ok {
				c = string(b)
			}
			out += fmt.Sprintf("%v,", c)
		}
		out += ";"
	}
	return out, rows.Err()
}
