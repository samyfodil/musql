//go:build sqlite_fts5

// Gates fts5's two LOCALE FUNCTIONS: fts5_locale(LOCALE, TEXT) and
// fts5_get_locale(<table>, iCol). Runs through flLockstep rather than differ()
// because -tags sqlite_fts5 doesn't reach differ()'s worker binaries.
package compat

import (
	"path/filepath"
	"testing"
)

// TestR38bLocaleValueSemantics verifies fts5_locale()'s value rules: NULL or
// empty locale returns TEXT as TEXT, and both args go through sqlite3_value_text.
func TestR38bLocaleValueSemantics(t *testing.T) {
	flExprParity(t,
		`SELECT typeof(fts5_locale(NULL,'xyz')), typeof(fts5_locale('','abc'))`,
		`SELECT fts5_locale(NULL,'xyz'), fts5_locale('','abc')`,
		`SELECT quote(fts5_locale(NULL,NULL)), typeof(fts5_locale(NULL,NULL))`,
		`SELECT fts5_locale(NULL,123), typeof(fts5_locale(NULL,123))`,
		`SELECT fts5_locale('',456.5)`,
		`SELECT typeof(fts5_locale('en','abc'))`,
		// The blob is <16-byte header><LOCALE>0x00<TEXT>: 16+2+1+3 = 22.
		`SELECT length(CAST(fts5_locale('en','abc') AS BLOB))`,
		`SELECT length(CAST(fts5_locale('en_US',NULL) AS BLOB))`,
		// Wrong arity is a plain rejection on both sides.
		`SELECT fts5_locale('en')`,
		`SELECT fts5_locale('en','a','b')`,
		`SELECT fts5_locale()`,
	)
}

// TestR38bLocaleRoundTrip verifies that locales written through fts5_locale()
// come back through fts5_get_locale(), the row TEXT is unwrapped, and the index
// holds the text's tokens since tokenizers are locale-blind.
func TestR38bLocaleRoundTrip(t *testing.T) {
	flLockstep(t, "locale round trip", []string{
		`CREATE VIRTUAL TABLE ft USING fts5(a, b, locale=1)`,
		`INSERT INTO ft VALUES(fts5_locale('th_TH', 'one two three'), 'four five six seven')`,
		`INSERT INTO ft VALUES('three two one', fts5_locale('en_AU', 'seven eight nine'))`,
	},
		`SELECT quote(a), quote(b) FROM ft ORDER BY rowid`,
		`SELECT quote(fts5_get_locale(ft, 0)), quote(fts5_get_locale(ft, 1)) FROM ft`,
		`SELECT quote(fts5_get_locale(ft, 0)), quote(fts5_get_locale(ft, 1)) FROM ft('one AND three')`,
		`SELECT quote(fts5_get_locale(ft, 0)), quote(fts5_get_locale(ft, 1)) FROM ft('one AND three') ORDER BY rank`,
		`SELECT quote(fts5_get_locale(ft, 0)), quote(fts5_get_locale(ft, 1)) FROM ft('one AND three') ORDER BY rowid`,
		// A locale is not allowed to shift the token stream.
		`SELECT rowid FROM ft('one') ORDER BY rowid`,
		`SELECT rowid FROM ft('seven') ORDER BY rowid`,
		`SELECT rowid FROM ft('th_TH') ORDER BY rowid`,
		// ...nor the shadow tables. %_content grows l0/l1; %_docsize and
		// %_config must be exactly a locale=0 table's.
		`SELECT * FROM ft_content ORDER BY id`,
		`SELECT * FROM ft_docsize ORDER BY id`,
		`SELECT k, quote(v) FROM ft_config ORDER BY k`,
	)
}

