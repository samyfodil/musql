// Tests ATTACH/DETACH grammar: double-quoted paths and expression names.
package compat

import "testing"

func TestAttachDoubleQuotedPath(t *testing.T) {
	differ(t, "double-quoted ATTACH path", []string{
		`ATTACH "" AS aux`,
		`CREATE TABLE aux.t(x)`,
		`INSERT INTO aux.t VALUES(1),(2)`,
		`SELECT x FROM aux.t ORDER BY x`,
		`DETACH aux`,
		// Database is gone after detach.
		`SELECT count(*) AS n FROM sqlite_master`,
	})
	// Single-quoted paths also work.
	differ(t, "single-quoted ATTACH path", []string{
		`ATTACH '' AS aux2`,
		`CREATE TABLE aux2.t(x)`,
		`INSERT INTO aux2.t VALUES(7)`,
		`SELECT x FROM aux2.t`,
		`DETACH aux2`,
	})
}

func TestDetachNonIdentifierName(t *testing.T) {
	// Both engines reject non-identifier names.
	differ(t, "DETACH with a non-identifier name", []string{
		`DETACH 123`,
		`DETACH null`,
		`SELECT 1 AS still_alive`,
	})
	// ...and a name that IS attached still detaches, whatever the spelling.
	differ(t, "DETACH still works by name", []string{
		`ATTACH '' AS "123"`,
		`CREATE TABLE "123".t(x)`,
		`INSERT INTO "123".t VALUES(5)`,
		`SELECT x FROM "123".t`,
		`DETACH "123"`,
		`SELECT 1 AS ok`,
	})
}
