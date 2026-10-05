//go:build sqlite_fts5

// Tests fts5's detail= modes (none, columns, full), verifying shadow table bytes
// and query behavior against C SQLite.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// fts5R31Schemas varies column count, UNINDEXED columns, columnsize, prefix, and detail mode.
var fts5R31Schemas = []struct {
	name   string
	create string
	// detail is what the CREATE means, used only to label the subtests.
	detail string
}{
	{"none one column", `CREATE VIRTUAL TABLE t USING fts5(a, detail=none)`, "none"},
	{"none three columns", `CREATE VIRTUAL TABLE t USING fts5(a, b, c, detail=none)`, "none"},
	{"none detail first", `CREATE VIRTUAL TABLE t USING fts5(detail=none, a, b)`, "none"},
	{"none quoted", `CREATE VIRTUAL TABLE t USING fts5(a, b, detail='none')`, "none"},
	{"none abbreviated", `CREATE VIRTUAL TABLE t USING fts5(a, b, detail=n)`, "none"},
	{"none unindexed", `CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, c, detail=none)`, "none"},
	{"none columnsize=0", `CREATE VIRTUAL TABLE t USING fts5(a, b, detail=none, columnsize=0)`, "none"},
	{"none prefix", `CREATE VIRTUAL TABLE t USING fts5(a, b, detail=none, prefix='1 3')`, "none"},
	{"none ascii", `CREATE VIRTUAL TABLE t USING fts5(a, b, detail=none, tokenize=ascii)`, "none"},

	{"columns one column", `CREATE VIRTUAL TABLE t USING fts5(a, detail=columns)`, "columns"},
	{"columns three columns", `CREATE VIRTUAL TABLE t USING fts5(a, b, c, detail=columns)`, "columns"},
	{"columns abbreviated col", `CREATE VIRTUAL TABLE t USING fts5(a, b, c, detail=col)`, "columns"},
	{"columns detail first", `CREATE VIRTUAL TABLE t USING fts5(detail=columns, a, b)`, "columns"},
	{"columns unindexed", `CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, c, detail=columns)`, "columns"},
	{"columns columnsize=0", `CREATE VIRTUAL TABLE t USING fts5(a, b, detail=columns, columnsize=0)`, "columns"},
	{"columns prefix", `CREATE VIRTUAL TABLE t USING fts5(a, b, detail=col, prefix=2)`, "columns"},
	{"columns ascii", `CREATE VIRTUAL TABLE t USING fts5(a, b, detail=col, tokenize=ascii)`, "columns"},

	// detail=full spelled explicitly and by prefix, so the same matrix runs
	// over the mode that must NOT have changed.
	{"full explicit", `CREATE VIRTUAL TABLE t USING fts5(a, b, c, detail=full)`, "full"},
	{"full abbreviated", `CREATE VIRTUAL TABLE t USING fts5(a, b, c, detail=f)`, "full"},
	{"full default", `CREATE VIRTUAL TABLE t USING fts5(a, b, c)`, "full"},
}

// fts5R31Fill is a single INSERT that fills whichever of the schemas above is
// in play (they declare one, two or three columns), chosen by column count.
// One statement, so both engines hold ONE segment and their %_data bytes are
// directly comparable (fts5_diff_test.go's TestFts5ShadowLayoutDiff comment).
//
// The data deliberately puts the SAME term in several columns of one row and
// several times within one column: those are exactly the two cases the
// detail=columns collist collapses ("a column is written only when it changes")
// and detail=none drops entirely.
func fts5R31Fill(nCol int) string {
	rows := [][]string{
		{"'shared alpha alpha'", "'shared beta'", "'gamma shared'"},
		{"'alpha only'", "'nothing here'", "'alpha'"},
		{"'zulu'", "'zulu zulu zulu'", "'zulu'"},
		{"''", "'lonely'", "''"},
	}
	cols := []string{"a", "b", "c"}[:nCol]
	var vals []string
	for i, r := range rows {
		vals = append(vals, fmt.Sprintf("(%d,%s)", i+1, strings.Join(r[:nCol], ",")))
	}
	return fmt.Sprintf("INSERT INTO t(rowid,%s) VALUES%s", strings.Join(cols, ","), strings.Join(vals, ","))
}

