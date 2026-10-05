package compat

// JSONB encoding and json_* functions against C SQLite.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var jsonbCases = []string{
	`SELECT hex(jsonb('[1]'))`,
	`SELECT hex(jsonb('{"a":[1,2.5,"x",true,false,null]}'))`,
	`SELECT hex(jsonb('"a\u0041"'))`,
	`SELECT hex(jsonb('''a\x41'''))`,
	`SELECT hex(jsonb('0x1F'))`,
	`SELECT hex(jsonb('-.5e3'))`,
	`SELECT hex(jsonb('{a:1,}'))`,
	`SELECT hex(jsonb(123))`,
	`SELECT hex(jsonb(1.5))`,
	`SELECT hex(jsonb('"' || printf('%.300c', 'x') || '"'))`,
	`SELECT length(jsonb('"' || printf('%.70000c', 'x') || '"'))`,
	`SELECT json_extract(jsonb('{"a":5}'),'$.a')`,
	`SELECT json(jsonb('{"a":[1,2.5,"x",true,false,null]}'))`,
	// ---- JSON5 normalization (json.c:2224-2366) ----
	`SELECT json('{a:.5}')`,
	`SELECT json('[0x10, -0x10, +5, .5, 5., 1e999, Infinity, -Infinity, NaN]')`,
	`SELECT json('[''a\x41\v\0'']')`,
	`SELECT json('{"a":1} // comment')`,
	`SELECT json('/* c */ [1]')`,
	`SELECT json_extract('[0xFFFFFFFFFFFFFFFF]','$[0]')`,
	`SELECT json_extract('[0x8000000000000000]','$[0]')`,
	`SELECT json_extract('[-9223372036854775808]','$[0]')`,
	`SELECT json_extract('[9223372036854775808]','$[0]')`,
	`SELECT json_extract('[123456789012345678901234567890]','$[0]')`,
	`SELECT json_pretty('{"a":[1,2,{"b":null}],"c":{}}')`,
	`SELECT json_pretty('[1,[2,[3]]]', '--')`,
	`SELECT json_error_position('[1,2,')`,
	`SELECT json_error_position('é{')`,
	`SELECT json_error_position(x'c00a')`,
	`SELECT json_array_insert('[1,2]','$[1]',9)`,
	`SELECT json_array_insert('[1,2]','$[#]',9)`,
	`SELECT json_array_insert('{"a":[1]}','$.a',9)`,
	`SELECT json_valid('{a:1}', 2)`,
	`SELECT json_valid(jsonb('{a:1}'), 8)`,
	`SELECT json_valid('x', 16)`,
	`SELECT hex(jsonb_array(1,'a',null,2.5))`,
	`SELECT hex(jsonb_object('a',1,'b','x'))`,
	`SELECT hex(jsonb_set('{"a":"xxxxxxxxxxxxxxxxxxxxxx","b":1}','$.a',2))`,
	`SELECT hex(jsonb_insert('[1,2,3]','$[#]',4))`,
	`SELECT hex(jsonb_remove('[1,2,3]','$[1]'))`,
	`SELECT hex(jsonb_patch('{"a":1,"b":2}','{"b":null,"c":3}'))`,
	`SELECT hex(jsonb_extract('{"a":[1,2]}','$.a','$.a[1]'))`,
	`SELECT hex(jsonb_group_object(k,v)) FROM (SELECT 'a' AS k, 1 AS v UNION ALL SELECT NULL, 2 UNION ALL SELECT 'b', 'x')`,
	`SELECT subtype(jsonb_group_array(1))`,
	`SELECT subtype(jsonb_group_object('a',1))`,
	`SELECT json(x'5b315d')`,
	`SELECT json_array(x'00')`,
	`SELECT json_quote(jsonb('[1,2]'))`,
	`SELECT json_quote(9e999)`,
	`SELECT json_extract('{"a b":1}','$["a b"]')`,
}

