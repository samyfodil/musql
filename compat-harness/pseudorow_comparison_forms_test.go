package compat

// Tests pseudo-row affinity across comparison operators: IN, BETWEEN, equality,
// and index-seek key building. Pseudo-rows contribute no affinity to operand coercion.

import "testing"

func TestTriggerPseudoRowAffinityAcrossOperatorForms(t *testing.T) {
	differ(t, "trigger pseudo-row affinity, operator forms: IN a literal list", []string{
		`CREATE TABLE t(a INTEGER)`,
		`CREATE TABLE dst(r)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN` +
			` INSERT INTO dst SELECT CASE WHEN new.a IN ('05') THEN 'hit' ELSE 'miss' END; END`,
		`INSERT INTO t VALUES(5)`,
		`SELECT r FROM dst`,
	})
	differ(t, "trigger pseudo-row affinity, operator forms: IN a subquery over a TEXT column", []string{
		`CREATE TABLE t(a INTEGER)`,
		`CREATE TABLE d(x TEXT)`,
		`CREATE TABLE dst(r)`,
		`INSERT INTO d VALUES('05')`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN` +
			` INSERT INTO dst SELECT CASE WHEN new.a IN (SELECT x FROM d) THEN 'hit' ELSE 'miss' END; END`,
		`INSERT INTO t VALUES(5)`,
		`SELECT r FROM dst`,
	})
	differ(t, "trigger pseudo-row affinity, operator forms: BETWEEN two text literals", []string{
		`CREATE TABLE t(a INTEGER)`,
		`CREATE TABLE dst(r)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN` +
			` INSERT INTO dst SELECT CASE WHEN new.a BETWEEN '04' AND '06' THEN 'hit' ELSE 'miss' END; END`,
		`INSERT INTO t VALUES(5)`,
		`SELECT r FROM dst`,
	})
	// A plain literal comparand, which needs no other column at all: the
	// reference contributes nothing and a literal contributes nothing, so
	// sqlite3CompareAffinity returns SQLITE_AFF_NONE and 5 stays unequal to '05'.
	differ(t, "trigger pseudo-row affinity, operator forms: against a bare text literal", []string{
		`CREATE TABLE t(a INTEGER)`,
		`CREATE TABLE dst(r)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN` +
			` INSERT INTO dst SELECT CASE WHEN new.a='05' THEN 'hit' ELSE 'miss' END; END`,
		`INSERT INTO t VALUES(5)`,
		`SELECT r FROM dst`,
	})
	// The INDEX SEEK, where the affinity decides which rows come back at all:
	// d holds both '05' and '5' and only the exactly-equal '5' may match.
	differ(t, "trigger pseudo-row affinity, operator forms: as an index seek key", []string{
		`CREATE TABLE t(a INTEGER)`,
		`CREATE TABLE d(x TEXT)`,
		`CREATE INDEX dx ON d(x)`,
		`CREATE TABLE dst(r)`,
		`INSERT INTO d VALUES('05'),('5')`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN` +
			` INSERT INTO dst SELECT x FROM d WHERE d.x=new.a; END`,
		`INSERT INTO t VALUES(5)`,
		`SELECT r FROM dst ORDER BY r`,
	})
	// The CONTROL that separates COMPARISON affinity from STORAGE affinity:
	// writing NEW.a into a TEXT column still applies that column's affinity, so
	// the stored value is text. Nothing here touches that path.
	differ(t, "trigger pseudo-row affinity, operator forms: stored into a TEXT column", []string{
		`CREATE TABLE t(a INTEGER)`,
		`CREATE TABLE dst(r TEXT)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO dst VALUES(new.a); END`,
		`INSERT INTO t VALUES(5)`,
		`SELECT r, typeof(r) FROM dst`,
	})
}

