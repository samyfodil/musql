//go:build sqlite_fts5

// Package compat tests fts5's locale option.
package compat

import (
	"path/filepath"
	"testing"
)

// TestR37BLocaleOptionSchema tests fts5's locale option schema generation.
func TestR37BLocaleOptionSchema(t *testing.T) {
	// locale=1 creates an l column for indexed columns.
	flLockstep(t, "locale-columns", []string{
		`CREATE VIRTUAL TABLE t1 USING fts5(x, y UNINDEXED, locale=1)`,
		`CREATE VIRTUAL TABLE t2 USING fts5(a, b, locale=1)`,
		`CREATE VIRTUAL TABLE t3 USING fts5(a UNINDEXED, b UNINDEXED, locale=1)`,
		`CREATE VIRTUAL TABLE t4 USING fts5(a, locale=0)`,
		`CREATE VIRTUAL TABLE t5 USING fts5(a)`,
	},
		`SELECT name, sql FROM sqlite_master WHERE name LIKE '%_content' ORDER BY name`,
		`SELECT count(*) FROM sqlite_master`,
	)
	// Locale value acceptance and parsing.
	flLockstep(t, "locale-values", []string{
		`CREATE VIRTUAL TABLE bad1 USING fts5(a, locale=2)`,
		`CREATE VIRTUAL TABLE bad2 USING fts5(a, locale=01)`,
		`CREATE VIRTUAL TABLE bad3 USING fts5(a, locale=)`,
		`CREATE VIRTUAL TABLE q1 USING fts5(a, locale='1')`,
		`CREATE VIRTUAL TABLE r1 USING fts5(a, locale=1, locale=0)`,
		`CREATE VIRTUAL TABLE r2 USING fts5(a, locale=0, locale=1)`,
	},
		`SELECT name, sql FROM sqlite_master WHERE name LIKE '%_content' ORDER BY name`,
	)
	// Locale option abbreviations.
	flLockstep(t, "locale-abbrev", []string{
		`CREATE VIRTUAL TABLE a1 USING fts5(a, l=1)`,
		`CREATE VIRTUAL TABLE a2 USING fts5(a, loc=1)`,
		`CREATE VIRTUAL TABLE a3 USING fts5(a, LOCALE=1)`,
	},
		`SELECT name, sql FROM sqlite_master WHERE name LIKE '%_content' ORDER BY name`,
	)
	// locale= composes with every other option this engine serves.
	flLockstep(t, "locale-combos", []string{
		`CREATE VIRTUAL TABLE c1 USING fts5(a, b, locale=1, columnsize=0, detail=none, prefix='2 3')`,
		`INSERT INTO c1(rowid,a,b) VALUES(1,'one two','bb')`,
		`CREATE VIRTUAL TABLE c2 USING fts5(a, b, locale=1, tokenize=ascii)`,
		`INSERT INTO c2(rowid,a,b) VALUES(1,'One TWO','bb')`,
		`CREATE VIRTUAL TABLE c3 USING fts5(a, b, locale=1, content='')`,
		`INSERT INTO c3(rowid,a,b) VALUES(1,'one two','bb')`,
	},
		`SELECT name, sql FROM sqlite_master WHERE name LIKE 'c%_content' ORDER BY name`,
		`SELECT rowid FROM c1 WHERE c1 MATCH 'one'`,
		`SELECT rowid FROM c2 WHERE c2 MATCH 'one'`,
		`SELECT rowid FROM c3 WHERE c3 MATCH 'two'`,
		`SELECT id, quote(c0), quote(c1), quote(l0), quote(l1) FROM c2_content ORDER BY id`,
	)
}

