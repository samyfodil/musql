package engine

import (
	"path/filepath"
	"testing"
)

// TestCTESeriesAnchorOrder: an aggregate reading the order rows arrive in,
// over a join of a materialized CTE whose body scans generate_series, answers
// as C does (#280): the planner places the CTE on the row estimate C's
// seriesBestIndex gives (cSeriesScanEst). The compat harness cannot check
// these, its oracle build having no generate_series, so each answer is
// C 3.53.3's, taken from its shell; the first changes with a wrong estimate.
func TestCTESeriesAnchorOrder(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "s.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE mid(a INT, b INT)`,
		`INSERT INTO mid SELECT value, value % 5 FROM generate_series(1, 1500)`,
	} {
		if err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	c := `WITH c AS MATERIALIZED (SELECT value % 7 k, value v FROM generate_series(1, 30)) `
	for _, tc := range []struct{ q, want string }{
		{c + `SELECT group_concat(c.v) FROM c, c d WHERE d.v = c.k`,
			`1,2,3,4,5,6,8,9,10,11,12,13,15,16,17,18,19,20,22,23,24,25,26,27,29,30;`},
		{c + `SELECT a.v, group_concat(b.v) FROM c a, c b WHERE b.k = a.v % 9 GROUP BY a.v`,
			`1 1,8,15,22,29;2 2,9,16,23,30;3 3,10,17,24;4 4,11,18,25;5 5,12,19,26;6 6,13,20,27;9 7,14,21,28;10 1,8,15,22,29;` +
				`11 2,9,16,23,30;12 3,10,17,24;13 4,11,18,25;14 5,12,19,26;15 6,13,20,27;18 7,14,21,28;19 1,8,15,22,29;` +
				`20 2,9,16,23,30;21 3,10,17,24;22 4,11,18,25;23 5,12,19,26;24 6,13,20,27;27 7,14,21,28;28 1,8,15,22,29;` +
				`29 2,9,16,23,30;30 3,10,17,24;`},
		{c + `SELECT a.v, b.v FROM c a, c b WHERE b.k = a.v % 9 GROUP BY a.v`,
			`1 1;2 2;3 3;4 4;5 5;6 6;9 7;10 1;11 2;12 3;13 4;14 5;15 6;18 7;19 1;20 2;21 3;22 4;23 5;24 6;27 7;28 1;29 2;30 3;`},
		{c + `SELECT m.a, group_concat(c.v) FROM mid m, c WHERE c.k = m.b AND m.a < 8 GROUP BY m.a`,
			`1 1,8,15,22,29;2 2,9,16,23,30;3 3,10,17,24;4 4,11,18,25;5 7,14,21,28;6 1,8,15,22,29;7 2,9,16,23,30;`},
		{`WITH c AS MATERIALIZED (SELECT value % 4 k, value v FROM generate_series(-20, 20, 3))
			SELECT group_concat(c.v || ':' || d.v) FROM c, c d WHERE d.k = c.k`,
			`-20:-20,-20:-8,-20:4,-20:16,-17:-17,-17:-5,-14:-14,-14:-2,-11:-11,-8:-20,-8:-8,-8:4,-8:16,-5:-17,-5:-5,-2:-14,` +
				`-2:-2,1:1,1:13,4:-20,4:-8,4:4,4:16,7:7,7:19,10:10,13:1,13:13,16:-20,16:-8,16:4,16:16,19:7,19:19;`},
	} {
		_, rows, err := db.Query(tc.q, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.q, err)
		}
		if got := rowsText(rows); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.q, got, tc.want)
		}
	}
}

