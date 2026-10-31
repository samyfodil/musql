package replication

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// uniqDDL is a table whose rows contend for two UNIQUE columns: a row can lose
// a value to a newer one, wait held out, and come back when the value is freed.
const uniqDDL = `CREATE TABLE t(id INTEGER PRIMARY KEY, u INTEGER UNIQUE, v INTEGER UNIQUE, w TEXT)`

var uniqID = objectID("table", "t", uniqDDL, 0)

func uniqCol(name string) string { return initialColID(uniqID, name) }

// uniqOps is n ops from three sites on 40 rows of uniqDDL's table: inserts,
// one-column updates and deletes, each site's tags rising, the sites'
// interleaved at random, so values are taken, lost and freed all along.
func uniqOps(rng *rand.Rand, n int) []Op {
	sites := []string{"A", "B", "C"}
	seq, hlc := map[string]uint64{}, map[string]HLC{}
	ops := make([]Op, 0, n)
	for range n {
		site := sites[rng.IntN(len(sites))]
		seq[site]++
		hlc[site] += HLC(1 + rng.IntN(30))
		op := Op{Site: site, Seq: seq[site], HLC: hlc[site] + 2, Tbl: uniqID, PK: EncodePK(int64(rng.IntN(40)))}
		switch r := rng.IntN(10); {
		case r < 2:
			op.Kind = OpDelete
		case r < 6:
			op.Kind = OpInsert
			op.Cells = []Cell{
				CellFromValue(uniqCol("u"), int64(rng.IntN(15))),
				CellFromValue(uniqCol("v"), int64(rng.IntN(15))),
				CellFromValue(uniqCol("w"), fmt.Sprint("w", rng.IntN(100))),
			}
		default:
			op.Kind = OpUpdate
			op.Cells = []Cell{CellFromValue(uniqCol([]string{"u", "v", "w"}[rng.IntN(3)]), int64(rng.IntN(15)))}
		}
		ops = append(ops, op)
	}
	return ops
}

func newUniqStore(t *testing.T, name string) *Store {
	t.Helper()
	s := newTestStore(t, name)
	op := Op{Site: "schema", Seq: 2, HLC: 2, Kind: OpSchema,
		Cells: schemaOp{Act: actCreate, ID: uniqID, Type: "table", Name: "t", SQL: uniqDDL}.cells()}
	if err := s.Ingest(context.Background(), op, false); err != nil {
		t.Fatal(err)
	}
	return s
}

