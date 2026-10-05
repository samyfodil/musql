package compat

// TestTailR30Subtype tests the subtype() function for JSON values across
// various constructs that either copy or lose the JSON subtype.

import "testing"

func TestTailR30Subtype(t *testing.T) {
	godb, cdb := tailR30PragmaPair(t)
	for _, s := range []string{
		"CREATE TABLE s1(k INTEGER PRIMARY KEY, v)",
		"INSERT INTO s1 VALUES(1, json('[1,2]')), (2, '[1,2]'), (3, json_quote(4))",
		"CREATE INDEX s1v ON s1(v)",
		"CREATE VIEW vj AS SELECT json(1) AS x",
	} {
		tailR30Both(t, godb, cdb, s)
	}

	for _, stmt := range []string{
		"SELECT subtype(1), subtype(NULL), subtype('abc'), subtype(x'00'), subtype(0.5)",
		"SELECT subtype(json(1)), subtype(json('{\"a\":1}')), subtype(json_array(1,2))",
		"SELECT subtype(json_object('a',1)), subtype(json_quote('x'))",
		"SELECT subtype(json_insert('{}','$.a',1)), subtype(json_replace('{\"a\":2}','$.a',1))",
		"SELECT subtype(json_set('{}','$.a',1)), subtype(json_patch('{}','{}'))",
		"SELECT subtype(json_remove('{\"a\":1}','$.a'))",
		"SELECT subtype(json_type('{\"a\":1}')), subtype(json_valid('{}'))",
		"SELECT subtype(json_extract('{\"a\":{\"b\":1}}','$.a')), subtype(json_extract('{\"a\":1}','$.a'))",
		"SELECT subtype(json_extract('{\"a\":\"x\"}','$.a')), subtype(json_extract('{\"a\":null}','$.a'))",
		"SELECT subtype(json_extract('[1,2]','$[0]','$[1]'))",
		"SELECT subtype('{\"a\":1}' -> '$.a'), subtype('{\"a\":1}' ->> '$.a')",
		"SELECT subtype(coalesce(json(1),1)), subtype(ifnull(NULL,json(1)))",
		"SELECT subtype(CASE WHEN 1 THEN json(1) END), subtype(nullif(json(1),9))",
		"SELECT subtype(iif(1,json(1),2)), subtype(likely(json(1))), subtype(unlikely(json(1)))",
		"SELECT subtype(likelihood(json(1),0.5)), subtype(+json(1)), subtype(json(1) COLLATE nocase)",
		"SELECT subtype(CAST(json(1) AS TEXT)), subtype((SELECT json(1)))",
		"SELECT subtype(json(1)||''), subtype(-json(1)), subtype(CAST(json(1) AS BLOB))",
		"SELECT subtype(CAST(json(1) AS INTEGER)), subtype(substr(json('[1,2]'),1,2))",
		"SELECT subtype(replace(json('[1,2]'),'x','y')), subtype(trim(json('[1,2]')))",
		"SELECT subtype(quote(json(1))), subtype(hex(json(1))), subtype(printf('%s',json(1)))",
		"SELECT subtype(json_valid(json(1))), subtype(sum(1))",
		"SELECT k, subtype(v) FROM s1 ORDER BY k",
		"SELECT k, subtype(v) FROM s1 WHERE v='[1,2]' ORDER BY k",
		"SELECT subtype(v) FROM s1 INDEXED BY s1v WHERE v>'' ORDER BY k",
		"WITH t4(a) AS MATERIALIZED (SELECT json(1)) SELECT subtype(a) FROM t4",
		"WITH t4(a) AS NOT MATERIALIZED (SELECT json(1)) SELECT subtype(a) FROM t4",
		"SELECT subtype(x) FROM (SELECT json(1) AS x)",
		"SELECT subtype(x) FROM (SELECT json(1) AS x) GROUP BY x",
		"SELECT subtype(max(x)) FROM (SELECT json(1) AS x)",
		"SELECT subtype(x) FROM vj",
		"SELECT subtype(x) FROM (SELECT json(1) AS x UNION ALL SELECT json(2))",
		"SELECT subtype(x) FROM (SELECT json(1) AS x UNION SELECT json(2))",
		"SELECT subtype(value) FROM json_each('[1,2]')",
		"SELECT subtype(value) FROM json_each('[[1],2]')",
		"SELECT subtype(value) FROM json_each('{\"a\":{\"b\":1},\"c\":2}')",
		"SELECT subtype(min(json(1))), subtype(group_concat(json(1))) FROM s1",
		"SELECT subtype(json_array(json(1))), subtype(json_object('a',json(1)))",
		"SELECT typeof(subtype(json(1))), subtype(subtype(json(1)))",
		"SELECT subtype(json(1)) FROM s1 GROUP BY k HAVING subtype(json(1))=74",
	} {
		tailR30AssertSame(t, godb, cdb, stmt)
	}

	// A wrong argument count is an error on both sides.
	for _, stmt := range []string{"SELECT subtype()", "SELECT subtype(1,2)"} {
		if _, _, cerr := tclRunCGOQuery(cdb, stmt); cerr == nil {
			t.Fatalf("premise broken: the oracle ACCEPTS %q", stmt)
		}
		if _, _, err, panicked, pv := tclSafeGoQuery(godb, stmt); panicked {
			t.Fatalf("go engine PANICKED on %q: %v", stmt, pv)
		} else if err == nil {
			t.Errorf("%q: this engine accepted a shape the oracle rejects", stmt)
		}
	}
}

