package libp2p_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/host"

	replp2p "github.com/samyfodil/musql/examples/libp2p"
	"github.com/samyfodil/musql/replication"
)

// openBlobs is openUsersWith for a table with a BLOB column.
func openBlobs(t *testing.T, h host.Host, name, topic string) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".db")
	db0, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db0.Exec(`CREATE TABLE b(id TEXT PRIMARY KEY, v BLOB)`); err != nil {
		t.Fatal(err)
	}
	db0.Close()
	db, err := replication.Open(context.Background(), path, replication.CRDT(), replp2p.Sync(h, topic), replication.WithSyncInterval(200*time.Millisecond))
	if err != nil {
		t.Fatalf("replication.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// dumpBlobs renders each row as id|typeof(v)|length(v), -1 for NULL.
func dumpBlobs(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT id, typeof(v), coalesce(length(v), -1) FROM b ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, typ string
		var n int
		if err := rows.Scan(&id, &typ, &n); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s|%s|%d", id, typ, n))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, " ")
}

// TestEmptyBlobReplicatesAsEmpty: an empty BLOB arrives on the other node as
// an empty BLOB, inserted and updated to, and NULL as NULL, over the libp2p
// transport, which does not send empty bytes; a nil []byte binds as NULL.
func TestEmptyBlobReplicatesAsEmpty(t *testing.T) {
	hA, hB := newHost(t), newHost(t)
	connectHosts(t, hA, hB)
	dbA := openBlobs(t, hA, "A", "repl-test-empty-blob")
	dbB := openBlobs(t, hB, "B", "repl-test-empty-blob")
	reach := func(want string) {
		t.Helper()
		got := ""
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if got = dumpBlobs(t, dbB); got == want {
				return
			}
		}
		t.Fatalf("B holds %q, want %q", got, want)
	}
	if _, err := dbA.Exec(`INSERT INTO b VALUES('e', x''), ('n', NULL), ('x', x'0102')`); err != nil {
		t.Fatal(err)
	}
	reach("e|blob|0 n|null|-1 x|blob|2")
	if _, err := dbA.Exec(`UPDATE b SET v = x'' WHERE id = 'x'`); err != nil {
		t.Fatal(err)
	}
	reach("e|blob|0 n|null|-1 x|blob|0")
}