// dumpUniq is uniqDDL's table and the rows it holds out, as q reads them.
func dumpUniq(t *testing.T, q dbtx) string {
	t.Helper()
	var b strings.Builder
	for _, query := range []string{
		`SELECT id, quote(u), quote(v), quote(w) FROM main.t ORDER BY id`,
		`SELECT tbl, quote(pk), '', '' FROM main._repl_withheld ORDER BY tbl, pk`,
	} {
		rows, err := q.QueryContext(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var a, c, d, e string
			if err := rows.Scan(&a, &c, &d, &e); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s|%s|%s|%s\n", a, c, d, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		b.WriteString("--\n")
	}
	return b.String()
}

// rebuiltUniq is dumpUniq after rebuildTable, rolled back: the table as the
// clock alone has it, which a node that rebuilt it holds.
func rebuiltUniq(t *testing.T, s *Store) string {
	t.Helper()
	ctx := context.Background()
	tx, err := s.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	s.q = tx
	defer func() { s.q = s.db }()
	if err := s.rebuildTable(ctx, s.cat.byID(uniqID)); err != nil {
		t.Fatal(err)
	}
	return dumpUniq(t, tx)
}

// ingestEach applies ops as ingest's transaction did before a batch merged
// a run of ops at a time: each op merged and materialized alone.
func ingestEach(t *testing.T, s *Store, ops []Op) {
	t.Helper()
	for done := 0; done < len(ops); {
		s.mu.Lock()
		n, err := s.ingestOnce(context.Background(), ops[done:], false, false)
		s.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		done += n
	}
}

// TestBatchAppliesAsOpsOneAtATime: ops applied a batch to a transaction, a
// run of them merged at once, leave the table, the rows it holds out and the
// clock as the same ops do applied one to a transaction, or each merged
// alone -- and the table is what a rebuild from the clock makes of it.
func TestBatchAppliesAsOpsOneAtATime(t *testing.T) {
	ctx := context.Background()
	seeds := 3
	if testing.Short() {
		seeds = 1
	}
	for seed := range seeds {
		rng := rand.New(rand.NewPCG(uint64(seed), 7))
		ops := uniqOps(rng, 2*applyRunMax+rng.IntN(applyRunMax))

		batch := newUniqStore(t, fmt.Sprintf("batch%d", seed))
		if n, err := batch.ingestAll(ctx, ops); err != nil {
			t.Fatalf("seed %d: op %d: %v", seed, n, err)
		}
		alone := newUniqStore(t, fmt.Sprintf("alone%d", seed))
		for _, op := range ops {
			if err := alone.Ingest(ctx, op, false); err != nil {
				t.Fatal(err)
			}
		}
		each := newUniqStore(t, fmt.Sprintf("each%d", seed))
		ingestEach(t, each, ops)

		want, wantClock := dumpUniq(t, each.db), dumpClock(t, each)
		if !strings.Contains(want, "\n--\n") || strings.HasPrefix(want, "--") {
			t.Fatalf("seed %d: no rows at all", seed)
		}
		for name, s := range map[string]*Store{"batch": batch, "alone": alone} {
			if got := dumpUniq(t, s.db); got != want {
				t.Errorf("seed %d: %s holds\n%s\nwant\n%s", seed, name, got, want)
			}
			if got := dumpClock(t, s); got != wantClock {
				t.Errorf("seed %d: %s's clock differs", seed, name)
			}
		}
		for name, s := range map[string]*Store{"batch": batch, "alone": alone, "each": each} {
			if got, rebuilt := dumpUniq(t, s.db), rebuiltUniq(t, s); got != rebuilt {
				t.Errorf("seed %d: %s holds\n%s\na rebuild from its clock\n%s", seed, name, got, rebuilt)
			}
		}
	}
}

// TestBatchRefusedOpAppliesThoseBefore: a batch that holds an op ingest
// refuses applies the ops before it and reports that op's index, as ops one
// at a time do.
func TestBatchRefusedOpAppliesThoseBefore(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(3, 9))
	ops := uniqOps(rng, applyRunMax+50)
	k := applyRunMax - 20
	bad := ops[k-30] // the same (site, seq) as an op before it, other cells
	bad.Cells = []Cell{CellFromValue(uniqCol("w"), "not the same op")}
	bad.Kind = OpUpdate
	ops = append(ops[:k:k], append([]Op{bad}, ops[k:]...)...)

	s := newUniqStore(t, "refused")
	n, err := s.ingestAll(ctx, ops)
	if n != k || err == nil || !strings.Contains(err.Error(), "two different ops") {
		t.Fatalf("got %d, %v; want %d and the two-writers refusal", n, err, k)
	}
	before := newUniqStore(t, "refused-before")
	ingestEach(t, before, ops[:k])
	if got, want := dumpUniq(t, s.db), dumpUniq(t, before.db); got != want {
		t.Fatalf("holds\n%s\nwant\n%s", got, want)
	}
	if dumpClock(t, s) != dumpClock(t, before) {
		t.Fatal("the clocks differ")
	}
	var logged int
	if err := s.db.QueryRow(`SELECT count(*) FROM _repl_oplog WHERE site <> 'schema'`).Scan(&logged); err != nil || logged != k {
		t.Fatalf("the log holds %d ops (%v), want %d", logged, err, k)
	}
}
