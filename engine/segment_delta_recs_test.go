package engine

import (
	"encoding/binary"
	"math"
	"math/rand"
	"reflect"
	"testing"
)

// TestApplyRecsMatchesApplyBatch holds segDeltaState.applyRecs to applyBatch
// over the batch the same records encode to: every value type, empty text and
// blobs, short rows, kills, a rowid put twice.
func TestApplyRecsMatchesApplyBatch(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	val := func() Value {
		switch rng.Intn(7) {
		case 0:
			return Value{Typ: Null}
		case 1:
			return Value{Typ: Int, I: rng.Int63() - rng.Int63()}
		case 2:
			return Value{Typ: Float, F: math.Float64frombits(rng.Uint64())}
		case 3:
			return Value{Typ: Float, F: float64(rng.Intn(100))}
		case 4:
			return Value{Typ: Text, S: []byte("t" + string(rune('a'+rng.Intn(26))))}
		case 5:
			return Value{Typ: Text} // empty, nil bytes
		default:
			b := make([]byte, rng.Intn(4))
			rng.Read(b)
			return Value{Typ: Blob, S: b}
		}
	}
	var recs []SegDeltaRecord
	var batch []byte
	for i := 0; i < 2000; i++ {
		r := SegDeltaRecord{Table: rng.Intn(3), Rowid: int64(rng.Intn(500)) - 50, Kill: rng.Intn(6) == 0}
		kind := byte(segDeltaPut)
		if r.Kill {
			kind = segDeltaKill
		} else {
			r.Vals = make([]Value, rng.Intn(5))
			for c := range r.Vals {
				r.Vals[c] = val()
			}
		}
		recs = append(recs, r)
		batch = append(batch, kind)
		batch = binary.LittleEndian.AppendUint32(batch, uint32(r.Table))
		batch = binary.LittleEndian.AppendUint64(batch, uint64(r.Rowid))
		if !r.Kill {
			rec := appendRecord(nil, r.Vals)
			batch = binary.LittleEndian.AppendUint32(batch, uint32(len(rec)))
			batch = append(batch, rec...)
		}
	}
	fresh := func() *segDeltaState {
		return &segDeltaState{live: map[int]map[int64][]Value{}, dead: map[int]map[int64]bool{}}
	}
	a, b := fresh(), fresh()
	if !a.applyRecs(recs) {
		t.Fatal("applyRecs refused records that all carry their values")
	}
	if err := b.applyBatch(batch); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.dead, b.dead) || len(a.live) != len(b.live) {
		t.Fatal("applyRecs and applyBatch left different deltas")
	}
	// Row by row, a float by its bits (a NaN is not DeepEqual to itself).
	for ti, rows := range a.live {
		if len(rows) != len(b.live[ti]) {
			t.Fatalf("table %d: %d rows, applyBatch %d", ti, len(rows), len(b.live[ti]))
		}
		for rid, got := range rows {
			want := b.live[ti][rid]
			if len(got) != len(want) {
				t.Fatalf("table %d row %d: %v, applyBatch %v", ti, rid, got, want)
			}
			for c := range got {
				g, w := got[c], want[c]
				if g.Typ == Float && w.Typ == Float && math.Float64bits(g.F) == math.Float64bits(w.F) {
					continue
				}
				if !reflect.DeepEqual(g, w) {
					t.Fatalf("table %d row %d col %d: %#v, applyBatch %#v", ti, rid, c, g, w)
				}
			}
		}
	}
}
