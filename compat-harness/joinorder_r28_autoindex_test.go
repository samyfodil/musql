package compat

// Automatic-index keys for two-table equi-joins over unindexed tables.
// The key comprises constrained columns (in WHERE-term order, each under its
// comparison's collating sequence), then unreferenced columns (table order, BINARY),
// then rowid. Varies column count, joined-column position, collations, and references.
//	   column there is BINARY either way -- which is exactly what a key that
//	   wrongly used the COLUMN's declared collation instead of the COMPARISON's
//	   would also produce.
//	4. Column count above 3, so extraCols never has to be ordered.
//
// So this battery varies those and only those, and keeps r27's own hard-won
// fixture rule: the observer values are a random PERMUTATION of 0..nRow-1, never
// ascending with rowid, because an ascending tag makes auto-index order and
// rowid order byte-identical and hides the whole thing.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r28Colls are the collations a key column can be DECLARED with. RTRIM is in
// here because it, unlike NOCASE, orders 'a ' and 'a' as equal while BINARY does
// not, so it separates a key built under the comparison's collation from one
// built under BINARY in a second, independent way.
var r28Colls = []string{"", " COLLATE NOCASE", " COLLATE RTRIM"}

// r28Keys is the value pool for a constrained column: small and heavily
// repeated, so several inner rows share every key and their relative order is
// what the query reports. The case and trailing-space variants are what make a
// NOCASE/RTRIM key column order differently from a BINARY one.
var r28Keys = []string{"'a'", "'A'", "'a '", "'b'", "1", "'1'", "NULL"}

