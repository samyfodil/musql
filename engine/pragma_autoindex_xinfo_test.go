package engine_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestIndexInfoOfAWithoutRowidAutoIndex verifies PRAGMA access to
// WITHOUT ROWID primary key indexes.
func TestIndexInfoOfAWithoutRowidAutoIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai.musq")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE wr(a TEXT, b INT, c, PRIMARY KEY(a,b)) WITHOUT ROWID`,
		// UNIQUE has schema row, PK does not.
		`CREATE TABLE wu(a TEXT, b TEXT UNIQUE, c, PRIMARY KEY(a)) WITHOUT ROWID`,
		`CREATE TABLE w3(p TEXT COLLATE NOCASE, q, PRIMARY KEY(p DESC, q)) WITHOUT ROWID`,
		// A ROWID table: its automatic index HAS a row, so nothing here
		// changes for it, and its name must not resolve to a PK index.
		`CREATE TABLE rt(x, y UNIQUE)`,
	} {
		mustExec(t, db, s)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, tc := range []struct {
		sql  string
		want []string // "seqno|cid|name|desc|coll|key", or "seqno|cid|name"
	}{
		// Key columns first, then every REMAINING table column in table
		// order: the PK index of a WITHOUT ROWID table IS the table's b-tree.
		{`PRAGMA index_xinfo('sqlite_autoindex_wr_1')`, []string{
			"0|0|a|0|BINARY|1", "1|1|b|0|BINARY|1", "2|2|c|0|BINARY|0"}},
		// index_info reports the KEY columns alone (pragma.c's nKeyCol).
		{`PRAGMA index_info('sqlite_autoindex_wr_1')`, []string{"0|0|a", "1|1|b"}},
		{`PRAGMA index_xinfo('sqlite_autoindex_wu_2')`, []string{
			"0|0|a|0|BINARY|1", "1|1|b|0|BINARY|0", "2|2|c|0|BINARY|0"}},
		// A PK column's own DESC and COLLATE ride along.
		{`PRAGMA index_xinfo('sqlite_autoindex_w3_1')`, []string{
			"0|0|p|1|NOCASE|1", "1|1|q|0|BINARY|1"}},
		// The UNIQUE on the same WITHOUT ROWID table still resolves through
		// its own schema row, and carries the PK column as its non-key tail.
		{`PRAGMA index_xinfo('sqlite_autoindex_wu_1')`, []string{
			"0|1|b|0|BINARY|1", "1|0|a|0|BINARY|0"}},
		// A rowid table's automatic index is unchanged: the trailing pseudo
		// -column is the implicit rowid (cid -1, NULL name).
		{`PRAGMA index_xinfo('sqlite_autoindex_rt_1')`, []string{
			"0|1|y|0|BINARY|1", "1|-1||0|BINARY|0"}},
		// A number that names no automatic index answers zero rows, exactly
		// as C's own failed sqlite3FindIndex does.
		{`PRAGMA index_xinfo('sqlite_autoindex_wr_9')`, nil},
		{`PRAGMA index_xinfo('nosuchindex')`, nil},
		// ...and the eponymous TVF, which could only be registered once the
		// bare pragma above was right (vtab_pragma.go's own note).
		{`SELECT * FROM pragma_index_xinfo('sqlite_autoindex_wr_1')`, []string{
			"0|0|a|0|BINARY|1", "1|1|b|0|BINARY|1", "2|2|c|0|BINARY|0"}},
	} {
		_, rows, err := p.Query(tc.sql)
		if err != nil {
			t.Errorf("%s: %v", tc.sql, err)
			continue
		}
		got := make([]string, len(rows))
		for i, r := range rows {
			f := make([]string, len(r))
			for j, v := range r {
				switch v.Typ {
				case engine.Int:
					f[j] = strconv.FormatInt(v.I, 10)
				case engine.Null:
					f[j] = ""
				default:
					f[j] = string(v.S)
				}
			}
			got[i] = strings.Join(f, "|")
		}
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s:\n got %v\nwant %v", tc.sql, got, tc.want)
		}
	}
}
