package vec1

import (
	"database/sql"
	"math/rand/v2"
	"path/filepath"
	"testing"

	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/driver"
)

// TestVec1Convert carries a vec1 table across the file formats both ways: a
// C SQLite file vec1 built, imported into musql, and a musql database
// exported to a C SQLite file, each then answering the same queries, and
// passing the same integrity check, as the database it came from.
func TestVec1Convert(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	vecs := clusteredVecs(r, 600, 16, 6)
	queries := randomVecs(r, 3, 16)
	dir := t.TempDir()
	build := func(db *sql.DB) {
		t.Helper()
		mustExec(t, db, "CREATE VIRTUAL TABLE v USING vec1(vector, m0, m1)")
		for i, v := range vecs {
			mustExec(t, db, "INSERT INTO v(rowid, vector, m0, m1) VALUES(?, ?, ?, ?)", i+1, vecBlob(v), metaValue(i, 0), metaValue(i, 3))
		}
	}
	queriesOf := func(db *sql.DB) []result {
		t.Helper()
		var out []result
		for _, q := range queries {
			qb := vecBlob(q)
			out = append(out, query(db, "SELECT rowid, distance, m0, m1 FROM v(?, 8)", qb))
			out = append(out, query(db, "SELECT rowid, distance FROM v(?, 8) WHERE m0 = 2", qb))
		}
		out = append(out, query(db, "SELECT id, bucket, first, last, hex(val) FROM v_idx ORDER BY id"))
		out = append(out, query(db, "PRAGMA integrity_check"))
		return out
	}
	// compare is the first mismatch between two engines' answers, or "".
	compare := func(got, want []result) string {
		for i := range want {
			if !equalResults(got[i], want[i]) {
				return clip(got[i].String()) + "--- against\n" + clip(want[i].String())
			}
		}
		return ""
	}

	// C to musql.
	cPath := filepath.Join(dir, "c.db")
	c, err := openOracle(cPath)
	if err != nil {
		t.Fatal(err)
	}
	build(c)
	model := trainModel(t, c, "v", `{nbucket: 6, codesize: 4, quantizer:"pq"}`)
	mustExec(t, c, "INSERT INTO v(cmd, arg) VALUES('rebuild', ?)", model)
	want := queriesOf(c)
	c.Close()
	mPath := filepath.Join(dir, "imported.musq")
	if err := sqliteconv.Import(cPath, mPath, sqliteconv.ImportOptions{}); err != nil {
		t.Fatalf("import: %v", err)
	}
	m, err := sql.Open(driver.DriverName, mPath)
	if err != nil {
		t.Fatal(err)
	}
	if d := compare(queriesOf(m), want); d != "" {
		t.Fatalf("imported, musql then C:\n%s", d)
	}
	m.Close()

	// musql to C.
	m2Path := filepath.Join(dir, "m.musq")
	m2, err := sql.Open(driver.DriverName, m2Path)
	if err != nil {
		t.Fatal(err)
	}
	m2.SetMaxOpenConns(1)
	build(m2)
	mustExec(t, m2, "INSERT INTO v(cmd, arg) VALUES('rebuild', ?)", model)
	want = queriesOf(m2)
	m2.Close()
	ePath := filepath.Join(dir, "exported.db")
	if err := sqliteconv.Export(m2Path, ePath, 0); err != nil {
		t.Fatalf("export: %v", err)
	}
	c2, err := openOracle(ePath)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if d := compare(queriesOf(c2), want); d != "" {
		t.Fatalf("exported, C then musql:\n%s", d)
	}
}
