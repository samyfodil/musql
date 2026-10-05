package compat

// json_each and json_tree table-valued functions tested against C SQLite.
// The "id" column requires broad shape sweep to catch offset-arithmetic bugs.

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func jsonEachCase(t *testing.T, q string) {
	t.Helper()
	if !differ(t, "jsoneach/"+q, []string{q}) {
		t.Errorf("diverged on: %s", q)
	}
}

// TestJSONEachShapes tests both json_each and json_tree with all column sets.
func TestJSONEachShapes(t *testing.T) {
	docs := []string{
		`[1,2.5,"x",null,true,false,{"a":9},[7]]`,
		`{"a":1,"b":"t","c":[2,3],"d":{"e":null}}`,
		`[]`, `{}`, `7`, `-0.5`, `"hi"`, `null`, `true`,
		`[[[1]]]`,
		`{"a":{"b":{"c":[1,2,3]}}}`,
		// Keys with bare-vs-quoted rules.
		`{"ab":1,"a1":2,"A":3,"Z9z":4,"_":5,"a_b":6,"9":7,"":8,"e$f":9,"g.h":10,"x y":11}`,
		`{"a":1,"a\/b":2,"c\\d":3,"tab\tx":4,"é":5}`,
		// Duplicate keys.
		`{"a":1,"a":2}`,
		// Payload sizes crossing JSONB header boundaries.
		`["12345678901","z"]`,
		`["123456789012","z"]`,
		`[1.50,1e3,-2E-3,"z"]`,
		`[123456789012,"x"]`,
		// Whitespace in JSON.
		`  [ 1 , { "a" : 2 } ]  `,
	}
	for _, fn := range []string{"json_each", "json_tree"} {
		for _, d := range docs {
			jsonEachCase(t, fmt.Sprintf(`SELECT * FROM %s(%s)`, fn, sqlQuote(d)))
			jsonEachCase(t, fmt.Sprintf(
				`SELECT key,value,type,atom,id,parent,fullkey,path,json,root,rowid FROM %s(%s)`,
				fn, sqlQuote(d)))
		}
	}
}

// TestJSONEachRootPath tests root path argument and subtree selection.
func TestJSONEachRootPath(t *testing.T) {
	const doc = `{"a":{"b":[1,{"c":2}]},"x y":[9],"n":7}`
	paths := []string{
		`$`, `$.a`, `$.a.b`, `$.a.b[0]`, `$.a.b[1]`, `$.a.b[1].c`, `$."x y"`,
		`$."x y"[0]`, `$.n`, `$."a"`, `$[#-1]`, `$.a.b[#-1]`, `$.a.b[#]`,
		// Paths that select nothing.
		`$.zz`, `$.a.b[9]`, `$.n.deeper`, `$[0]`,
	}
	for _, fn := range []string{"json_each", "json_tree"} {
		for _, p := range paths {
			jsonEachCase(t, fmt.Sprintf(`SELECT * FROM %s(%s,%s)`,
				fn, sqlQuote(doc), sqlQuote(p)))
		}
		// Root paths into arrays.
		jsonEachCase(t, fmt.Sprintf(`SELECT * FROM %s('[[9],[8,7]]','$[#-1]')`, fn))
		jsonEachCase(t, fmt.Sprintf(`SELECT * FROM %s('[[9],[8,7]]','$[1]')`, fn))
	}
}

// TestJSONTreeRootRow tests json_tree root row key and path rules.
func TestJSONTreeRootRow(t *testing.T) {
	for _, c := range []struct{ doc, path string }{
		{`{"a":{"b":2,"z":0}}`, `$.a.b`},     // First child splits.
		{`{"a":{"z":0,"b":2}}`, `$.a.b`},     // Non-first child unsplit.
		{`{"ab":{"z":0,"cd":1}}`, `$.ab.cd`}, // Bare name remainder.
		{`[[1],[2,3]]`, `$[1][0]`},           // First array element.
		{`[[1],[2,3]]`, `$[1][1]`},           // Non-first array element.
		{`[[1,2]]`, `$[0][1]`},               // Through first-child array.
		{`{"z":0,"b":2}`, `$."b"`},           // Bare prefix splits always.
		{`{"a":{"z":0,"b":2}}`, `$.a."b"`},   // Quoted accessor.
		{`{"a b":{"z":0,"c d":1}}`, `$."a b"."c d"`},
		{`{"a b":{"z":0,"cd":1}}`, `$."a b".cd`},
		{`{"a b":{"z":0,"c d":1}}`, `$."a b"."z"`},
		{`{"a b":[1,2]}`, `$."a b"[1]`},
		{`{"z":0,"a b":[1,2]}`, `$."a b"[1]`},
		{`{"9n":[10,20,30]}`, `$."9n"[1]`},
		{`[[9],[8,7]]`, `$[1]`},   // Integer key.
		{`[[9],[8,7]]`, `$[#-1]`}, // Computed array index.
		{`[9,8]`, `$[#-2]`},
		// Escaped keys in paths have a pre-existing gap.
		{`{"a":{"b":{"c":{"d":1}}}}`, `$.a.b.c.d`},
		{`{"a":{"z":0,"c":{"d":5}}}`, `$.a.c.d`}, // Non-first parent.
	} {
		jsonEachCase(t, fmt.Sprintf(`SELECT * FROM json_tree(%s,%s)`,
			sqlQuote(c.doc), sqlQuote(c.path)))
		jsonEachCase(t, fmt.Sprintf(`SELECT * FROM json_each(%s,%s)`,
			sqlQuote(c.doc), sqlQuote(c.path)))
	}
}

