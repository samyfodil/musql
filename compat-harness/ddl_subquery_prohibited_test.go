package compat

import "testing"

// TestDDLSubqueryProhibitedMatchesCSQLite compares with C SQLite a
// subquery written where a schema expression may not hold one: a column
// DEFAULT (not constant, build.c:1742), a generated column ("subqueries
// prohibited in generated columns", resolve.c:921-923), a CHECK, an index key
// and a partial index's WHERE -- and the FROM-less EXISTS an ATTACH path may
// hold, which C SQLite accepts.
func TestDDLSubqueryProhibitedMatchesCSQLite(t *testing.T) {
	for _, s := range []string{
		`CREATE TABLE c(a CHECK(EXISTS(SELECT 1)))`,
		`CREATE TABLE c(a DEFAULT (EXISTS(SELECT 1)))`,
		`CREATE TABLE c(a DEFAULT ((SELECT 1)))`,
		`CREATE TABLE c(a DEFAULT (1 + (SELECT 1)))`,
		`CREATE TABLE c(a DEFAULT (1 IN (SELECT 1)))`,
		`CREATE TABLE c(a DEFAULT (abs((SELECT -1))))`,
		`CREATE TABLE c(a, b AS (EXISTS(SELECT 1)))`,
		`CREATE TABLE c(a, b AS ((SELECT 1)))`,
		`CREATE TABLE c(a, b AS (a IN (SELECT 1)))`,
		`CREATE TABLE c(a, b GENERATED ALWAYS AS (CASE WHEN a THEN (SELECT 2) END) STORED)`,
		`CREATE INDEX ci ON s(a) WHERE EXISTS(SELECT 1)`,
		`CREATE INDEX ci ON s(a) WHERE (SELECT 1)`,
		`CREATE INDEX ci ON s((SELECT 1))`,
		`CREATE INDEX ci ON s(EXISTS(SELECT 1))`,
		`ALTER TABLE s ADD COLUMN z DEFAULT (EXISTS(SELECT 1))`,
		`ALTER TABLE s ADD COLUMN z AS ((SELECT 1))`,
	} {
		differ(t, s, []string{`CREATE TABLE s(a)`, `INSERT INTO s VALUES(1)`, s, `INSERT INTO c(a) VALUES(1)`, `SELECT * FROM c`, `SELECT * FROM s`})
	}
}