// TestJSONBFuzzDifferential drives every JSON function over generated JSON5
// text and generated (often malformed) JSONB. JSONB_FUZZ_N raises the
// iteration count for exploration; the default keeps it inside the routine
// gate.
func TestJSONBFuzzDifferential(t *testing.T) {
	n := 40
	if s := os.Getenv("JSONB_FUZZ_N"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			n = v
		}
	}
	seed := int64(20260917)
	if s := os.Getenv("JSONB_FUZZ_SEED"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			seed = v
		}
	}
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < n; i++ {
		var stmts []string
		docs := []string{
			sqlQuote(randJSON5Text(rng, 0)),
			"x'" + hex.EncodeToString(randJSONB(rng, 0)) + "'",
			"jsonb(" + sqlQuote(randJSONDoc(rng, 0)) + ")",
		}
		for k, d := range docs {
			stmts = append(stmts, jsonFuzzStatements(rng, d, k != 1)...)
		}
		if bad := jsonFuzzFirstDivergence(t, stmts); bad != "" {
			t.Fatalf("case %d diverged (seed %d):\n%s", i, seed, bad)
		}
	}
}

// jsonFuzzFirstDivergence runs stmts through both workers, as differ does, but
// reports only the first statement whose normalized result differs, so a
// divergence in a 100-statement batch is readable.
func jsonFuzzFirstDivergence(t *testing.T, stmts []string) string {
	t.Helper()
	want := run(t, "cgo", stmts)
	got := run(t, "musql", stmts)
	if len(want) != len(got) {
		return fmt.Sprintf("result count %d vs %d", len(want), len(got))
	}
	for i := range want {
		w, _ := json.Marshal(want[i])
		g, _ := json.Marshal(got[i])
		if string(w) != string(g) {
			return fmt.Sprintf("  sql:    %s\n  cgo:    %s\n  musql: %s", stmts[i], w, g)
		}
	}
	return ""
}

// jsonFuzzScalar reads one expression as its storage class, its exact value
// and its subtype.
func jsonFuzzScalar(e string) string {
	return "SELECT typeof(" + e + "), quote(" + e + "), subtype(" + e + ")"
}

// projectAny is false for a generated JSONB blob, which may be malformed in a
// way only some json_each columns detect. This engine materializes every
// column of a virtual table's row (vtab.go), while C reads only the columns a
// statement uses, so over such a blob a query naming a subset of columns can
// error here where C answers -- a decline, never a different answer -- and
// those statements read every column instead.
func jsonFuzzStatements(rng *rand.Rand, d string, projectAny bool) []string {
	p := func() string { return sqlQuote(randJSONPath(rng)) }
	v := func() string { return randJSONFuzzValue(rng) }
	d2 := sqlQuote(randJSON5Text(rng, 1))
	exprs := []string{
		"json(" + d + ")",
		"jsonb(" + d + ")",
		"json_valid(" + d + ")",
		"json_valid(" + d + "," + strconv.Itoa(1+rng.Intn(15)) + ")",
		"json_error_position(" + d + ")",
		"json_type(" + d + ")",
		"json_type(" + d + "," + p() + ")",
		"json_array_length(" + d + ")",
		"json_array_length(" + d + "," + p() + ")",
		"json_pretty(" + d + ")",
		"json_quote(" + d + ")",
		"json_extract(" + d + "," + p() + ")",
		"json_extract(" + d + "," + p() + "," + p() + ")",
		"jsonb_extract(" + d + "," + p() + ")",
		d + " -> " + p(),
		d + " ->> " + p(),
		d + " -> " + strconv.Itoa(rng.Intn(5)-2),
		"json_set(" + d + "," + p() + "," + v() + ")",
		"jsonb_set(" + d + "," + p() + "," + v() + "," + p() + "," + v() + ")",
		"json_insert(" + d + "," + p() + "," + v() + ")",
		"jsonb_insert(" + d + "," + p() + "," + v() + ")",
		"json_replace(" + d + "," + p() + "," + v() + ")",
		"jsonb_replace(" + d + "," + p() + "," + v() + ")",
		"json_array_insert(" + d + "," + p() + "," + v() + ")",
		"jsonb_array_insert(" + d + "," + p() + "," + v() + ")",
		"json_remove(" + d + "," + p() + ")",
		"jsonb_remove(" + d + "," + p() + "," + p() + ")",
		"json_patch(" + d + "," + d2 + ")",
		"jsonb_patch(" + d2 + "," + d + ")",
		"json_array(" + d + ")",
		"jsonb_object('k'," + d + ")",
	}
	var out []string
	for _, e := range exprs {
		out = append(out, jsonFuzzScalar(e))
	}
	out = append(out,
		"SELECT * FROM json_each("+d+")",
		"SELECT * FROM json_tree("+d+")",
		"SELECT key, quote(value), type, quote(atom), id, parent, fullkey, path FROM jsonb_tree("+d+")",
		"SELECT * FROM json_each("+d+","+p()+")",
		"SELECT key, quote(value), type, quote(atom), id, parent, fullkey, path FROM json_tree("+d+","+p()+")",
	)
	if projectAny {
		out = append(out,
			"SELECT quote(json_group_array(value)), quote(jsonb_group_object(fullkey, value)) FROM json_tree("+d+")",
			"SELECT key, fullkey FROM json_each("+d+","+p()+")",
		)
	}
	return out
}

