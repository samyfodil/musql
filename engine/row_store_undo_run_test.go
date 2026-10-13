package engine

import "testing"

// A bulk load logs a run of consecutive new rowids as one undo entry, but never
// extends an entry a snapshot's offset points past: rolling back to that offset
// must leave the rows before it.
func TestRowStoreUndoRunsStopAtASnapshot(t *testing.T) {
	s := &rowStore{m: map[uint64][]Value{}}
	row := func(i int) []Value { return []Value{{Typ: Int, I: int64(i)}} }
	s.startUndo()
	for i := 1; i <= 3; i++ {
		s.put(uint64(i), row(i))
	}
	mark := s.startUndo()
	for i := 4; i <= 9; i++ {
		s.put(uint64(i), row(i))
	}
	if got := len(s.undo); got != 2 {
		t.Fatalf("undo entries = %d, want one run before the mark and one after", got)
	}
	s.undoTo(mark)
	if got := s.len(); got != 3 {
		t.Fatalf("after rolling back to the mark %d rows remain, want 3", got)
	}
	for i := 1; i <= 3; i++ {
		if _, ok := s.get(uint64(i)); !ok {
			t.Fatalf("row %d lost by the rollback", i)
		}
	}
	s.undoTo(0)
	if got := s.len(); got != 0 {
		t.Fatalf("after rolling back to the start %d rows remain", got)
	}
}
