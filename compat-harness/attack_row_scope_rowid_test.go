// Tests rowid references in aggregate correlated subqueries.
// Each case uses deliberately unequal rowid and column values to detect wrong slot reads.
package compat

import "testing"

func rowidScopeBase() []string {
	return []string{
		`CREATE TABLE t1(a INTEGER)`,
		`INSERT INTO t1(rowid,a) VALUES(5,10),(9,20)`,
		`CREATE TABLE t2(b INTEGER)`,
		`INSERT INTO t2 VALUES(1),(5),(9),(10),(20)`,
	}
}

// TestAttackAggCorrelatedRowid tests rowid in aggregate correlated subqueries.
func TestAttackAggCorrelatedRowid(t *testing.T) {
	for _, c := range []struct {
		name string
		tail []string
	}{
		{"select-list-scalar-subquery", []string{
			`SELECT a, sum(a), (SELECT count(*) FROM t2 WHERE t2.b = t1.rowid) FROM t1 GROUP BY a ORDER BY a`,
		}},
		{"select-list-oid-spelling", []string{
			`SELECT a, sum(a), (SELECT count(*) FROM t2 WHERE t2.b = t1.oid) FROM t1 GROUP BY a ORDER BY a`,
		}},
		{"select-list-underscore-spelling", []string{
			`SELECT a, sum(a), (SELECT count(*) FROM t2 WHERE t2.b = t1._rowid_) FROM t1 GROUP BY a ORDER BY a`,
		}},
		{"having-exists-rowid", []string{
			`SELECT a, sum(a) FROM t1 GROUP BY a HAVING EXISTS(SELECT 1 FROM t2 WHERE t2.b = t1.rowid) ORDER BY a`,
		}},
		{"having-in-rowid", []string{
			`SELECT a, sum(a) FROM t1 GROUP BY a HAVING t1.rowid IN (SELECT b FROM t2 WHERE b < 9) ORDER BY a`,
		}},
		{"orderby-correlated-rowid", []string{
			`SELECT a, sum(a) FROM t1 GROUP BY a ORDER BY (SELECT -t1.rowid)`,
		}},
		// aggnested.test 7.1's own shape: the placeholder stands in the RESULT
		// clause of a body that is itself an aggregate query -- the shape whose
		// placeholder has to name its OWNER, because the register-block walk
		// otherwise stops at the INNER query's block.
		{"inner-agg-body-rowid", []string{
			`SELECT sum(a), a FROM t1 GROUP BY a HAVING (SELECT v > t1.rowid FROM (SELECT sum(a) v) x) ORDER BY a`,
		}},
		{"inner-agg-body-rowid-select-list", []string{
			`SELECT a, sum(a), (SELECT v - t1.rowid FROM (SELECT sum(a) v) x) FROM t1 GROUP BY a ORDER BY a`,
		}},
		// A row-value IN, which is evalRowIn's own materializeOuterRefs call.
		{"rowvalue-in-rowid", []string{
			`SELECT a, sum(a) FROM t1 GROUP BY a HAVING (t1.rowid, a) IN (SELECT b, b*2 FROM t2) ORDER BY a`,
		}},
	} {
		differ(t, "attack-rowid-"+c.name, append(rowidScopeBase(), c.tail...))
	}
}

// TestAttackAggRowidShadowing pins resolve.c:562-567's precedence through the
// same arm: a REAL column named rowid/oid/_rowid_ must win, so these cases
// must read the COLUMN and never the row's actual rowid.
func TestAttackAggRowidShadowing(t *testing.T) {
	for _, c := range []struct {
		name string
		tail []string
	}{
		{"real-oid-column-wins", []string{
			`CREATE TABLE sh(oid INTEGER, a INTEGER)`,
			// rowid 7 -> oid 9, rowid 8 -> oid 5: reading the rowid instead of
			// the column swaps which t2 row matches.
			`INSERT INTO sh(rowid,oid,a) VALUES(7,9,10),(8,5,20)`,
			`SELECT a, sum(a), (SELECT count(*) FROM t2 WHERE t2.b = sh.oid) FROM sh GROUP BY a ORDER BY a`,
		}},
		{"real-rowid-column-wins", []string{
			`CREATE TABLE sh2(rowid INTEGER, a INTEGER)`,
			`INSERT INTO sh2 VALUES(9,10),(5,20)`,
			`SELECT a, sum(a), (SELECT count(*) FROM t2 WHERE t2.b = sh2.rowid) FROM sh2 GROUP BY a ORDER BY a`,
		}},
		// INTEGER PRIMARY KEY is the rowid, so both spellings must agree.
		{"ipk-alias-agrees-with-rowid", []string{
			`CREATE TABLE k1(id INTEGER PRIMARY KEY, a INTEGER)`,
			`INSERT INTO k1 VALUES(5,10),(9,20)`,
			`SELECT a, sum(a), (SELECT count(*) FROM t2 WHERE t2.b = k1.id), (SELECT count(*) FROM t2 WHERE t2.b = k1.rowid) FROM k1 GROUP BY a ORDER BY a`,
		}},
		// WITHOUT ROWID has no pseudo-column at all: 3.53.3 raises
		// "no such column: w.rowid" and nothing may answer.
		{"without-rowid-has-none", []string{
			`CREATE TABLE w(k TEXT PRIMARY KEY, a INTEGER) WITHOUT ROWID`,
			`INSERT INTO w VALUES('p',10),('q',20)`,
			`SELECT a, sum(a), (SELECT count(*) FROM t2 WHERE t2.b = w.rowid) FROM w GROUP BY a ORDER BY a`,
		}},
	} {
		differ(t, "attack-rowid-shadow-"+c.name, append(rowidScopeBase(), c.tail...))
	}
}

