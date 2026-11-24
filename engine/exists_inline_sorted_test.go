package engine

import (
	"path/filepath"
	"testing"
)

// TestExistsInlineSortedSeekFurtherKeys: an EXISTS whose join seeks its inner
// table on two columns (an automatic index on d(yr, dsk), as C builds it) runs
// inlined into the outer program, and the seek's second key is read from the
// register the inlined code computes it in. Read from the sub-program's own
// numbering, it was another register of the outer program, and every EXISTS
// answered false (TPC-DS q10). The answers are C SQLite's.
func TestExistsInlineSortedSeekFurtherKeys(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "e.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE c(sk INTEGER)`,
		`CREATE TABLE ss(cust INTEGER, dt INTEGER)`,
		`CREATE TABLE d(dsk INTEGER, yr INTEGER, moy INTEGER)`,
		`INSERT INTO c SELECT value FROM generate_series(1, 300)`,
		`INSERT INTO d SELECT value, 1998 + value / 365, 1 + (value % 365) / 31 FROM generate_series(1, 2500)`,
		`INSERT INTO ss SELECT (value * 7919) % 400, 1 + (value * 104729) % 2500 FROM generate_series(1, 1500)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, c := range []struct {
		q    string
		want int64
	}{
		{`SELECT count(*) FROM c WHERE EXISTS (SELECT * FROM ss, d WHERE c.sk = ss.cust AND ss.dt = d.dsk AND d.yr = 2002)`, 148},
		{`SELECT count(*) FROM c WHERE NOT EXISTS (SELECT * FROM ss, d WHERE c.sk = ss.cust AND ss.dt = d.dsk AND d.yr = 2002 AND d.moy BETWEEN 1 AND 4)`, 243},
		{`SELECT sum(sk) FROM c WHERE EXISTS (SELECT * FROM ss, d WHERE c.sk = ss.cust AND ss.dt = d.dsk AND d.yr = 2001 AND d.moy BETWEEN 1 AND 4)`, 8550},
	} {
		_, rows, err := p.QueryArgs(c.q, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.q, err)
		}
		if len(rows) != 1 || rows[0][0].I != c.want {
			t.Errorf("%s:\n%v, want %d", c.q, typedRows(rows), c.want)
		}
	}
}
