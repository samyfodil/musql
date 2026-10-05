// printf() width and precision are in bytes for %s/%q/%Q/%w but characters for %c.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var printfQ = []string{
	`SELECT printf('(%.6s)','הנה מה־טוב')`,
	`SELECT printf('(%.5s)','הנה מה־טוב')`,
	`SELECT printf('(%.1s)','הנה')`,
	`SELECT printf('(%.2s)','הנה')`,
	`SELECT printf('(%.3s)','abcdef')`,
	`SELECT printf('(%8.6s)','הנה מה־טוב')`,
	`SELECT printf('(%-8.3s)','abcdef')`,
	`SELECT printf('(%.6s)','aébcdef')`,
	`SELECT printf('(%.4q)','הנה מה')`,
	`SELECT printf('(%8.4q)','הנה מה')`,
	`SELECT printf('(%8s)','הנה')`,
	`SELECT printf('(%8q)','הנה')`,
	`SELECT printf('(%8.4Q)','הנה מה')`,
	`SELECT printf('(%8.4w)','הנה מה')`,
	// %c width counts characters, not bytes.
	`SELECT printf('(%8c)',char(11106))`,
	`SELECT printf('(%-8c)',char(11106))`,
	`SELECT printf('(%5.3c)',char(1492))`,
	`SELECT printf('(%-5.3c)',char(1492))`,
	`SELECT printf('(%2c)',char(1513))`,
	`SELECT printf('(%-2c)',char(1513))`,
	`SELECT printf('[%5d|%-5d|%05.2f]', 42, 42, 3.14159)`,
}

func TestPrintfByteWidthParity(t *testing.T) {
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	p, _ := edb.SnapshotPager()
	for _, q := range printfQ {
		_, ev, eerr := p.QueryArgs(q, nil)
		_, cr, cerr := cgoSelect(t, cdb, q, nil)
		if eerr != nil || cerr != nil {
			t.Fatalf("[%s] engine=%v cgo=%v", q, eerr, cerr)
		}
		eRows := engineRowsToStrings(ev)
		if ok, reason := queryResultsMatch([]string{"p"}, eRows, []string{"p"}, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
}