// TestUpsertExcludedAffinityAcrossOperatorForms is the same sweep for the
// upsert pseudo-row, which drops the collation too.
func TestUpsertExcludedAffinityAcrossOperatorForms(t *testing.T) {
	differ(t, "excluded affinity, operator forms: IN a literal list", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, n INTEGER, r)`,
		`INSERT INTO u VALUES(1,0,'x')`,
		`INSERT INTO u VALUES(1,5,'y') ON CONFLICT(k) DO UPDATE ` +
			`SET r=CASE WHEN excluded.n IN ('05') THEN 'hit' ELSE 'miss' END`,
		`SELECT k,r FROM u ORDER BY k`,
	})
	differ(t, "excluded affinity, operator forms: BETWEEN two text literals", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, n INTEGER, r)`,
		`INSERT INTO u VALUES(1,0,'x')`,
		`INSERT INTO u VALUES(1,5,'y') ON CONFLICT(k) DO UPDATE ` +
			`SET r=CASE WHEN excluded.n BETWEEN '04' AND '06' THEN 'hit' ELSE 'miss' END`,
		`SELECT k,r FROM u ORDER BY k`,
	})
	// No index-seek case here, unlike the trigger sweep above: a SET subquery
	// with a FROM clause naming "excluded." is a PRE-EXISTING capability gap
	// (this engine declines it on b7d10fc too, where the oracle answers), so
	// the seek path cannot be reached through this pseudo-row today. It is a
	// decline, not a wrong answer, and it is out of this fixture's scope.

	// The storage-affinity control, as above.
	differ(t, "excluded affinity, operator forms: stored into a TEXT column", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, n INTEGER, r TEXT)`,
		`INSERT INTO u VALUES(1,0,'x')`,
		`INSERT INTO u VALUES(1,5,'y') ON CONFLICT(k) DO UPDATE SET r=excluded.n`,
		`SELECT k, r, typeof(r) FROM u ORDER BY k`,
	})
}

// TestUpsertExcludedDropsEvenTheRowidAffinity pins the ONE place the two
// pseudo-rows must NOT be made to match: the rowid.
//
// resolve.c folds an INTEGER PRIMARY KEY column's index to -1 for BOTH of them
// ("if( pTab->iPKey==iCol ) iCol = -1;", resolve.c:562), but only the NEW./OLD.
// arm acts on that -- "if( iCol<0 ){ pExpr->affExpr = SQLITE_AFF_INTEGER; }"
// sits inside the TK_TRIGGER else-branch (resolve.c:601-602). The EXCLUDED arm
// (resolve.c:582-584) returns before it ever gets there and assigns no affinity
// at all, so excluded.<ipk> is typeless exactly like every other excluded
// column.
//
// Concretely: excluded.k is 1 and u.t holds '01'. Typeless-on-the-left means
// the TEXT column governs and 1 renders as '1', which does NOT match '01'.
// Copying trigPseudoRowCols' rowid carve-out into excludedPseudoRowCols would
// make it numeric and answer 'hit' -- a wrong value written to disk. The
// trigger twin of this exact comparison is in
// trigger_pseudorow_affinity_test.go and correctly answers the OTHER way.
func TestUpsertExcludedDropsEvenTheRowidAffinity(t *testing.T) {
	differ(t, "excluded drops even the rowid affinity: INTEGER PRIMARY KEY column", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, t TEXT, r)`,
		`INSERT INTO u VALUES(1,'01','x')`,
		`INSERT INTO u VALUES(1,'zz','y') ON CONFLICT(k) DO UPDATE ` +
			`SET r=CASE WHEN excluded.k=u.t THEN 'hit' ELSE 'miss' END`,
		`SELECT k,r FROM u ORDER BY k`,
	})
	// The "excluded.rowid" SPELLING of the same reference is a KNOWN, STILL-LIVE
	// wrong answer and is deliberately NOT asserted here. Measured on both this
	// tree and b7d10fc with the identical schema above:
	//
	//	SET r=CASE WHEN excluded.rowid=u.t ...   3.53.3 'miss'; this engine 'hit'
	//
	// The cause is a different resolver arm, not this one. "excluded.k" is a
	// named column, so it comes out of the scope's own columnInfo list -- which
	// excludedPseudoRowCols rewrites, so the fix above reaches it. "rowid" is a
	// PSEUDO-column: resolveColumnEx answers it with the package-wide
	// rowidColumnInfo (column_scope.go), whose Aff is affInteger and whose
	// NoAffinity is false, and no per-scope view of it exists to rewrite.
	// Closing it needs a tableScope flag saying "this scope's rowid carries no
	// column identity either", i.e. a new field on tableScope. That is not
	// done here; the divergence is RECORDED rather than absorbed.
}
