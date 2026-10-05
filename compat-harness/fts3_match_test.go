// Differential gate for fts3/fts4 MATCH operator and offsets()/matchinfo()
// auxiliary functions: answers come from the segment index, not table rows.
package compat

import (
	"fmt"
	"strings"
	"testing"
)

// fts3MatchDocs is the document set every query battery runs against. It is
// deliberately arranged so that column, position and phrase order all matter:
// docid 1 and 6 hold the same three words in different orders, "beta" appears
// in column b of three rows and column a of none, and docid 5's terms share
// prefixes.
var fts3MatchDocs = []string{
	`INSERT INTO t(docid,a,b) VALUES(1,'one two three','alpha beta')`,
	`INSERT INTO t(docid,a,b) VALUES(2,'two three four','beta gamma')`,
	`INSERT INTO t(docid,a,b) VALUES(3,'three four five','gamma delta')`,
	`INSERT INTO t(docid,a,b) VALUES(4,'hello world','world hello')`,
	`INSERT INTO t(docid,a,b) VALUES(5,'abc abcd abcde','xyz')`,
	`INSERT INTO t(docid,a,b) VALUES(6,'one three two','beta alpha')`,
}

// fts3MatchQueries is the query language itself, one string per rule. Both
// engines must agree on every one -- including which of them are MALFORMED
// (the harness compares "errored" against "returned these rows", so a query
// this engine wrongly accepted, or wrongly rejected, fails here).
var fts3MatchQueries = []string{
	// --- single terms, folding, and the absent term ---
	`one`, `ONE`, `One`, `nosuchterm`, `4`,
	// --- implicit AND, and that it is NOT a phrase ---
	`one two`, `two three`, `three two`, `one four`,
	// --- OR / AND / NOT are UPPER CASE keywords only ---
	`one OR four`, `one or four`, `one AND four`, `one and four`,
	`two NOT four`, `two not four`, `four NOT two`,
	// --- precedence: NEAR > NOT > AND > OR ---
	`one OR four five`, `one four OR five`, `one OR two NOT four`,
	`five OR one NEAR three`, `one NEAR three OR five`,
	`three NOT one OR five`,
	// --- brackets ---
	`(one)`, `((one))`, `(one OR four) AND three`, `one AND (two OR five)`,
	`(one two) OR (four five)`, `(one OR two) NOT (four OR five)`,
	// --- quoted phrases ---
	`"two three"`, `"three two"`, `"one two three"`, `"one three"`,
	`"one" "two"`, `"one two" OR "four five"`, `"ONE TWO"`,
	`"abc abcd"`, `"abc abcd abcde"`,
	// --- prefix terms ---
	`abc*`, `ab*`, `abcde*`, `abcdef*`, `a*`, `*abc`, `*`,
	`"abc* xyz"`, `"one two*"`, `"one* two"`,
	`abc* xyz`, `abc* OR gamma`,
	// --- column filters (and the shapes that are NOT column filters) ---
	`a:one`, `b:beta`, `a:beta`, `b:one`, `a:one alpha`, `a:one a:two`,
	`b:beta alpha`, `a:abc*`, `a:"two three"`, `a:(one two)`,
	`c:one`, `docid:one`, `A:one`, `a:nosuchterm`,
	`a:one OR b:gamma`, `b:xyz OR a:hello`,
	// --- NEAR ---
	`one NEAR three`, `one NEAR/1 three`, `one NEAR/0 three`,
	`two NEAR/0 three`, `three NEAR/0 two`, `one NEAR/2 five`,
	`one NEAR one`, `alpha NEAR one`, `alpha NEAR beta`, `beta NEAR alpha`,
	`"one two" NEAR/0 four`, `"one two" NEAR/1 four`,
	`one NEAR/x three`, `one NEAR/ three`, `one NEAR/10 three`,
	`one NEAR three NEAR two`, `one NEAR/0 three NEAR/0 two`,
	`(one NEAR three) OR five`, `hello NEAR world`,
	// --- the '^' first-token modifier ---
	`^one`, `^two`, `^three`, `^one three`, `"^one two"`, `"^two three"`,
	`^abc*`, `b:^beta`, `b:^alpha`,
	// --- empty and degenerate queries (NOT errors) ---
	``, `   `, `()`, `(  )`, `""`, `" "`, `"" one`, `"" OR one`,
	`-one`, `two -four`, `- - -`, `:`, `::one`,
	// --- malformed queries (errors on BOTH engines) ---
	`AND`, `OR`, `NOT`, `NEAR`, `OR one`, `one OR`, `one AND`, `one NOT`,
	`NOT one`, `one NEAR`, `NEAR one`, `example AND (hello OR world))`,
	`"unclosed`, `(one`, `one)`, `((one)`, `one NEAR (three OR five)`,
	`(one OR two) NEAR three`, `one NEAR/1 (three)`,
}

// TestFts3MatchQueryLanguage runs every query in fts3MatchQueries against both
// engines over the same fts4 table.
func TestFts3MatchQueryLanguage(t *testing.T) {
	for _, module := range []string{"fts3", "fts4"} {
		module := module
		t.Run(module, func(t *testing.T) {
			// A batch per chunk keeps each differ() invocation's failure
			// output readable while still amortizing the two worker spawns.
			const chunk = 12
			for start := 0; start < len(fts3MatchQueries); start += chunk {
				end := start + chunk
				if end > len(fts3MatchQueries) {
					end = len(fts3MatchQueries)
				}
				stmts := []string{fmt.Sprintf(`CREATE VIRTUAL TABLE t USING %s(a,b)`, module)}
				stmts = append(stmts, fts3MatchDocs...)
				for _, q := range fts3MatchQueries[start:end] {
					stmts = append(stmts,
						fmt.Sprintf(`SELECT docid, a, b FROM t WHERE t MATCH '%s' ORDER BY docid`, strings.ReplaceAll(q, "'", "''")))
				}
				differ(t, fmt.Sprintf("%s match %d..%d", module, start, end), stmts)
			}
		})
	}
}