// TestR38bGetLocaleArgumentRules verifies fts5_get_locale()'s argument rules:
// column index is numeric_type (TEXT '0' accepted, TEXT '0.0' and REAL 0.0 rejected).
func TestR38bGetLocaleArgumentRules(t *testing.T) {
	flLockstep(t, "fts5_get_locale argument rules", []string{
		`CREATE VIRTUAL TABLE nolocale USING fts5(a, b)`,
		`INSERT INTO nolocale VALUES('one two three', 'four five six')`,
		`INSERT INTO nolocale VALUES('three two one', 'seven eight nine')`,
	},
		`SELECT fts5_get_locale(nolocale, 0) IS NULL FROM nolocale`,
		`SELECT fts5_get_locale(nolocale, 1) IS NULL FROM nolocale('one + two')`,
		`SELECT fts5_get_locale(nolocale, 0) IS NULL FROM nolocale('one AND two')`,
		`SELECT fts5_get_locale(nolocale, 1) IS NULL FROM nolocale('three AND two') ORDER BY rank`,
		`SELECT fts5_get_locale(nolocale, 2) IS NULL FROM nolocale('three AND two')`,
		`SELECT fts5_get_locale(nolocale, -1) IS NULL FROM nolocale('three AND two')`,
		`SELECT fts5_get_locale(nolocale) IS NULL FROM nolocale('three AND two')`,
		`SELECT fts5_get_locale(nolocale, 0, 0) IS NULL FROM nolocale('three AND two')`,
		`SELECT fts5_get_locale(nolocale, 'text') FROM nolocale('three AND two')`,
		`SELECT fts5_get_locale(nolocale, '0') IS NULL FROM nolocale('three AND two')`,
		`SELECT fts5_get_locale(nolocale, '0.0') IS NULL FROM nolocale('three AND two')`,
		`SELECT fts5_get_locale(nolocale, 0.0) IS NULL FROM nolocale('three AND two')`,
	)
}

// TestR38bLocaleRequiresLocale1 verifies that writing a locale value into a
// table without locale=1 errors before anything is stored.
func TestR38bLocaleRequiresLocale1(t *testing.T) {
	flLockstep(t, "fts5_locale() requires locale=1", []string{
		`CREATE VIRTUAL TABLE b2 USING fts5(x, y, locale=0)`,
		`INSERT INTO b2 VALUES('abc', 'one two three')`,
		`INSERT INTO b2 VALUES('def', fts5_locale('reverse', 'four five six'))`,
		`UPDATE b2 SET y = fts5_locale('en', 'changed')`,
	},
		`SELECT rowid, quote(x), quote(y) FROM b2 ORDER BY rowid`,
		`SELECT count(*) FROM b2`,
		`SELECT rowid FROM b2('four') ORDER BY rowid`,
		`SELECT rowid FROM b2('three') ORDER BY rowid`,
		// The value is still an ordinary blob everywhere OUTSIDE fts5: only an
		// fts5 write looks at the header at all.
		`SELECT typeof(fts5_locale('en','x'))`,
	)
}

// TestR38bLocaleUpdatePreservation verifies UPDATE preservation: kept columns
// retain their locale, SET columns take the given locale.
func TestR38bLocaleUpdatePreservation(t *testing.T) {
	flLockstep(t, "locale survives a partial UPDATE", []string{
		`CREATE VIRTUAL TABLE v1 USING fts5(a, b, locale=1)`,
		`INSERT INTO v1 VALUES(fts5_locale('en','one two'), fts5_locale('fr','three'))`,
		`INSERT INTO v1 VALUES(fts5_locale('de','four'), 'five')`,
		`UPDATE v1 SET b='changed' WHERE rowid=1`,
		`UPDATE v1 SET b=fts5_locale('es','six') WHERE rowid=2`,
	},
		`SELECT rowid, quote(a), quote(b) FROM v1 ORDER BY rowid`,
		`SELECT quote(fts5_get_locale(v1,0)), quote(fts5_get_locale(v1,1)) FROM v1 ORDER BY rowid`,
		`SELECT * FROM v1_content ORDER BY id`,
		`SELECT rowid FROM v1('changed') ORDER BY rowid`,
		`SELECT rowid FROM v1('six') ORDER BY rowid`,
	)
}

// TestR38bLocaleUnindexedAndNonText verifies that UNINDEXED columns drop
// locales and non-text values are stored verbatim.
func TestR38bLocaleUnindexedAndNonText(t *testing.T) {
	flLockstep(t, "unindexed columns and non-text values", []string{
		`CREATE VIRTUAL TABLE u1 USING fts5(a, b UNINDEXED, locale=1)`,
		`INSERT INTO u1 VALUES(fts5_locale('en','hello'), fts5_locale('fr','world'))`,
		`INSERT INTO u1 VALUES(X'abcd', X'1234')`,
		`INSERT INTO u1 VALUES(NULL, 'null')`,
		`INSERT INTO u1 VALUES(123, 'int')`,
		`INSERT INTO u1 VALUES(345.0, 'real')`,
	},
		`SELECT rowid, quote(a), quote(b) FROM u1 ORDER BY rowid`,
		`SELECT quote(fts5_get_locale(u1,0)), quote(fts5_get_locale(u1,1)) FROM u1 ORDER BY rowid`,
		`SELECT * FROM u1_content ORDER BY id`,
		`SELECT rowid FROM u1('hello') ORDER BY rowid`,
		`SELECT rowid FROM u1('world') ORDER BY rowid`,
	)
}

