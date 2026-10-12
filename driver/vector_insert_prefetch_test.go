package driver_test

import (
	"bytes"
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// TestVectorInsertPrefetched runs multi-row INSERTs big enough for their vector
// literals to be read ahead of the statement (engine.vecPrefetch), and holds
// them to what reading each at its row gives: the same blobs as statements too
// small to be read ahead, and, for a statement with a bad vector or a repeated
// key, the same error for the same row, in statement order, with nothing
// inserted.
func TestVectorInsertPrefetched(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, tbl := range []string{"big", "one"} {
		if _, err := db.Exec("CREATE TABLE " + tbl + "(id INTEGER PRIMARY KEY, a BLOB, b BLOB)"); err != nil {
			t.Fatal(err)
		}
	}
	rng := rand.New(rand.NewSource(8))
	vec := func(bad bool) string {
		parts := make([]string, 400)
		for i := range parts {
			parts[i] = fmt.Sprintf("%.6g", rng.NormFloat64()*float64(1+rng.Intn(3)))
		}
		if bad {
			parts[200] = "1.2.3"
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	const rows = 60
	var texts [rows]string
	for i := range texts {
		texts[i] = vec(false)
	}
	insert := func(tbl string, from, to int) string {
		var vals []string
		for i := from; i < to; i++ {
			vals = append(vals, fmt.Sprintf("(%d, vector('%s'), vector64('%s'))", i+1, texts[i%rows], texts[i%rows]))
		}
		return "INSERT INTO " + tbl + " VALUES " + strings.Join(vals, ",")
	}
	// One big statement, then the same rows one at a time.
	if _, err := db.Exec(insert("big", 0, rows)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.Exec(insert("one", i, i+1)); err != nil {
			t.Fatal(err)
		}
	}
	for _, col := range []string{"a", "b"} {
		for i := 0; i < rows; i++ {
			var x, y []byte
			if err := db.QueryRow("SELECT "+col+" FROM big WHERE id = ?", i+1).Scan(&x); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT "+col+" FROM one WHERE id = ?", i+1).Scan(&y); err != nil {
				t.Fatal(err)
			}
			if len(x) == 0 || !bytes.Equal(x, y) {
				t.Fatalf("%s row %d: big statement %d bytes, single %d bytes, equal=%v", col, i+1, len(x), len(y), bytes.Equal(x, y))
			}
		}
	}
	// Rows 101..160, the vector of row 131 bad. Row 105 taking id 3, which the
	// table holds, is the error instead, its row coming first.
	bad := strings.Replace(insert("big", 100, 160), texts[130%rows], vec(true), 1)
	_, vecErr := db.Exec(bad)
	if vecErr == nil || !strings.Contains(vecErr.Error(), "invalid float") {
		t.Fatalf("bad vector: %v", vecErr)
	}
	_, keyErr := db.Exec(strings.Replace(bad, "(105,", "(3,", 1))
	if keyErr == nil || !strings.Contains(keyErr.Error(), "UNIQUE") {
		t.Fatalf("repeated key: %v", keyErr)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM big").Scan(&n); err != nil || n != rows {
		t.Fatalf("rows after the failures: %d, %v", n, err)
	}
	// The bad statement again, with nothing read ahead of it any more: same error.
	if _, err := db.Exec(bad); err == nil || err.Error() != vecErr.Error() {
		t.Fatalf("second run: %v, want %q", err, vecErr)
	}
}
