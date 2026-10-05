//go:build sqlite_fts5

// Tests fts5 table-valued function argument handling.
// t1()" (vtab.go's resolveVtabModuleForItem), because a persisted "CREATE
// VIRTUAL TABLE ... USING fts5" table is never an eponymous module. See
// fts5TVFPatternArg's own doc comment (fts5_tablefunc.go) for the exact rule
// and its C citations; this file pins the corpus statements themselves,
// against the real oracle.
package compat

import (
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestFts5TVFNonTextLiteralArg gates fts5misc.test#18 stmt5's exact shape: a
// TVF call argument that is a non-negative INTEGER literal, over a
// trigram-tokenized table (chosen because it is the corpus's own fixture).
//
// REAL and BLOB literals, and a negative INTEGER, are deliberately NOT
// covered here even though they were probed: this engine's OWN fts5 query
// lexer wrongly ACCEPTS a coerced pattern C fts5 rejects whenever it
// contains a "-" or a ".", reachable ALREADY via a plain "t MATCH -5" /
// "t MATCH 3.5" with no TVF call involved at all -- a pre-existing,
// unrelated gap this rewrite must not gain a new way to reach. See
// fts5TVFPatternArg's own doc comment (fts5_tablefunc.go).
func TestFts5TVFNonTextLiteralArg(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t0 USING fts5(c0, t="trigram")`,
		`INSERT INTO t0 VALUES('assertionfaultproblem')`,
	}
	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		if err := fts5Exec(t, drv, dsn[drv], setup); err != nil {
			t.Fatalf("%s setup: %v", drv, err)
		}
	}
	for _, q := range []string{
		// The exact mined corpus statement (fts5misc.test#18 stmt5).
		`SELECT 0 FROM t0(0) WHERE c0 GLOB 0`,
		// A non-zero non-negative INTEGER, to pin the general rule (not just
		// the one corpus value 0).
		`SELECT 0 FROM t0(123) WHERE rowid=1`,
	} {
		goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
		cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
		switch {
		case goErr != nil && cgoErr != nil:
		case goErr != nil:
			t.Errorf("this engine declined a non-text-literal TVF call C fts5 answers\n  sql: %s\n  err: %v\n  cgo: %s", q, goErr, cgoOut)
		case cgoErr != nil:
			t.Errorf("this engine ACCEPTED a TVF call C fts5 rejects\n  sql: %s\n  go:  %s\n  cgo err: %v", q, goOut, cgoErr)
		case goOut != cgoOut:
			t.Errorf("non-text-literal TVF call DIVERGES from C fts5\n  sql: %s\n  go:  %q\n  cgo: %q", q, goOut, cgoOut)
		}
	}
}

// TestFts5TVFLocaleArg gates fts5faultI.test#0 stmt3's exact shape: a TVF
// call argument that is an fts5_locale(<locale-lit>, <text-lit>) call, over a
// locale=1 table -- and confirms the rewrite finds the REAL matching row
// (rowid 1), not just an empty result.
func TestFts5TVFLocaleArg(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t1 USING fts5(x, locale=1)`,
		`INSERT INTO t1 VALUES('origintext unicode61 ascii porter trigram')`,
		`INSERT INTO t1 VALUES('unrelated content entirely')`,
	}
	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		if err := fts5Exec(t, drv, dsn[drv], setup); err != nil {
			t.Fatalf("%s setup: %v", drv, err)
		}
	}
	for _, q := range []string{
		// The exact mined corpus statement (fts5faultI.test#0 stmt3).
		`SELECT rowid FROM t1(fts5_locale('en_US', 'origintext'))`,
		// A miss, through the same call spelling.
		`SELECT rowid FROM t1(fts5_locale('en_US', 'nosuchterm'))`,
		// Through an alias -- pins that the rewritten MATCH is qualified by
		// the alias, exactly like the plain-literal form already is.
		`SELECT x1.rowid FROM t1(fts5_locale('en_US', 'origintext')) AS x1`,
		// fts5_locale() with a NULL locale (still a provably non-NULL
		// RESULT, per fts5LocaleFunc's own doc comment: a NULL/empty locale
		// yields the text unchanged) -- pins that the "recurse one level"
		// rule isn't accidentally scoped to a non-NULL locale argument only.
		`SELECT rowid FROM t1(fts5_locale(NULL, 'origintext'))`,
	} {
		goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
		cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
		switch {
		case goErr != nil && cgoErr != nil:
		case goErr != nil:
			t.Errorf("this engine declined an fts5_locale() TVF call C fts5 answers\n  sql: %s\n  err: %v\n  cgo: %s", q, goErr, cgoOut)
		case cgoErr != nil:
			t.Errorf("this engine ACCEPTED an fts5_locale() TVF call C fts5 rejects\n  sql: %s\n  go:  %s\n  cgo err: %v", q, goOut, cgoErr)
		case goOut != cgoOut:
			t.Errorf("fts5_locale() TVF call DIVERGES from C fts5\n  sql: %s\n  go:  %q\n  cgo: %q", q, goOut, cgoOut)
		}
	}
}

// TestFts5TVFArgStillDeclined pins the shapes fts5TVFPatternArg must keep
// refusing: C SQLite is either a hard error or -- for the two locale=1
// NULL-argument shapes -- SILENTLY EMPTY, never something this engine could
// safely reach by rewriting into an ordinary MATCH (see fts5RewriteTableFunc's
// case-1 comment on why a NULL pattern can't be told apart from that here).
// A DECLINE on this engine's side is always safe (never counted as WRONG),
// whichever of those two the oracle does -- this only pins that it keeps
// declining rather than silently answering.
func TestFts5TVFArgStillDeclined(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t1 USING fts5(x, locale=1)`,
		`INSERT INTO t1 VALUES('origintext unicode61 ascii porter trigram')`,
	}
	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		if err := fts5Exec(t, drv, dsn[drv], setup); err != nil {
			t.Fatalf("%s setup: %v", drv, err)
		}
	}
	for _, q := range []string{
		`SELECT rowid FROM t1(NULL)`,
		`SELECT rowid FROM t1(fts5_locale(NULL, NULL))`,
		// A column reference and a non-literal, non-fts5_locale expression:
		// never provably NULL-free, and not the one recursion this rewrite
		// allows.
		`SELECT rowid FROM t1(x)`,
		`SELECT rowid FROM t1(upper('a'))`,
		// REAL, negative INTEGER, and BLOB literals: excluded on purpose
		// (fts5TVFPatternArg's own doc comment) -- not because the TVF
		// spelling itself is unsafe for them, but because this engine's own
		// fts5 query lexer has a pre-existing, unrelated gap that a "-" or
		// "." in the coerced pattern text would newly expose here.
		`SELECT rowid FROM t1(3.5)`,
		`SELECT rowid FROM t1(-5)`,
		`SELECT rowid FROM t1(x'616263')`,
	} {
		_, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
		if goErr == nil {
			t.Errorf("this engine ACCEPTED a TVF call it must keep declining (unprovable NULL-freedom, or a REAL/negative-INTEGER/BLOB literal excluded for the pre-existing query-lexer gap)\n  sql: %s", q)
		}
	}
}
