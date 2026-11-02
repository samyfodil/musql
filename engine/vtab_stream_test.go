package engine

import (
	"errors"
	"testing"
)

// TestVtabStreamOnlyWhereTheLoopRunsOnce: a virtual table streams in the
// outermost loop and as a correlated source, and keeps its materialized rows
// in an inner loop and under a RIGHT JOIN, which rewind and sweep them.
func TestVtabStreamOnlyWhereTheLoopRunsOnce(t *testing.T) {
	p := argRegsFixture(t)
	for _, c := range []struct {
		sql  string
		want []bool // vtabStream of each virtual-table OpOpenDerived, in program order
	}{
		{`SELECT count(*) FROM generate_series(1, 10)`, []bool{true}},
		{`SELECT a.value, b.value FROM generate_series(1, 3) a, generate_series(1, 3) b`, []bool{true, false}},
		{`SELECT k, value FROM t, generate_series(1, t.k)`, []bool{true}},
		{`SELECT k, value FROM t RIGHT JOIN generate_series(1, 3) ON value = k`, []bool{false}},
	} {
		stmt, err := ParseSelect(c.sql)
		if err != nil {
			t.Fatal(err)
		}
		prog, err := compileSelectScanRow(p, stmt, nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		var got []bool
		for _, in := range prog.Insns {
			if ds, ok := in.P4.(*derivedSource); ok && in.Op == OpOpenDerived && ds.vtab != nil {
				got = append(got, ds.vtabStream)
			}
		}
		if len(got) != len(c.want) {
			t.Fatalf("%s: %d virtual-table opens, want %d", c.sql, len(got), len(c.want))
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: open %d streams %v, want %v", c.sql, i, got[i], c.want[i])
			}
		}
	}
}

// stubVtabCursor yields 1..n, failing where told to.
type stubVtabCursor struct {
	n, at                 int64
	filters               int
	failFilter, failNext  bool
	failColumn, failRowid bool
}

func (c *stubVtabCursor) Filter(int, string, []Value) error {
	c.filters++
	c.at = 1
	if c.failFilter && c.filters > 1 {
		return errors.New("filter")
	}
	return nil
}

func (c *stubVtabCursor) Next() error {
	c.at++
	if c.failNext {
		return errors.New("next")
	}
	return nil
}

func (c *stubVtabCursor) Eof() bool { return c.at > c.n }

func (c *stubVtabCursor) Column(int) (Value, error) {
	if c.failColumn {
		return Value{}, errors.New("column")
	}
	return Value{Typ: Int, I: c.at}, nil
}

func (c *stubVtabCursor) Rowid() (int64, error) {
	if c.failRowid {
		return 0, errors.New("rowid")
	}
	return c.at, nil
}

func (c *stubVtabCursor) Close() error { return nil }

func stubStreamCursor(vc *stubVtabCursor) *vdbeCursor {
	return openVtabStreamCursor(&resolvedTable{cols: []columnInfo{{Name: "value"}}}, &vtabStream{
		start: func() (*vtabScan, error) {
			if err := vc.Filter(0, "", nil); err != nil {
				return nil, err
			}
			return &vtabScan{cur: vc}, nil
		},
	})
}

func streamedValues(t *testing.T, cur *vdbeCursor) ([]int64, error) {
	t.Helper()
	if err := cur.rewind(); err != nil {
		return nil, err
	}
	var got []int64
	for cur.advance() {
		if int64(cur.rowid) != cur.rowVals[0].I {
			t.Fatalf("rowid %d for value %d", cur.rowid, cur.rowVals[0].I)
		}
		got = append(got, cur.rowVals[0].I)
	}
	return got, cur.streamErrPending
}

// TestVtabStreamRewindsAndErrors: a second rewind filters the cursor again,
// as OP_VFilter does, and a failing Filter, Next, Column or Rowid is the
// scan's error.
func TestVtabStreamRewindsAndErrors(t *testing.T) {
	vc := &stubVtabCursor{n: 3}
	cur := stubStreamCursor(vc)
	for range 2 {
		got, err := streamedValues(t, cur)
		if err != nil || len(got) != 3 || got[0] != 1 || got[2] != 3 {
			t.Fatalf("scan %v %v", got, err)
		}
	}
	if vc.filters != 2 {
		t.Fatalf("filtered %d times, want 2", vc.filters)
	}

	for _, vc := range []*stubVtabCursor{
		{n: 3, failNext: true}, {n: 3, failColumn: true}, {n: 3, failRowid: true},
	} {
		if _, err := streamedValues(t, stubStreamCursor(vc)); err == nil {
			t.Errorf("%+v: no error", *vc)
		}
	}
	vc = &stubVtabCursor{n: 3, failFilter: true}
	cur = stubStreamCursor(vc)
	if _, err := streamedValues(t, cur); err != nil {
		t.Fatal(err)
	}
	if _, err := streamedValues(t, cur); err == nil {
		t.Error("a failing second filter: no error")
	}
	cur = openVtabStreamCursor(&resolvedTable{cols: []columnInfo{{Name: "value"}}}, &vtabStream{
		start: func() (*vtabScan, error) { return nil, errors.New("start") },
	})
	if _, err := streamedValues(t, cur); err == nil {
		t.Error("a failing start: no error")
	}
}
