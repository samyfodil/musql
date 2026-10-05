// Oracle probe C: does C SQLite's freePage2 refuse to free a page its
// own freelist chain ALREADY names as an ordinary LEAF? Same technique and
// helpers as oracle_probe_3c_test.go's probes A/B/self-trunk; only the
// collision DEPTH differs -- self-trunk collides with the chain's head,
// this one collides with a leaf of that head. Run against SQLite 3.53.3
// (manifest.uuid d4c0e51e...82c62) via mattn/go-sqlite3.
package compat

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestOracleProbeExistingLeafCollision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leafcoll.db")
	const pageSize = 4096

	db, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, stmt := range []string{
		fmt.Sprintf(`PRAGMA page_size=%d`, pageSize),
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b BLOB)`,
		`INSERT INTO t VALUES(1, zeroblob(5000))`,
		`INSERT INTO t VALUES(2, zeroblob(5000))`,
		`INSERT INTO t VALUES(3, zeroblob(5000))`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	var root int
	if err := db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name = 't'`).Scan(&root); err != nil {
		t.Fatalf("rootpage: %v", err)
	}
	// row1's chain page becomes the head trunk T; row2's becomes T's leaf 0.
	if _, err := db.Exec(`DELETE FROM t WHERE a = 1`); err != nil {
		t.Fatalf("delete 1: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM t WHERE a = 2`); err != nil {
		t.Fatalf("delete 2: %v", err)
	}
	t.Logf("after deleting rows 1 and 2: freelist_count=%s integrity=%q",
		probePragma(t, db, `PRAGMA freelist_count`), probePragma(t, db, `PRAGMA integrity_check`))
	db.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	trunkT := binary.BigEndian.Uint32(data[32:36])
	if trunkT == 0 {
		t.Fatalf("setup: no freelist trunk")
	}
	trunkOff := int(trunkT-1) * pageSize
	nLeaf := binary.BigEndian.Uint32(data[trunkOff+4 : trunkOff+8])
	if nLeaf < 1 {
		t.Fatalf("setup: trunk %d has %d leaves, want >= 1", trunkT, nLeaf)
	}
	leafL := binary.BigEndian.Uint32(data[trunkOff+8 : trunkOff+12])
	t.Logf("chain: trunk T=%d, nLeaf=%d, leaf[0] L=%d", trunkT, nLeaf, leafL)

	// Patch row3's trailing overflow pointer to name L -- a page the chain
	// already holds as an ordinary leaf.
	ptrOff3 := probeCellOverflowPtrOffset(t, path, pageSize, root, 3)
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, leafL)
	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteAt(buf, ptrOff3); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	f.Close()

	db2, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	before := probePragma(t, db2, `PRAGMA freelist_count`)
	_, delErr := db2.Exec(`DELETE FROM t WHERE a = 3`)
	after := probePragma(t, db2, `PRAGMA freelist_count`)
	check := probePragma(t, db2, `PRAGMA integrity_check`)
	t.Logf("DELETE row3 (existing-leaf collision, L=%d) err=%v, freelist_count %s -> %s, integrity_check=%q",
		leafL, delErr, before, after, check)
}