// TestFts3MatchColumnForm covers "<column> MATCH <query>", which confines the
// whole query to that column, and the whole-table form's interaction with an
// alias -- fts3 declares a HIDDEN COLUMN NAMED AFTER THE TABLE, so the table
// name resolves even when the FROM item is aliased, while the alias itself
// does not.
func TestFts3MatchColumnForm(t *testing.T) {
	stmts := []string{`CREATE VIRTUAL TABLE t USING fts4(a,b)`}
	stmts = append(stmts, fts3MatchDocs...)
	stmts = append(stmts,
		`SELECT docid FROM t WHERE a MATCH 'one' ORDER BY docid`,
		`SELECT docid FROM t WHERE b MATCH 'one' ORDER BY docid`,
		`SELECT docid FROM t WHERE b MATCH 'beta' ORDER BY docid`,
		`SELECT docid FROM t WHERE a MATCH 'a:one' ORDER BY docid`,
		`SELECT docid FROM t WHERE a MATCH 'b:beta' ORDER BY docid`,
		`SELECT docid FROM t WHERE a MATCH 'one OR beta' ORDER BY docid`,
		`SELECT docid FROM t WHERE t.a MATCH 'one' ORDER BY docid`,
		`SELECT docid FROM t AS x WHERE x.a MATCH 'one' ORDER BY docid`,
		`SELECT docid FROM t AS x WHERE t MATCH 'one' ORDER BY docid`,
		`SELECT docid FROM t WHERE t.t MATCH 'one' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'one' AND docid > 1 ORDER BY docid`,
		`SELECT docid FROM t WHERE (t MATCH 'one') ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH ('one') ORDER BY docid`,
		`SELECT count(*) FROM t WHERE t MATCH 'one'`,
		`SELECT max(docid) FROM t WHERE t MATCH 'three'`,
		`SELECT docid, count(*) FROM t WHERE t MATCH 'three' GROUP BY docid ORDER BY docid`,
		`SELECT docid FROM t WHERE 1 AND t MATCH 'one' ORDER BY docid`,
		`SELECT count(*) FROM t WHERE t MATCH 'three'`,
		`SELECT a FROM t WHERE t MATCH 'hello'`,
		`SELECT * FROM t WHERE t MATCH 'hello'`,
		`SELECT docid FROM t WHERE t MATCH 'one' LIMIT 1`,
		`SELECT docid FROM t WHERE t MATCH NULL`,
		`SELECT docid FROM t WHERE t MATCH 'one' ORDER BY docid DESC`,
		// A MATCH over a table with no rows at all still has to VALIDATE the
		// query: a per-row check would never run and would answer "no rows"
		// where C SQLite errors.
		`CREATE VIRTUAL TABLE e USING fts4(a,b,c)`,
		`SELECT * FROM e WHERE e MATCH 'example AND (hello OR world))'`,
		`SELECT * FROM e WHERE e MATCH 'hello'`,
		`SELECT * FROM e WHERE e MATCH ''`,
	)
	differ(t, "fts3 column-form match", stmts)
}

// TestFts3MatchSingleColumnTable pins the one-column case, where the "any
// column" default and the only real column coincide, plus the no-argument
// form's implicit "content" column.
func TestFts3MatchSingleColumnTable(t *testing.T) {
	differ(t, "fts3 single column", []string{
		`CREATE VIRTUAL TABLE t USING fts3`,
		`INSERT INTO t(docid,content) VALUES(1,'the quick brown fox')`,
		`INSERT INTO t(docid,content) VALUES(2,'jumps over the lazy dog')`,
		`INSERT INTO t(docid,content) VALUES(3,'the quick dog')`,
		`SELECT docid FROM t WHERE t MATCH 'quick' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'content:quick' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH '"the quick"' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'quick NEAR/1 dog' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'the NOT quick' ORDER BY docid`,
		`SELECT docid FROM t WHERE content MATCH 'dog' ORDER BY docid`,
	})
}

// TestFts3MatchNonTextValues checks that a MATCH finds the terms fts3 indexed
// from a NON-TEXT column value (which it tokenizes through its TEXT
// rendering), and that a row whose columns are all NULL matches nothing.
func TestFts3MatchNonTextValues(t *testing.T) {
	differ(t, "fts3 match over non-text values", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
		`INSERT INTO t VALUES(NULL,NULL)`,
		`INSERT INTO t VALUES(7, x'414243')`,
		`INSERT INTO t VALUES(1.5, 'plain')`,
		`SELECT docid FROM t WHERE t MATCH '7' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'abc' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH '1' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH '5' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH '"1 5"' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'plain' ORDER BY docid`,
	})
}