// TestJSONEachArguments tests argument error checking order.
func TestJSONEachArguments(t *testing.T) {
	for _, fn := range []string{"json_each", "json_tree"} {
		for _, q := range []string{
			`SELECT * FROM %s()`,
			`SELECT count(*) FROM %s()`,
			`SELECT * FROM %s(NULL)`,
			`SELECT * FROM %s(NULL,'bad')`,
			`SELECT * FROM %s(NULL,NULL)`,
			`SELECT * FROM %s('notjson')`,
			`SELECT * FROM %s('notjson',NULL)`,
			`SELECT * FROM %s('notjson','bad')`,
			`SELECT * FROM %s('[1,2]',NULL)`,
			`SELECT * FROM %s('[1,2]','bad')`,
			`SELECT * FROM %s('[1,2]','')`,
			`SELECT * FROM %s('[1,2]','$ ')`,
			`SELECT * FROM %s('[1,2]',' $')`,
			`SELECT * FROM %s('[1,2]',5)`,
			`SELECT * FROM %s('[1,2]','$',3)`,
			`SELECT * FROM %s('')`,
			`SELECT * FROM %s('   ')`,
			// A non-TEXT document is coerced to its text form and parsed.
			`SELECT * FROM %s(5)`,
			`SELECT * FROM %s(5.5)`,
			`SELECT * FROM %s(-0.0)`,
			// The eponymous (no-call) spelling, with the inputs as constraints.
			`SELECT count(*) FROM %s WHERE json='[1,2,3]'`,
			`SELECT * FROM %s WHERE json='{"a":[1]}' AND root='$.a'`,
			`SELECT count(*) FROM %s`,
			`SELECT count(*) FROM %s WHERE json IS NULL`,
			// Ordinary query machinery over the produced rows.
			`SELECT key FROM %s('[5,6,7]') WHERE key=1`,
			`SELECT key FROM %s('[5,6,7]') WHERE key='1'`,
			`SELECT id, id='1' FROM %s('[9]')`,
			`SELECT * FROM %s('[3,1,2]') ORDER BY value DESC`,
			`SELECT count(*), sum(id) FROM %s('{"a":[1,2],"b":3}')`,
			`SELECT type, count(*) FROM %s('[1,"a",null,{},[]]') GROUP BY type ORDER BY type`,
		} {
			jsonEachCase(t, fmt.Sprintf(q, fn))
		}
	}
}

