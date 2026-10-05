// Tests optional transaction names: BEGIN/COMMIT/ROLLBACK TRANSACTION <name>.
package compat

import "testing"

func TestTransactionOptionalName(t *testing.T) {
	// Names are ignored; row counts prove semantics.
	differ(t, "named transaction verbs", []string{
		`CREATE TABLE t(a)`,
		`BEGIN TRANSACTION foo`,
		`INSERT INTO t VALUES(1)`,
		`ROLLBACK TRANSACTION foo`,
		`SELECT count(*) AS n FROM t`,
		`BEGIN TRANSACTION 'bar'`,
		`INSERT INTO t VALUES(2)`,
		`COMMIT TRANSACTION 'bar'`,
		`SELECT count(*) AS n FROM t`,
		`BEGIN TRANSACTION`,
		`INSERT INTO t VALUES(3)`,
		`END TRANSACTION zzz`,
		`SELECT count(*) AS n FROM t`,
		// The name is never resolved: with nothing open this is the ordinary
		// "no transaction is active", not a complaint about "nosuch".
		`ROLLBACK TRANSACTION nosuch`,
		`SELECT count(*) AS n FROM t`,
	})
	// BEGIN modes also take transaction names.
	differ(t, "named transaction with an explicit mode", []string{
		`CREATE TABLE t(a)`,
		`BEGIN DEFERRED TRANSACTION zz`,
		`INSERT INTO t VALUES(1)`,
		`COMMIT`,
		`BEGIN IMMEDIATE TRANSACTION zz`,
		`INSERT INTO t VALUES(2)`,
		`COMMIT TRANSACTION zz`,
		`BEGIN EXCLUSIVE TRANSACTION "quoted name"`,
		`INSERT INTO t VALUES(3)`,
		`END TRANSACTION "quoted name"`,
		`SELECT count(*) AS n FROM t`,
	})
}

// TestTransactionNameDoesNotSwallowSavepointTO verifies ROLLBACK TRANSACTION TO
// is still the savepoint form, not a transaction name.
func TestTransactionNameDoesNotSwallowSavepointTO(t *testing.T) {
	differ(t, "ROLLBACK TRANSACTION TO is still the savepoint form", []string{
		`CREATE TABLE t(a)`,
		`BEGIN`,
		`INSERT INTO t VALUES(1)`,
		`SAVEPOINT sp`,
		`INSERT INTO t VALUES(2)`,
		// Rolls back to sp.
		`ROLLBACK TRANSACTION TO sp`,
		`SELECT count(*) AS n FROM t`,
		`INSERT INTO t VALUES(3)`,
		`ROLLBACK TRANSACTION TO SAVEPOINT sp`,
		`SELECT count(*) AS n FROM t`,
		`COMMIT`,
		`SELECT count(*) AS n FROM t`,
	})
}

// TestTransactionNameRequiresTheKeyword pins the two spellings that must STAY
// errors on both engines: a name with no TRANSACTION keyword, and a numeric
// literal where the "nm" production requires a name.
func TestTransactionNameRequiresTheKeyword(t *testing.T) {
	differ(t, "a transaction name without the TRANSACTION keyword", []string{
		`CREATE TABLE t(a)`,
		`BEGIN foo`,
		`SELECT 1 AS alive`,
		`BEGIN`,
		`COMMIT foo`,
		`ROLLBACK bar`,
		`COMMIT`,
		`SELECT 1 AS alive`,
	})
	differ(t, "a numeric transaction name", []string{
		`BEGIN TRANSACTION 123`,
		`SELECT 1 AS alive`,
	})
}
