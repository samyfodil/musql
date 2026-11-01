package engine

import (
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"testing"
)

// TestInListUTF16MatchesChain: a UTF-16 database compares TEXT in its own
// encoding, so its IN lists keep the comparison chain, and answer as the
// chain does for every affinity, collation and NULL.
func TestInListUTF16MatchesChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "u.musq")
	stmts := []string{`PRAGMA encoding = 'UTF-16le'`, `CREATE TABLE t(i INTEGER, s TEXT, n TEXT COLLATE NOCASE, x)`}
	vals := []string{`1`, `2`, `'2'`, `'a'`, `'A'`, `'b '`, `NULL`, `2.0`, `X'01'`}
	for k := range 90 {
		v, w := vals[k%len(vals)], vals[(k*4+1)%len(vals)]
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %s, %s, %s)`, k%7, v, w, v))
	}
	buildDB(t, path, stmts...)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, col := range []string{"i", "s", "n", "x"} {
		for _, list := range []string{`1, 2, 3`, `'a', 'B ', NULL`, `2, '2', 2.0`, `?, ?, ?`} {
			for _, form := range []string{`SELECT rowid FROM t WHERE %s IN (%s)`, `SELECT rowid, %s NOT IN (%s) FROM t`} {
				q := fmt.Sprintf(form, col, list)
				args := []Value{{Typ: Int, I: 2}, {Typ: Text, S: []byte("A")}, {Typ: Null}}
				inListOffForTest = true
				_, want, werr := p.QueryArgs(q, args)
				inListOffForTest = false
				_, got, gerr := p.QueryArgs(q, args)
				if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: got %v %v, want %v %v", q, got, gerr, want, werr)
				}
			}
		}
	}
}

func TestInListRunMembership(t *testing.T) {
	lit := func(v Value) Expr { return LiteralExpr{Val: v} }
	ints := newInListPlan([]Expr{lit(Value{Typ: Int, I: 1}), lit(Value{Typ: Int, I: 2}), lit(Value{Typ: Int, I: 3})}, affNone, "")
	r, ok := newInListRun(ints, nil, UTF8)
	if !ok {
		t.Fatal("no run for a list of integers")
	}
	for _, tc := range []struct {
		x    Value
		want Value
		ok   bool
	}{
		{Value{Typ: Null}, Value{Typ: Null}, true},
		{Value{Typ: Int, I: 2}, boolValue(true), true},
		{Value{Typ: Float, F: 2}, boolValue(true), true},
		{Value{Typ: Text, S: []byte("2")}, boolValue(false), true},
		{Value{Typ: Float, F: math.NaN()}, Value{}, false},
	} {
		got, ok := r.membership(tc.x)
		if ok != tc.ok || ok && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("membership(%+v) = %+v %v, want %+v %v", tc.x, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := newInListRun(ints, nil, UTF16LE); ok {
		t.Error("a UTF-16 database got keys")
	}
	withParam := newInListPlan([]Expr{lit(Value{Typ: Int, I: 1}), lit(Value{Typ: Int, I: 2}), ParamExpr{Index: 1}}, affNone, "")
	if _, ok := newInListRun(withParam, []Value{{Typ: Float, F: math.NaN()}}, UTF8); ok {
		t.Error("a NaN parameter was keyed")
	}
	if r, ok := newInListRun(withParam, []Value{{Typ: Text, S: []byte("x")}}, UTF8); !ok || r.set == nil {
		t.Error("a TEXT parameter did not make a hashed set")
	}
	if _, _, ok := withParam.nativeKeys(); !ok {
		t.Error("integer literals with a parameter have no native keys")
	}
	mixed := newInListPlan([]Expr{lit(Value{Typ: Int, I: 1}), lit(Value{Typ: Text, S: []byte("a")}), ParamExpr{Index: 1}}, affNone, "")
	if _, _, ok := mixed.nativeKeys(); ok {
		t.Error("a TEXT literal compiled native keys")
	}
}

func TestNewInListPlanDeclines(t *testing.T) {
	lit := func(i int64) Expr { return LiteralExpr{Val: Value{Typ: Int, I: i}} }
	defer func(was bool) { inListOffForTest = was }(inListOffForTest)
	inListOffForTest = true
	if newInListPlan([]Expr{lit(1), lit(2), lit(3)}, affNone, "") != nil {
		t.Error("inListOffForTest made a set")
	}
	inListOffForTest = false
	for name, list := range map[string][]Expr{
		"a column":        {lit(1), lit(2), ColumnExpr{Name: "a"}},
		"a NOT":           {lit(1), lit(2), UnaryExpr{Op: "NOT", X: lit(3)}},
		"a NaN":           {lit(1), lit(2), LiteralExpr{Val: Value{Typ: Float, F: math.NaN()}}},
		"a deferred fail": {lit(1), lit(2), LiteralExpr{Val: Value{Typ: Int}, deferredErr: "x"}},
	} {
		if newInListPlan(list, affNone, "") != nil {
			t.Errorf("%s: made a set", name)
		}
	}
	if newInListPlan([]Expr{lit(1), lit(2), lit(3)}, affNone, "custom") != nil {
		t.Error("a custom collation made a set")
	}
}
