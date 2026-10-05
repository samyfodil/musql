// Tests name context restrictions on LIMIT/OFFSET expressions.
package engine

import (
	"errors"
	"strings"
	"testing"
)

// TestLimitOffsetRejectsAggregateAndWindow tests error messages for invalid functions in LIMIT/OFFSET.
func TestLimitOffsetRejectsAggregateAndWindow(t *testing.T) {
	for _, c := range []struct {
		sql  string
		want string // the message the oracle uses, minus its "misuse of " prefix
	}{
		// A bare aggregate: is_agg && NC_AllowAgg==0, the first disjunct.
		{"count(*)", "aggregate function count()"},
		{"sum(1)", "aggregate function sum()"},
		{"avg(1)", "aggregate function avg()"},
		{"total(1)", "aggregate function total()"},
		{"max(1)", "aggregate function max()"},
		{"group_concat(1)", "aggregate function group_concat()"},
		{"json_group_array(1)", "aggregate function json_group_array()"},
		// With FILTER clause.
		{"count(*) FILTER (WHERE 1)", "aggregate function count()"},
		// Window functions with OVER clause.
		{"row_number() OVER ()", "window function row_number()"},
		{"ntile(2) OVER ()", "window function ntile()"},
		{"lag(1) OVER ()", "window function lag()"},
		{"first_value(1) OVER ()", "window function first_value()"},
		{"nth_value(1,1) OVER ()", "window function nth_value()"},
		{"rank() OVER ()", "window function rank()"},
		// Aggregates with OVER clause.
		{"sum(1) OVER ()", "window function sum()"},
		// Built-in window functions without OVER.
		{"row_number()", "window function row_number()"},
		{"rank()", "window function rank()"},
		// Nested in function calls and CASE expressions.
		{"abs(count(*))", "aggregate function count()"},
		{"CASE WHEN 1 THEN count(*) ELSE 2 END", "aggregate function count()"},
		{"abs(row_number() OVER ())", "window function row_number()"},
	} {
		e, perr := parseCheckExprText(c.sql)
		if perr != nil {
			t.Fatalf("%s: parse: %v", c.sql, perr)
		}
		err := requireZeroedNameContext(e)
		if err == nil {
			t.Errorf("LIMIT %s was ACCEPTED; 3.53.3 raises \"misuse of %s\"", c.sql, c.want)
			continue
		}
		if !strings.Contains(err.Error(), "misuse of "+c.want) {
			t.Errorf("LIMIT %s: got %q, want it to name \"misuse of %s\"", c.sql, err, c.want)
		}
		// errVDBESemantic, not errVDBEUnsupported: C SQLite raises this
		// too, so it must PROPAGATE rather than route the statement to a
		// second evaluator that would answer it again. That distinction is
		// what makes the UPSERT half of this bug stop WRITING.
		if !errors.Is(err, errVDBESemantic) {
			t.Errorf("LIMIT %s: error is not errVDBESemantic (%v); a decline would let the write path fall back", c.sql, err)
		}
		// And the whole seam must refuse it, not just the checker.
		if _, ferr := foldLimitOffsetExpr(nil, e, nil); ferr == nil {
			t.Errorf("foldLimitOffsetExpr answered LIMIT %s; the position rejects it", c.sql)
		}
	}
}

// TestLimitOffsetAcceptsNonAggregateCalls is the other half of the gate above:
// without it, every assertion there could be satisfied by refusing all function
// calls in the clause, which is not what 3.53.3 does. Each of these ANSWERS on
// the oracle.
func TestLimitOffsetAcceptsNonAggregateCalls(t *testing.T) {
	for _, sql := range []string{
		"abs(-2)",
		"1+1",
		// 2-or-more-argument min/max is SQLite's ordinary SCALAR min/max, not
		// an aggregate: "... LIMIT max(1,2)" returns two rows on the oracle.
		"max(1,2)",
		"min(1,2)",
		// A SUBQUERY is its own resolution scope with its own NameContext, so
		// an aggregate (or a window query) inside one is legal here.
		"(SELECT count(*) FROM u)",
		"1+(SELECT count(*) FROM u)",
		"(SELECT row_number() OVER () FROM u LIMIT 1)",
	} {
		e, perr := parseCheckExprText(sql)
		if perr != nil {
			t.Fatalf("%s: parse: %v", sql, perr)
		}
		if err := requireZeroedNameContext(e); err != nil {
			t.Errorf("LIMIT %s was REJECTED (%v); 3.53.3 accepts it", sql, err)
		}
	}
}