// TestTailR30SubtypeNeverWrongInWindowQuery tests subtype behavior in window
// functions, where this engine may differ from SQLite's ephemeral table behavior.
func TestTailR30SubtypeNeverWrongInWindowQuery(t *testing.T) {
	godb, cdb := tailR30PragmaPair(t)
	tailR30Both(t, godb, cdb, "CREATE TABLE s1(k INTEGER PRIMARY KEY, v)")
	tailR30Both(t, godb, cdb, "INSERT INTO s1 VALUES(1,'a'),(2,'b')")

	// Declines are allowed here; a DIFFERENT answer is not.
	for _, stmt := range []string{
		"SELECT subtype(first_value(json(1)) OVER ()) FROM s1",
		"SELECT subtype(last_value(json('[9]')) OVER ()) FROM s1",
		"SELECT subtype(nth_value(json('[9]'),1) OVER ()) FROM s1",
		"SELECT subtype(lead(json('[9]')) OVER ()) FROM s1",
		"SELECT subtype(lag(json('[9]')) OVER ()) FROM s1",
		"SELECT subtype(min(json('[9]')) OVER ()) FROM s1",
		"SELECT subtype(max(json('[9]')) OVER ()) FROM s1",
		"SELECT subtype(json_group_array(k) OVER ()) FROM s1",
		"SELECT subtype(row_number() OVER ()) FROM s1",
		"SELECT subtype(CASE WHEN 1 THEN first_value(json(1)) OVER () END) FROM s1",
	} {
		wantCols, wantRows, cerr := tclRunCGOQuery(cdb, stmt)
		if cerr != nil {
			t.Fatalf("oracle rejected %q: %v", stmt, cerr)
		}
		gotCols, gotRows, err, panicked, pv := tclSafeGoQuery(godb, stmt)
		if panicked {
			t.Fatalf("go engine PANICKED on %q: %v", stmt, pv)
		}
		if err != nil {
			continue // a clean decline is acceptable; a wrong answer is not
		}
		if len(gotCols) != len(wantCols) || len(gotRows) != len(wantRows) {
			t.Errorf("%q: shape %v/%d, oracle %v/%d", stmt, gotCols, len(gotRows), wantCols, len(wantRows))
			continue
		}
		for r := range gotRows {
			for c := range gotRows[r] {
				if gotRows[r][c] != wantRows[r][c] {
					t.Errorf("%q row %d col %d: %q, oracle says %q", stmt, r, c, gotRows[r][c], wantRows[r][c])
				}
			}
		}
	}

	// Control rows: window shapes that already agree between engines.
	for _, stmt := range []string{
		"SELECT json_array(json_group_array(k) OVER ()) FROM s1",
		"SELECT json_quote(json_group_array(k) OVER ()) FROM s1",
		"SELECT json_array(count(k) OVER ()) FROM s1",
		"SELECT json_array(row_number() OVER ()) FROM s1",
		"SELECT json_array(group_concat(v) OVER ()) FROM s1",
	} {
		tailR30AssertSame(t, godb, cdb, stmt)
	}
}
