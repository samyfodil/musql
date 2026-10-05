package compat

// Package compat tests the _time_decltype DSN option for time.Time conversion.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// declTimeSchema is a test schema for time type testing.
var declTimeSchema = []string{
	`CREATE TABLE t(
		id INTEGER PRIMARY KEY,
		d DATE, dt DATETIME, ts TIMESTAMP,
		dt3 "DATETIME(3)", tstz TIMESTAMPTZ, txt TEXT, none
	)`,
}

// scanAll queries and renders values to detect driver conversions.
func scanAll(db *sql.DB, q string) string {
	rows, err := db.Query(q)
	if err != nil {
		return "ERR:" + err.Error()
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := ""
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "SCANERR:" + err.Error()
		}
		for i, v := range vals {
			switch x := v.(type) {
			case time.Time:
				zone, _ := x.Zone()
				out += fmt.Sprintf("%s=TIME(%s|%s) ", cols[i], x.UTC().Format(time.RFC3339Nano), zone)
			case []byte:
				out += fmt.Sprintf("%s=BLOB(%x) ", cols[i], x)
			default:
				out += fmt.Sprintf("%s=%T(%v) ", cols[i], v, v)
			}
		}
		out += "; "
	}
	if rows.Err() != nil {
		return "ERR:" + rows.Err().Error()
	}
	return out
}

// declTimeCase is a test value for time conversion.
type declTimeCase struct {
	name string
	set  string // the VALUES tuple after the id
}

var declTimeCases = []declTimeCase{
	// TEXT, through each of mattn's nine formats in order.
	{"full offset", `'2024-02-29 13:45:56.789-05:00'`},
	{"full offset T", `'2024-02-29T13:45:56.789-05:00'`},
	{"frac", `'2024-02-29 13:45:56.789'`},
	{"frac T", `'2024-02-29T13:45:56.789'`},
	{"seconds", `'2024-02-29 13:45:56'`},
	{"seconds T", `'2024-02-29T13:45:56'`},
	{"minutes", `'2024-02-29 13:45'`},
	{"minutes T", `'2024-02-29T13:45'`},
	{"date only", `'2024-02-29'`},
	{"zulu", `'2024-02-29T13:45:56Z'`},
	{"zulu frac", `'2024-02-29 13:45:56.789Z'`},
	// Unparseable TEXT values.
	{"unparseable", `'not a date'`},
	{"empty", `''`},
	{"partial", `'2024-02'`},
	// Integer epoch values.
	{"epoch seconds", `1709213156`},
	{"epoch zero", `0`},
	{"epoch negative seconds", `-86400`},
	{"epoch ms", `1709213156789`},
	{"epoch ms negative", `-1709213156789`},
	{"just under the ms boundary", `1000000000000`},
	{"just over the ms boundary", `1000000000001`},
	// Non-convertible types.
	{"real", `2460370.5`},
	{"blob", `x'0001'`},
	{"null", `NULL`},
}

// TestDeclTypeTimeMatchesMattn verifies flag-ON behavior matches mattn.
func TestDeclTypeTimeMatchesMattn(t *testing.T) {
	for _, c := range declTimeCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var out [2]string
			for i, dsn := range [2]string{"sqlite3", "sqlite"} {
				p := filepath.Join(t.TempDir(), "x.db")
				db, err := sql.Open(dsn, p+"?_time_decltype=1")
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				for _, s := range declTimeSchema {
					if _, err := db.Exec(s); err != nil {
						t.Fatal(err)
					}
				}
				ins := fmt.Sprintf(`INSERT INTO t VALUES(1,%s,%s,%s,%s,%s,%s,%s)`,
					c.set, c.set, c.set, c.set, c.set, c.set, c.set)
				if _, err := db.Exec(ins); err != nil {
					t.Fatal(err)
				}
				out[i] = scanAll(db, `SELECT d, dt, ts, dt3, tstz, txt, none FROM t`)
				db.Close()
			}
			if out[0] != out[1] {
				t.Errorf("%s\n  cgo: %s\n  mus: %s", c.set, out[0], out[1])
			}
		})
	}
}

// TestDeclTypeTimeOffIsUnchanged verifies flag-OFF preserves default behavior.
func TestDeclTypeTimeOffIsUnchanged(t *testing.T) {
	for _, c := range declTimeCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "x.db")
			db, err := sql.Open("sqlite", p)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			for _, s := range declTimeSchema {
				if _, err := db.Exec(s); err != nil {
					t.Fatal(err)
				}
			}
			ins := fmt.Sprintf(`INSERT INTO t VALUES(1,%s,%s,%s,%s,%s,%s,%s)`,
				c.set, c.set, c.set, c.set, c.set, c.set, c.set)
			if _, err := db.Exec(ins); err != nil {
				t.Fatal(err)
			}
			got := scanAll(db, `SELECT d, dt, ts, dt3, tstz, txt, none FROM t`)
			if contains(got, "TIME(") {
				t.Errorf("a plain DSN converted a column to time.Time: %s", got)
			}
		})
	}
}

