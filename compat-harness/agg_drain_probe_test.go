package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// zzProbe runs setup on both engines, then every query, reporting divergences.
// Unlike flCompareQuery it does NOT stop on accept/reject: it prints the pair,
// so a "musql errors where C answers" shows up loudly.
func zzProbe(t *testing.T, setup []string, queries []string) {
	t.Helper()
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1)
	for _, s := range setup {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine setup %s: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup %s: %v", s, err)
		}
	}
	for _, q := range queries {
		func() {
			p, err := edb.SnapshotPager()
			if err != nil {
				t.Fatalf("SnapshotPager: %v", err)
			}
			defer p.Close()
			ec, ev, eerr := p.QueryArgs(q, nil)
			cc, cr, cerr := cgoSelect(t, cdb, q, nil)
			switch {
			case eerr != nil && cerr != nil:
				if !strings.EqualFold(eerr.Error(), cerr.Error()) {
					t.Logf("BOTH-ERR %s\n  engine: %v\n  cgo:    %v", q, eerr, cerr)
				}
			case eerr != nil && cerr == nil:
				t.Errorf("ENGINE-ONLY-ERROR %s\n  engine: %v\n  cgo:    %v %v", q, eerr, cc, cr)
			case eerr == nil && cerr != nil:
				t.Errorf("CGO-ONLY-ERROR %s\n  cgo: %v\n  engine: %v %v", q, cerr, ec, engineRowsToStrings(ev))
			default:
				if ok, reason := queryResultsMatch(ec, engineRowsToStrings(ev), cc, cr, true); !ok {
					t.Errorf("DIVERGES %s: %s\n  engine: %v %v\n  cgo:    %v %v", q, reason, ec, engineRowsToStrings(ev), cc, cr)
				}
			}
		}()
	}
}
