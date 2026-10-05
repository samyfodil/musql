//go:build r31probe

// Command-line scratch probe: prints the ORACLE's own error text for a
// statement sequence, which the normalized harness deliberately drops. Built
// only under -tags r31probe so it never joins a gate run.
package compat

import (
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestR31Probe(t *testing.T) {
	for _, seq := range [][]string{
		{
			`SELECT json('x')`,
			`SELECT json('x') IN ()`,
			`SELECT unicode(char(55296))`,
			`SELECT abs(-9223372036854775808)`,
		},
		{
			`CREATE TABLE t1(a, b)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true WHERE CAST(b AS INT) IN (); END`,
			`ALTER TABLE t1 RENAME b TO bbb`,
			`SELECT sql FROM sqlite_master ORDER BY name`,
		},
		{
			`CREATE TABLE t1(a, b)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true WHERE abs(b) COLLATE nocase IN (); END`,
			`ALTER TABLE t1 RENAME b TO bbb`,
			`SELECT sql FROM sqlite_master ORDER BY name`,
		},
		{
			`CREATE TABLE t1(a, b)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true WHERE abs(b) IN (); END`,
			`ALTER TABLE t1 RENAME b TO bbb`,
			`SELECT sql FROM sqlite_master ORDER BY name`,
		},
		{
			`CREATE TABLE t1(a, b)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true FROM t1 WHERE abs(b) IN (); END`,
			`ALTER TABLE t1 RENAME b TO bbb`,
			`SELECT sql FROM sqlite_master ORDER BY name`,
		},
		{
			`CREATE TABLE t1(a, b)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true FROM t1 WHERE (b LIKE 'x') IN (); END`,
			`ALTER TABLE t1 RENAME b TO bbb`,
			`SELECT sql FROM sqlite_master ORDER BY name`,
		},
		{
			`CREATE TABLE t1(a, b)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true FROM t1 WHERE abs(b) COLLATE nocase IN (); END`,
			`ALTER TABLE t1 RENAME b TO bbb`,
			`SELECT sql FROM sqlite_master ORDER BY name`,
		},
		{
			`CREATE TABLE t1(a, b)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true FROM t1 WHERE CAST(b AS INT) IN (); END`,
			`ALTER TABLE t1 RENAME b TO bbb`,
			`SELECT sql FROM sqlite_master ORDER BY name`,
		},
	} {
		db, err := sql.Open("sqlite3", t.TempDir()+"/x.db")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("=== sequence")
		for _, s := range seq {
			rows, err := db.Query(s)
			if err != nil {
				fmt.Printf("  %-60s ERR %v\n", s, err)
				continue
			}
			out := ""
			for rows.Next() {
				var v any
				rows.Scan(&v)
				out += fmt.Sprintf(" | %v", v)
			}
			if e := rows.Err(); e != nil {
				out += fmt.Sprintf(" ROWERR %v", e)
			}
			rows.Close()
			fmt.Printf("  %-60s OK%s\n", s, out)
		}
		db.Close()
	}
}
