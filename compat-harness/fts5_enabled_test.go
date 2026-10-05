//go:build sqlite_fts5

// This file is the FTS5-ON half of the harness's two-build arrangement; see
// fts5_disabled_test.go for the OFF half and the full rationale.
package compat

import "github.com/samyfodil/musql/engine"

// harnessFTS5 is true in this build: `go test -tags sqlite_fts5` compiles the
// CGo oracle (mattn/go-sqlite3) with -DSQLITE_ENABLE_FTS5, so C SQLite
// answers "CREATE VIRTUAL TABLE ... USING fts5" here instead of "no such
// module: fts5".
const harnessFTS5 = true

// The pure engine keeps fts5 OPT-IN (engine.RegisterFTS5) precisely so that the
// default oracle build -- which has no fts5 -- never sees the engine accept a
// module statement C SQLite rejects. Under this tag the oracle DOES have
// fts5, so registering it here is what finally puts the two implementations
// head to head instead of skipping every fts5 statement on both sides.
func init() { engine.RegisterFTS5() }