// TestFts3MatchManySegments builds an index of many level-0 segments and
// matches across all of them: a term's postings then live in several %_segdir
// rows at once, which is the merge this engine's reader has to get right.
func TestFts3MatchManySegments(t *testing.T) {
	stmts := []string{`CREATE VIRTUAL TABLE t USING fts4(a,b)`}
	for i := 0; i < 15; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t(docid,a,b) VALUES(%d,'common w%d','tail%d shared')`, i+1, i, i%3))
	}
	stmts = append(stmts,
		`SELECT docid FROM t WHERE t MATCH 'common' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'shared' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'tail0' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'w*' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'common w7' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH '"common w7"' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'common NOT tail1' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'a:common' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'b:common' ORDER BY docid`,
	)
	differ(t, "fts3 match across segments", stmts)
}

// TestFts3MatchLongDocument matches inside a document long enough that its
// position deltas need multi-byte varints, and far enough apart that the NEAR
// distances are actually discriminating.
func TestFts3MatchLongDocument(t *testing.T) {
	// Deliberately a SMALL VOCABULARY repeated many times: token positions
	// climb well past 127 (so their doclist deltas need multi-byte varints)
	// while the TERM count stays under the node-size limit this engine's
	// write path declines to spill past.
	var sb strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&sb, "w%d ", i%20)
	}
	sb.WriteString("needle")
	for i := 0; i < 160; i++ {
		fmt.Fprintf(&sb, " x%d", i%20)
	}
	differ(t, "fts3 match in a long document", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		fmt.Sprintf(`INSERT INTO t(docid,a) VALUES(1,'%s')`, sb.String()),
		`INSERT INTO t(docid,a) VALUES(2,'needle w0')`,
		`SELECT docid FROM t WHERE t MATCH 'needle' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'needle NEAR/1 w19' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'needle NEAR/0 w19' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'needle NEAR/0 w18' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'needle NEAR/1 w0' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'needle NEAR/200 w0' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH '"w19 needle x0"' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH '"x19 x0 x1"' ORDER BY docid`,
		`SELECT docid FROM t WHERE t MATCH 'w1*' ORDER BY docid`,
	})
}

// TestFts3MatchUnicodeTerms checks the "simple" tokenizer's non-ASCII rule
// through MATCH: bytes >= 0x80 are token characters passed through UNCHANGED,
// so a query term must match byte for byte with no Unicode case folding.
func TestFts3MatchUnicodeTerms(t *testing.T) {
	differ(t, "fts3 match unicode terms", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		"INSERT INTO t(docid,a) VALUES(1,'Ünïcode Ünïcode')",
		"INSERT INTO t(docid,a) VALUES(2,'ünïcode plain')",
		"SELECT docid FROM t WHERE t MATCH 'Ünïcode' ORDER BY docid",
		"SELECT docid FROM t WHERE t MATCH 'ünïcode' ORDER BY docid",
		"SELECT docid FROM t WHERE t MATCH 'ÜNÏCODE' ORDER BY docid",
		"SELECT docid FROM t WHERE t MATCH 'plain' ORDER BY docid",
	})
}

// fts3OffsetsQueries is every query language shape whose offsets() output has
// to come out byte for byte: the term numbering across phrases and columns,
// the per-column byte offsets, the NEAR trimming, and the phrases a failed
// NEAR cluster contributes nothing for.
var fts3OffsetsQueries = []string{
	`one`, `nosuch`, `three two`, `alpha beta`, `one two three`,
	`"one two" three`, `"one two"`, `"abc abcd"`, `"one one"`, `one one`,
	`a:one`, `b:beta`, `a:one b:beta`, `a:one alpha`,
	`one OR beta`, `one OR nosuch`, `nosuch OR beta`, `hello world`,
	`two NOT four`, `three NOT nosuch`,
	`one NEAR/0 three`, `one NEAR/10 three`, `three NEAR/0 two`,
	`(one NEAR/0 three) OR xyz`, `one NEAR three NEAR two`,
	`abc*`, `ab*`, `a:abc*`, `^one`, `^two`, `"^one two"`,
	`alpha NEAR beta`, `hello NEAR/0 world`,
}

// TestFts3Offsets gates offsets() against the oracle over the shared corpus.
func TestFts3Offsets(t *testing.T) {
	for _, module := range []string{"fts3", "fts4"} {
		module := module
		t.Run(module, func(t *testing.T) {
			const chunk = 12
			for start := 0; start < len(fts3OffsetsQueries); start += chunk {
				end := start + chunk
				if end > len(fts3OffsetsQueries) {
					end = len(fts3OffsetsQueries)
				}
				stmts := []string{fmt.Sprintf(`CREATE VIRTUAL TABLE t USING %s(a,b)`, module)}
				stmts = append(stmts, fts3MatchDocs...)
				for _, q := range fts3OffsetsQueries[start:end] {
					stmts = append(stmts, fmt.Sprintf(
						`SELECT docid, offsets(t) FROM t WHERE t MATCH '%s' ORDER BY docid`,
						strings.ReplaceAll(q, "'", "''")))
				}
				differ(t, fmt.Sprintf("%s offsets %d..%d", module, start, end), stmts)
			}
		})
	}
}

// TestFts3OffsetsShapes covers offsets() away from the plain "one query, one
// scan" case: no MATCH at all (every row is the empty string), a NULL or
// non-text column value, a column-form MATCH, and the calls C fts3 rejects.
func TestFts3OffsetsShapes(t *testing.T) {
	stmts := []string{`CREATE VIRTUAL TABLE t USING fts4(a,b)`}
	stmts = append(stmts, fts3MatchDocs...)
	stmts = append(stmts,
		`SELECT docid, offsets(t) FROM t ORDER BY docid`,
		`SELECT docid, offsets(t) FROM t WHERE docid=1`,
		`SELECT docid, offsets(t) FROM t WHERE t MATCH NULL`,
		`SELECT docid, offsets(t) FROM t WHERE a MATCH 'one' ORDER BY docid`,
		`SELECT docid, offsets(t) FROM t WHERE b MATCH 'beta' ORDER BY docid`,
		`SELECT docid, offsets(t) FROM t AS x WHERE t MATCH 'one' ORDER BY docid`,
		`SELECT docid, offsets(t) FROM t WHERE t MATCH 'one' AND docid>1 ORDER BY docid`,
		`SELECT length(offsets(t)) FROM t WHERE t MATCH 'one' ORDER BY docid`,
		`SELECT offsets(t) || '!' FROM t WHERE t MATCH 'one' ORDER BY docid`,
		`SELECT docid, offsets(a) FROM t WHERE t MATCH 'one'`,
		`SELECT docid, offsets() FROM t WHERE t MATCH 'one'`,
		`SELECT docid, offsets(t,1) FROM t WHERE t MATCH 'one'`,
		`SELECT docid, offsets(t) FROM t WHERE t MATCH 'one' AND offsets(t)=''`,
		// A second table in the FROM clause whose own cursor has no query.
		`CREATE TABLE ord(x)`,
		`INSERT INTO ord VALUES(1)`,
		`SELECT docid, offsets(t) FROM t, ord WHERE t MATCH 'one' ORDER BY docid`,
	)
	differ(t, "fts3 offsets shapes", stmts)

	differ(t, "fts3 offsets over NULL and non-text columns", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
		`INSERT INTO t(docid,a,b) VALUES(1,NULL,'one two')`,
		`INSERT INTO t(docid,a,b) VALUES(2,'one',NULL)`,
		`INSERT INTO t(docid,a,b) VALUES(3,7,'one one')`,
		"INSERT INTO t(docid,a,b) VALUES(4,'Ünï one','one')",
		`SELECT docid, offsets(t) FROM t WHERE t MATCH 'one' ORDER BY docid`,
		`SELECT docid, offsets(t) FROM t WHERE t MATCH '7' ORDER BY docid`,
		`SELECT docid, offsets(t) FROM t WHERE t MATCH 'one one' ORDER BY docid`,
		`SELECT docid, offsets(t) FROM t WHERE t MATCH '"one one"' ORDER BY docid`,
		"SELECT docid, offsets(t) FROM t WHERE t MATCH 'Ünï' ORDER BY docid",
	})
}

// fts3SnippetDocs is snippet()'s own document corpus. fts3MatchDocs' rows are
// three words wide, which every snippet fits whole; these are long enough (and
// irregular enough) that the parts of snippet() with no other observable
// consequence actually show:
//
//   - docid 1 is 18 tokens, longer than the default 15-token budget, so the
//     LEADING/TRAILING ellipsis and the window choice matter;
//   - docid 1's two matchable ends ("one" and "eighteen") cannot share one
//     window, which is what forces a SECOND fragment;
//   - docid 4's terms repeat, so the "1000 for a new phrase, 1 for a repeat"
//     scoring is what picks the window rather than position order;
//   - docid 5 has NO tokens at all in column a, and docid 6 has a NULL there
//     -- the two ends of "the fragment's column has nothing to render";
//   - docid 7's irregular punctuation is copied verbatim between tokens.
var fts3SnippetDocs = []string{
	`INSERT INTO t(docid,a,b) VALUES(1,'one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen','alpha beta')`,
	`INSERT INTO t(docid,a,b) VALUES(2,'the quick brown fox jumps over the lazy dog','lorem ipsum dolor sit amet')`,
	`INSERT INTO t(docid,a,b) VALUES(3,'abc abcd abcde abcdef','xyz')`,
	`INSERT INTO t(docid,a,b) VALUES(4,'aa bb aa cc aa dd aa ee aa ff aa gg','aa hh')`,
	`INSERT INTO t(docid,a,b) VALUES(5,'  ...  ','alpha gamma')`,
	`INSERT INTO t(docid,a,b) VALUES(6,NULL,'alpha beta gamma delta')`,
	`INSERT INTO t(docid,a,b) VALUES(7,'hello, world! hello -- world? hello.','world hello')`,
	`INSERT INTO t(docid,a,b) VALUES(8,'a b c d e f g h i j k l m n o p q r s t u v w x y z','a b c')`,
}

// fts3SnippetQueries and fts3SnippetArgs are crossed with each other: the
// queries decide which windows are candidates at all, the argument forms decide
// the budget, the column and the markers.
var fts3SnippetQueries = []string{
	`one`, `ten`, `eighteen`, `one OR eighteen`, `alpha`, `beta`, `one OR alpha`,
	`hello`, `world`, `hello world`, `"hello world"`, `hello NEAR/1 world`,
	`the`, `fox OR dog`, `quick brown`, `"quick brown fox"`,
	`abc*`, `a*`, `abcd`, `abc OR xyz`, `aa`, `aa OR hh`, `aa NEAR/3 gg`,
	`a OR z`, `a AND z`, `one NOT zzz`, `one OR two OR three OR four OR five`,
	`alpha OR beta OR gamma OR delta`, `a:one`, `b:alpha`, `^one`, `^a`,
	`one two three`, `"one two three"`, `one NEAR three`, `lorem OR ipsum`,
	`k OR l OR m OR n`, `gamma`, `nosuchterm`, `one OR nosuchterm`,
}

var fts3SnippetArgs = []string{
	``, `,'[',']'`, `,'[',']','~'`, `,'','',''`, `,'<<','>>','...'`,
	`,'[',']','~',-1,1`, `,'[',']','~',-1,2`, `,'[',']','~',-1,3`,
	`,'[',']','~',-1,4`, `,'[',']','~',-1,5`, `,'[',']','~',-1,7`,
	`,'[',']','~',-1,10`, `,'[',']','~',-1,15`, `,'[',']','~',-1,64`,
	`,'[',']','~',-1,-1`, `,'[',']','~',-1,-3`, `,'[',']','~',-1,-5`,
	`,'[',']','~',0,3`, `,'[',']','~',1,3`, `,'[',']','~',0,5`,
	`,'[',']','~',1,5`, `,'[',']','~',2,3`,
}

// TestFts3Snippet gates snippet() against the oracle: every query above under
// every argument form. The value is a whole rendered string, so ANY difference
// -- a window one token off, a missing ellipsis, punctuation copied wrongly --
// shows up here.
func TestFts3Snippet(t *testing.T) {
	for _, module := range []string{"fts3", "fts4"} {
		module := module
		t.Run(module, func(t *testing.T) {
			for _, args := range fts3SnippetArgs {
				args := args
				const chunk = 20
				for start := 0; start < len(fts3SnippetQueries); start += chunk {
					end := start + chunk
					if end > len(fts3SnippetQueries) {
						end = len(fts3SnippetQueries)
					}
					stmts := []string{fmt.Sprintf(`CREATE VIRTUAL TABLE t USING %s(a,b)`, module)}
					stmts = append(stmts, fts3SnippetDocs...)
					for _, q := range fts3SnippetQueries[start:end] {
						stmts = append(stmts, fmt.Sprintf(
							`SELECT docid, snippet(t%s) FROM t WHERE t MATCH '%s' ORDER BY docid`,
							args, strings.ReplaceAll(q, "'", "''")))
					}
					differ(t, fmt.Sprintf("%s snippet%s %d..%d", module, args, start, end), stmts)
				}
			}
		})
	}
}

// TestFts3SnippetShapes covers snippet()'s argument handling and the cases
// where it answers something other than a rendered window. Each of these
// pinned a rule (see engine/fts3_snippet.go's package comment):
//
//   - a NULL marker is an ERROR, and it is raised before both the zero-budget
//     and the no-query shortcuts -- so "snippet(t,NULL)" errors even with no
//     MATCH at all, while "snippet(t)" there is the empty string;
//   - the budget and column are read with sqlite3_value_int, i.e. truncated to
//     32 bits and (for text) parsed as an integer-only prefix;
//   - an out-of-range column renders column 0 from token 0 with nothing
//     highlighted, and a column holding SQL NULL makes the whole call NULL.
func TestFts3SnippetShapes(t *testing.T) {
	stmts := []string{`CREATE VIRTUAL TABLE t USING fts4(a,b)`}
	stmts = append(stmts, fts3SnippetDocs...)
	stmts = append(stmts,
		// No query on this cursor at all.
		`SELECT docid, snippet(t) FROM t ORDER BY docid`,
		`SELECT docid, snippet(t) FROM t WHERE docid=1`,
		`SELECT docid, snippet(t) FROM t WHERE t MATCH NULL`,
		// A NULL marker, with and without a query.
		`SELECT docid, snippet(t,NULL) FROM t`,
		`SELECT docid, snippet(t,NULL) FROM t WHERE t MATCH 'one'`,
		`SELECT docid, snippet(t,'[',NULL) FROM t WHERE t MATCH 'one'`,
		`SELECT docid, snippet(t,'[',']',NULL) FROM t WHERE t MATCH 'one'`,
		// A NULL column number is 0, a NULL budget is 0 (the empty string).
		`SELECT docid, snippet(t,'[',']','~',NULL) FROM t WHERE t MATCH 'alpha' ORDER BY docid`,
		`SELECT docid, snippet(t,'[',']','~',-1,NULL) FROM t WHERE t MATCH 'alpha' ORDER BY docid`,
		`SELECT docid, snippet(t,'[',']','~',-1,0) FROM t WHERE t MATCH 'alpha' ORDER BY docid`,
		// sqlite3_value_int: 32-bit truncation, REAL truncation toward zero,
		// an integer-only text/blob prefix.
		`SELECT docid, snippet(t,'[',']','~',-1,4294967296) FROM t WHERE t MATCH 'one'`,
		`SELECT docid, snippet(t,'[',']','~',4294967296,3) FROM t WHERE t MATCH 'one'`,
		`SELECT docid, snippet(t,'[',']','~',-1,3.9) FROM t WHERE t MATCH 'one'`,
		`SELECT docid, snippet(t,'[',']','~',-1,'1e3') FROM t WHERE t MATCH 'one'`,
		`SELECT docid, snippet(t,'[',']','~',-1,'  -3xyz') FROM t WHERE t MATCH 'one'`,
		`SELECT docid, snippet(t,'[',']','~',-1,x'33') FROM t WHERE t MATCH 'one'`,
		// Non-text markers, and an out-of-range column.
		`SELECT docid, snippet(t,1,2) FROM t WHERE t MATCH 'one'`,
		`SELECT docid, snippet(t,x'4142',x'4344') FROM t WHERE t MATCH 'one'`,
		`SELECT docid, snippet(t,'[',']','~',5,5) FROM t WHERE t MATCH 'one'`,
		`SELECT docid, snippet(t,'[',']','~',-2,5) FROM t WHERE t MATCH 'one'`,
		// The budget clamp, in both directions.
		`SELECT docid, snippet(t,'[',']','~',-1,1000) FROM t WHERE t MATCH 'one'`,
		`SELECT docid, snippet(t,'[',']','~',-1,-1000) FROM t WHERE t MATCH 'one'`,
		// A non-literal argument, which is evaluated per row.
		`SELECT docid, snippet(t,'['||docid,']') FROM t WHERE t MATCH 'alpha' ORDER BY docid`,
		`SELECT docid, snippet(t,'[',']','~',-1,docid) FROM t WHERE t MATCH 'alpha' ORDER BY docid`,
		// The column-form MATCH, an alias, and a second table in the FROM.
		`SELECT docid, snippet(t,'[',']','~') FROM t WHERE a MATCH 'one' ORDER BY docid`,
		`SELECT docid, snippet(t,'[',']','~') FROM t WHERE b MATCH 'alpha' ORDER BY docid`,
		`SELECT docid, snippet(t,'[',']','~') FROM t AS x WHERE t MATCH 'one' ORDER BY docid`,
		`SELECT docid, snippet(t) FROM t WHERE t MATCH 'one' AND docid>1 ORDER BY docid`,
		`SELECT typeof(snippet(t)), length(snippet(t)) FROM t WHERE t MATCH 'one'`,
		`SELECT snippet(t) || '!' FROM t WHERE t MATCH 'one' ORDER BY docid`,
		`SELECT docid, snippet(t), offsets(t) FROM t WHERE t MATCH 'one OR alpha' ORDER BY docid`,
		// The calls C fts3 rejects outright.
		`SELECT docid, snippet(a) FROM t WHERE t MATCH 'one'`,
		`SELECT docid, snippet() FROM t WHERE t MATCH 'one'`,
		`SELECT docid, snippet(t,1,2,3,4,5,6) FROM t WHERE t MATCH 'one'`,
	)
	differ(t, "fts3 snippet shapes", stmts)

	// A single-column table: "any column" and "column 0" coincide, and the
	// whole-table and column forms of MATCH have to agree.
	differ(t, "fts3 snippet single column", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(docid,a) VALUES(1,'alpha beta gamma delta epsilon zeta eta theta')`,
		`INSERT INTO t(docid,a) VALUES(2,'zeta')`,
		`SELECT docid, snippet(t,'[',']','~',-1,3) FROM t WHERE t MATCH 'alpha' ORDER BY docid`,
		`SELECT docid, snippet(t,'[',']','~',-1,3) FROM t WHERE t MATCH 'zeta' ORDER BY docid`,
		`SELECT docid, snippet(t,'[',']','~',-1,3) FROM t WHERE a MATCH 'alpha OR theta' ORDER BY docid`,
		`SELECT docid, snippet(t,'[',']','~',0,4) FROM t WHERE t MATCH 'delta' ORDER BY docid`,
	})

	// Non-text and non-ASCII column values: snippet() copies the document's own
	// BYTES between the markers, so the tokenizer's "everything >= 0x80 is a
	// token character, and is not folded" rule is visible in the output.
	differ(t, "fts3 snippet over NULL, numeric and unicode columns", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
		`INSERT INTO t(docid,a,b) VALUES(1,NULL,'one two')`,
		`INSERT INTO t(docid,a,b) VALUES(2,'one',NULL)`,
		`INSERT INTO t(docid,a,b) VALUES(3,7,'one one')`,
		"INSERT INTO t(docid,a,b) VALUES(4,'Ünï one zwei drei vier','one')",
		"INSERT INTO t(docid,a,b) VALUES(5,'é-ü one','one')",
		`SELECT docid, snippet(t,'[',']','~') FROM t WHERE t MATCH 'one' ORDER BY docid`,
		`SELECT docid, snippet(t,'[',']','~',-1,2) FROM t WHERE t MATCH 'one' ORDER BY docid`,
		`SELECT docid, snippet(t,'[',']','~',0,2) FROM t WHERE t MATCH 'one' ORDER BY docid`,
		`SELECT docid, snippet(t,'[',']','~') FROM t WHERE t MATCH '7' ORDER BY docid`,
		"SELECT docid, snippet(t,'[',']','~') FROM t WHERE t MATCH 'Ünï' ORDER BY docid",
	})
}

