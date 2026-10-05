// Tests COLLATE modifiers in table-level UNIQUE and PRIMARY KEY constraints.
package compat

import (
	"fmt"
	"testing"
)

// tccProbe inserts the pairs that separate BINARY from NOCASE from RTRIM, one
// statement at a time so a UNIQUE violation stops exactly one of them, and
// reads back what survived.
func tccProbe() []string {
	return append([]string{
		`INSERT INTO t VALUES('X','p')`,
		`INSERT INTO t VALUES('x','q')`,
		`INSERT INTO t VALUES('x  ','r')`,
		`INSERT INTO t VALUES('y','s')`,
		`SELECT quote(a), quote(b) FROM t ORDER BY rowid`,
		`SELECT count(*) FROM t`,
		`SELECT sql FROM sqlite_master WHERE type='table'`,
		`SELECT name FROM sqlite_master WHERE type='index' ORDER BY name`,
	}, tccPragmas()...)
}

// tccPragmas reads the automatic indexes back through the PRAGMAs.
func tccPragmas() []string {
	return []string{
		`SELECT name, origin FROM pragma_index_list('t') ORDER BY name`,
		`SELECT seqno, cid, name, "desc", coll, key FROM pragma_index_xinfo('sqlite_autoindex_t_1')`,
		`SELECT seqno, cid, name, "desc", coll, key FROM pragma_index_xinfo('sqlite_autoindex_t_2')`,
	}
}

func TestTableConstraintCollate(t *testing.T) {
	for _, create := range []string{
		// the defect
		`CREATE TABLE t(a, b, PRIMARY KEY(a COLLATE nocase))`,
		`CREATE TABLE t(a, b, UNIQUE(a COLLATE nocase))`,
		`CREATE TABLE t(a, b, UNIQUE(a COLLATE rtrim))`,
		`CREATE TABLE t(a, b, PRIMARY KEY(a COLLATE binary))`,
		// case-insensitive keyword and collation name
		`CREATE TABLE t(a, b, PRIMARY KEY(a collate NOCASE))`,
		`CREATE TABLE t(a, b, UNIQUE(a COLLATE RTrim))`,
		// COLLATE combined with a sort order, both orders of the two
		`CREATE TABLE t(a, b, UNIQUE(a COLLATE nocase ASC))`,
		`CREATE TABLE t(a, b, UNIQUE(a COLLATE nocase DESC))`,
		// a constraint-level COLLATE OVERRIDING the column's declared one,
		// and the reverse (declared one used when the constraint gives none)
		`CREATE TABLE t(a COLLATE nocase, b, UNIQUE(a COLLATE binary))`,
		`CREATE TABLE t(a COLLATE binary, b, UNIQUE(a COLLATE nocase))`,
		`CREATE TABLE t(a COLLATE nocase, b, UNIQUE(a))`,
		// multi-column, only one of them collated
		`CREATE TABLE t(a, b, UNIQUE(a COLLATE nocase, b))`,
		`CREATE TABLE t(a, b, UNIQUE(b, a COLLATE nocase))`,
		`CREATE TABLE t(a, b, PRIMARY KEY(b, a COLLATE nocase))`,
		// the single-quoted column name (SQLite's backwards compatibility)
		`CREATE TABLE t(a, b, PRIMARY KEY('a'))`,
		`CREATE TABLE t(a, b, PRIMARY KEY('a' ASC, "b" ASC))`,
		`CREATE TABLE t(a, b, UNIQUE('a' COLLATE nocase))`,
		// ...and the spellings that already worked, as controls
		`CREATE TABLE t(a, b, PRIMARY KEY(a))`,
		`CREATE TABLE t(a, b, PRIMARY KEY(a ASC))`,
		`CREATE TABLE t(a, b, PRIMARY KEY(a DESC, b ASC))`,
		`CREATE TABLE t(a, b, PRIMARY KEY([a], "b"))`,
		`CREATE TABLE t(a, b, UNIQUE(a), UNIQUE(b))`,
	} {
		differ(t, create, append([]string{create}, tccProbe()...))
	}
}

