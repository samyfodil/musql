package compat

// This file gates two numeric coercion rules: NUL-terminated string handling
// in arithmetic and string aggregates, and the result type of the % operator.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// nulNumericQ tests NUL handling in various numeric contexts.
var nulNumericQ = []string{
	// --- rule 1: arithmetic yields REAL, sum() yields INTEGER ---
	`SELECT typeof(cast(x'3100' as text)+0), cast(x'3100' as text)+0`,
	`SELECT typeof(cast(x'3100' as text)-0), cast(x'3100' as text)-0`,
	`SELECT typeof(cast(x'3100' as text)*1), cast(x'3100' as text)*1`,
	`SELECT typeof(cast(x'3100' as text)/1), cast(x'3100' as text)/1`,
	`SELECT typeof(-cast(x'3100' as text)), -cast(x'3100' as text)`,
	`SELECT typeof(sum(cast(x'3100' as text))), sum(cast(x'3100' as text))`,
	// A NUL is enough anywhere after the integer prefix, with only whitespace
	// allowed in between; after OTHER junk it changes nothing.
	`SELECT typeof(cast(x'310078' as text)+0), typeof(sum(cast(x'310078' as text)))`,
	`SELECT typeof(cast(x'312000' as text)+0), typeof(sum(cast(x'312000' as text)))`,
	`SELECT typeof(cast(x'310020' as text)+0), typeof(sum(cast(x'310020' as text)))`,
	`SELECT typeof(cast(x'31000000' as text)+0), typeof(sum(cast(x'31000000' as text)))`,
	`SELECT typeof(cast(x'203100' as text)+0), typeof(sum(cast(x'203100' as text)))`,
	`SELECT typeof(cast(x'313200' as text)+0), sum(cast(x'313200' as text))`,
	`SELECT typeof(cast(x'2d3100' as text)+0), sum(cast(x'2d3100' as text))`,
	`SELECT typeof(cast(x'3000' as text)+0), sum(cast(x'3000' as text))`,
	`SELECT typeof(cast(x'317800' as text)+0), typeof(sum(cast(x'317800' as text)))`, // "1x"+NUL: unaffected
	// A FLOAT prefix is unaffected, and so is text with no numeric prefix.
	`SELECT typeof(cast(x'312e3000' as text)+0), typeof(sum(cast(x'312e3000' as text)))`,
	`SELECT typeof(cast(x'31653000' as text)+0), typeof(sum(cast(x'31653000' as text)))`,
	`SELECT typeof(cast(x'3078313000' as text)+0), typeof(sum(cast(x'3078313000' as text)))`,
	`SELECT typeof(cast(x'003100' as text)+0), typeof(sum(cast(x'003100' as text)))`,
	`SELECT typeof(cast(x'00' as text)+0), typeof(sum(cast(x'00' as text)))`,
	// Plain trailing junk and plain trailing whitespace, for contrast.
	`SELECT typeof('1x'+0), typeof(sum('1x'))`,
	`SELECT typeof('1 '+0), typeof(sum('1 '))`,
	`SELECT typeof('1'+0), typeof(sum('1'))`,
	// BLOB: arithmetic DOES take the rule, sum() does NOT.
	`SELECT typeof(x'3100'+0), x'3100'+0`,
	`SELECT typeof(sum(x'3100')), sum(x'3100')`,
	// Consumers that must keep the plain reading.
	`SELECT typeof(abs(cast(x'3100' as text))), abs(cast(x'3100' as text))`,
	`SELECT typeof(round(cast(x'3100' as text)))`,
	`SELECT typeof(cast(cast(x'3100' as text) as integer))`,
	`SELECT typeof(cast(cast(x'3100' as text) as numeric))`,
	`SELECT typeof(cast(cast(x'3100' as text) as real))`,
	`SELECT typeof(avg(cast(x'3100' as text))), typeof(total(cast(x'3100' as text)))`,
	`SELECT typeof(min(cast(x'3100' as text))), typeof(max(cast(x'3100' as text)))`,
	`SELECT typeof(+cast(x'3100' as text))`, // unary "+" coerces nothing at all
	`SELECT cast(x'3100' as text)=1, cast(x'3100' as text)<2`,
	`SELECT typeof(cast(x'3100' as text)||'')`,
	// sum() over more than one row, so the accumulator's own int/real decision
	// is exercised rather than just a single contribution.
	`SELECT typeof(sum(v)), sum(v) FROM (SELECT cast(x'3100' as text) AS v UNION ALL SELECT 2)`,
	`SELECT typeof(sum(v)), sum(v) FROM (SELECT cast(x'3100' as text) AS v UNION ALL SELECT cast(x'3100' as text))`,
	`SELECT typeof(sum(v)), sum(v) FROM (SELECT cast(x'3100' as text) AS v UNION ALL SELECT '1x')`,

	// --- rule 2: "%" is REAL when either operand is real ---
	`SELECT typeof(5%2), 5%2`,
	`SELECT typeof(1.5%2), 1.5%2`,
	`SELECT typeof(5%2.0), 5%2.0`,
	`SELECT typeof(5.0%2.0), 5.0%2.0`,
	`SELECT typeof('5.0'%2), '5.0'%2`,
	`SELECT typeof('5'%2), '5'%2`,
	`SELECT typeof(7.9%3), 7.9%3`,
	`SELECT typeof(-7.5%3), -7.5%3`,
	`SELECT typeof(7%0)`, // a zero divisor is NULL whatever the types
	`SELECT typeof(7.5%0)`,
	`SELECT typeof(cast(x'3100' as text)%2), cast(x'3100' as text)%2`,
}

func TestNULNumericCoercionParity(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cdb.Close()
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer p.Close()
	for _, q := range nulNumericQ {
		_, ev, eerr := p.QueryArgs(q, nil)
		if eerr != nil {
			t.Errorf("[%s] engine: %v", q, eerr)
			continue
		}
		_, cr, cerr := cgoSelect(t, cdb, q, nil)
		if cerr != nil {
			t.Errorf("[%s] cgo: %v", q, cerr)
			continue
		}
		eRows := engineRowsToStrings(ev)
		if len(eRows) == 0 {
			t.Errorf("[%s] engine returned no rows", q)
			continue
		}
		cols := make([]string, len(eRows[0]))
		for i := range cols {
			cols[i] = fmt.Sprintf("c%d", i)
		}
		if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
}
