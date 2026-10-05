// fts3/fts4 porter stemmer tested against C SQLite.
package compat

import (
	"fmt"
	"strings"
	"testing"
)

// fts3PorterWords generates exhaustive word list covering all suffix rules.
func fts3PorterWords() []string {
	suffixes := []string{
		"", "s", "es", "ses", "ies", "ss",
		"eed", "ing", "ed", "y",
		"ational", "tional", "enci", "anci", "izer", "logi",
		"bli", "alli", "entli", "eli", "ousli",
		"ization", "ation", "ator",
		"alism", "iveness", "fulness", "ousness",
		"aliti", "iviti", "biliti",
		"icate", "ative", "alize", "iciti", "ical", "ful", "ness",
		"al", "ance", "ence", "er", "ic", "able", "ible", "ant",
		"ement", "ment", "ent", "ion", "ou", "ism", "ate", "iti",
		"ous", "ive", "ize", "e", "ll", "bb", "tt", "dd", "at", "bl", "iz",
	}
	stems := []string{
		"a", "ab", "abl", "cat", "hop", "rel", "feed", "agree", "plaster",
		"motor", "sky", "happy", "conflat", "troubl", "siz", "fail", "file",
		"radic", "electric", "hesitanc", "digitiz", "conform", "vietnam",
		"formal", "analog", "differenti", "vile", "analogous", "rational",
		"national", "valenc", "hesitanc", "sensibil", "formaliz", "operator",
		"feudal", "decisive", "hopeful", "callous", "formal", "sensit",
		"sensibil", "triplic", "format", "electric", "adjust", "airlin",
		"defensible", "irritant", "replacement", "adjustment", "dependent",
		"adoption", "homologou", "communism", "activate", "angulariti",
		"homologous", "effective", "bowdlerize", "controll", "roll",
	}
	seen := map[string]bool{}
	var out []string
	add := func(w string) {
		if w == "" || seen[w] {
			return
		}
		seen[w] = true
		out = append(out, w)
	}
	for _, st := range stems {
		for _, sf := range suffixes {
			add(st + sf)
		}
	}
	// The copy_stemmer boundaries: <3 bytes, >=21 bytes, a digit anywhere
	// (which drops mx to 3), and a non-[A-Za-z] byte that disqualifies the
	// stemmer outright.
	for n := 1; n <= 30; n++ {
		add(strings.Repeat("a", n))
		add(strings.Repeat("ab", n))
	}
	// The exact length gate: the stemmer runs for 3..20 bytes and the copy
	// stemmer takes over at 21, so every length either side needs a word whose
	// two answers DIFFER -- one carrying a suffix the stemmer would strip.
	for n := 4; n <= 26; n++ {
		add(strings.Repeat("b", n-3) + "ing")
		add(strings.Repeat("b", n-4) + "ness")
		if n >= 8 {
			add(strings.Repeat("b", n-7) + "ational")
		}
		add(strings.Repeat("ba", (n-2)/2) + "ed")
	}
	add("a1")
	add("a12")
	add("a123456")
	add("a1234567890123456789z")
	add("abc123def456ghi789jkl")
	add("one_two")
	add("_leading")
	add("trailing_")
	add("MiXeDcAsE")
	add("MIXEDCASELONGERWORD")
	add("abcdefghijklmnopqrstuvwxyz")
	add("ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	add("café")
	add("über")
	add("naïveté")
	add("yyy")
	add("sky")
	add("by")
	add("yes")
	add("syzygy")
	add("cannot")
	return out
}

func TestFts3PorterStemExhaustive(t *testing.T) {
	words := fts3PorterWords()
	// One document per chunk so no single INSERT gets unwieldy, and so the
	// terms are spread over several segments.
	const chunk = 60
	stmts := []string{
		`CREATE VIRTUAL TABLE p USING fts4(x, tokenize=porter)`,
	}
	for i := 0; i < len(words); i += chunk {
		end := i + chunk
		if end > len(words) {
			end = len(words)
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO p VALUES('%s')`, strings.Join(words[i:end], " ")))
	}
	stmts = append(stmts,
		`CREATE VIRTUAL TABLE pa USING fts4aux(p)`,
		`SELECT count(*) FROM pa WHERE col='*'`,
		// The whole term list, which IS the stemmer's output set.
		`SELECT group_concat(hex(term)) FROM pa WHERE col='*'`,
		// ...and the per-term document/occurrence counts, which pin WHICH
		// words stemmed to each term, not merely the set of stems.
		`SELECT term, documents, occurrences FROM pa WHERE col='*'`,
		`SELECT id, quote(value) FROM p_stat`,
		`INSERT INTO p(p) VALUES('integrity-check')`,
	)
	differ(t, "porter over a generated word list", stmts)
}

func TestFts3PorterDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"the tokenizer on both modules, and MATCH through the stem", []string{
			`CREATE VIRTUAL TABLE p4 USING fts4(words, tokenize=porter)`,
			`CREATE VIRTUAL TABLE p3 USING fts3(content, tokenize porter)`,
			`INSERT INTO p4 VALUES('Running quickly through the DEFENSIBLE gardens')`,
			`INSERT INTO p3 VALUES('Running quickly through the DEFENSIBLE gardens')`,
			`SELECT level, idx, quote(root) FROM p4_segdir ORDER BY level, idx`,
			`SELECT level, idx, quote(root) FROM p3_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM p4_stat`,
			`SELECT docid, quote(size) FROM p4_docsize`,
			// The query string is stemmed too, so the surface form finds it.
			`SELECT docid FROM p4 WHERE p4 MATCH 'run'`,
			`SELECT docid FROM p4 WHERE p4 MATCH 'running'`,
			`SELECT docid FROM p4 WHERE p4 MATCH 'runs'`,
			`SELECT docid FROM p4 WHERE p4 MATCH 'garden'`,
			`SELECT docid FROM p4 WHERE p4 MATCH 'defensible'`,
			`SELECT docid FROM p3 WHERE p3 MATCH 'quick'`,
			`SELECT offsets(p4) FROM p4 WHERE p4 MATCH 'garden'`,
			`SELECT snippet(p4) FROM p4 WHERE p4 MATCH 'defens'`,
			`INSERT INTO p4(p4) VALUES('integrity-check')`,
		}},
		// '_' is a token character for porter and a DELIMITER for simple.
		{"porter's token boundary is not simple's", []string{
			`CREATE VIRTUAL TABLE ps USING fts4(x)`,
			`CREATE VIRTUAL TABLE pp USING fts4(x, tokenize=porter)`,
			`INSERT INTO ps VALUES('one_two three-four a.b')`,
			`INSERT INTO pp VALUES('one_two three-four a.b')`,
			`SELECT quote(root) FROM ps_segdir`,
			`SELECT quote(root) FROM pp_segdir`,
			`SELECT docid FROM pp WHERE pp MATCH 'one_two'`,
			`SELECT docid FROM ps WHERE ps MATCH 'one'`,
		}},
		// The copy_stemmer truncation, which is not the published algorithm.
		{"the copy stemmer's length and digit rules", []string{
			`CREATE VIRTUAL TABLE p USING fts4(x, tokenize=porter)`,
			`INSERT INTO p VALUES('abcdefghijklmnopqrstuvwxyz a1234567890123456789z ab a abcdefghijklmnopqrst')`,
			`CREATE VIRTUAL TABLE pa USING fts4aux(p)`,
			`SELECT term, documents, occurrences FROM pa WHERE col='*'`,
			`SELECT quote(root) FROM p_segdir`,
			`INSERT INTO p(p) VALUES('integrity-check')`,
		}},
		// porterCreate ignores its arguments, so these are all accepted.
		{"porter takes any arguments and ignores them", []string{
			`CREATE VIRTUAL TABLE a1 USING fts4(x, tokenize=porter)`,
			`CREATE VIRTUAL TABLE a2 USING fts4(x, tokenize porter)`,
			`CREATE VIRTUAL TABLE a3 USING fts3(x, tokenize= porter)`,
			`CREATE VIRTUAL TABLE a4 USING fts4(x, tokenize=porter "anything at all")`,
			`CREATE VIRTUAL TABLE a5 USING fts3(x, tokenize blah)`,
			`SELECT type, name FROM sqlite_master WHERE name IN ('a1','a2','a3','a4','a5') ORDER BY name`,
			`INSERT INTO a4 VALUES('running gardens')`,
			`SELECT quote(root) FROM a4_segdir`,
		}},
		// DELETE/UPDATE/optimize/rebuild must all stem the same way.
		{"the write path stems everywhere", []string{
			`CREATE VIRTUAL TABLE p USING fts4(a, b, tokenize=porter)`,
			`INSERT INTO p VALUES('running gardens','defensible arguments')`,
			`INSERT INTO p VALUES('hopeful callousness','nationalization')`,
			`INSERT INTO p VALUES('third row','more text')`,
			`DELETE FROM p WHERE docid=1`,
			`SELECT level, idx, quote(root) FROM p_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM p_stat`,
			`UPDATE p SET a='formalized triplicate' WHERE docid=2`,
			`SELECT level, idx, quote(root) FROM p_segdir ORDER BY level, idx`,
			`SELECT docid FROM p WHERE p MATCH 'formal'`,
			`INSERT INTO p(p) VALUES('integrity-check')`,
			`INSERT INTO p(p) VALUES('optimize')`,
			`SELECT level, idx, quote(root) FROM p_segdir ORDER BY level, idx`,
			`INSERT INTO p(p) VALUES('rebuild')`,
			`SELECT level, idx, quote(root) FROM p_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM p_stat`,
			`INSERT INTO p(p) VALUES('integrity-check')`,
		}},
		// A prefix index over a porter table truncates the STEM.
		{"porter with prefix indexes", []string{
			`CREATE VIRTUAL TABLE pp USING fts4(words, tokenize=porter, prefix="2,3")`,
			`INSERT INTO pp VALUES('running gardens nationalization')`,
			`SELECT level, idx, quote(root) FROM pp_segdir ORDER BY level, idx`,
			`SELECT docid FROM pp WHERE pp MATCH 'run*'`,
			`INSERT INTO pp(pp) VALUES('integrity-check')`,
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}
