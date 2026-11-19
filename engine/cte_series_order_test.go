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
