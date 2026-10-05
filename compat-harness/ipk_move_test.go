package compat

import (
	"fmt"
	"testing"
)

// TestIPKMoveKillsTheOldRowid: an UPDATE that changes a row's rowid is a delete
// of the old rowid and an insert of the new one (update.c's chngKey path). The
// segment delta got only the insert, so count(*) -- which reads the file --
// counted the moved row twice, and the old row came back on the next open.
func TestIPKMoveKillsTheOldRowid(t *testing.T) {
	base := []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v)", "INSERT INTO t VALUES(1,'a'),(2,'b'),(3,'c'),(4,'d'),(5,'e')"}
	for i, u := range [][]string{
		{"UPDATE t SET id=1000 WHERE id=3"},
		{"UPDATE t SET id=-id"},
		{"UPDATE t SET id=id+10"},
		{"UPDATE t SET id=1000 WHERE id=3", "INSERT INTO t VALUES(3,'new')"},
		{"BEGIN", "UPDATE t SET id=1000 WHERE id=3", "UPDATE t SET id=3 WHERE id=1000", "COMMIT"},
		{"UPDATE t SET rowid=77 WHERE v='b'"},
	} {
		st := append(append(append([]string{}, base...), u...), "SELECT count(*) FROM t", "SELECT rowid, * FROM t")
		differ(t, fmt.Sprintf("move %d", i), st)
	}
}

// TestFKCascadeMovesAnIPKChild: an ON UPDATE action is an implicit UPDATE of
// the child, so when the child's INTEGER PRIMARY KEY is the foreign key the row
// moves to the new rowid (OP_MustBeInt, update.c:894; a collision is
// sqlite3RowidConstraint's error). The cascade kept the old rowid, so the child
// still named the old parent and the statement failed its FK check.
func TestFKCascadeMovesAnIPKChild(t *testing.T) {
	pre := []string{"PRAGMA foreign_keys=ON", "CREATE TABLE p(k INTEGER PRIMARY KEY)", "INSERT INTO p VALUES(1),(2),(3)"}
	ipkChild := "CREATE TABLE c(id INTEGER PRIMARY KEY REFERENCES p ON UPDATE CASCADE, v)"
	for i, c := range [][]string{
		{ipkChild, "INSERT INTO c VALUES(1,'x'),(2,'y')", "UPDATE p SET k=10 WHERE k=1"},
		{ipkChild, "INSERT INTO c VALUES(1,'x'),(2,'y')", "UPDATE p SET k=k+10"},
		{ipkChild, "INSERT INTO c VALUES(1,'x'),(2,'y')", "INSERT INTO c VALUES(10,'z')", "UPDATE p SET k=10 WHERE k=1"},
		{"CREATE TABLE c(id INTEGER PRIMARY KEY REFERENCES p ON UPDATE SET NULL, v)", "INSERT INTO c VALUES(1,'x')", "UPDATE p SET k=10 WHERE k=1"},
		{ipkChild, "CREATE TABLE lg(x)", "CREATE TRIGGER tr AFTER UPDATE ON c BEGIN INSERT INTO lg VALUES(new.rowid || ':' || old.rowid); END",
			"INSERT INTO c VALUES(1,'x')", "UPDATE p SET k=10 WHERE k=1", "SELECT * FROM lg"},
		{ipkChild, "INSERT INTO c VALUES(1,'x')", "BEGIN", "UPDATE p SET k=10 WHERE k=1", "ROLLBACK"},
	} {
		st := append(append(append([]string{}, pre...), c...), "SELECT rowid, * FROM c", "SELECT count(*) FROM c")
		differ(t, fmt.Sprintf("cascade %d", i), st)
	}
}
