package engine

import "testing"

// TestSegJoinMemoKeepsValuesApart: the join memo answers a value with what
// was put for that exact value -- an INTEGER, the REAL equal to it, an
// INTEGER stored as a REAL, a TEXT spelling it and a subtyped INTEGER are
// five keys -- and its check of the last INTEGER answers only that INTEGER.
func TestSegJoinMemoKeepsValuesApart(t *testing.T) {
	keys := []Value{
		{Typ: Int, I: 1},
		{Typ: Float, F: 1},
		{Typ: Int, I: 1, intReal: true},
		{Typ: Text, S: []byte("1")},
		{Typ: Int, I: 1, Subtype: 74},
		{Typ: Int, I: 2},
		{Typ: Int, I: -1},
		{Typ: Null},
	}
	m := newSegJoinMemo[int]()
	for i, k := range keys {
		if _, ok := m.get(k); ok {
			t.Fatalf("key %d (%v) found before it was put", i, k)
		}
		m.put(k, i)
	}
	for _, order := range [][]int{{0, 1, 2, 3, 4, 5, 6, 7}, {5, 0, 5, 6, 0, 0, 4, 0, 2, 5}} {
		for _, i := range order {
			if got, ok := m.get(keys[i]); !ok || got != i {
				t.Errorf("key %d (%v): got %d, %v", i, keys[i], got, ok)
			}
		}
	}
	if _, ok := m.get(Value{Typ: Int, I: 3}); ok {
		t.Error("an INTEGER never put was found")
	}
}
