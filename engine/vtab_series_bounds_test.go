package engine_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestSeriesBoundsAsC: a bound generate_series consumes is read as C's series
// reads it, and every other conjunct is tested as written (#138); the hidden
// columns of generate_series, json_each and the pragma functions have NUMERIC
// affinity, as C declares them. Every answer is C SQLite 3.53's (the oracle
// build has no generate_series, so they are pinned here).
func TestSeriesBoundsAsC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.musq")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, tc := range []struct {
		q, want string
	}{
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5'`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value > '3'`, "7"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value <= '4'`, "4"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5.0'`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = 5.5`, "0"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = x'35'`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = x'352e35'`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5x'`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5.5x'`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '1e1'`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = ' 5 '`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '0x5'`, "0"},
		{`SELECT count(*) FROM generate_series(-5,10) WHERE value = '-3'`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value >= '3.5'`, "7"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value > '3.0'`, "7"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value < '3x'`, "2"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value > 'abc'`, "10"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value <= x'33'`, "3"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value IS '5'`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value IN ('5', '6')`, "2"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value IN ('5', '6', 7.0, '8x')`, "4"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value BETWEEN '3' AND '6'`, "4"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE +value = '5'`, "0"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5' AND value = 5`, "0"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value > 3 AND value > 5`, "5"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value > '3' AND value > 5`, "0"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value > 5 AND value > '3'`, "5"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = 3 AND value = '5'`, "0"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5' AND value = 3`, "0"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE start = '1'`, "10"},
		{`SELECT count(*) FROM generate_series WHERE start = '2' AND stop = '5'`, "4"},
		{`SELECT count(*) FROM generate_series WHERE start = 1.9 AND stop = '5.9'`, "5"},
		{`SELECT group_concat(value) FROM generate_series('2', '4')`, "2,3,4"},
		{`SELECT group_concat(value) FROM generate_series(1.5, 4.5)`, "1,2,3,4"},
		{`SELECT count(*) FROM generate_series('2x', '4')`, "3"},
		{`SELECT group_concat(start = '1') || '|' || group_concat(value = '2') FROM generate_series(1,2)`, "1,1|0,0"},
		{`SELECT count(*) FROM generate_series(1,10) t WHERE EXISTS (SELECT 1 FROM generate_series(1,10) s WHERE s.value = t.value || '')`, "10"},
		{`SELECT count(*) FROM generate_series(1,5) t, generate_series(1,5) s WHERE s.value = t.value || ''`, "0"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE '5' = value`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE '3' < value`, "7"},
		{`SELECT count(*) FROM generate_series(1,10) t WHERE EXISTS (SELECT 1 FROM generate_series(1,10) s WHERE t.value || '' = s.value)`, "10"},
		{`SELECT count(*) FROM generate_series(1,10) t WHERE EXISTS (SELECT 1 FROM generate_series(1,10) s WHERE t.value || '' < s.value)`, "9"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value BETWEEN 1 AND 10 AND value BETWEEN '3' AND '6'`, "0"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value BETWEEN '3' AND '6' AND value BETWEEN 1 AND 10`, "4"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value BETWEEN 1 AND 10 AND value BETWEEN 1 AND '5'`, "10"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value <= '3' AND value <= 100`, "10"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value <= 100 AND value <= '3'`, "3"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value >= '3' AND value BETWEEN 1 AND 10`, "0"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value BETWEEN 1 AND 10 AND value >= '3'`, "0"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value BETWEEN '3' AND 6`, "4"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value NOT BETWEEN '3' AND '6'`, "10"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value > '3.5' AND value < '7.5'`, "4"},
		{`SELECT group_concat(value) FROM generate_series(1,10) WHERE '4' < value AND value <= '6'`, "5,6"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value > 5.5 AND value < 8`, "2"},
		{`SELECT count(*) FROM generate_series(9223372036854775807,9223372036854775807) WHERE value = '9223372036854775808'`, "1"},
		{`SELECT count(*) FROM generate_series(9223372036854775800,9223372036854775807) WHERE value >= 9223372036854775808.0`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5' AND rowid = 5`, "0"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE rowid = 5 AND value = '5'`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE oid = '5'`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE _rowid_ > '7'`, "3"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE rowid IN ('5', 5)`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value IN ('5', '6') AND value = '5'`, "0"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value IN ('5', '50')`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5' OR value = '6'`, "2"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5' OR value = '6' OR value = 7`, "3"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5' OR value = '5'`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5' OR value IS '6'`, "0"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = CAST(5 AS TEXT) OR value = '6'`, "0"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5' OR rowid = '6'`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value < '5' OR value = '5'`, "5"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5' OR value < '5'`, "5"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value > 3 OR value > 3`, "7"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE start = 2`, "0"},
		{`SELECT count(*) FROM generate_series WHERE start=1 AND stop=3 AND step=0`, "3"},
		{`SELECT count(*) FROM generate_series WHERE start=1 AND stop=3 AND step='0'`, "3"},
		{`SELECT step FROM generate_series(1,3,0) LIMIT 1`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5' AND step = '1'`, "1"},
		{`SELECT count(*) FROM generate_series(1,10,'2') WHERE step = '2'`, "5"},
		{`SELECT count(*) FROM generate_series(1,10) AS s CROSS JOIN (SELECT 5 AS x) AS t WHERE s.value = '5' AND s.value = t.x`, "0"},
		{`SELECT count(*) FROM generate_series(1,3) WHERE start IN (1,2)`, "3"},
		{`SELECT count(*) FROM generate_series WHERE start IN (1,2) AND stop = 3 AND start = 2`, "2"},
		{`SELECT count(*) FROM generate_series WHERE start IN (2) AND stop = 3`, "2"},
		// C runs a consumed IN once for every distinct value of it, so a row
		// two of them make comes out twice, and one on start, stop or step
		// makes a series for each value (#155).
		{`SELECT count(*) FROM generate_series(1,10) WHERE value IN ('5', 5)`, "2"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value IN ('5', ' 5', 5.0, 5)`, "3"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value = '5' OR value = 5`, "2"},
		{`SELECT count(*) FROM generate_series WHERE start IN (1,2) AND stop = 3`, "5"},
		{`SELECT count(*) FROM generate_series WHERE (start = 1 OR start = 2) AND stop = 3`, "5"},
		{`SELECT count(*) FROM generate_series WHERE start = 1 AND stop IN (2, 3)`, "5"},
		{`SELECT count(*) FROM generate_series WHERE start = 2 AND start IN (1,2) AND stop = 3`, "2"},
		{`SELECT group_concat(value) FROM generate_series WHERE start IN (2,1) AND stop = 3`, "1,2,3,2,3"},
		{`SELECT group_concat(value || ':' || start) FROM generate_series WHERE start IN (3,1) AND stop = 4`, "1:1,2:1,3:1,4:1,3:3,4:3"},
		{`SELECT count(*) FROM generate_series WHERE start IN (1,1,'1') AND stop = 3`, "3"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value IN (NULL, 5)`, "1"},
		{`SELECT count(*) FROM generate_series WHERE start IN (SELECT NULL) AND stop = 3`, "0"},
		{`SELECT count(*) FROM generate_series WHERE start IN (SELECT 1 WHERE 0) AND stop = 3`, "0"},
		{`SELECT group_concat(value || ':' || start || ':' || stop) FROM generate_series WHERE start IN (1,2) AND stop IN (3,4)`,
			"1:1:3,2:1:3,3:1:3,1:1:4,2:1:4,3:1:4,4:1:4,2:2:3,3:2:3,2:2:4,3:2:4,4:2:4"},
		{`SELECT group_concat(value || ':' || step) FROM generate_series WHERE start = 1 AND stop = 4 AND step IN (2,1)`, "1:1,2:1,3:1,4:1,1:2,3:2"},
		{`SELECT group_concat(value) FROM generate_series(10,1,-1) WHERE value IN (3,1,2)`, "1,2,3"},
		{`SELECT sum(value) FROM generate_series WHERE start IN (1,2,3) AND stop IN (5) AND step IN (1,2)`, "64"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value IN (SELECT '5')`, "1"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value IN (SELECT '5' UNION ALL SELECT 5)`, "2"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE rowid IN (SELECT '5')`, "1"},
		{`SELECT group_concat(value) FROM generate_series(1,10) WHERE value IN (SELECT 6 UNION SELECT 5)`, "5,6"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value IN (SELECT value FROM generate_series(3,5))`, "3"},
		{`SELECT count(*) FROM generate_series WHERE stop = 12 AND start IN (SELECT x COLLATE nocase FROM (SELECT '1e1' AS x UNION ALL SELECT '1E1'))`, "3"},
		{`SELECT count(*) FROM generate_series(1,20) WHERE value IN (SELECT x COLLATE nocase FROM (SELECT '1e1' AS x UNION ALL SELECT '1E1'))`, "1"},
		{`SELECT count(*) FROM generate_series(1,20) WHERE value IN (SELECT x COLLATE rtrim FROM (SELECT '5' AS x UNION ALL SELECT '5 '))`, "1"},
		{`SELECT count(*) FROM generate_series(1,20) WHERE value IN (SELECT x FROM (SELECT '5' AS x UNION ALL SELECT '5 '))`, "2"},
		{`SELECT count(*) FROM generate_series(1,5) a, generate_series(1,5) b WHERE a.value IN (1,2) AND b.value IN (SELECT 3 UNION SELECT 4)`, "4"},
		{`SELECT group_concat(a.value || ':' || b.value) FROM generate_series(1,3) a JOIN generate_series(1,3) b ON 1 WHERE b.value IN (2, '2') AND a.value = 1`, "1:2,1:2"},
		{`SELECT group_concat(value) FROM (SELECT value FROM generate_series WHERE start IN (1,2) AND stop = 3 LIMIT 2 OFFSET 2)`, "3,2"},
		{`SELECT count(*) FROM generate_series(1,10) WHERE value NOT IN (SELECT '5')`, "10"},
		// An IN subquery reading an enclosing query's row: one value for each
		// run of the series statement, consumed as C consumes it (#159).
		{`SELECT group_concat(x || ':' || (SELECT count(*) FROM generate_series(1,10) WHERE value IN (SELECT t.x)), ' ') FROM (SELECT '5' AS x UNION ALL SELECT 3 UNION ALL SELECT 'x' UNION ALL SELECT 5.0 UNION ALL SELECT '2.0') t`,
			"5:1 3:1 x:0 5.0:1 2.0:1"},
		{`SELECT group_concat(x || ':' || coalesce((SELECT group_concat(value) FROM generate_series(1,10) WHERE value IN (SELECT t.x + 1)), '-'), ' ') FROM (SELECT '5' AS x UNION ALL SELECT 3 UNION ALL SELECT 'x' UNION ALL SELECT '2.0') t`,
			"5:6 3:4 x:1 2.0:3"},
		{`SELECT group_concat(x || ':' || (SELECT count(*) FROM generate_series(1,10) WHERE value IN (SELECT t.x COLLATE NOCASE)), ' ') FROM (SELECT '5' AS x UNION ALL SELECT '1e1') t`,
			"5:1 1e1:1"},
		{`SELECT group_concat(x || ':' || coalesce((SELECT group_concat(value) FROM generate_series WHERE stop = 4 AND start IN (SELECT t.x)), '-'), ' ') FROM (SELECT '2' AS x UNION ALL SELECT '3.0') t`,
			"2:2,3,4 3.0:3,4"},
		{`SELECT group_concat(x || ':' || (SELECT count(*) FROM generate_series(1,10) s WHERE s.value IN (SELECT t.x) AND s.value > 1), ' ') FROM (SELECT '5' AS x UNION ALL SELECT '1') t`,
			"5:1 1:0"},
		{`SELECT count(*) FROM json_each('5') WHERE json = 5`, "1"},
		{`SELECT json = 5 FROM json_each('5')`, "1"},
		{`SELECT count(*) FROM json_each('[1]', '$') WHERE root = 5`, "0"},
	} {
		_, rows, err := p.Query(tc.q)
		if err != nil {
			t.Errorf("%s: %v", tc.q, err)
			continue
		}
		got := ""
		if len(rows) == 1 && len(rows[0]) == 1 {
			v := rows[0][0]
			if v.Typ == engine.Text {
				got = string(v.S)
			} else {
				got = fmt.Sprint(v.I)
			}
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q (C SQLite's)", tc.q, got, tc.want)
		}
	}
}
