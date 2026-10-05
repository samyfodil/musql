package compat

import (
	"path/filepath"
	"reflect"
	"testing"
)

// TestWithoutRowidVirtualColumnsReadBothWays covers a WITHOUT ROWID table with
// VIRTUAL generated columns, which have no slot in its record (build.c:2485-2500).
// musql stored one anyway and read one back, so each engine read the other's
// file with every later column shifted: C SQLite read musql's b as g's value,
// and musql read C SQLite's b as NULL. Every writer/reader pairing must
// answer what the writer itself answers.
func TestWithoutRowidVirtualColumnsReadBothWays(t *testing.T) {
	write := []string{
		`CREATE TABLE t1(g AS (b*10), a INTEGER PRIMARY KEY, h AS (c+100), b, c, s AS (a+b) STORED) WITHOUT ROWID`,
		`INSERT INTO t1(a,b,c) VALUES(1,2,3),(4,5,6)`,
		`CREATE INDEX t1g ON t1(g)`,
		`UPDATE t1 SET c=7 WHERE a=1`,
	}
	read := []string{
		`SELECT * FROM t1 ORDER BY a`,
		`SELECT b, c FROM t1 ORDER BY a`,
		`SELECT a FROM t1 WHERE g=50`,
		`PRAGMA integrity_check`,
	}
	// Each engine reads its OWN format, with the converter in between where the
	// writer was the other one (convert_for_oracle_test.go). The claim is the same
	// one it always was -- every writer/reader pairing answers what the writer
	// itself answers -- and the conversion is now part of it.
	for _, writer := range []string{"cgo", "musql"} {
		dsn := filepath.Join(t.TempDir(), "f.db")
		runWithDSN(t, writer, dsn, write)
		want := runWithDSN(t, writer, dsn, read)
		goPath, cgoPath := pathsForBothEngines(t, writerEngineName(writer), dsn)
		for _, reader := range []string{"cgo", "musql"} {
			path := goPath
			if reader == "cgo" {
				path = cgoPath
			}
			if got := runWithDSN(t, reader, path, read); !reflect.DeepEqual(got, want) {
				t.Errorf("written by %s, read by %s:\n got %v\nwant %v", writer, reader, got, want)
			}
		}
	}
}