func TestR28AutoIndexKey(t *testing.T) {
	rng := rand.New(rand.NewSource(28))
	const nCase = 240
	const batch = 20

	type gen struct {
		setup []string
		query string
		shape string
	}
	cases := make([]gen, nCase)
	for id := range cases {
		sfx := strconv.Itoa(id)
		ta, tb := "ka"+sfx, "kb"+sfx

		// The inner table (tb) is the one an automatic index gets built over.
		// nEq of its columns are constrained; the rest may or may not be
		// referenced, which is the colUsed half of the key.
		nCol := 3 + rng.Intn(4) // 3..6
		nEq := 1 + rng.Intn(3)  // 1..3 equalities
		if nEq > nCol-1 {
			nEq = nCol - 1
		}
		// Which of tb's columns are constrained, and in which WHERE-term ORDER
		// (deliberately not ascending: the key takes term order, not table
		// order, for its constrained prefix).
		perm := rng.Perm(nCol)
		eqCols := perm[:nEq]
		// Which of the REMAINING columns the query references. A random subset,
		// including the empty one -- with no extraCols the key is just
		// (constrained..., rowid) and the visit order collapses back to rowid
		// order, which is the case that proves the reorder is not unconditional.
		var refCols []int
		for _, c := range perm[nEq:] {
			if rng.Intn(2) == 0 {
				refCols = append(refCols, c)
			}
		}
		sort.Ints(refCols) // named in table order in the select list; the KEY's
		// own order must come from the table, not from here -- see the
		// deliberately reversed spelling below.
		if len(refCols) > 1 && rng.Intn(2) == 0 {
			for i, j := 0, len(refCols)-1; i < j; i, j = i+1, j-1 {
				refCols[i], refCols[j] = refCols[j], refCols[i]
			}
		}

		colls := make([]string, nCol)
		for _, c := range eqCols {
			colls[c] = r28Colls[rng.Intn(len(r28Colls))]
		}
		aCols := make([]string, nCol)
		bCols := make([]string, nCol)
		for i := 0; i < nCol; i++ {
			aCols[i] = "x" + strconv.Itoa(i+1)
			bCols[i] = "y" + strconv.Itoa(i+1) + colls[i]
		}
		setup := []string{
			"CREATE TABLE " + ta + "(" + strings.Join(aCols, ",") + ")",
			"CREATE TABLE " + tb + "(" + strings.Join(bCols, ",") + ")",
		}

		// Rows. The outer table is small (2-3 rows) so the output stays readable;
		// the inner one is wide enough that several rows share a key.
		emit := func(name, p string, nRow int, keyAt []int) {
			tag := rng.Perm(nRow)
			rows := make([]string, nRow)
			for r := 0; r < nRow; r++ {
				vals := make([]string, nCol)
				for c := range vals {
					vals[c] = "'" + p + strconv.Itoa(tag[r]) + "_" + strconv.Itoa(c) + "'"
				}
				for _, c := range keyAt {
					vals[c] = r28Keys[rng.Intn(len(r28Keys))]
				}
				rows[r] = "(" + strings.Join(vals, ",") + ")"
			}
			setup = append(setup, "INSERT INTO "+name+" VALUES"+strings.Join(rows, ","))
		}
		emit(ta, "a", 2+rng.Intn(2), eqCols)
		emit(tb, "b", 5+rng.Intn(4), eqCols)

		// The equalities, in a deliberate WHERE-term order, with an explicit
		// COLLATE on either side some of the time: sqlite3ExprCompareCollSeq
		// resolves the collation in the operands' ORIGINAL as-written order (an
		// explicit COLLATE on the left wins, else one on the right, else the
		// left's declared collation, else the right's), and which side of an
		// equality the INNER table sits on is varied here for exactly that
		// reason.
		var preds []string
		for _, c := range eqCols {
			l := ta + ".x" + strconv.Itoa(c+1)
			r := tb + ".y" + strconv.Itoa(c+1)
			switch rng.Intn(6) {
			case 0:
				l += " COLLATE NOCASE"
			case 1:
				r += " COLLATE RTRIM"
			case 2:
				l, r = r, l // the inner table on the LEFT of the comparison
			}
			preds = append(preds, l+"="+r)
		}

		sel := ta + ".x" + strconv.Itoa(eqCols[0]+1)
		for _, c := range refCols {
			sel += "||'/'||coalesce(" + tb + ".y" + strconv.Itoa(c+1) + ",'-')"
		}
		cases[id] = gen{
			setup: setup,
			query: "SELECT group_concat(" + sel + ") FROM " + ta + "," + tb +
				" WHERE " + strings.Join(preds, " AND "),
			shape: fmt.Sprintf("nCol=%d nEq=%d nRef=%d", nCol, nEq, len(refCols)),
		}
	}

	diverged := 0
	for start := 0; start < nCase; start += batch {
		end := start + batch
		if end > nCase {
			end = nCase
		}
		var stmts []string
		qAt := make([]int, 0, batch)
		for _, c := range cases[start:end] {
			stmts = append(stmts, c.setup...)
			qAt = append(qAt, len(stmts))
			stmts = append(stmts, c.query)
		}
		cgo := run(t, "cgo", stmts)
		mush := run(t, "musql", stmts)
		for k, c := range cases[start:end] {
			cb, _ := json.Marshal(cgo[qAt[k]])
			mb, _ := json.Marshal(mush[qAt[k]])
			if string(cb) != string(mb) {
				diverged++
				if diverged <= 5 {
					t.Errorf("automatic-index key DIVERGES (%s)\n  %s\n  %s\n  cgo:    %s\n  musql: %s",
						c.shape, strings.Join(c.setup, "; "), c.query, cb, mb)
				}
			}
		}
	}
	if diverged > 0 {
		t.Errorf("automatic-index key: %d/%d shapes diverge", diverged, nCase)
	}
}

