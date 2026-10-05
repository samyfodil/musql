// This file tests which SQL keywords may be used as bare column names.
// SQL keywords must agree with C SQLite about whether they are legal as
// unquoted column names in CREATE TABLE statements.
package compat

import (
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

var sqlKeywords = strings.Fields(`ABORT ACTION ADD AFTER ALL ALTER ALWAYS ANALYZE AND AS ASC ATTACH AUTOINCREMENT
BEFORE BEGIN BETWEEN BY CASCADE CASE CAST CHECK COLLATE COLUMN COMMIT CONFLICT CONSTRAINT CREATE CROSS
CURRENT CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP DATABASE DEFAULT DEFERRABLE DEFERRED DELETE DESC
DETACH DISTINCT DO DROP EACH ELSE END ESCAPE EXCEPT EXCLUDE EXCLUSIVE EXISTS EXPLAIN FAIL FILTER FIRST
FOLLOWING FOR FOREIGN FROM FULL GENERATED GLOB GROUP GROUPS HAVING IF IGNORE IMMEDIATE IN INDEX INDEXED
INITIALLY INNER INSERT INSTEAD INTERSECT INTO IS ISNULL JOIN KEY LAST LEFT LIKE LIMIT MATCH MATERIALIZED
NATURAL NO NOT NOTHING NOTNULL NULL NULLS OF OFFSET ON OR ORDER OTHERS OUTER OVER PARTITION PLAN PRAGMA
PRECEDING PRIMARY QUERY RAISE RANGE RECURSIVE REFERENCES REGEXP REINDEX RELEASE RENAME REPLACE RESTRICT
RETURNING RIGHT ROLLBACK ROW ROWS SAVEPOINT SELECT SET TABLE TEMP TEMPORARY THEN TIES TO TRANSACTION
TRIGGER UNBOUNDED UNION UNIQUE UPDATE USING VACUUM VALUES VIEW VIRTUAL WHEN WHERE WINDOW WITH WITHOUT`)

func TestKeywordAsColumnNameParity(t *testing.T) {
	var mismatch []string
	for _, kw := range sqlKeywords {
		ddl := `CREATE TABLE kwt(a, ` + kw + `, c)`
		edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		cdb, err := sql.Open("sqlite3", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		eErr := edb.Exec(ddl)
		_, cErr := cdb.Exec(ddl)
		if (eErr == nil) != (cErr == nil) {
			mismatch = append(mismatch, kw)
		}
		edb.Close()
		cdb.Close()
	}
	if len(mismatch) > 0 {
		sort.Strings(mismatch)
		t.Errorf("keywords where the engine and C SQLite disagree about being a bare column name: %s", strings.Join(mismatch, " "))
	}
}

// TestQuotedKeywordColumnAccepted tests that quoted keywords are legal column names.
func TestQuotedKeywordColumnAccepted(t *testing.T) {
	for _, q := range []string{
		`CREATE TABLE q1(a, "on", c)`,
		`CREATE TABLE q2(a, [select], c)`,
		"CREATE TABLE q3(a, `where`, c)",
	} {
		edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		cdb, err := sql.Open("sqlite3", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		eErr := edb.Exec(q)
		_, cErr := cdb.Exec(q)
		if (eErr == nil) != (cErr == nil) {
			t.Errorf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", q, eErr, cErr)
		}
		edb.Close()
		cdb.Close()
	}
}
