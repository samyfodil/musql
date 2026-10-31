package driver_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// TestNilBytesBindAsNull: a nil []byte binds as NULL, as sqlite3_bind_blob
// does with a NULL pointer and mattn/go-sqlite3 binds it, and an empty one as
// a zero-length BLOB, by position and by name, stored and compared.
func TestNilBytesBindAsNull(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "n.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v)`); err != nil {
		t.Fatal(err)
	}
	for i, v := range []any{[]byte(nil), []byte{}, sql.Named("v", []byte(nil)), sql.Named("v", []byte{})} {
		q := `INSERT INTO t VALUES(?, ?)`
		if _, named := v.(sql.NamedArg); named {
			q = `INSERT INTO t VALUES(?, :v)`
		}
		if _, err := db.Exec(q, i, v); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	rows, err := db.Query(`SELECT id, typeof(v), length(v) FROM t ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := []string{"null", "blob", "null", "blob"}
	n := 0
	for ; rows.Next(); n++ {
		var id int
		var typ string
		var length sql.NullInt64
		if err := rows.Scan(&id, &typ, &length); err != nil {
			t.Fatal(err)
		}
		if typ != want[id] || typ == "blob" && length.Int64 != 0 {
			t.Fatalf("row %d: typeof %s, length %v; want %s", id, typ, length, want[id])
		}
	}
	if err := rows.Err(); err != nil || n != len(want) {
		t.Fatalf("%d rows, %v", n, err)
	}
	var nilIsNull, emptyIsNull bool
	if err := db.QueryRow(`SELECT ? IS NULL, ? IS NULL`, []byte(nil), []byte{}).Scan(&nilIsNull, &emptyIsNull); err != nil {
		t.Fatal(err)
	}
	if !nilIsNull || emptyIsNull {
		t.Fatalf("bound: nil IS NULL %v, empty IS NULL %v", nilIsNull, emptyIsNull)
	}
}
