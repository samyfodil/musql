// Tests JSON functions from engine/json.go against C SQLite, verifying that
// both value and storage-class type match for each expression.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// jsonCases is the deterministic differential gate: every entry is a
// complete "SELECT ..." statement expected to either error identically on
// both engines, or produce an identical (type, value) result.
var jsonCases = []string{
	// ---- json(): validate + minify, preserving original number/string
	// formatting verbatim (NOT a generic re-serialization -- see
	// engine/json.go's own doc comment) ----
	`SELECT json('{"a": 1,   "b":[1, 2, 3]}')`,
	`SELECT json('  {"a" : 1 , "b" :[ 1 , 2 ]}  ')`,
	`SELECT json('{"a":1e10}')`,
	`SELECT json('{"a":1E5}')`,
	`SELECT json('{"a":1.50}')`,
	`SELECT json('{"a":1.0}')`,
	`SELECT json('{"a":-0}')`,
	`SELECT json('{"a":-5}')`,
	`SELECT json('{"a":"hi\nthere"}')`,
	`SELECT json('{"a":"unicode é"}')`,
	`SELECT json('{"a":"tab\tend"}')`,
	`SELECT json(NULL)`,
	`SELECT json(5)`,
	`SELECT json(5.5)`,
	`SELECT json('true')`,
	`SELECT json('false')`,
	`SELECT json('null')`,
	`SELECT json('"hi"')`,
	`SELECT json('[]')`,
	`SELECT json('{}')`,
	`SELECT json('not json')`,
	`SELECT json('')`,
	`SELECT json('{"a":1,"a":2}')`,

	// ---- json_valid(): never errors, just 0/1 (or NULL for NULL) ----
	`SELECT json_valid('{"a":1}')`,
	`SELECT json_valid('not json')`,
	`SELECT json_valid(NULL)`,
	`SELECT json_valid(123)`,
	`SELECT json_valid(x'0102')`,

	// ---- json_type(X[, path]) ----
	`SELECT json_type('{"a":1}')`,
	`SELECT json_type('[1,2,3]')`,
	`SELECT json_type('1')`,
	`SELECT json_type('1.5')`,
	`SELECT json_type('true')`,
	`SELECT json_type('false')`,
	`SELECT json_type('null')`,
	`SELECT json_type('"hi"')`,
	`SELECT json_type(NULL)`,
	`SELECT json_type('{"a":1}', '$.a')`,
	`SELECT json_type('{"a":1}', '$.b')`,
	`SELECT json_type('{"a":1}', '$.a.b')`,
	`SELECT json_type('not json')`,

	// ---- json_extract(X, path...) ----
	`SELECT json_extract('{"a":1}')`,
	`SELECT json_extract('{"a":1}','$.a','$.b')`,
	`SELECT typeof(json_extract('{"a":1}','$.a','$.b'))`,
	`SELECT json_extract('{"a":1}','$.z')`,
	`SELECT json_extract('{"a":1,"a":2}','$.a')`,
	`SELECT json_extract('{"a":1}','$foo')`,
	`SELECT json_extract('{"a":1}','foo')`,
	`SELECT json_extract('{"a":1}','')`,
	`SELECT json_extract('{"a b":1}','$."a b"')`,
	`SELECT json_extract('[1,2,3]','$[-1]')`,
	`SELECT json_extract('[1,2,3]','$[5]')`,
	`SELECT json_extract('{"a":1}',NULL)`,
	`SELECT json_extract(NULL,'$.a')`,
	`SELECT json_extract('not json','$.a')`,
	`SELECT json_extract('{"a":"hi\nthere"}','$.a')`,
	`SELECT typeof(json_extract('{"a":"hi\nthere"}','$.a'))`,
	`SELECT json_extract('{"arr":[1,2,3]}','$.arr[1]')`,
	`SELECT json_extract('{"a":1}','$.a[0]')`,
	`SELECT json_extract('{"a":9223372036854775807}','$.a')`,
	`SELECT typeof(json_extract('{"a":9223372036854775807}','$.a'))`,
	`SELECT json_extract('{"a":9223372036854775808}','$.a')`,
	`SELECT typeof(json_extract('{"a":9223372036854775808}','$.a'))`,
	`SELECT json_extract('{"a":1.0}','$.a')`,
	`SELECT typeof(json_extract('{"a":1.0}','$.a'))`,
	`SELECT json_extract('{"a":{"b c":5}}', '$.a."b c"')`,
	`SELECT json_extract('{"a":true,"b":false,"c":null}','$.a','$.b','$.c')`,

	// ---- -> / ->> operators ----
	`SELECT '{"a":1,"b":2}' -> '$.a'`,
	`SELECT '{"a":1,"b":2}' ->> '$.a'`,
	`SELECT typeof('{"a":1}' -> '$.a')`,
	`SELECT typeof('{"a":1}' ->> '$.a')`,
	`SELECT '{"a":1}' -> '$'`,
	`SELECT '{"a":1}' ->> '$'`,
	`SELECT '[1,2,3]' -> 1`,
	`SELECT '[1,2,3]' -> '1'`,
	`SELECT '[1,2,3]' -> -1`,
	`SELECT '[1,2,3]' -> -5`,
	`SELECT '[1,2,3]' -> 0`,
	`SELECT '[1,2,3]' -> 5`,
	`SELECT '[1,2,3]' -> 1.5`,
	`SELECT '{"a":1}' -> 1`,
	`SELECT '{"a":1}' -> 'a'`,
	`SELECT json_extract('{"a":1}', 'a')`,
	`SELECT '{"a":1}' ->> 'a'`,
	`SELECT '[1,2,3]' -> 'a'`,
	`SELECT '{"a":1}' -> 'a.b'`,
	`SELECT '{"a":{"b":2}}' -> 'a.b'`,
	`SELECT '{"a":1}' -> NULL`,
	`SELECT NULL -> '$.a'`,
	`SELECT 'not json' -> '$.a'`,
	`SELECT 5 -> '$'`,
	`SELECT '{"a":1}' -> x'0102'`,
	`SELECT '{"a":1}' -> '$.a' -> '$'`,
	`SELECT '{"a":{"c":5}}' -> '$.a' ->> '$.c'`,
	`SELECT '{"a":1}' -> '$.a' * 2`,
	`SELECT json_array('{"a":1}' -> '$.a')`,
	`SELECT json_array('{"a":1}' ->> '$.a')`,
	`SELECT json_array('{"a":{"b":1}}' ->> '$.a')`,
	`SELECT json_array('{"a":{"b":1}}' -> '$.a')`,
	// The subtype does NOT survive being read back as a COLUMN of a materialized
	// row source -- a VIEW, a CTE (MATERIALIZED or not), or a derived table --
	// even though the sub-query that produced it tagged the value. SQLite's own
	// subtype1.test states that rule and these cases come from it; the runtime tag
	// carried the subtype straight through that boundary until
	// openMaterializedCursor started stripping it, and the full corpus sweep is
	// what caught it. A scalar SUBQUERY used as an EXPRESSION is the opposite case
	// and DOES keep the tag: there the value is the expression's result, not a
	// column read out of a row.
	`SELECT json_quote(b) FROM v2`,
	`SELECT json_array(b) FROM v2`,
	`SELECT count(*) FROM v2, jt1 WHERE NOT json_quote(b)`,
	`SELECT count(*) FROM jt3, jt1 WHERE NOT json_quote(b)`,
	`SELECT json_quote(y) FROM (SELECT +json('1') AS y)`,
	`SELECT count(*) FROM (SELECT +json('1') AS y) WHERE json_quote(y)='"1"'`,
	`SELECT json_quote(y) FROM (SELECT json_array(1) AS y)`,
	`SELECT json_array(y) FROM (SELECT json_array(1) AS y)`,
	`WITH t4(a) AS (SELECT json(1)) SELECT json_quote(a) FROM t4`,
	`WITH t4(a) AS MATERIALIZED (SELECT json(1)) SELECT json_quote(a) FROM t4`,
	`WITH t4(a) AS NOT MATERIALIZED (SELECT json(1)) SELECT json_quote(a) FROM t4`,

	// ---- json_array_length(X[, path]) ----
	`SELECT json_array_length('[1,2,3]')`,
	`SELECT json_array_length('[1,2,3]','$')`,
	`SELECT json_array_length('{"a":1}')`,
	`SELECT json_array_length('123')`,
	`SELECT json_array_length(NULL)`,
	`SELECT json_array_length('not json')`,
	`SELECT json_array_length('[1,[2,3],4]', '$[1]')`,

	// ---- json_quote(X) ----
	`SELECT json_quote(1)`,
	`SELECT json_quote(1.5)`,
	`SELECT json_quote('abc')`,
	`SELECT json_quote(NULL)`,
	`SELECT json_quote(x'0102')`,
	`SELECT json_quote(1.0)`,
	`SELECT json_quote(100.0)`,
	`SELECT json_quote(1e300)`,
	`SELECT json_quote(0.0001)`,
	`SELECT json_quote(1.5e-10)`,
	`SELECT json_quote(char(8))`,
	`SELECT json_quote(char(12))`,
	`SELECT json_quote(char(13))`,
	`SELECT json_quote(char(9))`,
	`SELECT json_quote(char(10))`,
	`SELECT json_quote(char(31))`,
	`SELECT json_quote(char(47))`,
	`SELECT json_quote('quote"in')`,
	`SELECT json_quote('back\slash')`,
	`SELECT typeof(json_quote(NULL))`,

	// ---- json_array(...) ----
	`SELECT json_array(1,2)`,
	`SELECT json_array()`,
	`SELECT json_array(1,2.5,'txt',NULL)`,
	`SELECT json_array(json('{"a":1}'))`,
	`SELECT json_array(json_extract('{"a":1}','$.a'))`,
	`SELECT json_array(json_extract('{"a":{"b":1}}','$.a'))`,
	`SELECT json_array(json_quote(5))`,
	`SELECT json_array(json_quote('abc'))`,
	`SELECT json_array(9223372036854775807)`,
	`SELECT json_array(-9223372036854775808)`,

	// ---- json_object(k1,v1,...) ----
	`SELECT json_object('a',1,'b','txt')`,
	`SELECT json_object('a',NULL)`,
	`SELECT json_object()`,
	`SELECT json_object('a',1,'a',2)`,
	`SELECT json_object(1,2)`,
	`SELECT json_object(NULL,2)`,
	`SELECT json_object('k', json_quote(5))`,

	// ---- json_insert()/json_replace()/json_set() ----
	`SELECT json_insert('{"a":1}','$.b',2)`,
	`SELECT json_insert('{"a":1}','$.a',2)`,
	`SELECT json_replace('{"a":1}','$.a',2)`,
	`SELECT json_replace('{"a":1}','$.b',2)`,
	`SELECT json_set('{"a":1}','$.a',2)`,
	`SELECT json_set('{"a":1}','$.b',2)`,
	`SELECT json_set('{"a":1}','$.a',2,'$.c',3)`,
	`SELECT json_insert('[1,2,3]','$[1]',99)`,
	`SELECT json_insert('[1,2,3]','$[10]',99)`,
	`SELECT json_insert('[1,2,3]','$[#]',99)`,
	`SELECT json_insert('{"a":1}')`,
	`SELECT json_insert('{"a":1}','$.b')`,
	`SELECT json_set('{"a":1}','$.b.c',5)`,
	`SELECT json_insert('{"a":1}','$.b.c',5)`,
	`SELECT json_replace('{"a":1}','$.b.c',5)`,
	`SELECT json_set('[1,2,3]','$[1]',99,'$[10]',88)`,
	`SELECT json_set('{"a":1}','$.a',1,'$.a',2)`,
	`SELECT json_set('{"a":1}','$',5)`,
	`SELECT json_insert('{"a":1}','$',5)`,
	`SELECT json_set('{"a":5}','$.a.b',1)`,
	`SELECT json_insert('{"a":5}','$.a.b',1)`,
	`SELECT json_set('[]','$[0]',1)`,
	`SELECT json_insert('[]','$[5]',1)`,
	`SELECT json_insert('[1,2]','$[5]',1)`,
	`SELECT json_set('{}','$.a',1)`,
	`SELECT json_insert(NULL,'$.a',1)`,
	`SELECT json_set(NULL,'$.a',1)`,
	`SELECT json_insert('bad','$.a',1)`,
	`SELECT json_set('{"a":1}','$.a',NULL)`,
	`SELECT json_replace('{"a":1}','$.a',NULL)`,
	`SELECT json_set('{"a": 1.50,   "b": 2}','$.b',5)`,
	`SELECT json_insert('{"a":1.50}','$.c',5)`,
	`SELECT json_replace('  {"a":1}  ','$.a',5)`,
	`SELECT json_set('{"a":1}', NULL, 5)`,
	`SELECT json_insert('{"a":1}', NULL, 5)`,
	`SELECT json_insert('{"a":1}','$.b', json('{"c":2}'))`,
	`SELECT json_insert('{"a":1}','$.b', json_quote(5))`,
	`SELECT json_set('{"a":1}','$.a', json('[1,2]'))`,
	// An UNTOUCHED value is reproduced MINIFIED from its own source span,
	// not span-copied verbatim: its number/string text survives exactly
	// ("1.50" stays "1.50") while insignificant whitespace disappears --
	// including whitespace nested INSIDE an untouched container, which a
	// verbatim copy would have kept ({"a":5,"b":[1,2]}, never
	// {"a":5,"b":[ 1 , 2 ]}).
	`SELECT json_set('{ "a" : 1 , "b" : [ 1 , 2 ] }','$.a',5)`,
	`SELECT json_insert('{ "a" : 1 , "b" : [ 1 , 2 ] }','$.c',5)`,
	`SELECT json_replace('{ "a" : 1 , "b" : { "c" : 3 } }','$.a',5)`,
	`SELECT json_set('[ [ 1 , 2 ] , 3 ]','$[1]',5)`,
	`SELECT json_set('[ [ 1 , 2 ] , 3 ]','$[#]',5)`,
	`SELECT json_set('{"a":{"b":[ 1 , 2 ]}}','$.a.c',5)`,
	// ... and a call with no (path, value) pair AT ALL still minifies X.
	`SELECT json_insert('  {"a": 1}  ')`,
	`SELECT json_set('{ "a" : [ 1 , 2 ] }')`,
	`SELECT json_replace('  5  ')`,

	// ---- json_remove(X, path, ...) ----
	//
	// The paths are applied LEFT TO RIGHT, each one resolved against the
	// result of the last ('$[0]' below sees the already-shortened
	// [0,1,3,4], so the answer is [1,3,4] and not [1,2,3,4]); a path that
	// doesn't resolve is simply a no-op; and with no path at all the answer
	// is X minified.
	`SELECT json_remove('[0,1,2,3,4]','$[2]')`,
	`SELECT json_remove('[0,1,2,3,4]','$[2]','$[0]')`,
	`SELECT json_remove('{"x":25,"y":42}','$.x')`,
	`SELECT json_remove('{"x":25,"y":42}','$.z')`,
	`SELECT json_remove('{"x":25,"y":42}')`,
	`SELECT json_remove('[0,1,2]','$[5]')`,
	`SELECT json_remove('{"a":{"b":1,"c":2}}','$.a.b')`,
	`SELECT json_remove('{"a":[1,2,3]}','$.a[1]')`,
	`SELECT json_remove('{"a":{"b":[{"c":1,"d":2}]}}','$.a.b[0].c')`,
	`SELECT json_remove('[[[1,2]]]','$[0][0][1]')`,
	`SELECT json_remove('[0,1,2,3,4,5]','$[1]','$[1]','$[1]')`,
	`SELECT json_remove('{"a":1,"b":2,"c":3,"d":4}','$.a','$.c')`,
	// Dropping the FIRST, the LAST, and the ONLY member/element: the
	// separator handling, which has to leave {} and [] behind rather than
	// a stray comma.
	`SELECT json_remove('[1]','$[0]')`,
	`SELECT json_remove('{"a":1}','$.a')`,
	`SELECT json_remove('[1,2]','$[0]')`,
	`SELECT json_remove('[1,2]','$[1]')`,
	`SELECT json_remove('{"a":1,"b":2}','$.a')`,
	`SELECT json_remove('{"a":1,"b":2}','$.b')`,
	`SELECT json_remove('{"a":1,"b":2,"c":3}','$.b')`,
	`SELECT json_remove('[1,2,3]','$[0]','$[0]','$[0]')`,
	`SELECT json_remove('{"a":1,"b":2}','$.a','$.b')`,
	`SELECT json_remove('{"a":{"b":1}}','$.a.b')`,
	`SELECT json_remove('[[1],[2]]','$[0][0]')`,
	`SELECT json_remove('{}','$.a')`,
	`SELECT json_remove('[]','$[0]')`,
	// Untouched values keep their VERBATIM source text, and a duplicate
	// key resolves to its FIRST occurrence (so removing '$.a' twice from
	// {"a":1,"a":2} empties the object).
	`SELECT json_remove('{"a":1.50,"b":2}','$.b')`,
	`SELECT json_remove('{"a":1e2,"b":2}','$.b')`,
	`SELECT json_remove('{"a":-0,"b":2}','$.b')`,
	`SELECT json_remove('{"a":"h\u0041i","b":2}','$.b')`,
	`SELECT json_remove('{"a":"h\u0041i","b":2}','$.a')`,
	`SELECT json_remove('{"a":"tab\tend","b":2}','$.b')`,
	`SELECT json_remove('[1.50, 2.0, 1e2]','$[1]')`,
	`SELECT json_remove('{ "a" : 1 , "b" : [ 1 , 2 ] }','$.a')`,
	`SELECT json_remove('{"o":{ "p" : { "q" : [ 1 , 2.50 ] } },"z":1}','$.z')`,
	`SELECT json_remove('{"a":1,"a":2}','$.a')`,
	`SELECT json_remove('{"a":1,"a":2}','$.a','$.a')`,
	`SELECT json_remove('{"dup":1,"dup":2}','$.b')`,
	// A path blocked by the wrong container kind, or by a scalar in the
	// way, is a no-op -- never an error.
	`SELECT json_remove('{"a":1}','$[0]')`,
	`SELECT json_remove('[1,2]','$.a')`,
	`SELECT json_remove('{"a":5}','$.a.b')`,
	`SELECT json_remove('5','$.a')`,
	`SELECT json_remove('5')`,
	`SELECT json_remove('  5  ')`,
	// Removing the ROOT and a NULL path both make the WHOLE call SQL NULL,
	// and both SHORT-CIRCUIT: a later malformed path is never even parsed
	// (json_remove('{"a":1}','$','bad path') is NULL, while that same
	// argument one position earlier is the "bad JSON path" error). The NULL
	// path is where json_remove differs from json_set()/json_insert()/
	// json_replace(), for which a NULL path is a per-pair NO-OP instead.
	// X itself is still validated ahead of either short-circuit, so
	// json_remove('not json',NULL) is the malformed-JSON error.
	`SELECT json_remove('[1,2,3]','$')`,
	`SELECT json_remove('{"a":1}','$.a','$')`,
	`SELECT json_remove('{"a":1}','$','bad path')`,
	`SELECT json_remove('5','$')`,
	`SELECT json_remove('{"a":1}',NULL)`,
	`SELECT json_remove('{"a":1}',NULL,'bad path')`,
	`SELECT json_remove('{"a":1}','bad path',NULL)`,
	`SELECT json_remove(NULL,'$.a')`,
	`SELECT json_remove(NULL,'bad path')`,
	`SELECT json_remove('not json','$.a')`,
	`SELECT json_remove('not json',NULL)`,
	`SELECT json_remove('{"a":1}','not a path')`,
	// Zero arguments is NULL, not a "wrong number of arguments" error:
	// C SQLite's variadic entry point just leaves its result unset.
	`SELECT json_remove()`,
	`SELECT json_remove(5,'$.a')`,
	`SELECT json_remove(x'0102','$.a')`,
	`SELECT json_remove('{"a":1}',5)`,
	`SELECT typeof(json_remove('{}'))`,
	`SELECT typeof(json_remove('[1,2]','$'))`,
	// The result is already-JSON (C SQLite's subtype), so a builder
	// EMBEDS it instead of re-quoting it as a string.
	`SELECT json_array(json_remove('{"a":1,"b":2}','$.b'))`,
	`SELECT json_object('k', json_remove('{"a":1,"b":2}','$.b'))`,
	`SELECT json_insert('{}','$.x', json_remove('[1,2]','$[0]'))`,
	`SELECT json_remove(json_remove('{"a":1,"b":2}','$.a'),'$.b')`,
	`SELECT json_extract(json_remove('{"a":1,"b":2}','$.a'),'$.b')`,
	`SELECT json_array(json_patch('{"a":1}','{}'))`,

	// ---- "[#]" / "[#-K]" array path segments ----
	//
	// "#" is the ONE-PAST-THE-LAST (append) position, so "[#-0]" is that
	// same position -- json_set('[0,1,2]','$[#-0]',9) APPENDS -- and
	// "[#-1]" is the last existing element. C SQLite resolves both forms
	// in EVERY path, a plain read's included, always as a match-or-no-match
	// and never as an error, and not only in the final segment.
	`SELECT json_remove('[0,1,2,3,4]','$[#-1]')`,
	`SELECT json_remove('[0,1,2,3,4]','$[#-1]','$[#-1]')`,
	`SELECT json_remove('[0,1,2,3,4]','$[#-5]')`,
	`SELECT json_remove('[0,1,2,3,4]','$[#-6]')`,
	`SELECT json_remove('[0,1,2,3,4]','$[#-0]')`,
	`SELECT json_remove('[0,1,2,3,4]','$[#]')`,
	`SELECT json_remove('{"a":[1,2,3]}','$.a[#-1]')`,
	`SELECT json_remove('[[1,2],[3,4]]','$[#-1][#-1]')`,
	`SELECT json_remove('[{"a":1,"b":2}]','$[#-1].a')`,
	`SELECT json_remove('{"a":1}','$[#-1]')`,
	`SELECT json_extract('[0,1,2,3,4]','$[#-1]')`,
	`SELECT json_extract('[0,1,2]','$[#-3]')`,
	`SELECT json_extract('[0,1,2]','$[#-4]')`,
	`SELECT json_extract('[0,1,2]','$[#-0]')`,
	`SELECT json_extract('[0,1,2]','$[#]')`,
	`SELECT json_extract('{"a":1}','$[#-1]')`,
	`SELECT json_extract('[[1,2],[3,4]]','$[#-1][#-1]')`,
	`SELECT json_extract('[{"a":1}]','$[#-1].a')`,
	`SELECT json_type('[0,1,2]','$[#-1]')`,
	`SELECT json_type('[0,1,2]','$[#]')`,
	`SELECT json_array_length('[[0,1,2]]','$[#-1]')`,
	`SELECT '[1,2,3]' -> '$[#-1]'`,
	`SELECT '[1,2,3]' ->> '$[#-1]'`,
	`SELECT json_set('[0,1,2]','$[#-1]',9)`,
	`SELECT json_set('[0,1,2]','$[#-0]',9)`,
	`SELECT json_set('[1]','$[#-2]',9)`,
	`SELECT json_set('[]','$[#-1]',9)`,
	`SELECT json_insert('[0,1,2]','$[#-1]',9)`,
	`SELECT json_replace('[0,1,2]','$[#-1]',9)`,
	`SELECT json_replace('[]','$[#-1]',9)`,
	`SELECT json_set('[[1]]','$[#-1][#]',9)`,
	// Auto-vivification creates an EMPTY array, so only a segment landing
	// at index 0 of one -- "[0]", "[#]", "[#-0]" -- can create it; "[#-K]"
	// for K>0 resolves to a negative index there and creates nothing.
	`SELECT json_set('{}','$.a[#]',7)`,
	`SELECT json_set('{}','$.a[#-0]',7)`,
	`SELECT json_set('{}','$.a[#-1]',7)`,
	`SELECT json_set('{}','$.a[#-2]',7)`,
	`SELECT json_set('{}','$.a[#].b[#]',7)`,
	`SELECT json_insert('{}','$.a[#-1][#]',7)`,
	// Malformed "#" content is still a bad path on both engines.
	`SELECT json_remove('[0,1,2]','$[#-]')`,
	`SELECT json_remove('[0,1,2]','$[# -1]')`,
	`SELECT json_remove('[0,1,2]','$[#+1]')`,
	`SELECT json_remove('[0,1,2]','$[#-1x]')`,
	// An index too large to fit an int is merely OUT OF RANGE, not a bad
	// path (C SQLite answers no-match / no-op, not an error).
	`SELECT json_extract('[0,1,2]','$[99999999999999999999]')`,
	`SELECT json_extract('[0,1,2]','$[#-99999999999999999999]')`,
	`SELECT json_remove('[0,1,2]','$[99999999999999999999]')`,
	`SELECT json_set('[0,1,2]','$[99999999999999999999]',9)`,

	// ---- json_group_array() / json_group_object() aggregates ----
	`SELECT json_group_array(x) FROM (SELECT 1 AS x UNION ALL SELECT 2 UNION ALL SELECT NULL)`,
	`SELECT json_group_object(k,v) FROM (SELECT 'a' AS k, 1 AS v UNION ALL SELECT 'b',2)`,
	`SELECT json_group_array(x) FROM (SELECT 1 AS x WHERE 0)`,
	`SELECT json_group_object(k,v) FROM (SELECT 'a' AS k, 1 AS v WHERE 0)`,
	`SELECT json_group_array(DISTINCT x) FROM (SELECT 1 AS x UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT NULL UNION ALL SELECT NULL)`,
	`SELECT json_group_object(k,v) FROM (SELECT 'a' AS k, NULL AS v)`,
	`SELECT json_group_object(k,v) FROM (SELECT NULL AS k, 1 AS v)`,
	`SELECT json_group_object(k,v) FROM (SELECT 5 AS k, 1 AS v)`,
	`SELECT json_group_object(k,v) FROM (SELECT 5.5 AS k, 1 AS v)`,
	`SELECT json_group_array(json_extract(x,'$.a')) FROM (SELECT '{"a":1}' AS x UNION ALL SELECT '{"a":2}')`,

	// ---- json_patch(): RFC 7386 merge patch ----
	//
	// A non-object PATCH replaces the target outright; an object patch over a
	// non-object target merges into {}; a null IN THE PATCH deletes a key
	// while a null already in the TARGET survives; a brand-new key still has
	// its own nested nulls stripped; key order is the target's, with new keys
	// appended in patch order; and untouched values keep their VERBATIM
	// source text (1.50, 1e2, A all survive unchanged).
	`SELECT json_patch('{"a":"b"}','{"a":"c"}')`,
	`SELECT json_patch('{"a":"b"}','{"b":"c"}')`,
	`SELECT json_patch('{"a":"b"}','{"a":null}')`,
	`SELECT json_patch('{"a":"b","b":"c"}','{"a":null}')`,
	`SELECT json_patch('{"a":["b"]}','{"a":"c"}')`,
	`SELECT json_patch('{"a":"c"}','{"a":["b"]}')`,
	`SELECT json_patch('{"a":{"b":"c"}}','{"a":{"b":"d","c":null}}')`,
	`SELECT json_patch('{"a":[{"b":"c"}]}','{"a":[1]}')`,
	`SELECT json_patch('["a","b"]','["c","d"]')`,
	`SELECT json_patch('{"a":"b"}','["c"]')`,
	`SELECT json_patch('{"a":"foo"}','null')`,
	`SELECT json_patch('{"a":"foo"}','"bar"')`,
	`SELECT json_patch('{"e":null}','{"a":1}')`,
	`SELECT json_patch('[1,2]','{"a":"b","c":null}')`,
	`SELECT json_patch('{}','{"a":{"bb":{"ccc":null}}}')`,
	`SELECT json_patch('{ "a": "b", "c": { "d": "e", "f": "g" } }','{ "a":"z", "c": { "f": null } }')`,
	`SELECT json_patch('{"a":1}','{}')`,
	`SELECT json_patch('{"a":{"x":1}}','{"a":null}')`,
	`SELECT json_patch('{"a":1}','{"a":{"b":2}}')`,
	`SELECT json_patch('{"a":{"b":1}}','{"a":2}')`,
	`SELECT json_patch('{"a":{}}','{"a":{"b":null}}')`,
	`SELECT json_patch('{"a":1}','{"b":null,"c":2}')`,
	`SELECT json_patch('true','{"a":1}')`,
	`SELECT json_patch('{"a":1}','true')`,
	`SELECT json_patch('[]','{}')`,
	`SELECT json_patch('{}','[]')`,
	// verbatim source text of untouched values
	`SELECT json_patch('{"a":1.50,"b":1e2,"c":"xA"}','{"z":1}')`,
	`SELECT json_patch('{"a":1.50}','{}')`,
	`SELECT json_patch('{"a":{"b":1.50}}','{"a":{"c":2}}')`,
	`SELECT json_patch('{"dup":1,"dup":2}','{"z":1}')`,
	`SELECT json_patch('{"a":"é"}','{"z":1}')`,
	// NULL propagation and malformed input
	`SELECT json_patch(NULL,'{"a":1}')`,
	`SELECT json_patch('{"a":1}',NULL)`,
	`SELECT json_patch('not json','{}')`,
	`SELECT json_patch('{}','not json')`,
	`SELECT typeof(json_patch('{}','{}'))`,
	`SELECT json_patch('{}')`,
	`SELECT json_patch('{}','{}','{}')`,

	// ---- json_quote() over an ALREADY-JSON argument passes it THROUGH ----
	//
	// C SQLite tags a JSON-returning function's result with a SUBTYPE, and
	// json_quote() returns a subtyped argument verbatim instead of quoting it.
	// This package approximates that tag statically (engine/json.go), and the
	// approximation is applied to json_quote's own argument by ELIDING the call:
	// every case below answered the double-quoted string form before that.
	`SELECT json_quote(json('[1,2]'))`,
	`SELECT json_quote(json_array(1,2))`,
	`SELECT json_quote(json_object('a',1))`,
	`SELECT json_quote(json_insert('{}','$.a',1))`,
	`SELECT json_quote(json_set('{}','$.a',1))`,
	`SELECT json_quote(json_replace('{"a":1}','$.a',2))`,
	`SELECT json_quote(json_patch('{}','{"a":1}'))`,
	`SELECT json_quote(json_remove('[1,2]','$[0]'))`,
	`SELECT json_quote(json_quote('x'))`,
	`SELECT json_quote(json('null'))`,
	`SELECT json_quote(json('true'))`,
	`SELECT json_quote(json('"s"'))`,
	`SELECT json_quote(json('3'))`,
	`SELECT json_quote(json_array(1,2) -> '$[0]')`,
	`SELECT json_quote('[1]' -> '$[0]')`,
	// ... and is NOT applied to anything that only SOMETIMES returns JSON, nor
	// to the functions that return a plain SQL value. json_quote must still
	// QUOTE each of these.
	`SELECT json_quote('[1,2]')`,
	`SELECT json_quote('abc')`,
	`SELECT json_quote(42)`,
	`SELECT json_quote(3.14)`,
	`SELECT json_quote(NULL)`,
	`SELECT json_quote(json_type('[1]'))`,
	`SELECT json_quote(json_valid('[1]'))`,
	`SELECT json_quote(json_array_length('[1]'))`,
	`SELECT json_quote(json_array(1,2) ->> '$[0]')`,
	`SELECT json_quote(json_extract('[1]','$[0]'))`,      // an extracted SCALAR carries no subtype
	`SELECT json_quote(json_extract('{"a":"s"}','$.a'))`, // ... including a string
	`SELECT json_quote(json_array(1) || '')`,             // concatenation loses it
	// The subtype is a RUNTIME tag on the value (Value.Subtype), so it follows a
	// value through every construct that merely COPIES it, and is lost through
	// every construct that builds a fresh one. Both halves verified against the
	// oracle; the parse-time approximation this replaced got the whole first
	// group wrong, since none of them is a directly-nested call.
	`SELECT json_quote((SELECT json_array(1)))`,
	`SELECT json_quote(CASE WHEN 1 THEN json_array(1) ELSE 'x' END)`,
	`SELECT json_quote(CASE WHEN 0 THEN json_array(1) ELSE 'x' END)`,
	`SELECT json_quote(coalesce(json_array(1),'x'))`,
	`SELECT json_quote(coalesce(NULL,json_array(1)))`,
	`SELECT json_quote(iif(1,json_array(1),'x'))`,
	`SELECT json_quote(iif(0,'x',json_array(1)))`,
	`SELECT json_quote(+json_array(1))`,
	`SELECT json_quote(max(json_array(1)))`,
	`SELECT json_quote(min(json_array(1)))`,
	`SELECT json_quote(CAST(json_array(1) AS TEXT))`,
	`SELECT json_array(coalesce(json_array(1),'x'))`,
	`SELECT json_array(CASE WHEN 1 THEN json_array(1) ELSE 'x' END)`,
	`SELECT json_array((SELECT json_array(1)))`,
	`SELECT json_array(+json_array(1))`,
	`SELECT json_object('a',CASE WHEN 1 THEN json_array(1) ELSE 'x' END)`,
	`SELECT json_insert('{}','$.a',coalesce(json_array(1),'x'))`,
	`SELECT json_group_array(coalesce(json_array(1),'x'))`,
	// json_extract's subtype is CONDITIONAL -- set only for an array or object --
	// which is why a parse-time rule could never express it. And "->>" shares
	// json_extract's node-to-SQL-value converter but is NEVER subtyped, which the
	// existing json_array cases in this file caught when the tag was briefly put
	// in that shared converter instead of at json_extract's own call site.
	`SELECT json_quote(json_extract('{"a":[1]}','$.a'))`,
	`SELECT json_quote(json_extract('{"a":{"b":1}}','$.a'))`,
	`SELECT json_array(json_extract('{"a":[1]}','$.a'))`,
	`SELECT json_array(json_extract('{"a":"s"}','$.a'))`,
	`SELECT json_array('{"a":{"b":1}}' ->> '$.a')`,
	`SELECT json_array('{"a":{"b":1}}' -> '$.a')`,
	// The AGGREGATE builders are JSON producers too, in BOTH directions: their
	// results pass through json_quote, and they embed rather than re-quote each
	// other's results. Both were wrong before.
	`SELECT json_quote(json_group_array(1))`,
	`SELECT json_quote(json_group_object('k',1))`,
	`SELECT json_array(json_group_array(1))`,
	`SELECT json_array(json_group_object('k',1))`,
	`SELECT json_object('a',json_group_array(1))`,
	`SELECT json_group_array(json_group_array(1))`,
	// Zero-argument builders: json_quote's elision reads args[0], and a nil arg
	// slice is legitimate for these two. Reading it unconditionally PANICKED.
	`SELECT json_array()`,
	`SELECT json_object()`,
	// A QUOTED PATH KEY carrying a JSON escape. Nine of these were wrong at
	// once, across every json1 entry point, because parseJSONPath neither
	// skipped an escaped quote while scanning nor DECODED the key before
	// matching it against the document's (already decoded) key -- while a
	// MUTATION has to write the key's raw path text back out verbatim. See
	// jsonPathSeg / jsonPathKeyDecode (engine/json.go).
	`SELECT json_extract('{"a\tb":9}','$."a\tb"')`,
	`SELECT json_extract('{"a\/b":9}','$."a/b"')`,
	`SELECT json_extract('{"a/b":9}','$."a\/b"')`,
	`SELECT json_extract('{"a\\b":9}','$."a\\b"')`,
	`SELECT json_extract('{"q\"r":9}','$."q\"r"')`,
	`SELECT json_extract('{"a\nb":9}','$."a\nb"')`,
	`SELECT json_extract('{"a\rb":9}','$."a\rb"')`,
	`SELECT json_extract('{"a\bb":9}','$."a\bb"')`,
	`SELECT json_extract('{"a\fb":9}','$."a\fb"')`,
	// A LITERAL control character in the path key matches an escaped document
	// key -- strict RFC 8259 forbids it inside a JSON string, so the decoder
	// re-encodes it rather than rejecting the path.
	"SELECT json_extract('{\"a\\tb\":9}','$.\"a\tb\"')",
	// An escape JSON does not define matches NOTHING, and is not an error.
	`SELECT json_extract('{"aqb":1}','$."a\qb"')`,
	`SELECT json_extract('{"a\\qb":1}','$."a\qb"')`,
	// The mutation side writes the key's RAW path text through verbatim.
	`SELECT json_set('{}','$."a\tb"',1)`,
	`SELECT json_set('{}','$."a\/b"',1)`,
	`SELECT json_set('{}','$."a\"b"',1)`,
	`SELECT json_set('{}','$."a\\b"',1)`,
	`SELECT json_insert('{}','$."a\tb"',1)`,
	`SELECT json_replace('{"a\tb":1}','$."a\tb"',2)`,
	`SELECT json_remove('{"a\tb":9,"z":1}','$."a\tb"')`,
	`SELECT json_set('{}','$."a\tb"."c\nd"',1)`,
	`SELECT json_type('{"a\tb":9}','$."a\tb"')`,
	`SELECT '{"a\tb":9}' -> '$."a\tb"'`,
	`SELECT '{"a\tb":9}' ->> '$."a\tb"'`,
	`SELECT json_array_length('{"a\tb":[1,2]}','$."a\tb"')`,
	// The bare (unquoted) key form cannot carry an escape and is unchanged.
	`SELECT json_set('{}','$.a b',1)`,
	`SELECT json_extract('{"a b":1}','$.a b')`,
}

