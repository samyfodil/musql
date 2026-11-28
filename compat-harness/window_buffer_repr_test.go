package compat

import "testing"

// TestWindowBufferRepresentationParity holds every value representation the
// window batch stores (engine/vdbe_window_rows.go) to C's answer: INTEGER,
// REAL (with -0.0 and integral REALs), TEXT and BLOB (empty ones included,
// and values longer than an arena chunk), NULL, and a JSON subtype, through
// frames that read rows other than the current one, and over partitions large
// enough to span many cell, row and byte chunks.
func TestWindowBufferRepresentationParity(t *testing.T) {
	driverParity(t, "window-buffer-repr", []string{
		`CREATE TABLE r(k INTEGER PRIMARY KEY, g, v, t TEXT, b BLOB, f REAL)`,
		`INSERT INTO r VALUES` +
			`(1, 'a', 1, 'one', x'01', 1.5),` +
			`(2, 'a', 2.0, '', x'', -0.0),` +
			`(3, 'a', NULL, NULL, NULL, NULL),` +
			`(4, 'b', 'txt', 'four', x'0004ff', 4.0),` +
			`(5, 'b', x'cafe', 'five', zeroblob(3), 1e308),` +
			`(6, 'b', -7, 'six', x'06', -2.25),` +
			`(7, 'c', 9223372036854775807, 'seven', x'07', 0.1),` +
			`(8, 'c', -9223372036854775808, '8', x'', 8.0)`,
		`INSERT INTO r SELECT 100+value, 'z', value, printf('%.*c', 3000+value, 'x'), x'', value*0.5 FROM generate_series(1,3)`,
		`SELECT k, typeof(lag(v) OVER w), quote(lag(v) OVER w), quote(lead(t) OVER w), quote(lead(b) OVER w), quote(lag(f) OVER w)
		   FROM r WINDOW w AS (PARTITION BY g ORDER BY k) ORDER BY k`,
		`SELECT k, quote(first_value(v) OVER w), quote(last_value(t) OVER w), quote(nth_value(b, 2) OVER w), typeof(nth_value(f, 2) OVER w)
		   FROM r WINDOW w AS (ORDER BY k ROWS BETWEEN 1 PRECEDING AND 2 FOLLOWING) ORDER BY k`,
		`SELECT k, quote(max(v) OVER w), quote(min(t) OVER w), quote(max(b) OVER w), quote(sum(f) OVER w), group_concat(t, '|') OVER w
		   FROM r WINDOW w AS (PARTITION BY g ORDER BY k ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) ORDER BY k`,
		`SELECT k, length(t), length(lag(t, 2) OVER (ORDER BY k)), hex(lead(b, 3, x'ee') OVER (ORDER BY k)) FROM r ORDER BY k`,
		`SELECT k, quote(t), quote(b), quote(f), typeof(f), count(*) OVER () FROM r ORDER BY k`,
		`CREATE TABLE j(k INTEGER PRIMARY KEY, d)`,
		`INSERT INTO j VALUES(1, '[1,2]'), (2, '{"a":3}'), (3, '"s"')`,
		`SELECT k, json_array(last_value(json(d)) OVER (ORDER BY k)), json_array(lag(json(d)) OVER (ORDER BY k)),
		        json_group_array(json(d)) OVER (ORDER BY k ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING)
		   FROM j ORDER BY k`,
		// Large partitions: many chunks of cells, rows and bytes.
		`SELECT count(*), sum(length(x)), max(x), min(x), total(y), count(z), max(hex(w)), sum(typeof(y) = 'real')
		   FROM (SELECT lag(printf('%08d-%s', value, hex(value)), 3) OVER p AS x,
		                avg(value * 0.25) OVER (PARTITION BY value % 7 ORDER BY value ROWS BETWEEN 5 PRECEDING AND 2 FOLLOWING) AS y,
		                nth_value(CASE WHEN value % 5 THEN value END, 2) OVER p AS z,
		                last_value(CAST(value AS BLOB)) OVER p AS w
		           FROM generate_series(1, 60000)
		         WINDOW p AS (PARTITION BY value % 3 ORDER BY value))`,
		`SELECT max(i), min(i), sum(typeof(i) = 'integer')
		   FROM (SELECT sum(value) OVER (ORDER BY value) i FROM generate_series(1, 200000))`,
		`SELECT g, count(*), max(c), min(s), max(length(gc))
		   FROM (SELECT value % 4 g, count(*) OVER w c, sum(value * 1.5) OVER w s,
		                group_concat(char(65 + value % 26), '') OVER (PARTITION BY value % 4 ORDER BY value ROWS 9 PRECEDING) gc
		           FROM generate_series(1, 40000) WINDOW w AS (PARTITION BY value % 4 ORDER BY value / 10))
		  GROUP BY g ORDER BY g`,
	})
}
