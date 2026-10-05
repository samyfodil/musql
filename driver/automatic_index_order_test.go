package driver

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestAutomaticIndexOffJoinVisitOrder verifies PRAGMA automatic_index affects join order.
func TestAutomaticIndexOffJoinVisitOrder(t *testing.T) {
	db, err := sql.Open(DriverName, filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		`PRAGMA automatic_index=OFF`,
		`CREATE TABLE a(k, o)`,
		`CREATE TABLE b(k, o)`,
		`CREATE TABLE dst(v)`,
		`INSERT INTO a VALUES(1,'a3'),(2,'a1'),(1,'a2')`,
		`INSERT INTO b VALUES(1,'b9'),(1,'b4'),(2,'b7'),(1,'b1'),(2,'b2')`,
		`INSERT INTO dst SELECT group_concat(a.o||'-'||b.o) FROM a,b WHERE a.k=b.k`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	var got string
	if err := db.QueryRow(`SELECT v FROM dst`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	const want = "a3-b9,a3-b4,a3-b1,a1-b7,a1-b2,a2-b9,a2-b4,a2-b1"
	if got != want {
		t.Fatalf("group_concat over the join:\n got %s\nwant %s", got, want)
	}
}
