// Concat operator over BLOB in UTF-16 databases tests transcoding behavior.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

var concatBlobExprs = []string{
	`SELECT hex(x'610062006300' || 'Z')`,
	`SELECT length(x'610062006300' || 'Z')`,
	`SELECT typeof(x'610062006300' || 'Z')`,
	`SELECT hex('Z' || x'610062006300')`,
	`SELECT hex(x'610062006300' || x'5a00')`,
	`SELECT hex(x'61' || 'Z')`,
	`SELECT length(x'61' || 'Z')`,
	`SELECT hex('Z' || x'61')`,
	`SELECT hex(x'e6bca2' || 'Z')`,
	`SELECT length(x'e6bca2' || 'Z')`,
	`SELECT hex(x'610062006300' || '')`,
	`SELECT hex('' || x'610062006300')`,
	`SELECT hex(x'41' || x'42')`,
	`SELECT typeof(x'41' || x'42')`,
	`SELECT hex(x'2000410042002000' || 'Z')`,
	`SELECT length(x'2000410042002000' || 'Z')`,
	`SELECT hex(x'610062006300' || 7)`,
	`SELECT hex(7 || x'610062006300')`,
	`SELECT hex(x'610062006300' || 1.5)`,
	`SELECT x'610062006300' || NULL IS NULL`,
	`SELECT hex(NULL || x'61')`,
	`SELECT length(x'00d84100' || 'Z')`,
	`SELECT hex('ab' || 'cd')`,
	`SELECT length('ab' || 'cd')`,
	// Stored column exercises VDBE OpConcat.
	`SELECT hex(b || 'Z') FROM cb`,
	`SELECT length(b || 'Z') FROM cb`,
	`SELECT hex(t || b) FROM cb`,
}

// Known divergent shapes due to transcoding of UTF-16 invalid bytes; character counts agree.
var concatBlobKnownDivergent = map[string]string{
	`SELECT hex(x'00d84100' || 'Z')`: "UTF-16le",
	`SELECT hex(x'ff' || x'fe')`:     "UTF-16be",
}

func TestConcatBlobInUTF16Database(t *testing.T) {
	for _, enc := range []string{"UTF-8", "UTF-16le", "UTF-16be"} {
		t.Run(enc, func(t *testing.T) {
			godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer godb.Discard()
			cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer cgodb.Close()
			cgodb.SetMaxOpenConns(1)

			setup := []string{
				`PRAGMA encoding='` + enc + `'`,
				`CREATE TABLE cb(b, t)`,
				`INSERT INTO cb VALUES(x'610062006300', 'Z')`,
			}
			for _, s := range setup {
				if eerr := godb.Exec(s); eerr != nil {
					t.Fatalf("setup %q: engine: %v", s, eerr)
				}
				if _, cerr := cgodb.Exec(s); cerr != nil {
					t.Fatalf("setup %q: cgo: %v", s, cerr)
				}
			}

			for _, q := range concatBlobExprs {
				goCols, goRows, gerr, panicked, panicVal := tclSafeGoQuery(godb, q)
				if panicked {
					t.Fatalf("%s: engine PANICKED: %v", q, panicVal)
				}
				cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, q)
				if (gerr != nil) != (cerr != nil) {
					t.Errorf("%s: accept/reject disagrees\n  go:  %v\n  cgo: %v", q, gerr, cerr)
					continue
				}
				if gerr != nil {
					continue
				}
				if ok, reason := queryResultsMatch(goCols, goRows, cgoCols, cgoRows, true); !ok {
					t.Errorf("%s: %s\n  go:  %v\n  cgo: %v", q, reason, goRows, cgoRows)
				}
			}

			// The recorded divergences must still BE divergences in the exact
			// encoding named, and must agree everywhere else -- if one
			// converges, promote it into the table above rather than leaving a
			// stale exemption behind.
			for q, divergesIn := range concatBlobKnownDivergent {
				goCols, goRows, gerr, panicked, _ := tclSafeGoQuery(godb, q)
				if panicked || gerr != nil {
					t.Fatalf("%s: engine failed outright: panicked=%v err=%v", q, panicked, gerr)
				}
				cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, q)
				if cerr != nil {
					t.Fatalf("%s: cgo: %v", q, cerr)
				}
				ok, _ := queryResultsMatch(goCols, goRows, cgoCols, cgoRows, true)
				if enc != divergesIn && !ok {
					t.Errorf("%s: must AGREE in %s\n  go:  %v\n  cgo: %v", q, enc, goRows, cgoRows)
				}
				if enc == divergesIn && ok {
					t.Errorf("%s: now AGREES in %s -- move it into concatBlobExprs", q, enc)
				}
			}
		})
	}
}
