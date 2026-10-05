// This file is the differential gate for SQLITE_MAX_LENGTH: the largest string
// or blob SQLite will produce is 1,000,000,000 bytes, and exceeding it is
// "string or blob too big" from zeroblob(), randomblob(), hex() and printf()
// alike (zeroblob.test). This engine happily allocated multi-gigabyte results.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func TestStringOrBlobTooBigParity(t *testing.T) {
	got := map[string]string{}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		dsn := ":memory:"
		if drv == "sqlite" {
			dsn = filepath.Join(t.TempDir(), "e.sqlite")
		}
		db, _ := sql.Open(drv, dsn)
		db.SetMaxOpenConns(1)
		for _, q := range []string{
			`SELECT length(zeroblob(1000000000))`,
			`SELECT length(zeroblob(1000000001))`,
			`SELECT length(zeroblob(2147483648))`,
			`SELECT length(zeroblob(-5))`,
			`SELECT length(randomblob(1000000001))`,
			`SELECT length(hex(zeroblob(600000000)))`,
			`SELECT length(printf('%.*c', 1000000001, 'x'))`,
			`SELECT length(printf('%200000c', 'x'))`,
			`SELECT length(printf('%.*s', 300000, 'x'))`,
		} {
			var n any
			err := db.QueryRow(q).Scan(&n)
			out := "ok"
			if err != nil {
				out = strings.TrimPrefix(err.Error(), "engine: ")
			} else {
				out = fmt.Sprintf("%v", n)
			}
			if drv == "sqlite" {
				got[q] = out
				continue
			}
			if got[q] != out {
				t.Errorf("%s\n  engine: %s\n  cgo:    %s", q, got[q], out)
			}
		}
		db.Close()
	}
}
