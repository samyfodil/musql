// Tests INSERT ... SELECT trigger body statements in UPDATE/DELETE triggers.
// Such statements must be name-resolved at compile time even if the trigger
// never fires, and ORDER BY/LIMIT clauses must be accepted in trigger bodies.
package compat

import "testing"

// TestWpathR24InsertSelectTriggerBody pins the accepted shapes and, more
// importantly, the ZERO-ROW firings that still have to raise.
func TestWpathR24InsertSelectTriggerBody(t *testing.T) {
	// misc3.test 97: ORDER BY + LIMIT decides WHICH row is inserted.
	flLockstep(t, "delete-trigger-order-by-limit", []string{
		`CREATE TABLE y1(a)`,
		`CREATE TABLE y2(b)`,
		`CREATE TABLE y3(c)`,
		`INSERT INTO y1 VALUES(1),(2)`,
		`INSERT INTO y2 VALUES(30),(10),(20)`,
		`CREATE TRIGGER r1 AFTER DELETE ON y1 FOR EACH ROW BEGIN
		   INSERT INTO y3(c) SELECT b FROM y2 ORDER BY b LIMIT 1;
		 END`,
		`DELETE FROM y1 WHERE a=1`,
	}, `SELECT * FROM y3`, `SELECT * FROM y1`)

	// misc1.test 18: an UPDATE trigger whose source SELECT reads NEW.
	flLockstep(t, "update-trigger-new-limit", []string{
		`CREATE TABLE TempTable(TestString)`,
		`CREATE TABLE RealTable(TestString)`,
		`INSERT INTO TempTable VALUES('a'),('b')`,
		`CREATE TRIGGER trigTest_1 AFTER UPDATE ON TempTable BEGIN
		   INSERT INTO RealTable(TestString) SELECT new.TestString FROM TempTable LIMIT 1;
		 END`,
		`UPDATE TempTable SET TestString='z' WHERE TestString='a'`,
	}, `SELECT * FROM RealTable`, `SELECT * FROM TempTable ORDER BY TestString`)

	// OFFSET too, which rides the same predicate.
	flLockstep(t, "delete-trigger-limit-offset", []string{
		`CREATE TABLE y1(a)`,
		`CREATE TABLE y2(b)`,
		`CREATE TABLE y3(c)`,
		`INSERT INTO y1 VALUES(1)`,
		`INSERT INTO y2 VALUES(30),(10),(20)`,
		`CREATE TRIGGER r1 AFTER DELETE ON y1 BEGIN
		   INSERT INTO y3(c) SELECT b FROM y2 ORDER BY b LIMIT 1 OFFSET 1;
		 END`,
		`DELETE FROM y1 WHERE a=1`,
	}, `SELECT * FROM y3`)

	// An ORDER BY term naming an OUTPUT column -- an ordinal, or a result alias
	// -- is not an input-column reference, and validating it as one would
	// reject exactly what C SQLite accepts.
	for _, c := range []struct{ name, src string }{
		{"ordinal", `SELECT b FROM y2 ORDER BY 1 LIMIT 1`},
		{"alias", `SELECT b AS z FROM y2 ORDER BY z LIMIT 1`},
		{"expanded-name", `SELECT b FROM y2 ORDER BY b LIMIT 1`},
		{"expression", `SELECT b FROM y2 ORDER BY b*-1 LIMIT 1`},
		{"other-column", `SELECT b FROM y2 ORDER BY d LIMIT 1`},
	} {
		flLockstep(t, "order-by-output-"+c.name, []string{
			`CREATE TABLE y1(a)`,
			`CREATE TABLE y2(b, d)`,
			`CREATE TABLE y3(c)`,
			`INSERT INTO y1 VALUES(1)`,
			`INSERT INTO y2 VALUES(30,3),(10,1),(20,2)`,
			`CREATE TRIGGER r1 AFTER DELETE ON y1 BEGIN
			   INSERT INTO y3(c) ` + c.src + `;
			 END`,
			`DELETE FROM y1 WHERE a=99`,
			`DELETE FROM y1 WHERE a=1`,
		}, `SELECT * FROM y3`)
	}

	// The zero-row firings. Each names a column that exists nowhere, in a
	// different clause; C SQLite raises for all three even though the DELETE
	// matches NO row, because it compiles the body rather than running it.
	for _, c := range []struct{ name, src string }{
		{"bad-select-list", `SELECT nosuchcol FROM y2 LIMIT 1`},
		{"bad-where", `SELECT b FROM y2 WHERE nosuchcol=1 LIMIT 1`},
		{"bad-order-by", `SELECT b FROM y2 ORDER BY nosuchcol LIMIT 1`},
		{"bad-order-by-no-limit", `SELECT b FROM y2 ORDER BY nosuchcol`},
		{"bad-table", `SELECT b FROM nosuchtable LIMIT 1`},
	} {
		flLockstep(t, "zero-row-firing-"+c.name, []string{
			`CREATE TABLE y1(a)`,
			`CREATE TABLE y2(b)`,
			`CREATE TABLE y3(c)`,
			`INSERT INTO y2 VALUES(1)`,
			`CREATE TRIGGER r1 AFTER DELETE ON y1 BEGIN
			   INSERT INTO y3(c) ` + c.src + `;
			 END`,
			// y1 is EMPTY: this DELETE matches nothing and must still raise.
			`DELETE FROM y1 WHERE a=1`,
			// ...and so must one that really fires.
			`INSERT INTO y1 VALUES(1)`,
			`DELETE FROM y1 WHERE a=1`,
		}, `SELECT * FROM y3`, `SELECT * FROM y1`)
	}
}
