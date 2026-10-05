package compat

// JSON PATH grammar comprehensively tested with every path-taking function.

import (
	"fmt"
	"testing"
)

// jsonPathDocs: a nested object/array and a bare array, covering all JSON types.
var jsonPathDocs = []string{
	`{"a":1,"b":[2,3,{"c":4}],"d":{"e":"f"},"g":null,"h":true,"i j":5,"":6}`,
	`[1,[2,3],{"k":"v"},null,true,"s",2.5]`,
}

// jsonPaths: valid and invalid paths, errors caught by renderQuery.
var jsonPaths = []string{
	`$`, `$.a`, `$.b`, `$.d`, `$.g`, `$.h`, `$.z`,
	`$.b[0]`, `$.b[1]`, `$.b[2]`, `$.b[2].c`, `$.b[3]`, `$.b[-1]`,
	`$.b[#-1]`, `$.b[#-2]`, `$.b[#]`, `$.b[#+1]`,
	`$.d.e`, `$.d.z`, `$.d.e.f`,
	`$[0]`, `$[1]`, `$[1][0]`, `$[2].k`, `$[#-1]`, `$[#]`, `$[6]`, `$[7]`,
	// A key needing quotes, and the spellings that are and are not allowed.
	`$."i j"`, `$.i j`, `$['a']`, `$."a"`, `$.""`, `$."z"`,
	// No leading "$", an empty path, whitespace, and a trailing dot.
	``, `a`, `.a`, `$ `, ` $`, `$.`, `$..a`, `$[`, `$[]`, `$[a]`,
	// Numeric-looking keys, and "$" as a key.
	`$.0`, `$.1`, `$."0"`, `$.$`, `$."$"`,
	// Deep miss, and a very long path.
	`$.a.b.c.d.e`, `$.b[0].c`,
}

// TestJSONPathMatrix tests paths with every path-taking JSON function.
func TestJSONPathMatrix(t *testing.T) {
	c, m := boundPair(t, nil)
	// One call form per function with %s for path.
	forms := []string{
		`SELECT quote(json_extract(%s, %s))`,
		`SELECT quote(json_type(%s, %s))`,
		`SELECT quote(json_array_length(%s, %s))`,
		`SELECT quote(json_set(%s, %s, 'NEW'))`,
		`SELECT quote(json_insert(%s, %s, 'NEW'))`,
		`SELECT quote(json_replace(%s, %s, 'NEW'))`,
		`SELECT quote(json_remove(%s, %s))`,
		`SELECT quote(jsonb_extract(%s, %s))`,
		`SELECT quote(json(jsonb_set(%s, %s, 'NEW')))`,
		`SELECT quote(json(jsonb_remove(%s, %s)))`,
		`SELECT group_concat(quote(key)||':'||quote(value),'|') FROM json_each(%s, %s)`,
		`SELECT group_concat(quote(fullkey)||'='||quote(atom),'|') FROM json_tree(%s, %s)`,
	}
	n, total := 0, 0
	for _, doc := range jsonPathDocs {
		for _, p := range jsonPaths {
			for _, f := range forms {
				q := fmt.Sprintf(f, sqlQuote(doc), sqlQuote(p))
				total++
				cv, mv := renderQuery(c, q), renderQuery(m, q)
				if cv != mv {
					n++
					if n <= 40 {
						t.Errorf("%s\n  cgo: %s\n  mus: %s", q, cv, mv)
					}
				}
			}
		}
	}
	if n > 0 {
		t.Errorf("%d of %d path x function cells diverged", n, total)
	}
}

// TestJSONPathlessFunctions tests JSON functions with no path argument.
func TestJSONPathlessFunctions(t *testing.T) {
	c, m := boundPair(t, nil)
	docs := []string{
		`{"a":1}`, `[1,2]`, `1`, `"s"`, `null`, `true`, `false`, `2.5`, `-0.0`,
		``, ` `, `{`, `[`, `{"a":}`, `{a:1}`, `{'a':1}`, `[1,]`, `{"a":1,}`,
		`{"a":1} `, ` {"a":1}`, "\t{\"a\":1}\n",
		`[1,2,3]`, `{"a":{"b":[1,{"c":2}]}}`,
		// Escape sequences: valid and invalid.
		`"\u0041"`, `"\uD83D\uDE00"`, `"\uD83D"`, `"\uDE00"`, `"\u00"`,
		`"\x41"`, `"\/"`, `"\a"`, `"a\"b"`, `"tab\there"`,
		`1e400`, `0x10`, `+1`, `01`, `1.`, `.5`, `-`, `1e`, `Infinity`, `NaN`,
		`{"a":1,"a":2}`, `[[[[[1]]]]]`,
	}
	forms := []string{
		`SELECT quote(json_valid(%s))`,
		`SELECT quote(json_valid(%s, 1))`,
		`SELECT quote(json_valid(%s, 2))`,
		`SELECT quote(json_valid(%s, 4))`,
		`SELECT quote(json_valid(%s, 8))`,
		`SELECT quote(json_error_position(%s))`,
		`SELECT quote(json(%s))`,
		`SELECT quote(json_pretty(%s))`,
		`SELECT quote(json_quote(%s))`,
		`SELECT quote(json_type(%s))`,
		`SELECT quote(json_array_length(%s))`,
		`SELECT quote(json_patch(%s, '{"p":9}'))`,
		`SELECT quote(json_patch('{"p":9}', %s))`,
		`SELECT quote(json(jsonb(%s)))`,
		`SELECT typeof(jsonb(%s))`,
	}
	n, total := 0, 0
	for _, d := range docs {
		for _, f := range forms {
			q := fmt.Sprintf(f, sqlQuote(d))
			total++
			cv, mv := renderQuery(c, q), renderQuery(m, q)
			if cv != mv {
				n++
				if n <= 40 {
					t.Errorf("%s\n  cgo: %s\n  mus: %s", q, cv, mv)
				}
			}
		}
	}
	if n > 0 {
		t.Errorf("%d of %d pathless cells diverged", n, total)
	}
}

