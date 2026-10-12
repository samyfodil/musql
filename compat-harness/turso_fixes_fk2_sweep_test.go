package compat

import (
	"fmt"
	"testing"
)

// TestFKMismatchUpdateGatingSweep: sqlite3FkCheck locates a mismatched key's
// parent index only when the statement can touch it: an UPDATE must assign one
// of the key's columns (fkChildIsModified / fkParentIsModified), a single-row
// INSERT never looks at the keys that point AT its table, and a REPLACE codes a
// victim delete only when a conflict is possible.
func TestFKMismatchUpdateGatingSweep(t *testing.T) {
	var cases [][]string
	// Child side: c.b references p(zz), which p has no such column for.
	for _, fkdef := range []string{"b REFERENCES p(zz)", "b REFERENCES p(zz) DEFERRABLE INITIALLY DEFERRED", "b, FOREIGN KEY(b) REFERENCES p(zz)", "b, FOREIGN KEY(a,b) REFERENCES p(zz,k)"} {
		for _, op := range []string{
			"UPDATE c SET a=2", "UPDATE c SET b=2", "UPDATE c SET a=2, b=3", "UPDATE c SET id=5", "UPDATE c SET rowid=5",
			"DELETE FROM c", "INSERT INTO c VALUES(2,2,2)", "INSERT OR REPLACE INTO c VALUES(1,2,2)", "REPLACE INTO c(a,b) VALUES(2,2)",
			"INSERT INTO c(a,b) SELECT 2,2", "INSERT INTO c VALUES(1,5,5) ON CONFLICT(id) DO UPDATE SET a=9",
			"INSERT INTO c VALUES(1,5,5) ON CONFLICT(id) DO UPDATE SET b=9",
		} {
			cases = append(cases, []string{"PRAGMA foreign_keys=ON", "CREATE TABLE p(k INTEGER PRIMARY KEY, v)",
				"CREATE TABLE c(id INTEGER PRIMARY KEY, a, " + fkdef + ")", "INSERT INTO p VALUES(1,1)", op, "SELECT count(*) FROM c"})
		}
	}
	// Parent side: c.r references p's key; p has none usable.
	for _, ref := range []string{"p", "p(v)", "p(k)"} {
		for _, pdef := range []string{"k INTEGER, v", "k, v UNIQUE", "k INTEGER PRIMARY KEY, v", "k, v, PRIMARY KEY(k)", "k, v, UNIQUE(k,v)"} {
			for _, op := range []string{
				"UPDATE p SET v=9", "UPDATE p SET k=9", "UPDATE p SET v=9, k=9", "UPDATE p SET rowid=9", "DELETE FROM p", "INSERT INTO p VALUES(2,2)",
				"INSERT INTO p VALUES(2,2),(3,3)", "INSERT OR REPLACE INTO p VALUES(1,1)", "INSERT OR REPLACE INTO p(v) VALUES(7)", "REPLACE INTO p VALUES(1,5)",
				"INSERT INTO p SELECT 4,4", "INSERT INTO p VALUES(1,1) ON CONFLICT DO NOTHING", "INSERT INTO p VALUES(1,1) ON CONFLICT DO UPDATE SET v=2",
			} {
				cases = append(cases, []string{"PRAGMA foreign_keys=ON", "CREATE TABLE p(" + pdef + ")",
					fmt.Sprintf("CREATE TABLE c(id INTEGER PRIMARY KEY, r REFERENCES %s)", ref), "INSERT INTO p VALUES(1,1)", op, "SELECT count(*) FROM p"})
			}
		}
	}
	sweepAll(t, cases)
}
