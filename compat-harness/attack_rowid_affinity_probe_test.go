// Probe matrix isolating the rowid-affinity divergence found by
// TestAttackAggSubstitutedValueMetadata: which spellings lose the rowid
// pseudo-column's INTEGER affinity, and on which path.
package compat

import "testing"

func rowidAffBase() []string {
	return []string{
		`CREATE TABLE ta(x TEXT, a INTEGER)`,
		`INSERT INTO ta(rowid,x,a) VALUES(1,'1',10),(2,'02',20)`,
	}
}

func TestProbeRowidAffinity(t *testing.T) {
	for _, c := range []struct {
		name string
		tail []string
	}{
		// ---- plain row mode, no aggregate anywhere ----
		{"rowmode-direct", []string{
			`SELECT a, ta.rowid = ta.x FROM ta ORDER BY a`,
		}},
		{"rowmode-correlated-fromless", []string{
			`SELECT a, (SELECT ta.rowid = ta.x) FROM ta ORDER BY a`,
		}},
		{"rowmode-correlated-rowid-vs-textliteral", []string{
			`SELECT a, (SELECT ta.rowid = '1') FROM ta ORDER BY a`,
		}},
		{"rowmode-direct-rowid-vs-textliteral", []string{
			`SELECT a, ta.rowid = '1' FROM ta ORDER BY a`,
		}},
		{"rowmode-correlated-column-vs-column", []string{
			`SELECT a, (SELECT ta.a = ta.x) FROM ta ORDER BY a`,
		}},
		{"rowmode-correlated-oid-spelling", []string{
			`SELECT a, (SELECT ta.oid = ta.x) FROM ta ORDER BY a`,
		}},

		// ---- grouped (the shape that first showed it) ----
		{"grouped-direct", []string{
			`SELECT a, sum(a), ta.rowid = ta.x FROM ta GROUP BY a ORDER BY a`,
		}},
		{"grouped-correlated-fromless", []string{
			`SELECT a, sum(a), (SELECT ta.rowid = ta.x) FROM ta GROUP BY a ORDER BY a`,
		}},
		{"grouped-correlated-rowid-vs-textliteral", []string{
			`SELECT a, sum(a), (SELECT ta.rowid = '1') FROM ta GROUP BY a ORDER BY a`,
		}},
		{"grouped-correlated-column-vs-column", []string{
			`SELECT a, sum(a), (SELECT ta.a = ta.x) FROM ta GROUP BY a ORDER BY a`,
		}},

		// ---- correlated into a real inner FROM (not FROM-less) ----
		{"correlated-inner-from", []string{
			`CREATE TABLE u2(z TEXT)`,
			`INSERT INTO u2 VALUES('1'),('02')`,
			`SELECT a, (SELECT count(*) FROM u2 WHERE u2.z = ta.rowid) FROM ta ORDER BY a`,
		}},
		{"grouped-correlated-inner-from", []string{
			`CREATE TABLE u3(z TEXT)`,
			`INSERT INTO u3 VALUES('1'),('02')`,
			`SELECT a, sum(a), (SELECT count(*) FROM u3 WHERE u3.z = ta.rowid) FROM ta GROUP BY a ORDER BY a`,
		}},

		// ---- INTEGER PRIMARY KEY spelling of the same column ----
		{"ipk-correlated", []string{
			`CREATE TABLE kk(id INTEGER PRIMARY KEY, x TEXT, a INTEGER)`,
			`INSERT INTO kk VALUES(1,'1',10),(2,'02',20)`,
			`SELECT a, (SELECT kk.id = kk.x), (SELECT kk.rowid = kk.x) FROM kk ORDER BY a`,
		}},

		// ---- write paths: does the same loss skip a write? ----
		{"update-where-correlated-rowid", []string{
			`CREATE TABLE mk(x TEXT, mark TEXT)`,
			`INSERT INTO mk(rowid,x,mark) VALUES(1,'1',NULL),(2,'02',NULL)`,
			`UPDATE mk SET mark='HIT' WHERE (SELECT mk.rowid = mk.x)`,
			`SELECT rowid, x, mark FROM mk ORDER BY rowid`,
		}},
		{"delete-where-correlated-rowid", []string{
			`CREATE TABLE dk(x TEXT)`,
			`INSERT INTO dk(rowid,x) VALUES(1,'1'),(2,'02'),(3,'zz')`,
			`DELETE FROM dk WHERE (SELECT dk.rowid = dk.x)`,
			`SELECT rowid, x FROM dk ORDER BY rowid`,
		}},
		{"update-set-correlated-rowid", []string{
			`CREATE TABLE sk(x TEXT, c INTEGER)`,
			`INSERT INTO sk(rowid,x,c) VALUES(1,'1',0),(2,'02',0)`,
			`UPDATE sk SET c = (SELECT sk.rowid = sk.x)`,
			`SELECT rowid, x, c FROM sk ORDER BY rowid`,
		}},
		{"exists-where-correlated-rowid", []string{
			`CREATE TABLE ek(x TEXT, mark TEXT)`,
			`INSERT INTO ek(rowid,x,mark) VALUES(1,'1',NULL),(2,'zz',NULL)`,
			`CREATE TABLE eu(z TEXT)`,
			`INSERT INTO eu VALUES('1'),('2')`,
			`UPDATE ek SET mark='HIT' WHERE EXISTS(SELECT 1 FROM eu WHERE eu.z = ek.rowid)`,
			`SELECT rowid, x, mark FROM ek ORDER BY rowid`,
		}},

		// ---- IN / row-value spellings ----
		{"in-list-correlated-rowid", []string{
			`CREATE TABLE ik(x TEXT)`,
			`INSERT INTO ik(rowid,x) VALUES(1,'1'),(2,'02')`,
			`CREATE TABLE iu(z TEXT)`,
			`INSERT INTO iu VALUES('1'),('02')`,
			`SELECT rowid, (SELECT count(*) FROM iu WHERE iu.z IN (ik.rowid)) FROM ik ORDER BY rowid`,
		}},

		// ---- ORDER BY / GROUP BY of the substituted rowid ----
		{"orderby-correlated-rowid-compare", []string{
			`SELECT a FROM ta ORDER BY (SELECT ta.rowid = ta.x), a`,
		}},
	} {
		differ(t, "probe-rowidaff-"+c.name, append(rowidAffBase(), c.tail...))
	}
}
