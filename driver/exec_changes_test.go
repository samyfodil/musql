package driver

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestExecChanges(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "e.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(a UNIQUE)`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Raw(func(dc any) error {
		c := dc.(*Conn)
		if n, err := c.ExecChanges(ctx, `INSERT INTO t VALUES (?), (?)`, 1, 2); err != nil || n != 2 {
			t.Errorf("insert: %d, %v", n, err)
		}
		if n, err := c.ExecChanges(ctx, `INSERT OR IGNORE INTO t VALUES (?)`, 1); err != nil || n != 0 {
			t.Errorf("ignored insert: %d, %v", n, err)
		}
		if _, err := c.ExecChanges(ctx, `INSERT INTO`); err == nil {
			t.Error("a statement that does not parse ran")
		}
		if _, err := c.ExecChanges(ctx, `INSERT INTO t VALUES (?)`, struct{}{}); err == nil {
			t.Error("an argument of no SQL type was bound")
		}
		if _, err := c.ExecChanges(ctx, `INSERT INTO t VALUES (?)`, 2); err == nil {
			t.Error("a UNIQUE violation was not reported")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