// A locale=1 table this engine fills must be INDISTINGUISHABLE from its
// locale=0 twin everywhere but %_content's extra columns: the locale never
// reaches a tokenizer fts5 ships (ext/fts5/fts5_tokenize.c never mentions
// pLocale), so %_data, %_docsize and %_config are byte-identical.
//
// The gate compares each engine against the ORACLE rather than comparing the
// twins to each other, so it stays honest if that ever stops being true.
func TestR37BLocaleWritesMatchPlainTable(t *testing.T) {
	flLockstep(t, "locale-vs-plain", []string{
		`CREATE VIRTUAL TABLE w1 USING fts5(a, b, locale=1)`,
		`CREATE VIRTUAL TABLE w0 USING fts5(a, b)`,
		`INSERT INTO w1(rowid,a,b) VALUES(1,'one two','three'),(2,'four','five six')`,
		`INSERT INTO w0(rowid,a,b) VALUES(1,'one two','three'),(2,'four','five six')`,
		`UPDATE w1 SET b='rewritten' WHERE rowid=2`,
		`UPDATE w0 SET b='rewritten' WHERE rowid=2`,
		`DELETE FROM w1 WHERE rowid=1`,
		`DELETE FROM w0 WHERE rowid=1`,
		`INSERT INTO w1(rowid,a,b) VALUES(7,'seven eight','nine')`,
		`INSERT INTO w0(rowid,a,b) VALUES(7,'seven eight','nine')`,
	},
		`SELECT id, quote(c0), quote(c1), quote(l0), quote(l1) FROM w1_content ORDER BY id`,
		`SELECT id, quote(sz) FROM w1_docsize ORDER BY id`,
		`SELECT k, quote(v) FROM w1_config ORDER BY k`,
		`SELECT rowid, a, b FROM w1 ORDER BY rowid`,
		`SELECT rowid FROM w1 WHERE w1 MATCH 'seven' ORDER BY rowid`,
		`SELECT rowid FROM w1 WHERE w1 MATCH 'rewritten' ORDER BY rowid`,
		`SELECT highlight(w1,0,'[',']') FROM w1 WHERE w1 MATCH 'seven'`,
		// The whole point: the two tables' index and sizes agree.
		`SELECT (SELECT group_concat(quote(sz)) FROM w1_docsize)=(SELECT group_concat(quote(sz)) FROM w0_docsize)`,
		`SELECT (SELECT group_concat(quote(v)) FROM w1_config)=(SELECT group_concat(quote(v)) FROM w0_config)`,
	)
}

// r37bLockstepFiles runs the SAME mutation against two copies of one seed
// database -- one copy written by this engine, the other by C SQLite -- and
// requires the two files to answer every probe identically, each read through
// BOTH engines.
//
// The two-file shape is the whole point, and this gate was VACUOUS without it:
// reading one file through both engines can never catch a WRITER that loses
// data, because both readers then agree on the damaged file. Verified by
// mutation -- with the locale carry-over deleted from the engine, the
// single-file version of this test still passed while both engines reported
// the locale gone.
//
// seed is executed by C SQLite only: it is the half that needs
// fts5_locale(), which this engine deliberately does not implement
// (fts5_locale.go).
func r37bLockstepFiles(t *testing.T, seed, mutate, probes []string) {
	t.Helper()
	dir := t.TempDir()
	goFile := filepath.Join(dir, "go.db")
	cgoFile := filepath.Join(dir, "cgo.db")
	for _, f := range []string{goFile, cgoFile} {
		if err := fts5Exec(t, "sqlite3", f, seed); err != nil {
			t.Fatalf("cgo seeds %s: %v", filepath.Base(f), err)
		}
	}
	// This engine runs on its own format (RULE #3): it writes the IMPORT of the
	// oracle's file, and the oracle reads back the EXPORT of what it wrote.
	goFile = importedForMusql(t, goFile)
	if len(mutate) > 0 {
		if err := fts5Exec(t, "sqlite", goFile, mutate); err != nil {
			t.Fatalf("this engine mutates: %v", err)
		}
		if err := fts5Exec(t, "sqlite3", cgoFile, mutate); err != nil {
			t.Fatalf("cgo mutates: %v", err)
		}
	}
	for _, q := range probes {
		goOut, goErr := fts5Query(t, "sqlite", goFile, q)
		cgoOut, cgoErr := fts5Query(t, "sqlite3", exportedForOracle(t, goFile), q)
		wantOut, wantErr := fts5Query(t, "sqlite3", cgoFile, q)
		switch {
		case wantErr != nil:
			t.Fatalf("cgo cannot read its own file\n  sql: %s\n  err: %v", q, wantErr)
		case goErr != nil:
			t.Errorf("this engine cannot read back the file it wrote\n  sql: %s\n  err: %v", q, goErr)
		case cgoErr != nil:
			t.Errorf("C SQLite cannot read the file this engine wrote\n  sql: %s\n  err: %v", q, cgoErr)
		case goOut != wantOut:
			t.Errorf("this engine's file answers differently from C SQLite's\n  sql:  %s\n  ours: %q\n  want: %q", q, goOut, wantOut)
		case cgoOut != wantOut:
			t.Errorf("C SQLite reads our file differently from its own\n  sql:  %s\n  ours: %q\n  want: %q", q, cgoOut, wantOut)
		}
	}
	// fts5's own check recomputes the index from %_content, so it catches a
	// posting or %_docsize error every probe above misses.
	exported := exportedForOracle(t, goFile)
	ic, err := fts5Query(t, "sqlite3", exported, `PRAGMA integrity_check`)
	if err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if ic != "integrity_check\nT:ok" {
		t.Errorf("C SQLite reports integrity_check = %q on our file", ic)
	}
	if _, err := fts5Query(t, "sqlite3", exported, `INSERT INTO ft(ft) VALUES('integrity-check')`); err != nil {
		t.Errorf("fts5 integrity-check on our file: %v", err)
	}
}

