package driver_test

import (
	"bytes"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// TestBlobsReadThroughTheValueSlab: BLOB and TEXT values across many value
// chunks scan back as stored into []byte, any and sql.RawBytes; an empty BLOB
// is an empty []byte and NULL is nil.
func TestBlobsReadThroughTheValueSlab(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "b.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, b BLOB, s TEXT, m)`); err != nil {
		t.Fatal(err)
	}
	blob := func(i int) []byte {
		switch i % 5 {
		case 0:
			return []byte{}
		case 1:
			return nil
		}
		return bytes.Repeat([]byte{byte(i)}, i%40)
	}
	mixed := func(i int) any {
		switch i % 4 {
		case 0:
			return int64(i)
		case 1:
			return fmt.Sprint("text ", i)
		case 2:
			return []byte{byte(i), 0, byte(i)}
		}
		return float64(i) / 4
	}
	const n = 100
	for i := range n {
		var s any
		if i%3 != 0 {
			s = fmt.Sprint("row ", i)
		}
		var b any
		if v := blob(i); v != nil {
			b = v
		}
		if _, err := db.Exec(`INSERT INTO t VALUES(?, ?, ?, ?)`, i, b, s, mixed(i)); err != nil {
			t.Fatal(err)
		}
	}
	sameBlob := func(dest string, i int, got []byte) {
		t.Helper()
		want := blob(i)
		if (got == nil) != (want == nil) || !bytes.Equal(got, want) {
			t.Fatalf("%s row %d: got %#v, want %#v", dest, i, got, want)
		}
	}

	rows, err := db.Query(`SELECT id, b, s, m FROM t ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	i := 0
	for ; rows.Next(); i++ {
		var id int64
		var b []byte
		var s sql.NullString
		var m any
		if err := rows.Scan(&id, &b, &s, &m); err != nil {
			t.Fatal(err)
		}
		sameBlob("[]byte", i, b)
		if s.Valid != (i%3 != 0) || s.Valid && s.String != fmt.Sprint("row ", i) {
			t.Fatalf("row %d: text %+v", i, s)
		}
		if mb, ok := m.([]byte); ok {
			if !bytes.Equal(mb, mixed(i).([]byte)) {
				t.Fatalf("row %d: mixed %#v", i, m)
			}
		} else if m != mixed(i) {
			t.Fatalf("row %d: mixed %#v (%T), want %#v", i, m, m, mixed(i))
		}
	}
	if err := rows.Err(); err != nil || i != n {
		t.Fatalf("%d rows, %v", i, err)
	}
	rows.Close()

	rows, err = db.Query(`SELECT b FROM t ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for i = 0; rows.Next(); i++ {
		var v any
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v == nil {
			sameBlob("any", i, nil)
			continue
		}
		b, ok := v.([]byte)
		if !ok {
			t.Fatalf("any row %d: %T", i, v)
		}
		sameBlob("any", i, b)
	}
	rows.Close()

	rows, err = db.Query(`SELECT b FROM t ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for i = 0; rows.Next(); i++ {
		var raw sql.RawBytes
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		sameBlob("RawBytes", i, raw)
	}
	rows.Close()
}