// TestJSONEachSubtype pins the JSON SUBTYPE on the value column. A container's
// value is tagged (so json_quote/json_array embed it rather than quoting it)
// and an atom's is not -- and unlike a derived table's column, a VIRTUAL
// TABLE's keeps its tag, which is why openDerivedCursor (engine/vdbe_cursor.go)
// clears the tag where openMaterializedCursor does not.
func TestJSONEachSubtype(t *testing.T) {
	for _, q := range []string{
		`SELECT json_quote(value) FROM json_each('[[7]]')`,
		`SELECT json_quote(value) FROM json_each('[{"a":1}]')`,
		`SELECT json_quote(value), json_quote(atom) FROM json_each('["s",1,2.5,null,true]')`,
		`SELECT json_array(value) FROM json_each('[[7],{"a":1},"s",3]')`,
		`SELECT json_quote(value) FROM json_tree('{"a":[7]}') WHERE type='array'`,
		`SELECT json_extract(value,'$.a') FROM json_each('[{"a":1}]')`,
		`SELECT json_object('k',value) FROM json_each('[[7],"s"]')`,
		// A GROUP BY materializes its rows, and the tag does NOT survive that --
		// for a bare column, the group key, and an aggregate's ARGUMENT alike,
		// while a whole-table aggregate keeps it (clearJSONSubtype,
		// engine/vdbe_agg.go). Both sides are pinned here.
		`SELECT json_quote(value) FROM json_each('[[7]]') GROUP BY key`,
		`SELECT json_quote(value) FROM json_each('[[7]]') GROUP BY value`,
		`SELECT json_quote(value) FROM json_each('[[7]]') GROUP BY key HAVING count(*)>0`,
		`SELECT json_quote(value) FROM json_each('[[7],[8]]') GROUP BY key ORDER BY key`,
		`SELECT json_quote(min(value)) FROM json_each('[[7],[8]]') GROUP BY key`,
		`SELECT json_group_array(value) FROM json_each('[[7],[8]]') GROUP BY key`,
		`SELECT json_quote(max(value)) FROM json_each('[[7],[8]]')`,
		`SELECT json_group_array(value) FROM json_each('[[7],[8]]')`,
		// Not row-sourced: a subtype PRODUCED inside a grouped query survives.
		`SELECT json_quote(json_array(key)) FROM json_each('[1,2]') GROUP BY key`,
		// Shapes that keep the tag and must not regress with the above.
		`SELECT json_quote(value) FROM json_each('[[7],[8]]') ORDER BY key DESC`,
		`SELECT DISTINCT json_quote(value) FROM json_each('[[7]]')`,
		`SELECT json_quote(value) FROM json_each('[[7]]') LIMIT 1`,
		`SELECT json_quote(a.value) FROM json_each('[[7]]') a, json_each('[1]') b`,
		// The opposite side of the same rule: a DERIVED table's column loses the
		// tag, which this must not regress.
		`SELECT json_quote(v) FROM (SELECT json_array(1) AS v)`,
		`WITH c(v) AS (SELECT json_array(1)) SELECT json_quote(v) FROM c`,
		`SELECT json_quote((SELECT json_array(1)))`,
		`SELECT json_quote(j) FROM (SELECT json('[1]') AS j)`,
	} {
		jsonEachCase(t, q)
	}
}

// TestJSONEachNested covers json_each as a row source inside the rest of the
// query pipeline.
func TestJSONEachNested(t *testing.T) {
	for _, q := range []string{
		`SELECT * FROM (SELECT key, value FROM json_each('[1,2,3]'))`,
		`SELECT sum(value) FROM (SELECT value FROM json_each('[1,2,3]'))`,
		`WITH c AS (SELECT key,value FROM json_each('[1,2]')) SELECT * FROM c`,
		`SELECT (SELECT count(*) FROM json_each('[1,2,3]'))`,
		`SELECT * FROM json_each('[1,2]') a, json_each('[3,4]') b ORDER BY a.key, b.key`,
		`SELECT value FROM json_each('[1,2,3]') WHERE value IN (SELECT value FROM json_each('[2,3,4]')) ORDER BY value`,
		`SELECT count(*) FROM json_tree('{"a":{"b":{"c":1}}}') WHERE type='object'`,
		`SELECT fullkey FROM json_tree('{"a":[1,{"b":2}]}') WHERE atom IS NOT NULL ORDER BY fullkey`,
		`SELECT j.key FROM json_each('[10,20]') j ORDER BY j.value DESC`,
		`SELECT * FROM json_each('[1,2,3]') LIMIT 2 OFFSET 1`,
	} {
		jsonEachCase(t, q)
	}
}