// TestSeriesFlattenedOrder: the same aggregates over a CTE that C flattens
// (plain or NOT MATERIALIZED) and over generate_series joined directly, a
// constraint on its value among the WHERE terms (#280). seriesBestIndex takes
// such a constraint at no change of cost, so the loop without it is the one C
// keeps. Each answer is C 3.53.3's, taken from its shell.
func TestSeriesFlattenedOrder(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "s.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE mid(a INT, b INT)`,
		`INSERT INTO mid SELECT value, value % 5 FROM generate_series(1, 1500)`,
	} {
		if err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	c := `(SELECT value % 7 k, value v FROM generate_series(1, 30)) `
	for _, tc := range []struct{ q, want string }{
		{`WITH c AS ` + c + `SELECT a.v, group_concat(b.v) FROM c a, c b WHERE b.k = a.v % 9 GROUP BY a.v`,
			`1 1,8,15,22,29;2 2,9,16,23,30;3 3,10,17,24;4 4,11,18,25;5 5,12,19,26;6 6,13,20,27;9 7,14,21,28;10 1,8,15,22,29;` +
				`11 2,9,16,23,30;12 3,10,17,24;13 4,11,18,25;14 5,12,19,26;15 6,13,20,27;18 7,14,21,28;19 1,8,15,22,29;` +
				`20 2,9,16,23,30;21 3,10,17,24;22 4,11,18,25;23 5,12,19,26;24 6,13,20,27;27 7,14,21,28;28 1,8,15,22,29;` +
				`29 2,9,16,23,30;30 3,10,17,24;`},
		{`WITH c AS NOT MATERIALIZED ` + c + `SELECT a.v, b.v FROM c a, c b WHERE b.k = a.v % 9 GROUP BY a.v`,
			`1 1;2 2;3 3;4 4;5 5;6 6;9 7;10 1;11 2;12 3;13 4;14 5;15 6;18 7;19 1;20 2;21 3;22 4;23 5;24 6;27 7;28 1;29 2;30 3;`},
		{`WITH c AS ` + c + `SELECT m.a, group_concat(c.v) FROM mid m, c WHERE c.k = m.b AND m.a < 8 GROUP BY m.a`,
			`1 1,8,15,22,29;2 2,9,16,23,30;3 3,10,17,24;4 4,11,18,25;5 7,14,21,28;6 1,8,15,22,29;7 2,9,16,23,30;`},
		{`SELECT group_concat(x.value || ':' || y.value) FROM generate_series(1, 20) x, generate_series(1, 20) y WHERE y.value = x.value + 3`,
			`1:4,2:5,3:6,4:7,5:8,6:9,7:10,8:11,9:12,10:13,11:14,12:15,13:16,14:17,15:18,16:19,17:20;`},
		{`SELECT group_concat(y.value) FROM generate_series(1, 40) x, generate_series(1, 40, 3) y WHERE x.value = 3 AND y.value % 7 = 1`,
			`1,22;`},
		{`SELECT group_concat(x.value || ':' || y.value) FROM generate_series(1, 12) x, generate_series(1, 12) y WHERE y.value BETWEEN x.value AND x.value + 1`,
			`1:1,1:2,2:2,2:3,3:3,3:4,4:4,4:5,5:5,5:6,6:6,6:7,7:7,7:8,8:8,8:9,9:9,9:10,10:10,10:11,11:11,11:12,12:12;`},
		{`SELECT x.value, group_concat(y.value) FROM generate_series(1, 12) x, generate_series(-6, 6) y WHERE y.value % 5 = x.value % 4 GROUP BY x.value`,
			`1 1,6;2 2;3 3;4 -5,0,5;5 1,6;6 2;7 3;8 -5,0,5;9 1,6;10 2;11 3;12 -5,0,5;`},
		{`SELECT group_concat(m.a || ':' || x.value) FROM mid m, generate_series(1, 40) x WHERE x.value = m.b + 30 AND m.a < 12`,
			`1:31,2:32,3:33,4:34,5:30,6:31,7:32,8:33,9:34,10:30,11:31;`},
		{`SELECT x.value, group_concat(y.value) FROM generate_series(1, 12) x, generate_series(-6, 6) y WHERE y.value % 5 = x.value % 4 GROUP BY x.value ORDER BY x.value`,
			`1 1,6;2 2;3 3;4 -5,0,5;5 1,6;6 2;7 3;8 -5,0,5;9 1,6;10 2;11 3;12 -5,0,5;`},
		{`SELECT y.value, group_concat(x.value) FROM generate_series(1, 12) x, generate_series(-6, 6) y WHERE y.value % 5 = x.value % 4 GROUP BY y.value ORDER BY y.value`,
			`-5 4,8,12;0 4,8,12;1 1,5,9;2 2,6,10;3 3,7,11;5 4,8,12;6 1,5,9;`},
		{`SELECT x.value, group_concat(m.a) FROM mid m, generate_series(1, 9) x WHERE m.b = x.value % 5 AND m.a < 12 GROUP BY x.value ORDER BY x.value`,
			`1 1,6,11;2 2,7;3 3,8;4 4,9;5 5,10;6 1,6,11;7 2,7;8 3,8;9 4,9;`},
		{`SELECT m.a, x.value FROM mid m, generate_series(1, 9) x WHERE m.b = x.value % 5 AND m.a < 12 ORDER BY x.value`,
			`1 1;6 1;11 1;2 2;7 2;3 3;8 3;4 4;9 4;5 5;10 5;1 6;6 6;11 6;2 7;7 7;3 8;8 8;4 9;9 9;`},
	} {
		_, rows, err := db.Query(tc.q, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.q, err)
		}
		if got := rowsText(rows); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.q, got, tc.want)
		}
	}
}

