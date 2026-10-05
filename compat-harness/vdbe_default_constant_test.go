// This file is the differential gate for SQLite's "default value of column
// [y] is not constant" rule: a DEFAULT expression may not reference a COLUMN.
// Verified directly (default.test) -- any bare identifier is non-constant
// whether or not it names a real column, while a function call, arithmetic,
// a concatenation, a CASE, CURRENT_TIMESTAMP, TRUE and NULL are all fine.
// This engine accepted every one of them, and the rejected CREATE then
// cascaded: two later statements referenced a table SQLite never created.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var defaultConstQ = []string{
	`CREATE TABLE d1(x, y DEFAULT (max(1,2)))`,
	`CREATE TABLE d2(x, y DEFAULT (1+2))`,
	`CREATE TABLE d3(x, y DEFAULT 5)`,
	`CREATE TABLE d4(x, y DEFAULT (x))`,
	`CREATE TABLE d5(x, y DEFAULT CURRENT_TIMESTAMP)`,
	`CREATE TABLE d6(x, y DEFAULT (random()))`,
	`CREATE TABLE d7(x, y DEFAULT (-1))`,
	`CREATE TABLE d8(x, y DEFAULT ('a'||'b'))`,
	`CREATE TABLE d9(x, y DEFAULT (julianday('now')))`,
	`CREATE TABLE da(x, y DEFAULT (nosuchcol))`,
	`CREATE TABLE db(x, y DEFAULT ('text'))`,
	`CREATE TABLE dc(x, y DEFAULT (CURRENT_TIMESTAMP))`,
	`CREATE TABLE dd(x, y DEFAULT (TRUE))`,
	`CREATE TABLE de(x, y DEFAULT (NULL))`,
	`CREATE TABLE df(x, y DEFAULT (abs(-1)))`,
	`CREATE TABLE dg(x, y DEFAULT (1 + x))`,
	`CREATE TABLE dh(x, y DEFAULT (CASE WHEN 1 THEN 2 ELSE 3 END))`,
}

func TestDefaultIsConstantParity(t *testing.T) {
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	for _, s := range defaultConstQ {
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", s, eerr, cerr)
			continue
		}
		if cerr != nil {
			if got := strings.TrimPrefix(eerr.Error(), "engine: "); got != cerr.Error() {
				t.Errorf("[%s] error text mismatch\n  engine: %q\n  cgo:    %q", s, got, cerr.Error())
			}
		}
	}
}
