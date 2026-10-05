package compat

import (
	"strconv"
	"testing"
)

// TestColumnDefGrammar pins which column definitions CREATE TABLE accepts to
// parse.y's own typetoken and ccons rules (engine/sql_parser.go's
// validateColumnDefGrammar): every constraint form C accepts, and the
// neighbours it refuses. The constraint reader skips what it does not know, so
// without the grammar check "b AS (c+1) %TYPE%" (session_gen.test) and a stray
// word after a constraint were both accepted where C reports a syntax error.
func TestColumnDefGrammar(t *testing.T) {
	defs := []string{
		// accepted
		"x INTEGER PRIMARY KEY ASC ON CONFLICT REPLACE AUTOINCREMENT",
		"y TEXT NOT NULL ON CONFLICT IGNORE DEFAULT 'a' COLLATE NOCASE CHECK(length(y)>0) UNIQUE ON CONFLICT FAIL",
		"z REFERENCES p(a) ON DELETE CASCADE ON UPDATE SET NULL MATCH FULL DEFERRABLE INITIALLY DEFERRED",
		"z REFERENCES p ON INSERT NO ACTION ON DELETE SET DEFAULT ON UPDATE RESTRICT",
		"w NOT DEFERRABLE INITIALLY IMMEDIATE",
		"v DEFAULT -5", "u DEFAULT +5.5", "s DEFAULT CURRENT_TIMESTAMP", "r DEFAULT (1+2)",
		"q DEFAULT x'00'", "p DEFAULT TRUE", "o DEFAULT abc", "n DEFAULT -'x'", "m DEFAULT NULL",
		"k DEFAULT key", "g AS (1) STORED", "h GENERATED ALWAYS AS (2) VIRTUAL", "i AS (3) NOT NULL",
		"t VARCHAR(10)", "d DECIMAL(10,2)", "e DOUBLE PRECISION", "f INT(-5)", "c 'text'", "b INT(+3, -4)",
		"a CONSTRAINT c1 NOT NULL CONSTRAINT c2 DEFAULT 1", "a NULL", "a NULL ON CONFLICT ABORT",
		"a UNIQUE CONSTRAINT dangling", `a "quoted type"`, "a key value", "a INT CHECK((a))",
		// refused
		"b AS (c+1) %TYPE%", "x INT PRIMARY KEY foo", "x INT %", "x INT(5", "x DEFAULT",
		"x CHECK x>0", "x REFERENCES", "x COLLATE", "x INT(1,2,3)", "x NOT", "x PRIMARY",
		"x DEFAULT +abc", "x SELECT", "x INT ON CONFLICT", "x AS (1) foo bar", "x INT UNIQUE ON",
		"x DEFAULT ?", "x INT NOT NULL ON CONFLICT", "x REFERENCES p ON DELETE", "x INT()",
	}
	var stmts []string
	for i, d := range defs {
		stmts = append(stmts, "CREATE TABLE t"+strconv.Itoa(i)+"("+d+")")
	}
	differ(t, "column definition grammar", stmts)
}