// TestR38bLocaleAsMatchPattern verifies that a MATCH pattern can be a locale
// value and is unwrapped correctly.
func TestR38bLocaleAsMatchPattern(t *testing.T) {
	flLockstep(t, "a locale value as the MATCH query", []string{
		`CREATE VIRTUAL TABLE q1 USING fts5(a, locale=1)`,
		`CREATE VIRTUAL TABLE q0 USING fts5(a)`,
		`INSERT INTO q1 VALUES(fts5_locale('en_US','one two three'))`,
		`INSERT INTO q1 VALUES('four five six')`,
		`INSERT INTO q0 VALUES('one two three')`,
		`INSERT INTO q0 VALUES('four five six')`,
	},
		`SELECT rowid FROM q1 WHERE q1 MATCH fts5_locale('en_US','two') ORDER BY rowid`,
		`SELECT rowid FROM q1 WHERE q1 MATCH fts5_locale('','five') ORDER BY rowid`,
		`SELECT rowid FROM q1 WHERE q1 MATCH fts5_locale(NULL,'five') ORDER BY rowid`,
		`SELECT rowid FROM q1 WHERE q1 MATCH fts5_locale('xx','one AND three') ORDER BY rowid`,
		// The same query over a locale=0 table: fts5ExtractExprText unwraps it
		// there too, with no bLocale guard at all.
		`SELECT rowid FROM q0 WHERE q0 MATCH fts5_locale('en_US','two') ORDER BY rowid`,
		`SELECT rowid FROM q0 WHERE q0 MATCH fts5_locale('reverse','five') ORDER BY rowid`,
		`SELECT rowid FROM q1('one AND three') ORDER BY rowid`,
	)
}

// TestR38bLocaleTruncatedValue verifies that a header-carrying blob without
// the 0x00 terminator errors in sqlite3Fts5DecodeLocaleValue.
func TestR38bLocaleTruncatedValue(t *testing.T) {
	flLockstep(t, "a header-carrying blob with no terminator", []string{
		`CREATE VIRTUAL TABLE tr USING fts5(a, locale=1)`,
		`INSERT INTO tr VALUES('first row')`,
		// 17 bytes: the whole header plus one non-NUL byte.
		`INSERT INTO tr VALUES(substr(CAST(fts5_locale('en','x') AS BLOB),1,17))`,
		// The header alone is NOT long enough to be a locale value at all
		// (the C's test is strictly ">"), so this one is stored as a plain
		// blob. Its BYTES are the two engines' respective random headers and
		// so cannot agree by construction -- only its type and length are
		// comparable, which is exactly what "stored verbatim" means here.
		`INSERT INTO tr VALUES(substr(CAST(fts5_locale('en','x') AS BLOB),1,16))`,
	},
		`SELECT count(*) FROM tr`,
		`SELECT rowid, typeof(a), length(CAST(a AS BLOB)) FROM tr ORDER BY rowid`,
		`SELECT id, typeof(c0), length(CAST(c0 AS BLOB)), quote(l0) FROM tr_content ORDER BY id`,
		`SELECT rowid FROM tr('first') ORDER BY rowid`,
	)
}

