package compat

import "testing"

// TestAutoIndexPinnedLazyAsC: an automatic index on the rows a hint pins,
// below an outer loop with no rows and one with some, answers as C does.
func TestAutoIndexPinnedLazyAsC(t *testing.T) {
	differ(t, "auto-index-pinned-lazy", []string{
		`CREATE TABLE site(sk INTEGER, co TEXT)`,
		`CREATE TABLE ca(ask INTEGER, st TEXT)`,
		`CREATE TABLE ws(site INTEGER, addr INTEGER, cost INTEGER)`,
		`INSERT INTO site VALUES (1, 'pri'), (2, 'ought')`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 2000) INSERT INTO ca SELECT v, CASE v % 50 WHEN 0 THEN 'IL' ELSE 'S' || (v % 50) END FROM g`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 20000) INSERT INTO ws SELECT 1 + v % 2, v % 2100, v % 7 FROM g`,
		`ANALYZE`,
		`SELECT count(*), sum(cost) FROM site, ws, ca WHERE ws.site = site.sk AND site.co || '' = 'none' AND ws.addr = ca.ask AND ca.st = 'IL'`,
		`SELECT count(*), sum(cost) FROM site, ws, ca WHERE ws.site = site.sk AND site.co || '' = 'pri' AND ws.addr = ca.ask AND ca.st = 'IL'`,
		`SELECT site.co, count(*), sum(cost) FROM site, ws, ca WHERE ws.site = site.sk AND ws.addr = ca.ask AND ca.st = 'IL' GROUP BY site.co ORDER BY 1`,
	})
}
