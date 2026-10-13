package engine

import (
	"path/filepath"
	"testing"
)

// A record built from ExecArgsOwned's parameters keeps their bytes rather than
// a copy (vdbe.isOwnedParam). The same parameter in two columns, a slice of one
// and a value computed from one must all read back as written; and through
// plain ExecArgs, whose caller keeps its bytes, the record must not alias them.
func TestOwnedParamRecord(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "o.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Exec("CREATE TABLE t(a, b, c, d)"); err != nil {
		t.Fatal(err)
	}
	p := []byte("hello")
	if _, _, err := s.DB.ExecArgsOwned("INSERT INTO t VALUES (?1, ?1, substr(?1, 2, 3), ?1 || 'x')", []Value{{Typ: Text, S: p}}); err != nil {
		t.Fatal(err)
	}
	q := []byte("world")
	if _, _, err := s.DB.ExecArgs("INSERT INTO t VALUES (?1, ?1, substr(?1, 2, 3), ?1 || 'x')", []Value{{Typ: Text, S: q}}); err != nil {
		t.Fatal(err)
	}
	copy(q, "XXXXX") // a plain ExecArgs caller may reuse its bytes
	_, rows, err := s.Query("SELECT a, b, c, d FROM t ORDER BY rowid", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"hello", "hello", "ell", "hellox"}, {"world", "world", "orl", "worldx"}}
	for i, r := range rows {
		for j, v := range r {
			if string(v.S) != want[i][j] {
				t.Errorf("row %d col %d: %q, want %q", i, j, v.S, want[i][j])
			}
		}
	}
}