// TestFts3Matchinfo gates matchinfo() -- a blob of little-endian u32s, so a
// single miscounted hit shows up as different bytes.
func TestFts3Matchinfo(t *testing.T) {
	for _, module := range []string{"fts3", "fts4"} {
		module := module
		t.Run(module, func(t *testing.T) {
			// 'n'/'a'/'l' need an FTS4 table's %_stat / %_docsize; on fts3
			// they are "unrecognized matchinfo request", which this compares
			// too (both engines must reject). 's' (the longest-consecutive-
			// phrase-run score) is legal on BOTH modules.
			formats := []string{"", ",'pcx'", ",'p'", ",'c'", ",'x'", ",'xxp'", ",''",
				",'n'", ",'a'", ",'l'", ",'nalpcx'", ",'q'",
				",'y'", ",'b'", ",'yb'", ",'pcxyb'", ",'byx'", ",'bb'",
				",'s'", ",'ps'", ",'ss'", ",'pcxs'", ",'sy'", ",'sb'"}
			queries := []string{
				`one`, `one two`, `three two`, `alpha beta`, `"one two"`, `abc*`,
				`a:one`, `b:beta`, `one OR beta`, `two NOT four`,
				`two NOT four alpha`, `(two NOT four) OR xyz`,
				`one NEAR/0 three`, `one NEAR/10 three`, `(one NEAR/0 three) OR xyz`,
				`one one`, `hello world`, `nosuch OR one`,
			}
			for _, f := range formats {
				stmts := []string{fmt.Sprintf(`CREATE VIRTUAL TABLE t USING %s(a,b)`, module)}
				stmts = append(stmts, fts3MatchDocs...)
				for _, q := range queries {
					stmts = append(stmts, fmt.Sprintf(
						`SELECT docid, quote(matchinfo(t%s)) FROM t WHERE t MATCH '%s' ORDER BY docid`,
						f, strings.ReplaceAll(q, "'", "''")))
				}
				differ(t, fmt.Sprintf("%s matchinfo%s", module, f), stmts)
			}
		})
	}
}