// TestLimitOffsetPseudoRowWhitelist pins the ASYMMETRY between lookupName's two
// pseudo-row arms, which is the whole content of limitOffsetPseudoRowRefs
// (query.go): the TRIGGER arm is gated on pParse->pTriggerTab (resolve.c:525),
// which resolve.c:1903's memset cannot clear, so new./old. still resolve inside
// a LIMIT -- while the UPSERT arm is gated on the NameContext's own NC_UUpsert
// (resolve.c:547), which the memset DOES clear, so excluded. must not.
//
// Getting this backwards overwrote committed rows; see the doc comment on
// limitOffsetPseudoRowRefs and compat-harness/limit_offset_name_context_test.go.
func TestLimitOffsetPseudoRowWhitelist(t *testing.T) {
	scope := func(name string, hidden bool, off int) tableScope {
		return tableScope{
			name:              name,
			cols:              []columnInfo{{Name: "a"}},
			colIndex:          map[string]int{"a": 0},
			offset:            off,
			unqualifiedHidden: hidden,
		}
	}
	// A trigger firing an UPSERT: "new", "old" and "excluded" are all in reach,
	// plus an ordinary table scope "t".
	ctx := &evalCtx{
		tables: []tableScope{
			scope("old", true, 0), scope("new", true, 1),
			scope("excluded", true, 2), scope("t", false, 3),
		},
		vals: []Value{{Typ: Int, I: 1}, {Typ: Int, I: 2}, {Typ: Int, I: 3}, {Typ: Int, I: 4}},
	}
	substituted := func(qual string) bool {
		out := limitOffsetPseudoRowRefs(ColumnExpr{Qualifier: qual, Name: "a"}, ctx)
		_, stillAColumn := out.(ColumnExpr)
		return !stillAColumn
	}
	for _, qual := range []string{"new", "old", "NEW", "Old"} {
		if !substituted(qual) {
			t.Errorf("LIMIT %s.a was not resolved; resolve.c:525's arm is gated on "+
				"pParse->pTriggerTab, which the LIMIT clause's memset cannot clear", qual)
		}
	}
	for _, qual := range []string{"excluded", "EXCLUDED", "t"} {
		if substituted(qual) {
			t.Errorf("LIMIT %s.a RESOLVED; only a trigger pseudo-row may, and an "+
				"UPSERT's excluded. is gated on NC_UUpsert (resolve.c:547) which "+
				"resolve.c:1903's memset clears", qual)
		}
	}

	// The unqualifiedHidden half of the whitelist is load-bearing on its own: a
	// REAL TABLE named "new" is an ordinary scope, and 3.53.3 answers "no such
	// column: new.a" for "UPDATE new SET a=9 WHERE EXISTS(SELECT 1 FROM u LIMIT
	// new.a)" because its arm needs pParse->pTriggerTab.
	plain := &evalCtx{tables: []tableScope{scope("new", false, 0)}, vals: []Value{{Typ: Int, I: 1}}}
	out := limitOffsetPseudoRowRefs(ColumnExpr{Qualifier: "new", Name: "a"}, plain)
	if _, stillAColumn := out.(ColumnExpr); !stillAColumn {
		t.Error("LIMIT new.a resolved against a REAL TABLE named \"new\"; outside a " +
			"trigger that arm never fires and the name is \"no such column\"")
	}
}