// THE load-bearing case. A locale is only ever CREATED by fts5_locale(), which
// this engine declines -- so every locale it holds came out of a file real
// SQLite wrote, and losing one on a rewrite would be a silent data loss no
// query over this engine alone could see.
func TestR37BLocaleSurvivesThisEnginesWrites(t *testing.T) {
	seed := []string{
		`CREATE VIRTUAL TABLE ft USING fts5(a, b, locale=1)`,
		`INSERT INTO ft(rowid,a,b) VALUES(1,fts5_locale('en','one two'),fts5_locale('fr','three'))`,
		`INSERT INTO ft(rowid,a,b) VALUES(2,'four','five six')`,
	}
	probes := []string{
		`SELECT id, quote(c0), quote(c1), quote(l0), quote(l1) FROM ft_content ORDER BY id`,
		`SELECT rowid, a, b FROM ft ORDER BY rowid`,
		`SELECT id, quote(sz) FROM ft_docsize ORDER BY id`,
		`SELECT k, quote(v) FROM ft_config ORDER BY k`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM ft WHERE ft MATCH 'one' ORDER BY rowid)`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM ft WHERE ft MATCH 'five' ORDER BY rowid)`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM ft WHERE ft MATCH 'changed' ORDER BY rowid)`,
		`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
	}
	cases := []struct {
		name   string
		mutate []string
	}{
		{
			name: "update of one column clears only that locale",
			// Real fts5 binds l<i> from the SAVED ROW for every column the SET
			// list did not name and leaves it NULL for one it did
			// (fts5_storage.c:994) -- so l0 survives and l1 goes NULL.
			mutate: []string{`UPDATE ft SET b='changed' WHERE rowid=1`},
		},
		{
			name:   "update of an untouched row leaves both locales",
			mutate: []string{`UPDATE ft SET a='other' WHERE rowid=2`},
		},
		{
			name:   "delete of the locale-free row leaves the other's locales",
			mutate: []string{`DELETE FROM ft WHERE rowid=2`},
		},
		{
			name:   "insert beside a locale row adds a locale-free one",
			mutate: []string{`INSERT INTO ft(rowid,a,b) VALUES(3,'nine ten','eleven')`},
		},
		{
			name: "rowid change carries the locales with the row",
			// fts5 rewrites %_content under the new id; the l columns move with
			// the values they belong to.
			mutate: []string{`UPDATE ft SET rowid=5 WHERE rowid=1`},
		},
		{
			name:   "no write at all -- pure read-back",
			mutate: nil,
		},
		{
			// 'rebuild' re-derives the whole index from %_content, so it is the
			// one command that reads the l columns back and must put them back.
			name:   "rebuild re-derives the index and keeps the locales",
			mutate: []string{`INSERT INTO ft(ft) VALUES('rebuild')`},
		},
		{
			name:   "optimize leaves every row alone",
			mutate: []string{`INSERT INTO ft(ft) VALUES('optimize')`},
		},
		{
			// A ROLLBACK restores the store from vtabClone's copy, which has to
			// deep-copy the locales or the rollback puts back a row with none.
			name: "rollback restores the locales the transaction cleared",
			mutate: []string{
				`BEGIN`,
				`UPDATE ft SET a='wiped', b='wiped'`,
				`ROLLBACK`,
			},
		},
		{
			name: "committed transaction keeps the surviving locale",
			mutate: []string{
				`BEGIN`,
				`UPDATE ft SET b='changed' WHERE rowid=1`,
				`INSERT INTO ft(rowid,a,b) VALUES(4,'ten','eleven')`,
				`COMMIT`,
			},
		},
		{
			// Re-INSERTing at a rowid whose old row carried locales: the new row
			// has none, and nothing may be left over from the old one. Spelled
			// DELETE-then-INSERT rather than INSERT OR REPLACE only because this
			// engine still declines a conflict clause on a virtual table
			// (vtab_write.go, a separate gap).
			name: "reuse of a locale rowid leaves no locale behind",
			mutate: []string{
				`DELETE FROM ft WHERE rowid=1`,
				`INSERT INTO ft(rowid,a,b) VALUES(1,'fresh row','value')`,
			},
		},
		{
			name:   "delete of the locale row itself",
			mutate: []string{`DELETE FROM ft WHERE rowid=1`},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			r37bLockstepFiles(t, seed, c.mutate, probes)
		})
	}
}