// TestAttackAggSubstitutedValueMetadata pins what the SUBSTITUTED literal must
// still carry -- the affinity and declared collation rewriteExprOuterRefs
// copies off the resolved columnInfo. Every case compares the substituted
// value against something whose answer CHANGES if either is dropped.
func TestAttackAggSubstitutedValueMetadata(t *testing.T) {
	for _, c := range []struct {
		name string
		tail []string
	}{
		// rowidColumnInfo declares INTEGER affinity. Comparing it to a TEXT
		// column applies numeric affinity to the text side, so '1' and '02'
		// both compare EQUAL to their rowids; a literal with no affinity
		// compares INTEGER < TEXT and answers 0 twice.
		{"rowid-integer-affinity-vs-text", []string{
			`CREATE TABLE ta(x TEXT, a INTEGER)`,
			`INSERT INTO ta(rowid,x,a) VALUES(1,'1',10),(2,'02',20)`,
			`SELECT a, sum(a), (SELECT ta.rowid = ta.x) FROM ta GROUP BY a ORDER BY a`,
		}},
		{"column-integer-affinity-vs-text-literal", []string{
			`CREATE TABLE tb(n INTEGER, a INTEGER)`,
			`INSERT INTO tb VALUES(1,10),(2,20)`,
			`SELECT a, sum(a), (SELECT tb.n = '1.0') FROM tb GROUP BY a ORDER BY a`,
		}},
		// A DECLARED collation survives the substitution.
		{"declared-nocase-collation", []string{
			`CREATE TABLE tc(y TEXT COLLATE NOCASE, a INTEGER)`,
			`INSERT INTO tc VALUES('abc',10),('DEF',20)`,
			`SELECT a, sum(a), (SELECT tc.y = 'ABC'), (SELECT tc.y = 'def') FROM tc GROUP BY a ORDER BY a`,
		}},
		// ... and must NOT outrank an EXPLICIT one on the other operand.
		{"explicit-collation-outranks-declared", []string{
			`CREATE TABLE td(y TEXT COLLATE NOCASE, a INTEGER)`,
			`INSERT INTO td VALUES('abc ',10),('ABC',20)`,
			`SELECT a, sum(a), (SELECT td.y = 'abc' COLLATE RTRIM) FROM td GROUP BY a ORDER BY a`,
		}},
		// A NULL-extended LEFT JOIN row's rowid is NULL, not 0 and not the
		// left row's.
		{"left-join-null-extended-rowid", []string{
			`CREATE TABLE l1(a INTEGER)`,
			`INSERT INTO l1 VALUES(10),(20)`,
			`CREATE TABLE r1(b INTEGER, c INTEGER)`,
			`INSERT INTO r1(rowid,b,c) VALUES(4,10,100)`,
			`SELECT l1.a, sum(l1.a), (SELECT r1.rowid IS NULL), (SELECT r1.rowid) FROM l1 LEFT JOIN r1 ON r1.b=l1.a GROUP BY l1.a ORDER BY l1.a`,
		}},
		// A BLOB storage class must survive the round trip (the substituted
		// literal is compared against a typed column).
		{"blob-storage-class", []string{
			`CREATE TABLE te(z BLOB, a INTEGER)`,
			`INSERT INTO te VALUES(x'3031',10),('01',20)`,
			`SELECT a, sum(a), (SELECT typeof(te.z)), (SELECT te.z = '01') FROM te GROUP BY a ORDER BY a`,
		}},
		// json_quote consumes a SUBTYPE: the substituted value must not carry
		// one where a real column read would not, and must where it would.
		{"json-subtype-through-substitution", []string{
			`CREATE TABLE tf(j TEXT, a INTEGER)`,
			`INSERT INTO tf VALUES('[7]',10),('[8]',20)`,
			`SELECT a, sum(a), (SELECT json_quote(json(tf.j))), (SELECT json_quote(tf.j)) FROM tf GROUP BY a ORDER BY a`,
		}},
	} {
		differ(t, "attack-substval-"+c.name, append(rowidScopeBase(), c.tail...))
	}
}
