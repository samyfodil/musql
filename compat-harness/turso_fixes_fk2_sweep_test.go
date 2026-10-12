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
	// A table that is both a child of a good key and the parent of a mismatched
	// one: assigning a column of its own key makes update.c ask for EVERY key
	// pointing at it (sqlite3FkOldmask), the mismatched one included.
	for _, ref := range []string{"p2", "p2(f)", "p2(e)"} {
		for _, op := range []string{
			"UPDATE p2 SET e=5", "UPDATE p2 SET e=1", "UPDATE p2 SET f=5", "UPDATE p2 SET e=1, f=2", "UPDATE p2 SET rowid=9",
			"UPDATE OR REPLACE p2 SET e=1", "DELETE FROM p2", "INSERT INTO p2 VALUES(1,2),(1,3)", "INSERT INTO p2 VALUES(1,2)",
			"INSERT INTO p2 VALUES(1,2) ON CONFLICT DO UPDATE SET e=1", "UPDATE p2 SET e=NULL WHERE f=3",
		} {
			cases = append(cases, []string{"PRAGMA foreign_keys=ON", "CREATE TABLE p1(a INTEGER PRIMARY KEY, b, c, UNIQUE(b,c))",
				"CREATE TABLE p2(e REFERENCES p1, f)", "CREATE TABLE c3(g REFERENCES " + ref + ")",
				"INSERT INTO p1 VALUES(1,2,3)", "INSERT INTO p2 VALUES(1,2)", "INSERT INTO p2 VALUES(NULL,3)", op, "SELECT count(*) FROM p2"})
		}
	}
	sweepAll(t, cases)
}