// TestSeriesDescOrder: seriesBestIndex consumes an ORDER BY (or GROUP BY
// taking its direction) on the series' value descending as well as ascending,
// so an order-reading aggregate over a join the planner arranges around it
// answers (#283). Each answer is C 3.53.3's, taken from its shell.
func TestSeriesDescOrder(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "s.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE mid(a INT, b INT)`,
		`INSERT INTO mid SELECT value, value % 5 FROM generate_series(1, 1500)`,
	} {
		if err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	xy := `FROM generate_series(1, 12) x, generate_series(1, 12) y WHERE y.value % 5 = x.value % 4 `
	for _, tc := range []struct{ q, want string }{
		{`SELECT x.value, group_concat(y.value) ` + xy + `GROUP BY x.value ORDER BY x.value DESC`,
			`12 5,10;11 3,8;10 2,7,12;9 1,6,11;8 5,10;7 3,8;6 2,7,12;5 1,6,11;4 5,10;3 3,8;2 2,7,12;1 1,6,11;`},
		{`SELECT x.value, group_concat(y.value) ` + xy + `GROUP BY x.value ORDER BY x.value DESC LIMIT 3`,
			`12 5,10;11 3,8;10 2,7,12;`},
		{`SELECT x.value, y.value ` + xy + `GROUP BY x.value ORDER BY x.value DESC`,
			`12 5;11 3;10 2;9 1;8 5;7 3;6 2;5 1;4 5;3 3;2 2;1 1;`},
		{`SELECT x.value, group_concat(y.value) ` + xy + `AND y.value > x.value GROUP BY x.value ORDER BY x.value DESC`,
			`10 12;9 11;8 10;7 8;6 7,12;5 6,11;4 5,10;3 8;2 7,12;1 6,11;`},
		{`SELECT x.value, count(*), group_concat(m.a) FROM mid m, generate_series(1, 9) x
			WHERE m.b = x.value % 5 AND m.a < 12 GROUP BY x.value ORDER BY x.value DESC`,
			`9 2 4,9;8 2 3,8;7 2 2,7;6 3 1,6,11;5 2 5,10;4 2 4,9;3 2 3,8;2 2 2,7;1 3 1,6,11;`},
	} {
		_, rows, err := db.Query(tc.q, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.q, err)
		}
		if got := rowsText(rows); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.q, got, tc.want)
		}
	}
}

