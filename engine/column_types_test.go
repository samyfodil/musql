package engine

import (
	"path/filepath"
	"testing"
)

// PlanColumn is the decision the columnar segment writer makes per column
// (docs/format-design.md): which physical type a block is encoded in, and which
// rows go to the exception list. Getting it wrong is not a slow answer, it is a
// wrong one -- a value that does not fit its block and is not in the exception
// list is gone -- so every branch is pinned here.
func TestPlanColumn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		c       ColumnTypeCensus
		want    PhysicalType
		wantExc int64
	}{
		{"all NULL has no type to choose", ColumnTypeCensus{Null: 10}, PhysNull, 0},
		{"pure int", ColumnTypeCensus{Int: 100}, PhysInt64, 0},
		{"pure real", ColumnTypeCensus{Real: 100}, PhysFloat64, 0},
		{"pure text", ColumnTypeCensus{Text: 100}, PhysText, 0},
		{"pure blob", ColumnTypeCensus{Blob: 100}, PhysBlob, 0},
		{"NULLs do not count against the type", ColumnTypeCensus{Null: 900, Int: 100}, PhysInt64, 0},
		{"one stray text in a hundred ints", ColumnTypeCensus{Int: 99, Text: 1}, PhysInt64, 1},
		{"exactly at the threshold stays fixed width", ColumnTypeCensus{Int: 95, Text: 5}, PhysInt64, 5},
		{"past the threshold goes tagged", ColumnTypeCensus{Int: 94, Text: 6}, PhysTagged, 0},
		{"an even split is tagged", ColumnTypeCensus{Int: 50, Text: 50}, PhysTagged, 0},
		{"the majority wins, not the first class", ColumnTypeCensus{Int: 1, Text: 99}, PhysText, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, exc := PlanColumn(tc.c)
			if got != tc.want || exc != tc.wantExc {
				t.Fatalf("got (%v, %d exceptions), want (%v, %d)", got, exc, tc.want, tc.wantExc)
			}
			if w := got.Width(); (got == PhysNull || got == PhysTagged) != (w == 0) {
				t.Fatalf("%v has width %d, which contradicts whether it is fixed width", got, w)
			}
		})
	}
}

// TestCensusColumnTypesCountsWhatIsStored pins the census against a database
// whose every value is known, including the two cases that decide whether the
// numbers it reports can be trusted: a NULL is not a type, and a row written
// before an ALTER TABLE ADD COLUMN stores the column's default (NULL here) --
// on this format ADD COLUMN writes it into every existing row, where C's
// records simply end early.
func TestCensusColumnTypesCountsWhatIsStored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE t(a, b, c)`,
		`INSERT INTO t VALUES(1, 'x', NULL)`,
		`INSERT INTO t VALUES(2, 'y', NULL)`,
		`INSERT INTO t VALUES('three', 2.5, NULL)`,
		`ALTER TABLE t ADD COLUMN d`,
		`INSERT INTO t VALUES(4, 'z', NULL, x'00ff')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rp, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rp.Close()
	census, err := rp.CensusColumnTypes()
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	byCol := map[int]ColumnTypeCensus{}
	for _, c := range census {
		if c.Table == "t" {
			byCol[c.Column] = c
		}
	}
	if got := byCol[0]; got.Int != 3 || got.Text != 1 {
		t.Errorf("column a: got int=%d text=%d, want 3 and 1", got.Int, got.Text)
	}
	if got, exc := PlanColumn(byCol[0]); got != PhysTagged {
		t.Errorf("column a is 3 ints and a text, which is past the threshold: got %v (%d exceptions)", got, exc)
	}
	if got := byCol[1]; got.Text != 3 || got.Real != 1 {
		t.Errorf("column b: got text=%d real=%d, want 3 and 1", got.Text, got.Real)
	}
	if got := byCol[2]; got.Null != 4 || got.Typed() != 0 {
		t.Errorf("column c is NULL in every row: got null=%d typed=%d", got.Null, got.Typed())
	}
	if got, _ := PlanColumn(byCol[2]); got != PhysNull {
		t.Errorf("an all-NULL column has no physical type: got %v", got)
	}
	// d was added by ALTER after three rows were written: those three store its
	// default, NULL, and only the fourth stores a value.
	if got := byCol[3]; got.Blob != 1 || got.Null != 3 || got.Typed() != 1 {
		t.Errorf("column d: got blob=%d null=%d typed=%d, want 1, 3 and 1", got.Blob, got.Null, got.Typed())
	}
}