// TestR28AutoIndexKeyRules pins, one statement per rule, each clause of
// constructAutomaticIndex's key that a randomized battery can only cover
// statistically. Every expected answer below was read out of the C first and
// then confirmed against the oracle; the comment on each says which clause it
// is. When one of these fails it names the rule, which the battery above cannot.
func TestR28AutoIndexKeyRules(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// extraCols = colUsed & ~idxCols: b.o is NEVER referenced, so colUsed is
		// just {k}, extraCols is empty, and the key is (k, rowid) -- the inner
		// rows come out in ROWID order even though they are not in o order.
		{"colUsed-empty", []string{
			"CREATE TABLE a1(k)", "CREATE TABLE b1(k, o)",
			"INSERT INTO a1 VALUES(1)",
			"INSERT INTO b1 VALUES(1,'z'),(1,'m'),(1,'a')",
			"SELECT group_concat(a1.k) FROM a1, b1 WHERE a1.k=b1.k",
		}},
		// The same shape with b.o referenced: colUsed is {k,o}, so the key is
		// (k, o, rowid) and the rows come out in o order instead.
		{"colUsed-one", []string{
			"CREATE TABLE a2(k)", "CREATE TABLE b2(k, o)",
			"INSERT INTO a2 VALUES(1)",
			"INSERT INTO b2 VALUES(1,'z'),(1,'m'),(1,'a')",
			"SELECT group_concat(b2.o) FROM a2, b2 WHERE a2.k=b2.k",
		}},
		// An INTEGER PRIMARY KEY reference sets NO colUsed bit: lookupName
		// rewrites it to iColumn -1 (resolve.c), and the accumulation is guarded
		// by "if( pExpr->iColumn>=0 )". So naming b.id here leaves extraCols
		// empty and the key is (k, rowid) -- rowid order, NOT id order.
		{"colUsed-ipk-sets-no-bit", []string{
			"CREATE TABLE a3(k)", "CREATE TABLE b3(id INTEGER PRIMARY KEY, k)",
			"INSERT INTO a3 VALUES(1)",
			"INSERT INTO b3 VALUES(30,1),(10,1),(20,1)",
			"SELECT group_concat(b3.id) FROM a3, b3 WHERE a3.k=b3.k",
		}},
		// extraCols is walked in TABLE order (for i=0..mxBitCol), never in the
		// order the query named them.
		{"extraCols-table-order", []string{
			"CREATE TABLE a4(k)", "CREATE TABLE b4(k, o1, o2)",
			"INSERT INTO a4 VALUES(1)",
			"INSERT INTO b4 VALUES(1,'b','2'),(1,'b','1'),(1,'a','9')",
			"SELECT group_concat(b4.o2||b4.o1) FROM a4, b4 WHERE a4.k=b4.k",
		}},
		// nKeyCol is re-derived over ALL terms that can drive the index, not just
		// the one whereLoopAddBtree priced the loop with, so two equalities give
		// a two-column key prefix in WHERE-TERM order.
		{"two-equalities", []string{
			"CREATE TABLE a5(k, j)", "CREATE TABLE b5(j, k, o)",
			"INSERT INTO a5 VALUES(1,5)",
			"INSERT INTO b5 VALUES(5,1,'z'),(5,1,'m'),(5,1,'a')",
			"SELECT group_concat(b5.o) FROM a5, b5 WHERE a5.k=b5.k AND a5.j=b5.j",
		}},
		// azColl[n] is sqlite3ExprCompareCollSeq(pParse, pX) -- the COMPARISON's
		// collating sequence, not the column's declared one. b6.k is BINARY, so
		// a key built from the column would order 'X' before 'x'; the comparison
		// is NOCASE, which makes them equal and lets o decide.
		{"key-collation-is-the-comparison's", []string{
			"CREATE TABLE a6(k)", "CREATE TABLE b6(k, o)",
			"INSERT INTO a6 VALUES('x')",
			"INSERT INTO b6 VALUES('X','2'),('x','3'),('X','1')",
			"SELECT group_concat(b6.k||b6.o) FROM a6, b6 WHERE a6.k=b6.k COLLATE NOCASE",
		}},
		// The mirror image: no COLLATE is written, but the inner column DECLARES
		// NOCASE, and sqlite3BinaryCompareCollSeq falls through to the declared
		// collation of the left operand and then the right. So the key column is
		// NOCASE here too, for a different reason.
		{"key-collation-declared", []string{
			"CREATE TABLE a7(k)", "CREATE TABLE b7(k TEXT COLLATE NOCASE, o)",
			"INSERT INTO a7 VALUES('x')",
			"INSERT INTO b7 VALUES('X','2'),('x','3'),('X','1')",
			"SELECT group_concat(b7.k||b7.o) FROM a7, b7 WHERE a7.k=b7.k",
		}},
		// wherePathSolver's "do not build an automatic index for a loop expected
		// to run less than 1.25 times" keeps the index out of the OUTERMOST loop
		// (the seed path's nRow is 0), so the outer table is always a plain scan
		// in rowid order however its columns are used.
		{"outer-loop-never-auto-indexed", []string{
			"CREATE TABLE a8(k, o)", "CREATE TABLE b8(k)",
			"INSERT INTO a8 VALUES(1,'z'),(1,'m'),(1,'a')",
			"INSERT INTO b8 VALUES(1)",
			"SELECT group_concat(a8.o) FROM a8, b8 WHERE a8.k=b8.k",
		}},
	}
	for _, c := range cases {
		cgo := run(t, "cgo", c.stmts)
		mush := run(t, "musql", c.stmts)
		cb, _ := json.Marshal(cgo[len(c.stmts)-1])
		mb, _ := json.Marshal(mush[len(c.stmts)-1])
		if string(cb) != string(mb) {
			t.Errorf("automatic-index key rule %q DIVERGES\n  %s\n  cgo:    %s\n  musql: %s",
				c.name, strings.Join(c.stmts, "; "), cb, mb)
		}
	}
}