// fts5R31NCol counts the schema's declared columns by looking for b/c in the
// CREATE -- crude, but this file's CREATEs are all written right here.
func fts5R31NCol(create string) int {
	switch {
	case strings.Contains(create, " c,") || strings.Contains(create, " c)") || strings.Contains(create, "c, detail"):
		return 3
	case strings.Contains(create, " b,") || strings.Contains(create, " b)") || strings.Contains(create, "b UNINDEXED") || strings.Contains(create, "b, detail"):
		return 2
	}
	return 1
}

// TestFts5R31DetailShadowBytes pins the doclist bytes for a table built by one
// statement: same %_data, %_idx, %_content, %_docsize, %_config and
// sqlite_master as C fts5, byte for byte, in every detail= mode.
func TestFts5R31DetailShadowBytes(t *testing.T) {
	for _, sc := range fts5R31Schemas {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			stmts := []string{sc.create, fts5R31Fill(fts5R31NCol(sc.create))}
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			for _, q := range fts5ShadowDump {
				// %_docsize does not exist under columnsize=0.
				if strings.Contains(q, "t_docsize") && strings.Contains(sc.create, "columnsize=0") {
					continue
				}
				goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  over this engine's file: %v\n  over C SQLite's:     %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("%s DIVERGES\n  sql: %s\n%s", sc.name, q, fts5DiffLines(goOut, cgoOut))
				}
			}
		})
	}
}

// TestFts5R31DetailBigSegmentBytes is the same byte claim over a MULTI-PAGE
// segment, where the doclist splits across leaf pages. detail=none's entries
// are a single varint each, so its pages pack far more of them per page than
// detail=full's -- a different split arithmetic, and the one
// fts5_index.go's appendPoslistData comment warns C fts5 still READS
// correctly while reporting a checksum mismatch.
func TestFts5R31DetailBigSegmentBytes(t *testing.T) {
	// Every LEAF page, in id order. The one %_data address space left out is
	// the DOCLIST INDEX (bit 36 of the id, 2^36 == 68719476736), which real
	// fts5 writes for a doclist that spans leaf pages and this engine
	// deliberately never writes -- engine/fts5_index.go's package comment says
	// why, and the interchange half of this file is what keeps that honest:
	// C SQLite must still MATCH and integrity_check the result. The
	// fixtures the older byte gate used never spanned enough pages to reach a
	// doclist index at all, which is why this exclusion is new here.
	//
	// %_idx's pgno carries that same omission in its LOW BIT ("pgno ==
	// (leaf pgno << 1) | hasDoclistIndex"), so the comparison here is over
	// pgno/2 -- the leaf each separator points at, which must match exactly.
	dump := make([]string, len(fts5ShadowDump))
	copy(dump, fts5ShadowDump)
	dump[0] = `SELECT id, quote(block) FROM t_data WHERE (id & 68719476736)=0 ORDER BY id`
	dump[1] = `SELECT quote(segid), quote(term), quote(pgno/2) FROM t_idx ORDER BY segid, term`

	for _, detail := range []string{"none", "columns", "full"} {
		detail := detail
		t.Run(detail, func(t *testing.T) {
			stmts := []string{
				fmt.Sprintf(`CREATE VIRTUAL TABLE t USING fts5(a, b, detail=%s)`, detail),
				fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 3000, func(i int) string {
					return fmt.Sprintf("(%d,'qqq word%04d','col%d qqq')", i+1, i, i%7)
				}),
			}
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			for _, q := range dump {
				goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  over this engine's file: %v\n  over C SQLite's: %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("detail=%s DIVERGES\n  sql: %s\n%s", detail, q, fts5DiffLines(goOut, cgoOut))
				}
			}
			// ...and the segment this engine wrote must still be one real
			// SQLite can search and validate, doclist index or not.
			for _, q := range []string{
				`SELECT count(*) FROM t WHERE t MATCH 'qqq'`,
				`SELECT count(*) FROM t WHERE t MATCH 'col3'`,
				`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'word2999' ORDER BY rowid)`,
				`PRAGMA integrity_check`,
			} {
				goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s: C SQLite over this engine's file: %v / over its own: %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("detail=%s: C SQLite reads this engine's big segment differently\n  sql: %s\n  over go's file:  %q\n  over its own:    %q", detail, q, goOut, cgoOut)
				}
			}
		})
	}
}

