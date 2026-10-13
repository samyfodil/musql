package engine

import (
	"fmt"
	"maps"
	"math/rand"
	"path/filepath"
	"testing"
)

// TestRowidUpperBoundAnswersAsTheRows drives explicit-rowid INSERTs -- whose
// conflict probe answers from rowStore.ub when the rowid is past it --
// through deletes, whole-table clears, savepoints rolled back to and released,
// transactions committed and rolled back, and holds every INSERT's outcome
// (stored or a conflict) to a plain model of the rowids.
func TestRowidUpperBoundAnswersAsTheRows(t *testing.T) {
	n, err := Create(filepath.Join(t.TempDir(), "ub.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	if err := n.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v)`); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(8))
	have := map[int]bool{}
	var saves []map[int]bool // the model at each open savepoint
	var txStart map[int]bool
	inTx := false
	for step := 0; step < 4000; step++ {
		switch op := rng.Intn(20); {
		case op < 11:
			id := rng.Intn(60) + 1
			if rng.Intn(3) == 0 {
				id = 60 + step // past every rowid so far
			}
			err := n.Exec(fmt.Sprintf(`INSERT INTO t VALUES(%d, %d)`, id, step))
			if have[id] != (err != nil) {
				t.Fatalf("step %d: INSERT %d: err %v, model has it: %v", step, id, err, have[id])
			}
			have[id] = true
		case op < 14:
			id := rng.Intn(70) + 1
			if err := n.Exec(fmt.Sprintf(`DELETE FROM t WHERE id = %d`, id)); err != nil {
				t.Fatal(err)
			}
			delete(have, id)
		case op == 14 && rng.Intn(4) == 0:
			if err := n.Exec(`DELETE FROM t`); err != nil {
				t.Fatal(err)
			}
			clear(have)
		case op == 15 && !inTx:
			if err := n.Exec(`BEGIN`); err != nil {
				t.Fatal(err)
			}
			inTx, txStart = true, maps.Clone(have)
		case op == 16 && inTx:
			if err := n.Exec(fmt.Sprintf(`SAVEPOINT s%d`, len(saves))); err != nil {
				t.Fatal(err)
			}
			saves = append(saves, maps.Clone(have))
		case op == 17 && len(saves) > 0:
			k := rng.Intn(len(saves))
			if err := n.Exec(fmt.Sprintf(`ROLLBACK TO s%d`, k)); err != nil {
				t.Fatal(err)
			}
			have = maps.Clone(saves[k])
			saves = saves[:k+1]
		case op == 18 && inTx:
			if rng.Intn(2) == 0 {
				if err := n.Exec(`COMMIT`); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := n.Exec(`ROLLBACK`); err != nil {
					t.Fatal(err)
				}
				have = txStart
			}
			inTx, saves = false, nil
			if _, err := n.Commit(); err != nil {
				t.Fatal(err)
			}
		default:
			if !inTx {
				if _, err := n.Commit(); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