// TestTableConstraintCollateWithoutRowid runs the same constraint shapes on a
// WITHOUT ROWID table, whose PRIMARY KEY is the clustered key itself, so the
// collation decides the physical key as well as the conflicts.
func TestTableConstraintCollateWithoutRowid(t *testing.T) {
	for _, create := range []string{
		`CREATE TABLE t(a, b, PRIMARY KEY(a COLLATE nocase)) WITHOUT ROWID`,
		`CREATE TABLE t(a, b, PRIMARY KEY(a COLLATE rtrim)) WITHOUT ROWID`,
		`CREATE TABLE t(a, b, PRIMARY KEY(a COLLATE nocase, b)) WITHOUT ROWID`,
		`CREATE TABLE t(a, b, PRIMARY KEY(a)) WITHOUT ROWID`,
	} {
		stmts := []string{create,
			`INSERT INTO t VALUES('X','p')`,
			`INSERT INTO t VALUES('x','q')`,
			`INSERT INTO t VALUES('x  ','r')`,
			`INSERT INTO t VALUES('y','s')`,
			// no rowid to order by: order by the key itself
			`SELECT quote(a), quote(b) FROM t ORDER BY a, b`,
			`SELECT count(*) FROM t`,
			`SELECT sql FROM sqlite_master WHERE type='table'`,
		}
		differ(t, create, append(stmts, tccPragmas()...))
	}
}

// TestTableConstraintCollateStaysDeclined pins the shapes that must NOT be
// quietly accepted: an unknown collation name, which C SQLite rejects at
// CREATE TABLE time. Accepting one and treating it as BINARY would return the
// wrong rows whenever it mattered.
func TestTableConstraintCollateStaysDeclined(t *testing.T) {
	for _, create := range []string{
		`CREATE TABLE t(a, b, UNIQUE(a COLLATE nosuchcollation))`,
		`CREATE TABLE t(a, b, PRIMARY KEY(a COLLATE nosuchcollation))`,
	} {
		stmts := []string{create, `SELECT count(*) FROM sqlite_master WHERE name='t'`}
		if res := run(t, "cgo", stmts); res[0]["kind"] != "error" {
			t.Errorf("oracle now ACCEPTS %q -- this decline may need to become an accept", create)
		}
		if res := run(t, "musql", stmts); res[0]["kind"] != "error" {
			t.Errorf("%q must be rejected: treating an unknown collation as BINARY returns "+
				"the wrong rows whenever it matters", create)
		}
	}
}

// TestTableConstraintCollateRedundancy checks the rule buildAutoIndexes
// already documents -- two constraints collapse into ONE automatic index only
// when their key columns, positions AND collations all match -- now that a
// constraint can name a collation of its own.
func TestTableConstraintCollateRedundancy(t *testing.T) {
	for i, create := range []string{
		// same column, DIFFERENT collations: two indexes
		`CREATE TABLE t(a, b, UNIQUE(a), UNIQUE(a COLLATE nocase))`,
		`CREATE TABLE t(a, b, PRIMARY KEY(a), UNIQUE(a COLLATE nocase))`,
		// same column, SAME collation written two ways: one index
		`CREATE TABLE t(a COLLATE nocase, b, UNIQUE(a), UNIQUE(a COLLATE nocase))`,
		`CREATE TABLE t(a, b, UNIQUE(a COLLATE nocase), UNIQUE(a COLLATE NOCASE))`,
	} {
		stmts := []string{create,
			`SELECT name FROM sqlite_master WHERE type='index' ORDER BY name`,
			`SELECT count(*) FROM sqlite_master WHERE type='index'`,
			`SELECT sql FROM sqlite_master WHERE type='table'`,
		}
		differ(t, fmt.Sprintf("redundancy%d %s", i, create), append(stmts, tccPragmas()...))
	}
}
