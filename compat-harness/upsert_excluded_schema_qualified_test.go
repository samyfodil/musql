package compat

// Tests that schema-qualified excluded pseudo-column references error.

import "testing"

// TestUpsertExcludedRejectsASchemaQualifiedReference tests schema-qualified excluded errors.
func TestUpsertExcludedRejectsASchemaQualifiedReference(t *testing.T) {
	for _, tc := range []struct{ name, tail string }{
		{"SET value", `SET v=main.excluded.v`},
		{"SET value, temp qualifier", `SET v=temp.excluded.v`},
		{"SET subquery", `SET v=(SELECT main.excluded.v)`},
		{"DO UPDATE ... WHERE", `SET v=99 WHERE main.excluded.v=20`},
	} {
		differ(t, "excluded rejects a schema-qualified reference: "+tc.name, []string{
			`CREATE TABLE u(k INTEGER PRIMARY KEY, v)`,
			`INSERT INTO u VALUES(1,10)`,
			`INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE ` + tc.tail,
			`SELECT k,v FROM u ORDER BY k`,
		})
	}
	// The CONTROL: the plain two-part spelling still resolves and still writes,
	// so a fix that simply stopped answering "excluded" would fail here.
	differ(t, "excluded rejects a schema-qualified reference: two-part control still writes", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v)`,
		`INSERT INTO u VALUES(1,10)`,
		`INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		`SELECT k,v FROM u ORDER BY k`,
	})
	// And the control for a REAL table actually named "excluded" reached
	// through its schema: that one is an ordinary FROM item, resolves before
	// the pseudo-row block is ever admitted (the "cnt==0" half of
	// resolve.c:522), and must keep working.
	differ(t, "excluded rejects a schema-qualified reference: a real table named excluded", []string{
		`CREATE TABLE excluded(k INTEGER PRIMARY KEY, v)`,
		`INSERT INTO excluded VALUES(1,10)`,
		`INSERT INTO excluded VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=main.excluded.v+100`,
		`SELECT k,v FROM excluded ORDER BY k`,
	})
}