// jsonFormerlyDeclinedCases are the constructs the text-only json.go used to
// decline (an engine error was all this file asserted). Most SUCCEED in real
// SQLite -- JSON5 is parsed and normalized, a blob that looks like JSONB is
// read as JSONB -- and they are now compared against it like any other case.
var jsonFormerlyDeclinedCases = []string{
	`SELECT json('{a:1}')`,                                  // JSON5 unquoted key
	`SELECT json("{'a':1}")`,                                // JSON5 single-quoted strings
	`SELECT json('{"a":1,}')`,                               // JSON5 trailing comma
	`SELECT json('{"a":.5}')`,                               // JSON5 leading-dot number
	`SELECT json('{"a":5.}')`,                               // JSON5 trailing-dot number
	`SELECT json('{"a":+5}')`,                               // JSON5 leading '+'
	`SELECT json('{"a":0x1F}')`,                             // JSON5 hex integer
	`SELECT json('{"a":NaN}')`,                              // JSON5 NaN
	`SELECT json('{"a":Infinity}')`,                         // JSON5 Infinity
	`SELECT json('{"a":-Infinity}')`,                        // JSON5 -Infinity
	`SELECT json_array(x'0102')`,                            // JSONB (2-byte blob, not a valid single-byte JSONB scalar either)
	`SELECT json_object('a', x'0102')`,                      // JSONB
	`SELECT json_insert('{"a":1}','$.b',x'0102')`,           // JSONB
	`SELECT json_group_array(x) FROM (SELECT x'0102' AS x)`, // JSONB
	`SELECT json_array(x'00')`,                              // JSONB: x'00' happens to be a valid single-byte JSONB "null" scalar in C SQLite
	`SELECT json_extract('[10,20]','$["0"]')`,               // bracket-quoted-digit path (C SQLite itself errors here too, but for a different, obscure reason this package doesn't attempt to reproduce)
	`SELECT json_extract('{"a b":1}','$["a b"]')`,           // bracket-quoted-string path (C SQLite succeeds here, as a no-match NULL -- this package declines any quoted bracket content, not just the ambiguous digit case above)
	`SELECT json_extract('[1,2,3]', -1)`,                    // non-text/non-"$..."-prefixed path argument for json_extract (only -> / ->> get the bare-integer shortcut)
}

func TestJSONFunctionsMatchCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			testJSONScenario(t, pageSize)
		})
	}
}

func testJSONScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("json_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Close()

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()

	// A small fixture, for the cases that need a real row source: the JSON
	// SUBTYPE is dropped when a value is read back as a COLUMN of a VIEW, CTE or
	// derived table, and kept when it is a scalar subquery's own result. Both
	// halves come from SQLite's own subtype1.test. Everything else in jsonCases
	// is a pure expression and needs no schema.
	for _, ddl := range []string{
		`CREATE TABLE jt1(a)`,
		`INSERT INTO jt1 VALUES('x')`,
		`CREATE TABLE jt3(b)`,
		`INSERT INTO jt3 VALUES(json(TRUE))`,
		`CREATE VIEW v2(b) AS SELECT json(TRUE)`,
	} {
		if eerr := db.Exec(ddl); eerr != nil {
			t.Fatalf("engine fixture %s: %v", ddl, eerr)
		}
		if _, eerr := sdb.Exec(ddl); eerr != nil {
			t.Fatalf("cgo fixture %s: %v", ddl, eerr)
		}
	}

	for _, s := range jsonCases {
		t.Run(s, func(t *testing.T) {
			compareOneScalar(t, db, sdb, s)
		})
	}
	for _, s := range jsonFormerlyDeclinedCases {
		t.Run(s, func(t *testing.T) {
			compareOneScalar(t, db, sdb, s)
		})
	}
	for _, s := range jsonbCases {
		t.Run(s, func(t *testing.T) {
			compareOneScalar(t, db, sdb, s)
		})
	}
}