// TestR28IpkSeekStillDeclined records the one loop-order shape the port
// DELIBERATELY declines, so that closing it trips this test as an upgrade rather
// than passing unnoticed.
//
// whereLoopAddBtree (where.c) builds a FAKE Index for every rowid table's own
// rowid -- `sPk`, idxType SQLITE_IDXTYPE_IPK -- and hands it to
// whereLoopAddBtreeIndex, which prices "rowid = <expr>" as a one-row seek and a
// rowid range as a bounded scan. whereLoopAddBtreeIndex is not ported, so those
// loops do not exist in engine/where_plan.go and the solver would choose an
// order SQLite never would. markWherePlanEligibility therefore declines any FROM
// clause whose WHERE or ON mentions a rowid (wherePlanRowidConstrained), and the
// engine falls back to its own pre-port ordering rule -- which is what still
// diverges below.
//
// The count, not the individual answers, is the assertion: each of these has a
// single right answer that only a port of whereLoopAddBtreeIndex can produce.
func TestR28IpkSeekStillDeclined(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"ipk-equality", []string{
			"CREATE TABLE p1(id INTEGER PRIMARY KEY, v)", "CREATE TABLE q1(w, x)",
			"INSERT INTO p1 VALUES(3,'p2'),(1,'p0'),(2,'p1')",
			"INSERT INTO q1 VALUES(2,'q1'),(3,'q2'),(1,'q0')",
			"SELECT group_concat(p1.v||'-'||q1.x) FROM p1, q1 WHERE p1.id = q1.w",
		}},
		{"rowid-equality", []string{
			"CREATE TABLE p2(v)", "CREATE TABLE q2(w, x)",
			"INSERT INTO p2 VALUES('p0'),('p1'),('p2')",
			"INSERT INTO q2 VALUES(2,'q1'),(3,'q2'),(1,'q0')",
			"SELECT group_concat(p2.v||'-'||q2.x) FROM p2, q2 WHERE p2.rowid = q2.w",
		}},
		{"rowid-range", []string{
			"CREATE TABLE p3(v)", "CREATE TABLE q3(w, x)",
			"INSERT INTO p3 VALUES('p0'),('p1'),('p2')",
			"INSERT INTO q3 VALUES(2,'q1'),(3,'q2'),(1,'q0')",
			"SELECT group_concat(p3.v||'-'||q3.x) FROM p3, q3 WHERE p3.rowid > q3.w",
		}},
	}
	diverged := 0
	for _, c := range cases {
		cgo := run(t, "cgo", c.stmts)
		mush := run(t, "musql", c.stmts)
		cb, _ := json.Marshal(cgo[len(c.stmts)-1])
		mb, _ := json.Marshal(mush[len(c.stmts)-1])
		if string(cb) != string(mb) {
			diverged++
			t.Logf("R28 sPk gap still open (%s)\n  cgo:    %s\n  musql: %s", c.name, cb, mb)
			continue
		}
		// Row SETS must agree even while the order does not: this shape is an
		// ORDER gap, and anything more than that is a real bug.
		t.Logf("R28 sPk gap CLOSED for %s -- whereLoopAddBtreeIndex may now be "+
			"ported; re-check wherePlanRowidConstrained's decline", c.name)
	}
	// The gap is CLOSED: whereLoopAddBtreeIndex is ported into the multi-table
	// planner, the sPk loops it was waiting on are built, and
	// wherePlanRowidConstrained is deleted. This is now a regression pin, not a
	// known hole -- every one of these must keep agreeing.
	if diverged != 0 {
		t.Errorf("sPk (rowid/IPK) loop order: %d of 3 shapes diverge, expected 0. "+
			"These were closed by the multi-table whereLoopAddBtreeIndex port; a "+
			"divergence here is a REGRESSION.", diverged)
	}
}
