package engine

// Tests UPDATE ... FROM with schema-qualified targets.

import "testing"

// TestUpdateFromTargetQualifierPicksTheCatalog tests UPDATE ... FROM picks correct catalog.
func TestUpdateFromTargetQualifierPicksTheCatalog(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE t(k,v)`, `CREATE TEMP TABLE t(k,v)`, `CREATE TABLE m(k,nv)`,
		`INSERT INTO main.t VALUES(1,'a')`, `INSERT INTO temp.t VALUES(9,'z')`,
		`INSERT INTO m VALUES(1,'A'),(9,'Z')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	const stmt = `UPDATE main.t SET v=m.nv FROM m WHERE m.k=t.k`
	if _, cerr := db.compileWrite(stmt); cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	if err := db.Exec(stmt); err != nil {
		t.Fatalf("%q: %v", stmt, err)
	}
	for _, c := range []struct{ query, want string }{
		{`SELECT k||','||v FROM main.t`, "1,A"},
		{`SELECT k||','||v FROM temp.t`, "9,z"},
	} {
		got := rvdRowStrings(rvdQuery(t, db, c.query))
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s = %v, want [%s]. Pass one resolved the target name temp-first and joined\n"+
				"against the WRONG catalog's rows (update.c:222/228/230, expr.c:1913).", c.query, got, c.want)
		}
	}
}

// TestViewUpdateFromTargetQualifierPicksTheCatalog tests UPDATE ... FROM on views.
func TestViewUpdateFromTargetQualifierPicksTheCatalog(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`,
		`CREATE TEMP TABLE b(k,v)`, `CREATE TEMP VIEW v1 AS SELECT k,v FROM b`,
		`CREATE TABLE m(k,nv)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON main.v1 BEGIN INSERT INTO log VALUES('main:'||old.k||'->'||new.v); END`,
		`CREATE TEMP TRIGGER vut INSTEAD OF UPDATE ON temp.v1 BEGIN INSERT INTO log VALUES('temp:'||old.k||'->'||new.v); END`,
		`INSERT INTO main.b VALUES(1,'a')`, `INSERT INTO temp.b VALUES(9,'z')`,
		`INSERT INTO m VALUES(1,'A'),(9,'Z')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	const stmt = `UPDATE main.v1 SET v=m.nv FROM m WHERE m.k=v1.k`
	if _, cerr := db.compileWrite(stmt); cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	if err := db.Exec(stmt); err != nil {
		t.Fatalf("%q: %v", stmt, err)
	}
	got := rvdRowStrings(rvdQuery(t, db, `SELECT x FROM log ORDER BY rowid`))
	if len(got) != 1 || got[0] != "main:1->A" {
		t.Errorf("log = %v, want [main:1->A]. Pass one resolved the view name temp-first, so MAIN's\n"+
			"INSTEAD OF trigger fired with the TEMP view's row (update.c:222/228/230, expr.c:1913).", got)
	}

	// OR-clause spelling: target qualifier still picks the catalog.
	if err := db.Exec(`DELETE FROM log`); err != nil {
		t.Fatal(err)
	}
	const orStmt = `UPDATE OR IGNORE main.v1 SET v=m.nv FROM m WHERE m.k=v1.k`
	if _, icerr := db.compileWrite(orStmt); icerr != nil {
		t.Fatalf("compile %q: %v", orStmt, icerr)
	}
	if err := db.Exec(orStmt); err != nil {
		t.Fatalf("%q: %v", orStmt, err)
	}
	got = rvdRowStrings(rvdQuery(t, db, `SELECT x FROM log ORDER BY rowid`))
	if len(got) != 1 || got[0] != "main:1->A" {
		t.Errorf("OR-clause spelling: log = %v, want [main:1->A]. Pass one's join resolved the\n"+
			"view name temp-first (update.c:222/228/230, expr.c:1913).", got)
	}
}