// TestJSONMultiPathAndAggregates tests variadic forms, aggregates, and edge cases.
func TestJSONMultiPathAndAggregates(t *testing.T) {
	c, m := boundPair(t, []string{
		`CREATE TABLE t(k TEXT, v)`,
		`INSERT INTO t VALUES('a',1),('b','two'),('c',NULL),('d',2.5),('e',x'00ff')`,
	})
	n := 0
	for _, q := range []string{
		// Left-to-right application of paths.
		`SELECT quote(json_set('{}', '$.a', 1, '$.a.b', 2))`,
		`SELECT quote(json_set('{}', '$.a', json('{}'), '$.a.b', 2))`,
		`SELECT quote(json_insert('[1]', '$[#]', 2, '$[#]', 3))`,
		`SELECT quote(json_remove('[1,2,3]', '$[0]', '$[0]'))`,
		`SELECT quote(json_remove('[1,2,3]', '$[2]', '$[1]'))`,
		`SELECT quote(json_replace('{"a":1}', '$.a', 2, '$.b', 3))`,
		`SELECT quote(json_insert('{"a":1}', '$.a', 2, '$.b', 3))`,
		// Edge case argument counts.
		`SELECT quote(json_set('{}', '$.a'))`,
		`SELECT quote(json_set('{}'))`,
		`SELECT quote(json_remove('[1,2]'))`,
		`SELECT quote(json_extract('{"a":1}'))`,
		`SELECT quote(json_extract('{"a":1,"b":2}', '$.a', '$.b'))`,
		`SELECT quote(json_extract('{"a":1,"b":2}', '$.a', '$.z'))`,
		`SELECT quote(json_extract('[1,2]', '$[0]', '$[1]', '$[2]'))`,
		// Value type determines JSON vs string embedding.
		`SELECT quote(json_set('{}', '$.a', json('[1]')))`,
		`SELECT quote(json_set('{}', '$.a', '[1]'))`,
		`SELECT quote(json_set('{}', '$.a', NULL))`,
		`SELECT quote(json_set('{}', '$.a', x'00'))`,
		`SELECT quote(json_set('{}', '$.a', 2.5))`,
		`SELECT quote(json_set('{}', '$.a', jsonb('[1]')))`,
		// Aggregate functions.
		`SELECT quote(json_group_array(v)) FROM t`,
		`SELECT quote(json_group_array(v)) FROM t WHERE 0`,
		`SELECT quote(json_group_object(k, v)) FROM t`,
		`SELECT quote(json_group_object(k, v)) FROM t WHERE 0`,
		`SELECT quote(json_group_array(DISTINCT v)) FROM t`,
		`SELECT quote(json_group_array(v ORDER BY k DESC)) FROM t`,
		`SELECT quote(json(json_group_array(json_object('k',k,'v',v)))) FROM t`,
		`SELECT quote(json(jsonb_group_array(v))) FROM t`,
		`SELECT quote(json(jsonb_group_object(k, v))) FROM t`,
		// NULL key for json_group_object.
		`SELECT quote(json_group_object(NULL, 1))`,
		// json_object and json_array forms.
		`SELECT quote(json_object('a',1,'b',2))`,
		`SELECT quote(json_object('a'))`,
		`SELECT quote(json_object(1,2))`,
		`SELECT quote(json_object('a',json('[1]')))`,
		`SELECT quote(json_object('a',NULL))`,
		`SELECT quote(json_object('a',x'00'))`,
		`SELECT quote(json_object('a',1,'a',2))`,
		`SELECT quote(json_array(1,'a',NULL,2.5,json('[1]')))`,
		`SELECT quote(json_array(x'00'))`,
		`SELECT quote(json_array())`,
		`SELECT quote(json_object())`,
		// json_each and json_tree functions.
		`SELECT group_concat(je.key||'='||je.type,'|') FROM t, json_each(json_object('v',t.v)) je ORDER BY t.k`,
		`SELECT count(*) FROM json_tree('{"a":{"b":{"c":[1,2,3]}}}')`,
		`SELECT group_concat(fullkey,'|') FROM json_tree('{"a":{"b":{"c":[1,2]}}}')`,
		`SELECT group_concat(path,'|') FROM json_tree('{"a":[1,{"b":2}]}')`,
		`SELECT group_concat(quote(parent),'|') FROM json_tree('{"a":[1]}')`,
	} {
		cv, mv := renderQuery(c, q), renderQuery(m, q)
		if cv != mv {
			n++
			t.Errorf("%s\n  cgo: %s\n  mus: %s", q, cv, mv)
		}
	}
	if n > 0 {
		t.Errorf("%d json forms diverged", n)
	}
}