// TestR38bLocaleExternalContent verifies that external-content tables store
// locales inside content values, and fts5TextFromStmt unwraps them on read.
func TestR38bLocaleExternalContent(t *testing.T) {
	flLockstep(t, "external content + locale=1", []string{
		`CREATE TABLE c(id INTEGER PRIMARY KEY, a, b)`,
		`CREATE VIRTUAL TABLE ft USING fts5(a, b, content=c, content_rowid=id, locale=1)`,
		`INSERT INTO c VALUES(1, fts5_locale('en','one two three'), 'plain')`,
		`INSERT INTO c VALUES(2, 'four five six', fts5_locale('fr','sept huit'))`,
		`INSERT INTO c VALUES(3, 'seven eight nine', 'also plain')`,
		`INSERT INTO ft(ft) VALUES('rebuild')`,
		`CREATE VIRTUAL TABLE ft_v USING fts5vocab('ft', row)`,
	},
		`SELECT rowid, quote(a), quote(b) FROM ft ORDER BY rowid`,
		`SELECT rowid, quote(fts5_get_locale(ft,0)), quote(fts5_get_locale(ft,1)) FROM ft ORDER BY rowid`,
		`SELECT rowid FROM ft('two') ORDER BY rowid`,
		`SELECT rowid FROM ft('huit') ORDER BY rowid`,
		`SELECT rowid FROM ft('nine') ORDER BY rowid`,
		`SELECT term, doc FROM ft_v ORDER BY term`,
	)
	flLockstep(t, "external content + locale=0 keeps the blob", []string{
		`CREATE TABLE c0(id INTEGER PRIMARY KEY, a)`,
		`CREATE VIRTUAL TABLE f0 USING fts5(a, content=c0, content_rowid=id)`,
		`INSERT INTO c0 VALUES(1, fts5_locale('en','one two three'))`,
		`INSERT INTO c0 VALUES(2, 'four five six')`,
		`INSERT INTO f0(f0) VALUES('rebuild')`,
	},
		// typeof/length only: the blob's bytes are each implementation's own
		// random header and can never agree. That it is still a BLOB of the
		// full wrapped length is the whole assertion.
		`SELECT rowid, typeof(a), length(CAST(a AS BLOB)) FROM f0 ORDER BY rowid`,
		`SELECT rowid FROM f0('five') ORDER BY rowid`,
		`SELECT rowid, quote(fts5_get_locale(f0,0)) FROM f0 ORDER BY rowid`,
	)
}

// TestR38bLocaleFileInterchange verifies both-directions file interchange: a
// file C SQLite wrote with locales must read back through fts5_get_locale(),
// and vice versa.
func TestR38bLocaleFileInterchange(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "go.db")
	cgoFile := filepath.Join(dir, "cgo.db")
	// The WHOLE script runs on both, fts5_locale() included -- which is the
	// difference from r37b's interchange gate, where only C SQLite could
	// make a locale and this engine's file had to be handed to it to be given
	// one. Both writers now produce the l<i> columns themselves.
	build := []string{
		`CREATE VIRTUAL TABLE ft USING fts5(a, b, locale=1)`,
		`INSERT INTO ft(rowid,a,b) VALUES(1,fts5_locale('th_TH','one two three'),'plain')`,
		`INSERT INTO ft(rowid,a,b) VALUES(2,'bare',fts5_locale('en_AU','seven eight'))`,
		`INSERT INTO ft(rowid,a,b) VALUES(3,'third row','also plain')`,
		`UPDATE ft SET b='changed' WHERE rowid=1`,
		`DELETE FROM ft WHERE rowid=3`,
	}
	if err := fts5Exec(t, "sqlite", goFile, build); err != nil {
		t.Fatalf("this engine writes: %v", err)
	}
	if err := fts5Exec(t, "sqlite3", cgoFile, build); err != nil {
		t.Fatalf("cgo writes: %v", err)
	}
	for _, q := range []string{
		`SELECT id, quote(c0), quote(c1), quote(l0), quote(l1) FROM ft_content ORDER BY id`,
		`SELECT rowid, a, b FROM ft ORDER BY rowid`,
		`SELECT rowid, quote(fts5_get_locale(ft,0)), quote(fts5_get_locale(ft,1)) FROM ft ORDER BY rowid`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM ft WHERE ft MATCH 'one' ORDER BY rowid)`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM ft WHERE ft MATCH 'changed' ORDER BY rowid)`,
		`SELECT id, quote(sz) FROM ft_docsize ORDER BY id`,
		`SELECT k, quote(v) FROM ft_config ORDER BY k`,
	} {
		goOut, goErr := fts5Query(t, "sqlite", goFile, q)
		cgoOut, cgoErr := fts5Query(t, "sqlite3", exportedForOracle(t, goFile), q) // RULE #3: C reads the export
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
	exported := exportedForOracle(t, goFile)
	ic, err := fts5Query(t, "sqlite3", exported, `PRAGMA integrity_check`)
	if err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if ic != "integrity_check\nT:ok" {
		t.Errorf("C SQLite reports integrity_check = %q", ic)
	}
	if _, err := fts5Query(t, "sqlite3", exported, `INSERT INTO ft(ft) VALUES('integrity-check')`); err != nil {
		t.Errorf("C SQLite's fts5 integrity-check over the file this engine wrote: %v", err)
	}
}