// fts5R31InterchangeProbes are read back over the SAME file by both engines,
// so they may only use queries every detail= mode can answer -- a phrase or a
// column filter is an ERROR under detail=none, which is what
// TestFts5R31DetailQueries covers instead.
var fts5R31InterchangeProbes = []string{
	`SELECT count(*) FROM t`,
	`SELECT group_concat(rowid) FROM (SELECT rowid FROM t ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'shared' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'alpha' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'zulu' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'alph*' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'shared AND gamma' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'alpha OR lonely' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'shared NOT gamma' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'nosuchterm')`,
	`SELECT k, quote(v) FROM t_config ORDER BY k`,
	`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
}

// TestFts5R31DetailInterchange is the load-bearing one: whichever engine wrote
// the file, both must read the same answers out of it, and C SQLite --
// which never saw it written -- must report integrity_check "ok". That is what
// checks the doclist against the content rather than merely parsing it.
func TestFts5R31DetailInterchange(t *testing.T) {
	for _, sc := range fts5R31Schemas {
		sc := sc
		if strings.Contains(sc.create, "prefix=") {
			// A prefix index is a second term space in the same segment; it is
			// covered by the byte gate above, and its extra terms make the
			// probe list below no more discriminating.
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			for _, writer := range []string{"sqlite", "sqlite3"} {
				dsn := filepath.Join(t.TempDir(), "fts5.db")
				stmts := []string{sc.create, fts5R31Fill(fts5R31NCol(sc.create))}
				if err := fts5Exec(t, writer, dsn, stmts); err != nil {
					t.Fatalf("%s writes: %v", writer, err)
				}
				// Each engine reads its OWN format, with the converter in between where
				// the writer was the other one (convert_for_oracle_test.go).
				goPath, cgoPath := pathsForBothEngines(t, writer, dsn)

				for _, q := range fts5R31InterchangeProbes {
					goOut, goErr := fts5Query(t, "sqlite", goPath, q)
					cgoOut, cgoErr := fts5Query(t, "sqlite3", cgoPath, q)
					switch {
					case goErr != nil:
						t.Errorf("[%s wrote] this engine cannot read it back\n  sql: %s\n  err: %v", writer, q, goErr)
					case cgoErr != nil:
						t.Errorf("[%s wrote] C SQLite cannot read it\n  sql: %s\n  err: %v", writer, q, cgoErr)
					case goOut != cgoOut:
						t.Errorf("[%s wrote] the two engines read it differently\n  sql: %s\n  go:  %q\n  cgo: %q", writer, q, goOut, cgoOut)
					}
				}
				ic, err := fts5Query(t, "sqlite3", oraclePathFor(t, writer, dsn), `PRAGMA integrity_check`)
				if err != nil {
					t.Fatalf("[%s wrote] integrity_check: %v", writer, err)
				}
				if ic != "integrity_check\nT:ok" {
					t.Errorf("[%s wrote] C SQLite reports integrity_check = %q", writer, ic)
				}
			}
		})
	}
}

// fts5R31Queries is the QUERY axis. Every one of these is legal under
// detail=full; which of them fts5 refuses under detail=none and detail=columns
// is exactly what the two rules ported from sqlite3Fts5ParseSetColset and
// sqlite3Fts5ParseNode decide -- and this test never states which, it asks the
// oracle.
var fts5R31Queries = []string{
	// Single terms and boolean combinations: legal everywhere.
	`shared`,
	`alpha`,
	`alph*`,
	`shared AND alpha`,
	`shared OR lonely`,
	`shared NOT gamma`,
	`(shared OR zulu) AND alpha`,
	`nosuchterm`,
	// Implicit AND of two single terms: still one phrase per nearset.
	`shared alpha`,
	// Multi-term PHRASEs -- sqlite3Fts5ParseNode's "phrase" refusal.
	`"shared alpha"`,
	`shared+alpha`,
	`"alpha alpha"`,
	`"shared alpha" OR zulu`,
	// The initial-token operator, whose bFirst is the third clause of the
	// same refusal even with a single term.
	`^shared`,
	`^"shared alpha"`,
	`^zulu`,
	// NEAR sets -- the same refusal, reported as "NEAR" when the set holds
	// more than one phrase and as "phrase" when it holds exactly one.
	`NEAR(shared alpha)`,
	`NEAR(shared alpha, 2)`,
	`NEAR("shared alpha")`,
	`NEAR(shared)`,
	// Column filters -- sqlite3Fts5ParseSetColset's refusal, in all three
	// spellings the grammar has.
	`a:shared`,
	`b:shared`,
	`{a b}:shared`,
	`{a}:alpha`,
	`a:shared OR b:beta`,
	// NOT `-{a}:shared` / `-a:shared`: the MINUS colset-INVERT productions
	// (fts5parse.y's `colset ::= MINUS LCP colsetlist RCP`) are declined by
	// this engine's query lexer in EVERY detail mode, which fts5_query.go's
	// package comment already records. Leaving it in this matrix would report
	// a pre-existing gap outside this bucket once per schema.
	// A column filter in front of a parenthesized group, the other
	// sqlite3Fts5ParseSetColset call site.
	`a:(shared OR alpha)`,
	`{a b}:(shared AND alpha)`,
	// Both rules violated at once: which error wins is decided by
	// fts5parse.y running ParseNode before ParseSetColset.
	`a:"shared alpha"`,
	`a:NEAR(shared alpha)`,
	`{a b}:^shared`,
}

// fts5R31QueryForms wraps a MATCH query in each of the SQL shapes that reach
// fts5 differently: the table form, the COLUMN form (whose implicit colset is
// a third sqlite3Fts5ParseSetColset call site), and the aux-function form
// (which re-parses the same query, engine/fts5_vdbe_aux.go).
var fts5R31QueryForms = []struct {
	name string
	sql  func(q string) string
}{
	{"table MATCH", func(q string) string {
		return fmt.Sprintf(`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH '%s' ORDER BY rowid)`, q)
	}},
	{"column MATCH", func(q string) string {
		return fmt.Sprintf(`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE a MATCH '%s' ORDER BY rowid)`, q)
	}},
	{"highlight", func(q string) string {
		return fmt.Sprintf(`SELECT ifnull(group_concat(highlight(t,0,'[',']')),'') FROM (SELECT * FROM t WHERE t MATCH '%s' ORDER BY rowid)`, q)
	}},
	{"bm25 order", func(q string) string {
		return fmt.Sprintf(`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH '%s' ORDER BY rank, rowid)`, q)
	}},
}

// TestFts5R31DetailQueries is the answer/refusal matrix. For each schema and
// each query it compares this engine's outcome with the oracle's: both must
// error, or both must return the same rows. A query that errors on one side
// only is the failure -- in one direction a wrong answer where fts5 refuses,
// in the other a decline where fts5 answers.
func TestFts5R31DetailQueries(t *testing.T) {
	for _, sc := range fts5R31Schemas {
		sc := sc
		if strings.Contains(sc.create, "prefix=") || strings.Contains(sc.create, "columnsize=0") {
			continue // covered by the byte gate; the answers do not depend on either
		}
		t.Run(sc.name, func(t *testing.T) {
			nCol := fts5R31NCol(sc.create)
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			stmts := []string{sc.create, fts5R31Fill(nCol)}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			for _, q := range fts5R31Queries {
				if nCol < 2 && (strings.Contains(q, "b:") || strings.Contains(q, "{a b}")) {
					continue
				}
				for _, form := range fts5R31QueryForms {
					stmt := form.sql(q)
					goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], stmt)
					cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], stmt)
					switch {
					case goErr != nil && cgoErr != nil:
						// Both refuse. Real fts5's message is the thing this
						// engine ported, so require its exact tail somewhere in
						// ours -- otherwise a refusal for an unrelated reason
						// would pass this arm silently.
						if !fts5R31SameRefusal(goErr.Error(), cgoErr.Error()) {
							t.Errorf("[%s] %s %q: both refuse but for different reasons\n  go:  %v\n  cgo: %v", sc.name, form.name, q, goErr, cgoErr)
						}
					case goErr != nil:
						t.Errorf("[%s] %s %q: this engine DECLINES a query C fts5 answers %q\n  err: %v", sc.name, form.name, q, cgoOut, goErr)
					case cgoErr != nil:
						t.Errorf("[%s] %s %q: this engine ANSWERS %q a query C fts5 REFUSES\n  cgo err: %v", sc.name, form.name, q, goOut, cgoErr)
					case goOut != cgoOut:
						t.Errorf("[%s] %s %q DIVERGES\n  go:  %q\n  cgo: %q", sc.name, form.name, q, goOut, cgoOut)
					}
				}
			}
		})
	}
}

// fts5R31SameRefusal reports whether the two engines refused for the same
// reason. It only insists on that when the ORACLE gave one of the two detail=
// messages this round ported; any other refusal (a column that does not exist,
// a syntax error, an aux-function scope limit this engine declines on its own)
// is left to the outcome comparison alone.
func fts5R31SameRefusal(goErr, cgoErr string) bool {
	for _, msg := range []string{
		"column queries are not supported (detail=none)",
		"phrase queries are not supported (detail!=full)",
		"NEAR queries are not supported (detail!=full)",
	} {
		if strings.Contains(cgoErr, msg) {
			return strings.Contains(goErr, msg)
		}
	}
	return true
}

// TestFts5R31DetailVocab compares every fts5vocab shape over every detail=
// mode. fts5vocab is a view of the INDEX, so it is the one reader whose
// ANSWERS the mode changes -- cnt and offset become NULL where the mode does
// not record them, and detail=none's 'col' shape collapses onto a single
// NULL-column row per term.
func TestFts5R31DetailVocab(t *testing.T) {
	for _, sc := range fts5R31Schemas {
		sc := sc
		if strings.Contains(sc.create, "prefix=") {
			continue // fts5vocab reads the main term space only
		}
		t.Run(sc.name, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			stmts := []string{sc.create, fts5R31Fill(fts5R31NCol(sc.create))}
			for _, kind := range []string{"row", "col", "instance"} {
				stmts = append(stmts, fmt.Sprintf(`CREATE VIRTUAL TABLE v%s USING fts5vocab(t, '%s')`, kind, kind))
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			probes := []string{
				`SELECT term, quote(doc), quote(cnt) FROM vrow ORDER BY term`,
				`SELECT term, quote(col), quote(doc), quote(cnt) FROM vcol ORDER BY term, col`,
				`SELECT term, quote(doc), quote(col), quote(offset) FROM vinstance ORDER BY term, doc, col, offset`,
				`SELECT count(*) FROM vrow`,
				`SELECT count(*) FROM vcol`,
				`SELECT count(*) FROM vinstance`,
				`SELECT rowid, term FROM vrow ORDER BY rowid`,
				`SELECT term, quote(doc), quote(cnt) FROM vrow WHERE term='shared'`,
				`SELECT term, quote(col), quote(doc), quote(cnt) FROM vcol WHERE term>'s' ORDER BY term, col`,
			}
			for _, q := range probes {
				goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				switch {
				case goErr != nil && cgoErr != nil:
				case goErr != nil:
					t.Errorf("[%s] %s: this engine DECLINES, C fts5 answers %q\n  err: %v", sc.name, q, cgoOut, goErr)
				case cgoErr != nil:
					t.Errorf("[%s] %s: this engine ANSWERS %q, C fts5 refuses: %v", sc.name, q, goOut, cgoErr)
				case goOut != cgoOut:
					t.Errorf("[%s] %s DIVERGES\n  go:  %q\n  cgo: %q", sc.name, q, goOut, cgoOut)
				}
			}
		})
	}
}

// fts5R31EmptyPhraseQueries are queries containing a phrase that tokenizes to
// NO TERMS. fts5 drops such a phrase rather than matching nothing with it
// (sqlite3Fts5ParseNearset / sqlite3Fts5ParseImplicitAnd), which is a rule
// about the PARSE and so is invisible to any query that has no empty phrase.
// Under the default tokenizer only an all-punctuation quoted string reaches it;
// under trigram every query shorter than three characters does, so both
// tokenizers are run below.
var fts5R31EmptyPhraseQueries = []string{
	`""`,
	`"+++"`,
	`""*`,
	`"+++"*`,
	`^"+++"`,
	`x:"+++"`,
	`{x y}:"+++"`,
	`"+++" AND hello`,
	`"+++" OR hello`,
	`hello NOT "+++"`,
	`"+++" NOT hello`,
	`"+++" + hello`,
	// the two shapes the drop rule is really about: an implicit AND, and a
	// NEAR set, each mixing an empty phrase with a real one
	`hello "+++"`,
	`"+++" hello`,
	`"+++" hello world`,
	`hello "+++" world`,
	`hello "+++" "***"`,
	`"+++" "***" hello`,
	`NEAR("+++" hello)`,
	`NEAR(hello "+++")`,
	`NEAR("+++" hello world)`,
	`NEAR(hello "+++" world, 3)`,
	`NEAR("+++" "***")`,
	`NEAR("+++")`,
	// ...and in combination with the operators around them
	`("+++" hello) OR world`,
	`world AND ("+++" hello)`,
	`x:("+++" hello)`,
	`hello "+++" AND world`,
	`hello AND "+++" world`,
}

// TestFts5R31EmptyPhrase pins the drop rule against the oracle, over both
// tokenizers and with both a matching and a non-matching row present, so that
// "dropped" and "matched nothing" are distinguishable outcomes.
func TestFts5R31EmptyPhrase(t *testing.T) {
	for _, tk := range []string{``, `, tokenize=trigram`, `, tokenize=ascii`} {
		tk := tk
		t.Run("tokenize"+tk, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			stmts := []string{
				fmt.Sprintf(`CREATE VIRTUAL TABLE t USING fts5(x, y%s)`, tk),
				`INSERT INTO t(rowid,x,y) VALUES(1,'hello world','beta')`,
				`INSERT INTO t(rowid,x,y) VALUES(2,'world only','hello')`,
				`INSERT INTO t(rowid,x,y) VALUES(3,'nothing','here')`,
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			for _, q := range fts5R31EmptyPhraseQueries {
				stmt := fmt.Sprintf(`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH '%s' ORDER BY rowid)`, q)
				goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], stmt)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], stmt)
				switch {
				case goErr != nil && cgoErr != nil:
				case goErr != nil:
					t.Errorf("%q: this engine DECLINES, C fts5 answers %q\n  err: %v", q, cgoOut, goErr)
				case cgoErr != nil:
					t.Errorf("%q: this engine ANSWERS %q, C fts5 refuses: %v", q, goOut, cgoErr)
				case goOut != cgoOut:
					t.Errorf("%q DIVERGES\n  go:  %q\n  cgo: %q", q, goOut, cgoOut)
				}
			}
		})
	}
}

// TestFts5R31IntegrityCheck compares fts5's own 'integrity-check' command
// across the schema axis. The command decodes %_data and compares it with a
// fresh tokenization of %_content (engine/fts5_decode.go), so it is the most
// discriminating single statement in this file -- and, unlike the byte gate,
// it runs over a table built by SEVERAL statements, where the two engines'
// segment layouts legitimately differ and only the postings must agree.
//
// It is self-verifying: whatever the oracle does with the statement, this
// engine must do. A decline where the oracle succeeds is a coverage gap; an
// acceptance where the oracle reports corruption would be far worse.
func TestFts5R31IntegrityCheck(t *testing.T) {
	creates := []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED)`,
		`CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED, b UNINDEXED)`,
		`CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED, b, c UNINDEXED)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, prefix=2)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, prefix='1 3')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, prefix=2)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, columnsize=0)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, detail=none)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, detail=columns)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, detail=none, prefix=1)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, detail=col, prefix="1")`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, detail=none)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize=ascii, prefix=2)`,
	}
	for _, create := range creates {
		create := create
		t.Run(create, func(t *testing.T) {
			stmts := []string{
				create,
				`INSERT INTO t(rowid,a,b) VALUES(1,'shared alpha alpha','shared beta')`,
				`INSERT INTO t(rowid,a,b) VALUES(2,'alpha only','nothing here')`,
				`INSERT INTO t(rowid,a,b) VALUES(3,'zulu','zulu zulu zulu')`,
				`INSERT INTO t(t) VALUES('integrity-check')`,
				`DELETE FROM t WHERE rowid=2`,
				`INSERT INTO t(t) VALUES('integrity-check')`,
				`UPDATE t SET a='rewritten shared' WHERE rowid=3`,
				`INSERT INTO t(rowid,a,b) VALUES(9,'late','shared late')`,
				`INSERT INTO t(t) VALUES('integrity-check')`,
				`INSERT INTO t(t) VALUES('rebuild')`,
				`INSERT INTO t(t) VALUES('integrity-check')`,
			}
			dir := t.TempDir()
			goErr := fts5Exec(t, "sqlite", filepath.Join(dir, "musql.db"), stmts)
			cgoErr := fts5Exec(t, "sqlite3", filepath.Join(dir, "cgo.db"), stmts)
			switch {
			case goErr != nil && cgoErr == nil:
				t.Errorf("this engine fails a statement C fts5 accepts\n  create: %s\n  err: %v", create, goErr)
			case goErr == nil && cgoErr != nil:
				t.Errorf("this engine ACCEPTS what C fts5 rejects\n  create: %s\n  cgo err: %v", create, cgoErr)
			}
		})
	}
}

// TestFts5R31DetailWriteMatch is the WRITE-path half of the refusal rules. A
// DELETE or UPDATE whose WHERE holds a MATCH is evaluated by
// engine/vtab_write.go, whose evalCtx carries no pager to look the table up
// in -- so the detail= mode has to be carried on the scope the same way the
// tokenizer already is. Without that, a phrase or column-filtered MATCH would
// be ANSWERED there (and rows really deleted) where C fts5 errors: a wrong
// answer with a side effect, which no read-side test can reach.
func TestFts5R31DetailWriteMatch(t *testing.T) {
	writes := []string{
		`DELETE FROM t WHERE t MATCH '"shared alpha"'`,
		`DELETE FROM t WHERE t MATCH 'a:shared'`,
		`DELETE FROM t WHERE t MATCH '^shared'`,
		`DELETE FROM t WHERE t MATCH 'NEAR(shared alpha)'`,
		`DELETE FROM t WHERE t MATCH 'shared'`,
		`UPDATE t SET b='x' WHERE t MATCH '"shared alpha"'`,
		`UPDATE t SET b='x' WHERE t MATCH '{a b}:shared'`,
		`UPDATE t SET b='x' WHERE t MATCH 'shared'`,
	}
	for _, detail := range []string{"none", "columns", "full"} {
		for _, w := range writes {
			detail, w := detail, w
			t.Run(detail+" "+w, func(t *testing.T) {
				dir := t.TempDir()
				dsn := map[string]string{
					"sqlite":  filepath.Join(dir, "musql.db"),
					"sqlite3": filepath.Join(dir, "cgo.db"),
				}
				setup := []string{
					fmt.Sprintf(`CREATE VIRTUAL TABLE t USING fts5(a, b, detail=%s)`, detail),
					fts5R31Fill(2),
				}
				for _, drv := range []string{"sqlite", "sqlite3"} {
					if err := fts5Exec(t, drv, dsn[drv], setup); err != nil {
						t.Fatalf("%s setup: %v", drv, err)
					}
				}
				goErr := fts5Exec(t, "sqlite", dsn["sqlite"], []string{w})
				cgoErr := fts5Exec(t, "sqlite3", dsn["sqlite3"], []string{w})
				switch {
				case goErr != nil && cgoErr == nil:
					t.Errorf("this engine refuses a write C fts5 performs: %v", goErr)
				case goErr == nil && cgoErr != nil:
					t.Errorf("this engine PERFORMED a write C fts5 refuses: %v", cgoErr)
				}
				// The surviving rows must agree either way -- an error is
				// allowed to leave the table alone, never half-written.
				const probe = `SELECT rowid, a, b FROM t ORDER BY rowid`
				goOut, _ := fts5Query(t, "sqlite", dsn["sqlite"], probe)
				cgoOut, _ := fts5Query(t, "sqlite3", dsn["sqlite3"], probe)
				if goOut != cgoOut {
					t.Errorf("rows after the write DIVERGE\n  go:  %q\n  cgo: %q", goOut, cgoOut)
				}
			})
		}
	}
}

// TestFts5R31DetailMutations runs the same comparison after DELETEs and
// UPDATEs, which is where the two engines' segment layouts diverge (C fts5
// appends a segment and writes DELETE MARKERS; this one re-encodes from
// %_content). The bytes are allowed to differ there -- the ANSWERS, the
// %_docsize/%_config rows and C SQLite's own integrity_check are not.
func TestFts5R31DetailMutations(t *testing.T) {
	for _, detail := range []string{"none", "columns", "full"} {
		detail := detail
		t.Run(detail, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			stmts := []string{
				fmt.Sprintf(`CREATE VIRTUAL TABLE t USING fts5(a, b, c, detail=%s)`, detail),
				fts5R31Fill(3),
				`DELETE FROM t WHERE rowid=2`,
				`UPDATE t SET a='rewritten shared' WHERE rowid=3`,
				`INSERT INTO t(rowid,a,b,c) VALUES(9,'late','shared late','zulu')`,
				`DELETE FROM t WHERE t MATCH 'lonely'`,
				`UPDATE t SET c='changed' WHERE t MATCH 'late'`,
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			probes := append([]string{
				`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
				`SELECT k, quote(v) FROM t_config ORDER BY k`,
				`SELECT rowid, a, b, c FROM t ORDER BY rowid`,
			}, fts5R31InterchangeProbes...)
			for _, q := range probes {
				goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  over this engine's file: %v\n  over C SQLite's: %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("detail=%s after mutations DIVERGES\n  sql: %s\n  go:  %q\n  cgo: %q", detail, q, goOut, cgoOut)
				}
			}
			ic, err := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), `PRAGMA integrity_check`)
			if err != nil {
				t.Fatalf("integrity_check: %v", err)
			}
			if ic != "integrity_check\nT:ok" {
				t.Errorf("detail=%s: C SQLite reports integrity_check = %q over this engine's mutated file", detail, ic)
			}
			// ...and this engine's own 'integrity-check' command, which
			// compares the decoded %_data against a fresh tokenization of
			// %_content (engine/fts5_decode.go), must accept it too.
			godb, err := sql.Open("sqlite", dsn["sqlite"])
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer godb.Close()
			if _, err := godb.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
				t.Errorf("detail=%s: this engine's own integrity-check rejects its own file: %v", detail, err)
			}
		})
	}
}
