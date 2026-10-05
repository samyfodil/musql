package replication

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

const itemsDDL = `CREATE TABLE items(id TEXT PRIMARY KEY, price REAL, data BLOB)`

// wrapItems is openSynced over an items table (REAL + BLOB columns, no NOT
// NULL besides the PK).
func wrapItems(t *testing.T, site string) (*Syncer, *sql.DB) {
	t.Helper()
	return openSynced(t, site, itemsDDL)
}

// TestCaptureFloatAndBlobRoundTrip covers the value codec for REAL and BLOB
// columns end to end: capture on a live INSERT, then replay the captured ops
// onto a fresh store over the same schema and confirm both values survive
// intact (float64 exactly, via the IEEE-754 bit round trip; blob byte for
// byte).
func TestCaptureFloatAndBlobRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, db := wrapItems(t, "floatblob")

	blob := []byte{0x00, 0x01, 0x02, 0xff, 0x10, 0x20}
	const price = 19.99
	if _, err := db.Exec(`INSERT INTO items(id, price, data) VALUES(?, ?, ?)`, "i1", price, blob); err != nil {
		t.Fatal(err)
	}

	var gotPrice float64
	var gotData []byte
	if err := db.QueryRow(`SELECT price, data FROM items WHERE id='i1'`).Scan(&gotPrice, &gotData); err != nil {
		t.Fatal(err)
	}
	if gotPrice != price {
		t.Fatalf("price = %v, want %v", gotPrice, price)
	}
	if !bytes.Equal(gotData, blob) {
		t.Fatalf("data = %v, want %v", gotData, blob)
	}

	// Replay the captured ops onto a brand-new store over the same schema.
	ops, err := s.Store().OpsSince(ctx, s.Site(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "replay-items.db")
	rdb, err := openApplyDB("file:" + path + "?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	rs, err := OpenStore(ctx, rdb, "replay") // the schema arrives with the ops
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if err := rs.Ingest(ctx, op, false); err != nil {
			t.Fatalf("replay ingest: %v", err)
		}
	}

	var rPrice float64
	var rData []byte
	if err := rdb.QueryRow(`SELECT price, data FROM items WHERE id='i1'`).Scan(&rPrice, &rData); err != nil {
		t.Fatal(err)
	}
	if rPrice != price {
		t.Fatalf("replayed price = %v, want %v", rPrice, price)
	}
	if !bytes.Equal(rData, blob) {
		t.Fatalf("replayed data = %v, want %v", rData, blob)
	}
}

// TestValueEqualBranches drives every branch of valueEqual through real
// UPDATE statements on a live capture-enabled connection:
//   - a change to a BLOB column (the []byte branch, unequal),
//   - a column going from a value to NULL and back (the nil branches, both
//     the mixed nil/non-nil case and the both-nil case),
//   - an UPDATE that rewrites every column to its current value (nothing
//     actually changes, so nothing is captured).
func TestValueEqualBranches(t *testing.T) {
	ctx := context.Background()
	s, db := wrapItems(t, "valeq")

	blobA := []byte{1, 2, 3}
	blobB := []byte{1, 2, 3, 4}

	if _, err := db.Exec(`INSERT INTO items(id, price, data) VALUES(?, ?, ?)`, "i1", 1.5, blobA); err != nil {
		t.Fatal(err)
	}

	countOps := func() int {
		t.Helper()
		ops, err := s.Store().OpsSince(ctx, s.Site(), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(ops)
	}
	base := countOps()

	// 1. Update that changes the blob column: must be captured ([]byte branch).
	if _, err := db.Exec(`UPDATE items SET data=? WHERE id='i1'`, blobB); err != nil {
		t.Fatal(err)
	}
	if got := countOps() - base; got != 1 {
		t.Fatalf("blob-changing update captured %d new ops, want 1", got)
	}
	base = countOps()

	// 2. Update that sets a column to NULL: must be captured (mixed nil branch).
	if _, err := db.Exec(`UPDATE items SET price=NULL WHERE id='i1'`); err != nil {
		t.Fatal(err)
	}
	if got := countOps() - base; got != 1 {
		t.Fatalf("set-to-NULL update captured %d new ops, want 1", got)
	}
	base = countOps()

	// 2b. A redundant UPDATE setting the already-NULL column to NULL again:
	// both sides nil -> valueEqual reports equal -> nothing captured.
	if _, err := db.Exec(`UPDATE items SET price=NULL WHERE id='i1'`); err != nil {
		t.Fatal(err)
	}
	if got := countOps() - base; got != 0 {
		t.Fatalf("redundant NULL->NULL update captured %d new ops, want 0", got)
	}

	// 3. Update that sets a NULL column back to a value: must be captured
	// (mixed nil branch, other direction).
	if _, err := db.Exec(`UPDATE items SET price=? WHERE id='i1'`, 2.5); err != nil {
		t.Fatal(err)
	}
	if got := countOps() - base; got != 1 {
		t.Fatalf("clear-NULL update captured %d new ops, want 1", got)
	}
	base = countOps()

	// 4. Update that rewrites identical values for every synced column: no
	// column actually changed, so nothing is captured at all.
	if _, err := db.Exec(`UPDATE items SET data=?, price=? WHERE id='i1'`, blobB, 2.5); err != nil {
		t.Fatal(err)
	}
	if got := countOps() - base; got != 0 {
		t.Fatalf("no-op update captured %d new ops, want 0", got)
	}
}
