package compat

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestDistinctAggJoinPlan: a DISTINCT aggregate over a join is planned as C
// plans it, its argument the ordering the solver prices (WHERE_DISTINCTBY),
// rather than left to the FROM order. TPC-DS q95's web_site, which no row
// passes, was the innermost loop, so every web_sales row built and probed two
// IN lists first: 3.7 s where C, scanning web_site first, answers at once.
// The answers and the order the loops nest in are C's.
func TestDistinctAggJoinPlan(t *testing.T) {
	schema := []string{
		`CREATE TABLE web_sales(ws_order_number INT, ws_warehouse_sk INT, ws_ship_date_sk INT, ws_ship_addr_sk INT, ws_web_site_sk INT, ws_ext_ship_cost REAL, ws_net_profit REAL)`,
		`CREATE TABLE date_dim(d_date_sk INT, d_date TEXT)`,
		`CREATE TABLE customer_address(ca_address_sk INT, ca_state TEXT)`,
		`CREATE TABLE web_site(web_site_sk INT, web_company_name TEXT)`,
		`CREATE TABLE web_returns(wr_order_number INT)`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v+1 FROM g WHERE v<5000) INSERT INTO web_sales SELECT v/3, v%5, v%300, v%700, v%15, v*0.1, v*0.3 FROM g`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v+1 FROM g WHERE v<3000) INSERT INTO date_dim SELECT v, date('1998-01-01', '+'||v||' days') FROM g`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v+1 FROM g WHERE v<700) INSERT INTO customer_address SELECT v, CASE v%50 WHEN 0 THEN 'IL' ELSE 'XX' END FROM g`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v+1 FROM g WHERE v<15) INSERT INTO web_site SELECT v, CASE v%4 WHEN 0 THEN 'pri' ELSE 'abc' END FROM g`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v+1 FROM g WHERE v<500) INSERT INTO web_returns SELECT v*3 FROM g`,
		`ANALYZE`,
	}
	join := `FROM web_sales, date_dim, customer_address, web_site
	  WHERE ws_ship_date_sk = d_date_sk AND ws_ship_addr_sk = ca_address_sk AND ca_state = 'IL'
	    AND ws_web_site_sk = web_site_sk AND web_company_name = %s`
	in := ` AND ws_order_number IN (SELECT wr_order_number FROM web_returns)`
	var qs []string
	for _, co := range []string{`'pri'`, `'abc'`, `'none'`} {
		j := fmt.Sprintf(join, co)
		qs = append(qs,
			`SELECT count(DISTINCT ws_order_number) `+j+in,
			`SELECT count(DISTINCT ws_order_number) `+j,
			`SELECT count(DISTINCT ws_order_number), sum(ws_ext_ship_cost), sum(ws_net_profit) `+j+in+` ORDER BY count(DISTINCT ws_order_number) LIMIT 100`,
			`SELECT sum(DISTINCT ws_ext_ship_cost) `+j,
			`SELECT ca_state, count(DISTINCT ws_order_number) `+j+` GROUP BY ca_state`,
		)
	}
	for _, q := range qs {
		differ(t, "distinct-agg-join", append(append([]string{}, schema...), q))
	}

	c, m := boundPair(t, schema)
	ctx := context.Background()
	cc, err := c.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	mc, err := m.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close()
	for _, q := range qs[:3] {
		if want, got := loopOrder(t, cc, q), loopOrder(t, mc, q); !slices.Equal(want, got) {
			t.Errorf("%s\nloops: cgo %q, musql %q", q, want, got)
		}
	}
}

// loopOrder is the FROM items the top-level loops of q's plan walk, outermost
// first.
func loopOrder(t *testing.T, c *sql.Conn, q string) []string {
	t.Helper()
	rows, err := c.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN %s: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		if f := strings.Fields(detail); parent == 0 && len(f) > 1 && (f[0] == "SCAN" || f[0] == "SEARCH") {
			out = append(out, f[1])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