// The inverse direction: a locale=1 table THIS ENGINE creates from nothing must
// be byte-for-byte the table C SQLite would have created from the same
// statements -- and must stay writable by it, locale value included.
//
// Same two-file discipline as r37bLockstepFiles, in the other order: this
// engine builds one file, C SQLite builds the other from the SAME
// statements, and only then does C SQLite write a locale into both.
func TestR37BLocaleFileInterchange(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "go.db")
	cgoFile := filepath.Join(dir, "cgo.db")
	build := []string{
		`CREATE VIRTUAL TABLE ft USING fts5(a, b UNINDEXED, locale=1)`,
		`INSERT INTO ft(rowid,a,b) VALUES(1,'hello world','foo')`,
		`INSERT INTO ft(rowid,a,b) VALUES(2,'second row','bar')`,
		`DELETE FROM ft WHERE rowid=2`,
		`INSERT INTO ft(rowid,a,b) VALUES(3,'hello again','baz')`,
	}
	if err := fts5Exec(t, "sqlite", goFile, build); err != nil {
		t.Fatalf("this engine writes: %v", err)
	}
	if err := fts5Exec(t, "sqlite3", cgoFile, build); err != nil {
		t.Fatalf("cgo writes: %v", err)
	}
	// C SQLite then writes BOTH -- including a locale only it can make. It writes
	// the EXPORT of this engine's file (RULE #3), which this engine then reads
	// back through an import.
	goFile = exportedForOracle(t, goFile)
	for _, f := range []string{goFile, cgoFile} {
		if err := fts5Exec(t, "sqlite3", f, []string{
			`INSERT INTO ft(rowid,a,b) VALUES(4,fts5_locale('de','vier fuenf'),'qux')`,
		}); err != nil {
			t.Fatalf("cgo writes %s: %v", filepath.Base(f), err)
		}
	}
	for _, q := range []string{
		`SELECT id, quote(c0), quote(c1), quote(l0) FROM ft_content ORDER BY id`,
		`SELECT rowid, a, b FROM ft ORDER BY rowid`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM ft WHERE ft MATCH 'hello' ORDER BY rowid)`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM ft WHERE ft MATCH 'vier' ORDER BY rowid)`,
		`SELECT id, quote(sz) FROM ft_docsize ORDER BY id`,
		`SELECT k, quote(v) FROM ft_config ORDER BY k`,
		`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
	} {
		goOut, goErr := fts5Query(t, "sqlite", importedForMusql(t, goFile), q)
		cgoOut, cgoErr := fts5Query(t, "sqlite3", goFile, q)
		wantOut, wantErr := fts5Query(t, "sqlite3", cgoFile, q)
		switch {
		case wantErr != nil:
			t.Fatalf("cgo cannot read its own file\n  sql: %s\n  err: %v", q, wantErr)
		case goErr != nil:
			t.Errorf("this engine cannot read back the file it wrote\n  sql: %s\n  err: %v", q, goErr)
		case cgoErr != nil:
			t.Errorf("C SQLite cannot read the file this engine wrote\n  sql: %s\n  err: %v", q, cgoErr)
		case goOut != wantOut:
			t.Errorf("this engine's file answers differently from C SQLite's\n  sql:  %s\n  ours: %q\n  want: %q", q, goOut, wantOut)
		case cgoOut != wantOut:
			t.Errorf("C SQLite reads our file differently from its own\n  sql:  %s\n  ours: %q\n  want: %q", q, cgoOut, wantOut)
		}
	}
	ic, err := fts5Query(t, "sqlite3", goFile, `PRAGMA integrity_check`)
	if err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if ic != "integrity_check\nT:ok" {
		t.Errorf("C SQLite reports integrity_check = %q", ic)
	}
}
