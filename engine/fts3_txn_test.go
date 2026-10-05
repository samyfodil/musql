package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestFts3SavepointSealsTheSegment verifies FTS3 segment sealing with savepoints.
func TestFts3SavepointSealsTheSegment(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`BEGIN`,
		`INSERT INTO t VALUES('alpha')`,
		`SAVEPOINT sp`,
		`INSERT INTO t VALUES('beta')`,
		`ROLLBACK TO sp`,
		`INSERT INTO t VALUES('gamma')`,
		`COMMIT`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	_, rows, err := p.QueryArgs(`SELECT level, idx, end_block FROM t_segdir ORDER BY level, idx`, nil)
	if err != nil {
		t.Fatalf("reading t_segdir: %v", err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%v/%v/%s", r[0].I, r[1].I, r[2].S))
	}
	if len(got) != 2 || got[0] != "0/0/0 11" || got[1] != "0/1/0 11" {
		t.Errorf("t_segdir = %v, want two segments [0/0/0 11 0/1/0 11] -- the SAVEPOINT did not seal", got)
	}

	_, rows, err = p.QueryArgs(`SELECT docid, a FROM t ORDER BY docid`, nil)
	if err != nil {
		t.Fatalf("reading t: %v", err)
	}
	var content []string
	for _, r := range rows {
		content = append(content, fmt.Sprintf("%d=%s", r[0].I, r[1].S))
	}
	if len(content) != 2 || content[0] != "1=alpha" || content[1] != "2=gamma" {
		t.Errorf("table = %v, want [1=alpha 2=gamma]", content)
	}
}