func randJSONFuzzValue(rng *rand.Rand) string {
	vals := []string{
		"1", "2.5", "'str'", "NULL", "json('[1,{\"a\":2}]')", "jsonb('[3]')",
		"x'00'", "x'0102'", "'{\"x\":1}'", "json_quote('q')", "-0.0", "9e999",
	}
	return vals[rng.Intn(len(vals))]
}

func randJSONPath(rng *rand.Rand) string {
	steps := []string{
		".a", ".b", "[0]", "[1]", "[#]", "[#-1]", "[#-2]", `."x y"`, `."a\"b"`,
		".", "[", "[x]", "[99999999999]", `."`, ".$", "[#-9]", ".é",
	}
	switch rng.Intn(12) {
	case 0:
		return "$"
	case 1:
		return "a"
	case 2:
		return ""
	}
	var sb strings.Builder
	sb.WriteByte('$')
	for k := rng.Intn(3) + 1; k > 0; k-- {
		sb.WriteString(steps[rng.Intn(len(steps))])
	}
	return sb.String()
}

// randJSON5Text generates JSON with JSON5 extensions, and occasionally a
// defect, so the text parser's every arm is reached.
func randJSON5Text(rng *rand.Rand, depth int) string {
	ws := func() string {
		switch rng.Intn(12) {
		case 0:
			return " "
		case 1:
			return "\n\t"
		case 2:
			return "/* c */"
		case 3:
			return "// line\n"
		case 4:
			return " "
		case 5:
			return " "
		case 6:
			return "\v"
		}
		return ""
	}
	var s string
	if depth < 3 && rng.Intn(3) == 0 {
		n := rng.Intn(4)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = ws() + randJSON5Text(rng, depth+1) + ws()
		}
		trail := ""
		if n > 0 && rng.Intn(4) == 0 {
			trail = ","
		}
		s = "[" + strings.Join(parts, ",") + trail + "]"
	} else if depth < 3 && rng.Intn(2) == 0 {
		n := rng.Intn(4)
		parts := make([]string, n)
		keys := []string{`"a"`, `a`, `'b'`, `"x y"`, `$_k9`, `"a\"b"`, `\u0061bc`, `"é"`, `""`}
		for i := range parts {
			parts[i] = ws() + keys[rng.Intn(len(keys))] + ws() + ":" + ws() + randJSON5Text(rng, depth+1)
		}
		trail := ""
		if n > 0 && rng.Intn(4) == 0 {
			trail = ","
		}
		s = "{" + strings.Join(parts, ",") + trail + "}"
	} else {
		atoms := []string{
			"null", "true", "false", "0", "-0", "12", "-7", "1.50", "1e3", "-2E-3",
			"0x1F", "-0xab", "+5", ".5", "5.", "-.5", "+.5e2", "Infinity", "-Infinity",
			"NaN", "inf", "-INF", "QNaN", "9223372036854775807", "9223372036854775808",
			"-9223372036854775809", "123456789012345678901234567890", "1e400", "4.9e-325",
			`"s"`, `"a\u0041\n\t\\"`, `"\ud83d\ude00"`, `"\ud83d"`, `'q\'x'`, `"\x41\v\0"`,
			"\"line\\\ncont\"", `"tab	raw"`, `"é"`, `""`, `"12345678901234"`,
			"00", "1.", "0x", "--1", "tru", `"unterminated`, "[1,,2]", "{a}",
		}
		s = atoms[rng.Intn(len(atoms))]
	}
	if depth == 0 && rng.Intn(10) == 0 {
		s += " x"
	}
	return s
}