// TestSeriesLimit: a LIMIT sqlite3WhereAddLimit offers generate_series stops
// the series there, walked in the order of an ORDER BY the module consumes, so one with START alone answers rather than exceeding
// what this engine materializes, and a CTE over one takes seriesBestIndex's
// 2500 rows for START with a LIMIT (#284). Each answer is C 3.53.3's, taken
// from its shell, including "ORDER BY value DESC LIMIT 2" over an unbounded
// series: C offers the LIMIT for an ORDER BY of the module's columns and the
// module takes it without consuming that ORDER BY.
func TestSeriesLimit(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "s.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE mid(a INT, b INT)`,
		`INSERT INTO mid SELECT value, value % 5 FROM generate_series(1, 1500)`,
	} {
		if err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ q, want string }{
		{`SELECT value FROM generate_series(-9223372036854775808, 9223372036854775807, 2) LIMIT 3`,
			`-9223372036854775808;-9223372036854775806;-9223372036854775804;`},
		{`SELECT value FROM generate_series(-9223372036854775808, 9223372036854775807) LIMIT 3`,
			`-9223372036854775808;-9223372036854775807;-9223372036854775806;`},
		{`SELECT value FROM generate_series(9223372036854775807, -9223372036854775808, -3) LIMIT 2`,
			`9223372036854775807;9223372036854775804;`},
		{`SELECT value FROM generate_series(9223372036854775807, -9223372036854775808, -1) LIMIT 2 OFFSET 1`,
			`9223372036854775806;9223372036854775805;`},
		{`SELECT value FROM generate_series(5) LIMIT 3 OFFSET 2`, `7;8;9;`},
		{`SELECT value FROM generate_series(4294967290) LIMIT 10`,
			`4294967290;4294967291;4294967292;4294967293;4294967294;4294967295;`},
		{`SELECT count(*) FROM (SELECT value FROM generate_series(1) LIMIT 100000)`, `100000;`},
		{`SELECT max(value) FROM (SELECT value FROM generate_series(10) LIMIT 7)`, `16;`},
		{`SELECT value FROM generate_series(5) WHERE value > 10 LIMIT 3`, `11;12;13;`},
		{`SELECT value FROM generate_series(5) WHERE value BETWEEN 10 AND 4000000000 LIMIT 2`, `10;11;`},
		{`SELECT value FROM generate_series(5) ORDER BY value LIMIT 2 OFFSET 4`, `9;10;`},
		{`SELECT value FROM generate_series(5) ORDER BY value DESC LIMIT 2`, `6;5;`},
		{`SELECT value FROM generate_series(5) LIMIT 0`, ``},
		// With START and STOP the module walks the series in the order of
		// the ORDER BY it consumed, which its LIMIT makes observable.
		{`SELECT value FROM (SELECT value FROM generate_series(1,20) ORDER BY value DESC LIMIT 3)`, `20;19;18;`},
		{`SELECT value FROM generate_series(10,1,-1) ORDER BY value LIMIT 2`, `1;2;`},
		{`SELECT value FROM generate_series(10,1,-1) ORDER BY value DESC LIMIT 2`, `10;9;`},
		{`SELECT value FROM generate_series(1,20,3) ORDER BY value DESC LIMIT 3 OFFSET 2`, `13;10;7;`},
		{`SELECT value FROM generate_series(20,1,-3) ORDER BY value LIMIT 3 OFFSET 1`, `5;8;11;`},
		{`SELECT value FROM generate_series(1,100) WHERE value > 50 AND value <= 77 ORDER BY value DESC LIMIT 2`, `77;76;`},
		{`SELECT value FROM generate_series(-9223372036854775808, 9223372036854775807, 7) ORDER BY value DESC LIMIT 2`,
			`9223372036854775806;9223372036854775799;`},
		{`SELECT value FROM generate_series(1,20) ORDER BY start, value DESC LIMIT 3`, `3;2;1;`},
		{`SELECT group_concat(value) FROM (SELECT value FROM generate_series(1,20,2) ORDER BY value DESC LIMIT 4)`, `19,17,15,13;`},
		// A constraint the module leaves unused keeps the LIMIT from it
		// (whereLoopAddVirtualOne's retry), or the rows it drops would leave
		// fewer than the LIMIT.
		{`SELECT value FROM generate_series(5, 100) WHERE value > 12 AND value >= (SELECT 10) LIMIT 3`, `13;14;15;`},
		// A window query's FROM is a subquery by the time its WHERE runs.
		{`SELECT value, sum(value) OVER () FROM generate_series(1,10) LIMIT 2`, `1 55;2 55;`},
		{`SELECT value, row_number() OVER (ORDER BY value DESC) FROM generate_series(1,10) LIMIT 2`, `10 1;9 2;`},
		{`SELECT value, sum(value) OVER () FROM generate_series(1,10) ORDER BY value DESC LIMIT 2`, `10 55;9 55;`},
		{`WITH c AS MATERIALIZED (SELECT value % 7 k, value v FROM generate_series(5) LIMIT 40)
			SELECT a.v, group_concat(b.v) FROM c a, c b WHERE b.k = a.v % 9 AND a.v < 15 GROUP BY a.v`,
			`5 5,12,19,26,33,40;6 6,13,20,27,34,41;9 7,14,21,28,35,42;10 8,15,22,29,36,43;11 9,16,23,30,37,44;` +
				`12 10,17,24,31,38;13 11,18,25,32,39;14 5,12,19,26,33,40;`},
		{`WITH c AS (SELECT value % 7 k, value v FROM generate_series(5) LIMIT 40) SELECT a.v, b.v FROM c a, c b WHERE b.k = a.v % 9 GROUP BY a.v`,
			`5 5;6 6;9 7;10 8;11 9;12 10;13 11;14 5;15 6;18 7;19 8;20 9;21 10;22 11;23 5;24 6;27 7;28 8;29 9;30 10;31 11;` +
				`32 5;33 6;36 7;37 8;38 9;39 10;40 11;41 5;42 6;`},
		{`WITH c AS (SELECT value % 7 k, value v FROM generate_series(5) LIMIT 400)
			SELECT m.a, group_concat(c.v) FROM mid m, c WHERE c.k = m.b AND m.a < 4 AND c.v < 60 GROUP BY m.a`,
			`1 8,15,22,29,36,43,50,57;2 9,16,23,30,37,44,51,58;3 10,17,24,31,38,45,52,59;`},
		{`WITH c AS MATERIALIZED (SELECT value % 7 k, value v FROM generate_series(5) LIMIT 3000)
			SELECT m.a, group_concat(c.v) FROM mid m, c WHERE c.k = m.b AND m.a < 4 AND c.v < 60 GROUP BY m.a`,
			`1 8,15,22,29,36,43,50,57;2 9,16,23,30,37,44,51,58;3 10,17,24,31,38,45,52,59;`},
	} {
		_, rows, err := db.Query(tc.q, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.q, err)
		}
		if got := rowsText(rows); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.q, got, tc.want)
		}
	}
}
