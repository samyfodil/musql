package engine

import (
	"fmt"
	"testing"
)

// TestAutoIndexPinnedWaitsForItsLoop: TPC-DS q95's shape. An automatic index
// on the rows a hint pins (customer_address's, ca_state = 'IL') is built
// when its loop is first reached, as C builds one under OP_Once, so an outer
// loop with no rows builds none. With rows, the answer is the one the pin
// left out gives.
func TestAutoIndexPinnedWaitsForItsLoop(t *testing.T) {
	p := newSegPair(t,
		`CREATE TABLE site(sk INTEGER, co TEXT)`,
		`CREATE TABLE ca(ask INTEGER, st TEXT)`,
		`CREATE TABLE ws(site INTEGER, addr INTEGER, cost INTEGER)`,
		`INSERT INTO site VALUES (1, 'pri'), (2, 'ought')`,
		`INSERT INTO ca SELECT value, CASE value % 50 WHEN 0 THEN 'IL' ELSE 'S' || (value % 50) END FROM generate_series(1, 2000)`,
		`INSERT INTO ws SELECT 1 + value % 2, value % 2100, value % 7 FROM generate_series(1, 20000)`,
		`ANALYZE`)
	const q = `SELECT count(*), sum(cost) FROM site, ws, ca WHERE ws.site = site.sk AND site.co || '' = '%s' AND ws.addr = ca.ask AND ca.st = 'IL'`
	for _, co := range []string{"none", "pri"} {
		seek := fmt.Sprintf(q, co)
		autoIndexOrdersBuilt.Store(0)
		got := p.mustFast(seek)
		if built := autoIndexOrdersBuilt.Load(); co == "none" && built != 0 {
			t.Errorf("%s: %d automatic indexes built for a loop never reached", seek, built)
		}
		want := p.mustFast(fmt.Sprintf(q, co) + ` AND +ca.st = ca.st`)
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s:\n got  %s\n want %s", seek, g, w)
		}
	}
}