// TestJSONEachFuzzDifferential generates random JSON documents and compares
// every column of both modules against the oracle. This is the real gate on the
// "id" column: the JSONB size classes are crossed only by documents whose
// payloads land near 11, 255 and 65535 bytes, so the generator deliberately
// produces long strings and long numbers alongside small ones.
func TestJSONEachFuzzDifferential(t *testing.T) {
	// Deliberately NOT skipped under -short: it runs in about ten seconds and it
	// is the only broad check on the id column's JSONB arithmetic, so it belongs
	// in the routine gate rather than in a mode nobody runs by default.
	rng := rand.New(rand.NewSource(20260728))
	for i := 0; i < 60; i++ {
		doc := randJSONDoc(rng, 0)
		stmts := []string{
			`SELECT * FROM json_each(` + sqlQuote(doc) + `)`,
			`SELECT * FROM json_tree(` + sqlQuote(doc) + `)`,
			`SELECT key,value,type,atom,id,parent,fullkey,path,rowid FROM json_tree(` + sqlQuote(doc) + `)`,
		}
		// Every fullkey the document produces is itself a valid root path, so
		// re-rooting the walk at each of them exercises json_tree's root row --
		// whose key/path depend on whether the node is its parent's FIRST child
		// (jsonTreeRootKey, engine/vtab_json_each.go) -- across both branches of
		// that rule without having to construct the paths by hand.
		for _, p := range jsonFullkeys(t, doc) {
			stmts = append(stmts,
				`SELECT * FROM json_each(`+sqlQuote(doc)+`,`+sqlQuote(p)+`)`,
				`SELECT * FROM json_tree(`+sqlQuote(doc)+`,`+sqlQuote(p)+`)`)
		}
		if !differ(t, "jsoneachfuzz/"+strconv.Itoa(i), stmts) {
			t.Fatalf("case %d diverged; doc=%s", i, doc)
		}
	}
}

// jsonFullkeys asks the ORACLE for every fullkey in doc, so the fuzz can re-root
// a walk at each one. Reading them from the oracle rather than from musql is
// deliberate: a bug in musql's fullkey would otherwise narrow its own test.
func jsonFullkeys(t *testing.T, doc string) []string {
	t.Helper()
	res := run(t, "cgo", []string{`SELECT fullkey FROM json_tree(` + sqlQuote(doc) + `)`})
	if len(res) != 1 {
		t.Fatalf("fullkey probe returned %d results", len(res))
	}
	rows, _ := res[0]["rows"].([]any)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		cells, _ := r.([]any)
		if len(cells) != 1 {
			continue
		}
		s, _ := cells[0].(string)
		// The worker's normalized cells are type-tagged ("T:" for text).
		if !strings.HasPrefix(s, "T:") {
			continue
		}
		// A path whose quoted key carries a JSON escape used to be excluded here,
		// because the text-only json.go could not resolve one; the json.c port
		// compares labels with jsonLabelCompare, escapes and all, so every
		// fullkey is re-rooted.
		out = append(out, strings.TrimPrefix(s, "T:"))
	}
	return out
}

// randJSONDoc builds a random strict-RFC-8259 document. depth bounds the
// nesting so a case stays small enough to diff by eye when one fails.
func randJSONDoc(rng *rand.Rand, depth int) string {
	if depth >= 3 {
		return randJSONAtom(rng)
	}
	switch rng.Intn(6) {
	case 0, 1: // array
		n := rng.Intn(5)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = randJSONDoc(rng, depth+1)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case 2, 3: // object
		n := rng.Intn(5)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = randJSONKey(rng) + ":" + randJSONDoc(rng, depth+1)
		}
		return "{" + strings.Join(parts, ",") + "}"
	default:
		return randJSONAtom(rng)
	}
}

func randJSONAtom(rng *rand.Rand) string {
	switch rng.Intn(9) {
	case 0:
		return "null"
	case 1:
		return "true"
	case 2:
		return "false"
	case 3:
		return strconv.Itoa(rng.Intn(2000) - 1000)
	case 4: // a long number: crosses the 11/12-byte header boundary
		return strconv.FormatInt(rng.Int63(), 10)
	case 5:
		return "1.50"
	case 6:
		return strconv.FormatFloat(rng.Float64()*100, 'e', 4, 64)
	case 7: // a long string: crosses the 11/12 and 255/256 boundaries
		return `"` + strings.Repeat("a", rng.Intn(300)) + `"`
	default:
		return `"` + randJSONWord(rng) + `"`
	}
}

// randJSONKey emits keys spanning the bare-vs-quoted fullkey rule, including
// escapes, which are compared byte-for-byte in fullkey.
func randJSONKey(rng *rand.Rand) string {
	switch rng.Intn(8) {
	case 0:
		return `""`
	case 1:
		return `"a b"`
	case 2:
		return `"_x"`
	case 3:
		return `"9n"`
	case 4:
		return `"a\/b"`
	case 5:
		return `"q\"r"`
	case 6:
		return `"` + strings.Repeat("k", rng.Intn(20)+1) + `"`
	default:
		return `"` + randJSONWord(rng) + `"`
	}
}

func randJSONWord(rng *rand.Rand) string {
	const alpha = "abcXYZ019"
	n := rng.Intn(6) + 1
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteByte(alpha[rng.Intn(len(alpha))])
	}
	return sb.String()
}

// sqlQuote renders s as a SQL string literal.
func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
