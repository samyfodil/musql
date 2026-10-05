package compat

import "testing"

// TestJSONEachHiddenColumnsMatchCSQLite compares with C SQLite what the
// hidden json and root columns report: the input's text rendering or the JSONB
// blob, and the root path text (json.c:5426-5437) -- and that an equality on
// them, which C consumes with omit=1, neither drops nor invents rows.
func TestJSONEachHiddenColumnsMatchCSQLite(t *testing.T) {
	var q []string
	for _, arg := range []string{"5", "5.5", "-7", "'5'", "x'01'", "jsonb('[1]')", "NULL", "'[1,2]'", "json('{\"a\":1}')", "x'7b7d'", "'[1]' || char(0) || 'x'"} {
		q = append(q,
			`SELECT typeof(json), quote(json), quote(root), key, value FROM json_each(`+arg+`)`,
			`SELECT typeof(json), quote(json), quote(root), fullkey FROM json_tree(`+arg+`)`,
			`SELECT quote(json), key FROM jsonb_each(`+arg+`)`,
		)
	}
	q = append(q,
		`SELECT quote(root), fullkey FROM json_tree('{"a":[1,{"b":2}]}', '$.a')`,
		`SELECT quote(root), fullkey FROM json_each('{"a":[1]}', CAST('$.a' AS BLOB))`,
		`SELECT key, quote(json) FROM json_each WHERE json = 5`,
		`SELECT key, quote(json) FROM json_each WHERE 5.5 = json`,
		`SELECT key, quote(json) FROM json_each WHERE json = x'7b7d'`,
		`SELECT key FROM json_each WHERE json = '[1,2]' AND json = 5`,
		`SELECT key FROM json_each WHERE json = 5 AND json = '[1,2]'`,
		`SELECT key FROM json_each WHERE json = '[1,2]' AND json = '[1,2]'`,
		`SELECT key FROM json_each('[1]') WHERE json = '[1]'`,
		`SELECT key FROM json_each('[1]') WHERE json = 5`,
		`SELECT key FROM json_each(5) WHERE json = '5'`,
		`SELECT key, quote(root) FROM json_each('{"a":[1]}') WHERE root = '$.a'`,
		`SELECT key, quote(root) FROM json_each WHERE json = '{"a":[1]}' AND root = '$.a'`,
		`CREATE TABLE t(j)`,
		`INSERT INTO t VALUES (5), ('[7,8]'), (x'7b7d'), (NULL)`,
		`SELECT t.j, e.key, quote(e.json) FROM t, json_each(t.j) e`,
		`SELECT e.key, quote(e.json) FROM t, json_each e WHERE e.json = t.j AND typeof(t.j) = 'text'`,
	)
	differ(t, "json_each hidden columns", q)
}