// TestFts3MatchinfoLcs gates matchinfo()'s 's' directive (engine's
// lcsPerColumn, a port of fts3MatchinfoLcs) on documents and queries chosen so
// the answer is actually discriminating rather than all ones: over these four
// rows C SQLite reports per-column runs of 0, 1, 2 AND 3 -- e.g. for the
// query "one two three", docid 3's 'alpha one two three beta' scores 3 in
// column a and 0 in column b, while docid 4's 'one x two x three' scores 1
// there and 3 in its own 'one two three' column b. A phrase-bias or
// tie-handling mistake in the port moves those numbers.
func TestFts3MatchinfoLcs(t *testing.T) {
	for _, module := range []string{"fts3", "fts4"} {
		module := module
		t.Run(module, func(t *testing.T) {
			stmts := []string{
				fmt.Sprintf(`CREATE VIRTUAL TABLE t USING %s(a,b)`, module),
				`INSERT INTO t(docid,a,b) VALUES(1,'one two three four','one two')`,
				`INSERT INTO t(docid,a,b) VALUES(2,'two one three','three one two')`,
				`INSERT INTO t(docid,a,b) VALUES(3,'alpha one two three beta','zzz')`,
				`INSERT INTO t(docid,a,b) VALUES(4,'one x two x three','one two three')`,
				`INSERT INTO t(docid,a,b) VALUES(5,NULL,'one one two two')`,
				`INSERT INTO t(docid,a,b) VALUES(6,'one','')`,
			}
			for _, q := range []string{
				`one`, `one two`, `one two three`, `three two one`,
				`"one two" three`, `"one two three"`, `one "two three"`,
				`one NEAR/1 two`, `one NEAR/0 two`, `(one NEAR/1 two) three`,
				`one OR two`, `one two nosuchterm`, `nosuchterm`,
				`one NOT four`, `a:one two`, `b:one b:two`, `on* tw*`,
			} {
				stmts = append(stmts, fmt.Sprintf(
					`SELECT docid, quote(matchinfo(t,'s')), quote(matchinfo(t,'pcs')) FROM t WHERE t MATCH '%s' ORDER BY docid`,
					strings.ReplaceAll(q, "'", "''")))
			}
			differ(t, module+" matchinfo lcs", stmts)
		})
	}

	// Three columns, so the per-column loop is exercised past two, with a
	// query whose phrases run consecutively in different columns.
	differ(t, "fts4 matchinfo lcs three columns", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a,b,c)`,
		`INSERT INTO t(docid,a,b,c) VALUES(1,'x one two','one two three','two one')`,
		`INSERT INTO t(docid,a,b,c) VALUES(2,'one','two','three')`,
		`SELECT docid, quote(matchinfo(t,'s')) FROM t WHERE t MATCH 'one two' ORDER BY docid`,
		`SELECT docid, quote(matchinfo(t,'s')) FROM t WHERE t MATCH 'one two three' ORDER BY docid`,
		`SELECT docid, quote(matchinfo(t,'pcxs')) FROM t WHERE t MATCH 'one two three' ORDER BY docid`,
	})
}

// TestFts3MatchinfoShapes covers matchinfo() away from the plain case.
func TestFts3MatchinfoShapes(t *testing.T) {
	stmts := []string{`CREATE VIRTUAL TABLE t USING fts4(a,b)`}
	stmts = append(stmts, fts3MatchDocs...)
	stmts = append(stmts,
		`SELECT docid, quote(matchinfo(t)) FROM t WHERE a MATCH 'one' ORDER BY docid`,
		`SELECT docid, quote(matchinfo(t)) FROM t WHERE b MATCH 'beta' ORDER BY docid`,
		`SELECT docid, quote(matchinfo(t)) FROM t AS x WHERE t MATCH 'one' ORDER BY docid`,
		`SELECT docid, quote(matchinfo(t)) FROM t WHERE t MATCH 'one' AND docid>1 ORDER BY docid`,
		`SELECT length(matchinfo(t)) FROM t WHERE t MATCH 'one' ORDER BY docid`,
		`SELECT docid, quote(matchinfo(a)) FROM t WHERE t MATCH 'one'`,
		`SELECT docid, quote(matchinfo()) FROM t WHERE t MATCH 'one'`,
		`SELECT docid, quote(matchinfo(t,'p',1)) FROM t WHERE t MATCH 'one'`,
		`SELECT docid, quote(matchinfo(t)), offsets(t) FROM t WHERE t MATCH 'one OR beta' ORDER BY docid`,
	)
	differ(t, "fts3 matchinfo shapes", stmts)

	// An FTS4 table with columns of very different lengths, so 'a' (average
	// column length, rounded to nearest) and 'l' (this row's lengths) are
	// actually discriminating.
	differ(t, "fts3 matchinfo lengths", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
		`INSERT INTO t(docid,a,b) VALUES(1,'x','one two three four five')`,
		`INSERT INTO t(docid,a,b) VALUES(2,'x y z','one')`,
		`INSERT INTO t(docid,a,b) VALUES(3,'x y','one two')`,
		`SELECT docid, quote(matchinfo(t,'nal')) FROM t WHERE t MATCH 'one' ORDER BY docid`,
		`SELECT docid, quote(matchinfo(t,'nalpcx')) FROM t WHERE t MATCH 'x' ORDER BY docid`,
		`SELECT docid, quote(matchinfo(t,'lllaa')) FROM t WHERE t MATCH 'one' ORDER BY docid`,
	})
}

// TestFts3MatchinfoNoQuery gates matchinfo() over a cursor that has no MATCH
// at all to report on -- either because the statement has none, or because
// the one it has targets a DIFFERENT table. Real fts3's own matchinfo()
// returns the EMPTY BLOB (X'') there UNCONDITIONALLY: verified against the
// oracle that this holds for every directive this engine serves ('p', 'c',
// 'n', 'a', 'l', 's', 'x' and the default "pcx"), for a table lacking the
// module support a directive needs (fts3's own 'n'/'a', and 'l' without
// %_docsize -- both "unrecognized matchinfo request" WITH a query, but empty
// here), for a completely UNRECOGNIZED format character, for a NON-LITERAL
// (column-valued) format argument, and for a NULL one -- the format is not
// even LOOKED AT when the cursor has no query, so none of those reach the
// errors they would over a real query.
func TestFts3MatchinfoNoQuery(t *testing.T) {
	for _, module := range []string{"fts3", "fts4"} {
		module := module
		t.Run(module, func(t *testing.T) {
			stmts := []string{
				fmt.Sprintf(`CREATE VIRTUAL TABLE t USING %s(a,b)`, module),
				`INSERT INTO t(docid,a,b) VALUES(1,'aa bb','cc')`,
				`INSERT INTO t(docid,a,b) VALUES(2,'dd','aa ee')`,
			}
			for _, f := range []string{"", ",'p'", ",'c'", ",'n'", ",'a'", ",'l'", ",'s'",
				",'x'", ",'pcxnal'", ",''", ",'q'", ",'bogus'", ",NULL"} {
				stmts = append(stmts, fmt.Sprintf(`SELECT docid, quote(matchinfo(t%s)) FROM t`, f))
			}
			// A non-literal (column-valued) format argument.
			stmts = append(stmts,
				`CREATE TABLE q(fmt)`,
				`INSERT INTO q VALUES('nal')`,
				`INSERT INTO q VALUES('bogus')`,
				`SELECT t.docid, quote(matchinfo(t,q.fmt)) FROM t, q ORDER BY t.docid, q.fmt`,
			)
			differ(t, module+" matchinfo no query", stmts)
		})
	}

	// A MATCH exists in the statement, but on a DIFFERENT fts table.
	differ(t, "matchinfo no query, MATCH on another table", []string{
		`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
		`INSERT INTO t(docid,a,b) VALUES(1,'aa bb','cc')`,
		`CREATE VIRTUAL TABLE u USING fts4(z)`,
		`INSERT INTO u VALUES('zz')`,
		`SELECT t.docid, quote(matchinfo(t)) FROM t, u WHERE u MATCH 'zz' ORDER BY t.docid`,
	})

	// The WITH-a-query format errors are unaffected: an out-of-scope
	// directive still errors once there IS a MATCH on the same table.
	differ(t, "matchinfo with query, out-of-scope format still errors", []string{
		`CREATE VIRTUAL TABLE t3 USING fts3(a,b)`,
		`INSERT INTO t3(docid,a,b) VALUES(1,'aa bb','cc')`,
		`SELECT docid, quote(matchinfo(t3,'nal')) FROM t3 WHERE t3 MATCH 'aa'`,
	})
}

