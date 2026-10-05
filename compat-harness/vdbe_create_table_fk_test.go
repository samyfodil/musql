// Tests CREATE TABLE checks for duplicate column names and foreign key definitions.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var fkQ = []string{
	`CREATE TABLE d1(a,b,a)`,
	`CREATE TABLE d2(a,b,A)`,
	`CREATE TABLE d3(a,b,"a")`,
	`CREATE TABLE f1(a,b,c, FOREIGN KEY(a,b) REFERENCES t4(x))`,
	`CREATE TABLE f2(a,b,c, FOREIGN KEY(a) REFERENCES t4(x,y))`,
	`CREATE TABLE f3(a,b, c REFERENCES t4(x,y))`,
	`CREATE TABLE f4(a,b,c, FOREIGN KEY(a) REFERENCES t4(nosuch))`,
	`CREATE TABLE f5(a,b,c, FOREIGN KEY(nosuch) REFERENCES t4(x))`,
	`CREATE TABLE f6(a,b,c, FOREIGN KEY(a) REFERENCES t4(x))`,
	`CREATE TABLE f7(a,b,c, FOREIGN KEY(a,b) REFERENCES t4(x,y))`,
	`CREATE TABLE f8(a,b,c, FOREIGN KEY(a) REFERENCES nosuchtable(x))`,
	`CREATE TABLE f9(a,b,c REFERENCES t4)`,
}

func TestCreateTableFKAndDuplicateColumnParity(t *testing.T) {
	for _, q := range fkQ {
		edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		cdb, _ := sql.Open("sqlite3", ":memory:")
		edb.Exec(`CREATE TABLE t4(x, y)`)
		cdb.Exec(`CREATE TABLE t4(x, y)`)
		eerr := edb.Exec(q)
		_, cerr := cdb.Exec(q)
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", q, eerr, cerr)
		} else if cerr != nil {
			if got := strings.TrimPrefix(eerr.Error(), "engine: "); got != cerr.Error() {
				t.Errorf("[%s] error text mismatch\n  engine: %q\n  cgo:    %q", q, got, cerr.Error())
			}
		}
		edb.Close()
		cdb.Close()
	}
}
