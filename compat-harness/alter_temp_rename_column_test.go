package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestAlterTempRenameColumnSecondPass: renaming a TEMP table's column that a
// temp view or trigger names fails in C SQLite -- its temp-schema pass for
// objects over a main table runs a second time over the already-renamed text
// (alter.c:657-672) -- and the whole ALTER rolls back. The error TEXT is
// compared too, through each driver directly.

func TestAlterTempRenameColumnSecondPass(t *testing.T) {
	for _, prog := range [][]string{
		{`CREATE TEMP TABLE tt(x, y)`, `CREATE TEMP VIEW tv AS SELECT y FROM tt`, `ALTER TABLE tt RENAME COLUMN y TO z`},
		{`CREATE TEMP TABLE tt(x, y)`, `CREATE TEMP TRIGGER tr AFTER INSERT ON tt BEGIN SELECT new.y; END`, `ALTER TABLE tt RENAME COLUMN y TO z`},
		{`CREATE TEMP TABLE tt(x, y)`, `CREATE TEMP VIEW tv AS SELECT x FROM tt WHERE tt.y>0`, `ALTER TABLE tt RENAME COLUMN y TO z`},
		{`CREATE TEMP TABLE tt(x, y)`, `CREATE TEMP TRIGGER tr AFTER INSERT ON tt BEGIN SELECT 1; END`, `CREATE TEMP VIEW tv AS SELECT y FROM tt`, `ALTER TABLE tt RENAME COLUMN y TO z`},
	} {
		msgs := map[string]string{}
		for _, drv := range []string{"sqlite3", "sqlite"} {
			db, err := sql.Open(drv, filepath.Join(t.TempDir(), "c.db"))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			var last error
			for _, s := range prog {
				if _, err := db.Exec(s); err != nil {
					last = err
				}
			}
			msgs[drv] = "<nil>"
			if last != nil {
				msgs[drv] = last.Error()
			}
			db.Close()
		}
		if want, got := msgs["sqlite3"], strings.TrimPrefix(msgs["sqlite"], "engine: "); got != want {
			t.Errorf("%v: musql %q, C SQLite %q", prog[1:], got, want)
		}
	}
	differ(t, "temp rename column with temp view", []string{
		`CREATE TEMP TABLE tt(x, y)`, `CREATE TEMP VIEW tv AS SELECT y FROM tt`,
		`ALTER TABLE tt RENAME COLUMN y TO z`, `SELECT sql FROM sqlite_temp_master ORDER BY name`,
		`INSERT INTO tt VALUES(1,2)`, `SELECT * FROM tv`,
	})
}
