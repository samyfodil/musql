package engine

import (
	"fmt"
	"testing"
)

// TestTempTriggerQualifiedAttachedTarget pins a TEMP trigger's body writing
// into an ATTACHed database by naming it -- "INSERT INTO aux.a1 ..." -- which
// only a TEMP trigger may do (trigger.c:478-485) and which then resolves in
// that database alone (build.c:487): the write is routed to the attachment's
// own session (vdbe_trigger_routed.go).
func TestTempTriggerQualifiedAttachedTarget(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(dir+"/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		"ATTACH '" + dir + "/aux.db' AS aux",
		"CREATE TABLE aux.a1(c, d)",
		"INSERT INTO aux.a1 VALUES(1, 2)",
		"CREATE TABLE a1(c, d)",
		"CREATE TABLE m2(x)",
		"CREATE TEMP TRIGGER tr2 AFTER INSERT ON m2 BEGIN INSERT INTO aux.a1 VALUES(new.x, new.x); UPDATE aux.a1 SET d = d + 1; DELETE FROM aux.a1 WHERE c = 1; END",
		"INSERT INTO m2 VALUES(8)",
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		"SELECT c, d FROM aux.a1":  "[[8 9]]",
		"SELECT count(*) FROM a1": "[[0]]",
	} {
		_, rows, err := p.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		var got [][]int64
		for _, r := range rows {
			var g []int64
			for _, v := range r {
				g = append(g, v.I)
			}
			got = append(got, g)
		}
		if s := fmt.Sprint(got); s != want {
			t.Errorf("%s = %s, want %s", q, s, want)
		}
	}
}