// TestDeclTypeTimeScansIntoTimeTime tests ORM-style scanning into time.Time.
func TestDeclTypeTimeScansIntoTimeTime(t *testing.T) {
	for _, drv := range []string{"sqlite3", "sqlite"} {
		drv := drv
		t.Run(drv, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "x.db")
			for _, tc := range []struct {
				suffix string
				wantOK bool
			}{
				{"", drv == "sqlite3"},
				{"?_time_decltype=1", true},
			} {
				db, err := sql.Open(drv, p+tc.suffix)
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				db.Exec(`CREATE TABLE u(id INTEGER PRIMARY KEY, created_at DATETIME)`)
				db.Exec(`INSERT INTO u VALUES(1,'2024-02-29 13:45:56')`)
				var tv time.Time
				err = db.QueryRow(`SELECT created_at FROM u WHERE id=1`).Scan(&tv)
				db.Close()
				if (err == nil) != tc.wantOK {
					t.Errorf("%s%s: scan into time.Time err=%v, want ok=%v", drv, tc.suffix, err, tc.wantOK)
					continue
				}
				if err == nil && tv.UTC().Format("2006-01-02 15:04:05") != "2024-02-29 13:45:56" {
					t.Errorf("%s%s: got %v", drv, tc.suffix, tv)
				}
			}
		})
	}
}

// TestDeclTypeTimeLoc verifies _loc parameter handling.
func TestDeclTypeTimeLoc(t *testing.T) {
	for _, loc := range []string{"UTC", "America/New_York", "Asia/Tokyo", "auto"} {
		loc := loc
		t.Run(loc, func(t *testing.T) {
			var out [2]string
			for i, drv := range [2]string{"sqlite3", "sqlite"} {
				p := filepath.Join(t.TempDir(), "x.db")
				db, err := sql.Open(drv, p+"?_time_decltype=1&_loc="+loc)
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				db.Exec(`CREATE TABLE u(id INTEGER PRIMARY KEY, a DATETIME, b DATETIME)`)
				db.Exec(`INSERT INTO u VALUES(1,'2024-02-29 13:45:56',1709213156)`)
				out[i] = scanAll(db, `SELECT a, b FROM u`)
				db.Close()
			}
			if out[0] != out[1] {
				t.Errorf("_loc=%s\n  cgo: %s\n  mus: %s", loc, out[0], out[1])
			}
		})
	}
}

// TestDeclTypeTimeExpressionShapes verifies declared type availability.
func TestDeclTypeTimeExpressionShapes(t *testing.T) {
	setup := []string{
		`CREATE TABLE u(id INTEGER PRIMARY KEY, created_at DATETIME, note TEXT)`,
		`INSERT INTO u VALUES(1,'2024-02-29 13:45:56','x')`,
		`CREATE VIEW v AS SELECT id, created_at FROM u`,
	}
	for _, q := range []string{
		`SELECT created_at FROM u`,
		`SELECT u.created_at FROM u`,
		`SELECT created_at AS whenever FROM u`,
		`SELECT * FROM u`,
		`SELECT created_at FROM v`,
		`SELECT * FROM v`,
		`SELECT created_at FROM (SELECT created_at FROM u)`,
		`SELECT x.created_at FROM (SELECT created_at FROM u) x`,
		`SELECT (SELECT created_at FROM u)`,
		`SELECT id FROM u`,
		`SELECT note FROM u`,
		// Expressions without declared types.
		`SELECT CAST(created_at AS DATETIME) FROM u`,
		`SELECT created_at || '' FROM u`,
		`SELECT max(created_at) FROM u`,
		`SELECT datetime(created_at) FROM u`,
		`SELECT '2024-02-29 13:45:56'`,
		`SELECT coalesce(created_at, created_at) FROM u`,
		// Compound types from leftmost arm.
		`SELECT created_at FROM u UNION ALL SELECT note FROM u`,
		`SELECT note FROM u UNION ALL SELECT created_at FROM u`,
	} {
		q := q
		t.Run(q, func(t *testing.T) {
			var out [2]string
			for i, drv := range [2]string{"sqlite3", "sqlite"} {
				p := filepath.Join(t.TempDir(), "x.db")
				db, err := sql.Open(drv, p+"?_time_decltype=1")
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				for _, s := range setup {
					if _, err := db.Exec(s); err != nil {
						t.Fatal(err)
					}
				}
				out[i] = scanAll(db, q)
				db.Close()
			}
			if out[0] != out[1] {
				t.Errorf("%s\n  cgo: %s\n  mus: %s", q, out[0], out[1])
			}
		})
	}
}

// TestDeclTypeTimeBadLoc verifies invalid _loc values are rejected.
func TestDeclTypeTimeBadLoc(t *testing.T) {
	for _, drv := range []string{"sqlite3", "sqlite"} {
		p := filepath.Join(t.TempDir(), "x.db")
		db, err := sql.Open(drv, p+"?_time_decltype=1&_loc=Not/AZone")
		if err == nil {
			// Error reported on first use.
			err = db.Ping()
			db.Close()
		}
		if err == nil {
			t.Errorf("%s: an invalid _loc opened successfully", drv)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