// TestFts3MatchDeclinedShapes pins the MATCH shapes deliberately left out.
// Each must fail cleanly here; C SQLite either fails too (the first group,
// which differ() therefore also covers) or SUCCEEDS (the second), and an
// engine that quietly started answering one of those would be wrong.
func TestFts3MatchDeclinedShapes(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t USING fts4(a,b)`,
		`INSERT INTO t(docid,a,b) VALUES(1,'one two','alpha')`,
	}
	bad := []string{
		// C SQLite rejects these too ("unable to use function MATCH in the
		// requested context"): fts3 can only answer a MATCH its own xBestIndex
		// was handed.
		`SELECT docid FROM t WHERE NOT (t MATCH 'one')`,
		`SELECT docid FROM t WHERE t MATCH 'one' AND t MATCH 'two'`,
		`SELECT docid, t MATCH 'one' FROM t`,
		`SELECT docid FROM t WHERE CASE WHEN t MATCH 'one' THEN 1 END`,
		`SELECT docid FROM t WHERE +t MATCH 'one'`,
		`SELECT docid FROM t WHERE docid MATCH 'one'`,
		`SELECT docid FROM t WHERE 'one' MATCH t`,
		// C SQLite ACCEPTS this; this engine declines it rather than answer
		// it, because it only recognises a MATCH that is a top-level
		// AND-conjunct of the WHERE clause.
		//
		// "t MATCH 'one'||''" used to sit here too, as "the query text has to
		// be a literal". It no longer does: a non-literal query is read per
		// row now, exactly where C fts3's xFilter reads it
		// (engine/fts3_search.go's per-row pattern register), and
		// fts3_order_and_match_test.go gates it against the oracle.
		`SELECT docid FROM t WHERE t MATCH 'one' OR docid = 3`,
		// matchinfo()'s format string is resolved at PREPARE time, so a
		// computed one is declined (every DIRECTIVE is now served).
		`SELECT matchinfo(t,'pc'||'x') FROM t WHERE t MATCH 'one'`,
		// "SELECT matchinfo(t) FROM t" (no MATCH at all) used to sit here too.
		// It no longer does: matchinfo() over a cursor with no query is the
		// EMPTY BLOB for every format string, format validation and all --
		// TestFts3MatchinfoNoQuery gates it (and the WITH-a-query format
		// errors, which are unaffected) against the oracle.
	}
	for _, q := range bad {
		q := q
		t.Run(q, func(t *testing.T) {
			db := openMusqlFts(t)
			for _, s := range setup {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			rows, err := db.Query(q)
			if err == nil {
				iterErr := rows.Err()
				for rows.Next() {
				}
				if iterErr == nil {
					iterErr = rows.Err()
				}
				rows.Close()
				err = iterErr
			}
			if err == nil {
				t.Fatalf("engine ACCEPTED an out-of-scope MATCH shape: %s", q)
			}
		})
	}
}
