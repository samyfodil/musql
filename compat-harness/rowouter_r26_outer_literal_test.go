package compat

// Regression gate for correlated columns substituted as literals:
// the substituted LiteralExpr now carries the column's affinity and collation,
// and aggregate calls whose arguments name no tables reject the substitution.

import (
	"encoding/json"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// olSchema deliberately mixes a TEXT column holding a numeric string with an
// INTEGER column holding the same number: that is the pair sqlite3CompareAffinity
// resolves to NUMERIC when BOTH sides are columns and to TEXT when one side is a
// literal, which is the whole divergence.
var olSchema = []string{
	"CREATE TABLE tt(a TEXT)",
	"INSERT INTO tt VALUES('1.0')",
	"CREATE TABLE uu(b INTEGER, mark)",
	"INSERT INTO uu VALUES(1,'x')",
	"CREATE TABLE t2(a INTEGER, b INTEGER)",
	"INSERT INTO t2 VALUES(1,10)",
	"CREATE TABLE ot(q INTEGER, k INTEGER)",
	"INSERT INTO ot VALUES(5,1)",
	"CREATE TABLE w(x INTEGER, y INTEGER)",
	"INSERT INTO w VALUES(1,5),(2,6)",
}

var olCases = []struct {
	name  string
	tail  []string
	wrong bool
}{
	// --- AFFINITY, read side. The control above each pair is the SAME
	// comparison where the column is NOT substituted, and it must keep
	// agreeing: that is what proves the divergence is the substitution and not
	// this engine's comparison rules.
	{"aff-control-direct", []string{"SELECT a='1.0' FROM t2"}, false},
	{"aff-control-rowmode", []string{"SELECT a, (SELECT t2.a='1.0') FROM t2"}, false},
	{"aff-control-literals", []string{"SELECT 1='1.0'"}, false},
	{"aff-grouped-qualified", []string{"SELECT a, (SELECT t2.a='1.0') FROM t2 GROUP BY a"}, false},
	{"aff-grouped-where", []string{"SELECT a, (SELECT count(*) WHERE t2.a='1.0') FROM t2 GROUP BY a"}, false},
	{"aff-grouped-text-col", []string{"SELECT b, (SELECT t2.b='10.0') FROM t2 GROUP BY b"}, false},

	// --- AFFINITY, write side: the substitution happens in rowEvalCtx, and the
	// row that should have been marked silently is not.
	{"aff-write-exists", []string{
		"UPDATE uu SET mark='HIT' WHERE EXISTS (SELECT 1 FROM tt WHERE tt.a = uu.b)",
		"SELECT b, mark FROM uu"}, false},
	{"aff-write-scalar", []string{
		"UPDATE uu SET mark='HIT' WHERE (SELECT count(*) FROM tt WHERE tt.a = uu.b)",
		"SELECT b, mark FROM uu"}, false},
	{"aff-write-set", []string{
		"UPDATE uu SET mark=(SELECT group_concat(a) FROM tt WHERE tt.a = uu.b)",
		"SELECT b, mark FROM uu"}, false},
	{"aff-write-compound-arm", []string{
		"UPDATE uu SET mark='HIT' WHERE EXISTS (SELECT a FROM tt WHERE tt.a = uu.b INTERSECT SELECT '1.0')",
		"SELECT b, mark FROM uu"}, false},

	// --- A correlated compound arm still round-trips: the substitution does NOT
	// break the compound path by itself. Kept as the control for the channel
	// that was conjectured here and refuted (see the file comment).
	{"compound-arm-control", []string{
		"SELECT k, (SELECT ot.q UNION SELECT 9 ORDER BY 1 LIMIT 1) FROM ot GROUP BY k"}, false},

	// --- AGGREGATE ASSOCIATION on the WRITE path. sqlite3ReferencesSrcList
	// answers 0 for "sum(w.x)" in the FROM-less subquery and 1 for the UPDATE's
	// own SrcList, so the resolver hands the call to the UPDATE -- which builds
	// no AggInfo, so codegen raises "misuse of aggregate: sum()". Substituting
	// w.x with the row's value turns it into sum(1), which names no table, so
	// the walk never leaves the subquery, an AggInfo IS built there, and the
	// statement compiles and WRITES. The DELETE form removes every row.
	{"assoc-write-update-sum", []string{
		"UPDATE w SET y=(SELECT sum(w.x))", "SELECT x,y FROM w ORDER BY x"}, false},
	{"assoc-write-update-count", []string{
		"UPDATE w SET y=(SELECT count(w.x))", "SELECT x,y FROM w ORDER BY x"}, false},
	{"assoc-write-delete", []string{
		"DELETE FROM w WHERE (SELECT sum(w.x))>0", "SELECT x,y FROM w ORDER BY x"}, false},
	// The UNQUALIFIED spelling of the same statement is never substituted, so
	// it declines and agrees. That is what identifies the substitution as the
	// cause rather than the aggregate handling.
	{"assoc-control-unqualified", []string{
		"UPDATE w SET y=(SELECT sum(x))", "SELECT x,y FROM w ORDER BY x"}, false},
}

func TestR26OuterLiteralLosesColumnProperties(t *testing.T) {
	for _, tc := range olCases {
		t.Run(tc.name, func(t *testing.T) {
			stmts := append(append([]string(nil), olSchema...), tc.tail...)
			oracle, _ := json.Marshal(run(t, "cgo", stmts)[len(stmts)-1])
			got, _ := json.Marshal(run(t, "musql", stmts)[len(stmts)-1])
			switch {
			case string(got) == string(oracle) && tc.wrong:
				t.Fatalf("%v now MATCHES the oracle -- clear its wrong flag\n  both: %s", tc.tail, oracle)
			case string(got) != string(oracle) && !tc.wrong:
				t.Fatalf("%v DIVERGES\n  cgo:    %s\n  musql: %s", tc.tail, oracle, got)
			case tc.wrong:
				t.Logf("KNOWN WRONG (the substituted literal dropped the column's affinity/name): %v\n  cgo:    %s\n  musql: %s", tc.tail, oracle, got)
			}
		})
	}
}