// randJSONB generates a JSONB element directly, choosing among a valid
// encoding, a non-minimal header, and outright corruption.
func randJSONB(rng *rand.Rand, depth int) []byte {
	var typ byte
	var payload []byte
	switch r := rng.Intn(20); {
	case r < 2:
		typ = byte(rng.Intn(3)) // null/true/false
	case r < 4:
		typ = 3
		payload = []byte([]string{"1", "-12", "007", "-", "1a", "9223372036854775808", "99999999999999999999"}[rng.Intn(7)])
	case r < 5:
		typ = 4
		payload = []byte([]string{"0x1F", "-0xff", "0x", "0xg", "1x2", "0xFFFFFFFFFFFFFFFFF", "0x8000000000000000"}[rng.Intn(7)])
	case r < 7:
		typ = byte(5 + rng.Intn(2))
		payload = []byte([]string{"1.5", "-.5", "5.", "1e3", "9e999", ".", "1e", "0.0", "01.5", "-9e999"}[rng.Intn(10)])
	case r < 11:
		typ = byte(7 + rng.Intn(4))
		payload = []byte([]string{"abc", `a\"b`, `\u0041`, `\x41`, `\'`, "\n", `\`, `"`, "é", "", `\ud83d\ude00`, `\q`, "a\x00b", "\\\n"}[rng.Intn(14)])
	case r < 17 && depth < 3:
		typ = byte(11 + rng.Intn(2))
		for k := rng.Intn(4); k > 0; k-- {
			payload = append(payload, randJSONB(rng, depth+1)...)
		}
	default:
		typ = byte(rng.Intn(16))
		payload = []byte("zz")
	}
	sz := len(payload)
	var hdr []byte
	switch rng.Intn(8) {
	case 0:
		hdr = []byte{typ | 0xc0, byte(sz)}
	case 1:
		hdr = []byte{typ | 0xd0, byte(sz >> 8), byte(sz)}
	case 2:
		hdr = []byte{typ | 0xe0, byte(sz >> 24), byte(sz >> 16), byte(sz >> 8), byte(sz)}
	case 3:
		hdr = []byte{typ | 0xf0, 0, 0, 0, 0, byte(sz >> 24), byte(sz >> 16), byte(sz >> 8), byte(sz)}
	case 4:
		// A size that disagrees with the payload.
		hdr = []byte{typ | byte((sz+1+rng.Intn(2))&0x0f)<<4}
	default:
		if sz <= 11 {
			hdr = []byte{typ | byte(sz)<<4}
		} else {
			hdr = []byte{typ | 0xc0, byte(sz)}
		}
	}
	return append(hdr, payload...)
}
