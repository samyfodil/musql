package replication

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/samyfodil/musql/driver"
)

// TestPersistenceRestart gates Store persistence: reopening recovers LocalVV,
// handles gaps in site sequences, and re-seeds the clock past durable HLC values.
func TestPersistenceRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)"

	db1, err := openApplyDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	s1, err := OpenStore(ctx, db1, "X")
	if err != nil {
		t.Fatal(err)
	}
	installUsers(t, s1)

	// Far-future HLC (24h ahead) to verify clock re-seeding.
	future := HLC(uint64(time.Now().Add(24*time.Hour).UnixMilli()) << 16)

	// Site X: seqs 1,2,3 (contiguous), ingested out of order. The last op
	// carries the far-future HLC.
	opsX := []Op{
		insertOp("X", 1, 100, "u1", "alice", 30),
		updateOp("X", 2, 200, "u1", CellFromValue(ucol("age"), int64(31))),
		insertOp("X", 3, future, "u2", "bob", 25),
	}
	ingestAll(t, s1, []Op{opsX[1], opsX[0], opsX[2]})

	// Site Y: seqs 1,2,4 -- seq 3 never arrives, so the contiguous VV must
	// stop at 2. Ingested out of order too.
	opsY := []Op{
		insertOp("Y", 1, 150, "u3", "carol", 40),
		updateOp("Y", 2, 250, "u3", CellFromValue(ucol("age"), int64(41))),
		insertOp("Y", 4, 350, "u4", "dave", 22),
	}
	ingestAll(t, s1, []Op{opsY[2], opsY[0], opsY[1]})

	if vv := s1.LocalVV(); vv["X"] != 3 || vv["Y"] != 2 {
		t.Fatalf("before restart: LocalVV = %+v, want X=3 Y=2", vv)
	}

	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen a brand-new Store over the same file.
	db2, err := openApplyDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db2.Close() })
	s2, err := OpenStore(ctx, db2, "X")
	if err != nil {
		t.Fatal(err)
	}

	if vv := s2.LocalVV(); vv["X"] != 3 || vv["Y"] != 2 {
		t.Fatalf("after restart: LocalVV = %+v, want X=3 Y=2 (gap at Y seq 3 must stop the count)", vv)
	}

	// The reopened store's clock must be seeded past the highest HLC durable
	// in _repl_clock (the far-future value), not just past real wall-clock
	// time.
	if got := s2.Clock().Now(); got <= future {
		t.Fatalf("Clock().Now() = %d after restart, want > %d (future HLC durable before close)", got, future)
	}

	// The materialized rows themselves must also have survived the restart.
	if got, want := dumpUsers(t, s2), "u1|alice|31\nu2|bob|25\nu3|carol|41\nu4|dave|22\n"; got != want {
		t.Fatalf("after restart users = %q, want %q", got, want)
	}
}
