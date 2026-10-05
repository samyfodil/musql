//go:build sqlite_fts5

// Package compat tests fts5 anchor and opaque constraint behavior.
package compat

import (
	"path/filepath"
	"testing"
)

func TestFts5SingleTableAnchorUnblockedByUnrelatedIndex(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t10 USING fts5(idx UNINDEXED, value)`,
		`INSERT INTO t10(idx, value) VALUES (1, 'one'),(2, 'two'),(3, 'three')`,
		`CREATE TABLE unrelated(a INTEGER, b INTEGER)`,
		`CREATE INDEX ix_unrelated ON unrelated(a)`,
	}
	// FTS5 query-language OR for the probe.
	probe := `SELECT idx, value FROM t10 WHERE t10 MATCH 'one OR two' GROUP BY idx > 1`

	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		if err := fts5Exec(t, drv, dsn[drv], setup); err != nil {
			t.Fatalf("setup(%s): %v", drv, err)
		}
	}
	goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], probe)
	cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], probe)
	switch {
	case goErr != nil:
		t.Errorf("this engine declined a query C fts5 answers\n  err: %v\n  cgo: %s", goErr, cgoOut)
	case cgoErr != nil:
		t.Fatalf("oracle rejected the probe: %v", cgoErr)
	case goOut != cgoOut:
		t.Errorf("DIVERGES from C fts5\n  go:  %q\n  cgo: %q", goOut, cgoOut)
	}
}

func TestFts5LeftJoinAnchorUnblocked(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t10 USING fts5(value)`,
		`INSERT INTO t10(value) VALUES ('apple'),('banana'),('cherry')`,
		`CREATE TABLE r(id INTEGER, tag INTEGER)`,
		`INSERT INTO r VALUES (1,10)`,
		`CREATE TABLE unrelated(a INTEGER, b INTEGER)`,
		`CREATE INDEX ix_unrelated ON unrelated(a)`,
	}
	probe := `SELECT r.tag, t10.value FROM r LEFT JOIN t10 ON t10 MATCH 'apple OR banana' GROUP BY r.tag`

	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		if err := fts5Exec(t, drv, dsn[drv], setup); err != nil {
			t.Fatalf("setup(%s): %v", drv, err)
		}
	}
	goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], probe)
	cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], probe)
	switch {
	case goErr != nil:
		t.Errorf("this engine declined a query C fts5 answers\n  err: %v\n  cgo: %s", goErr, cgoOut)
	case cgoErr != nil:
		t.Fatalf("oracle rejected the probe: %v", cgoErr)
	case goOut != cgoOut:
		t.Errorf("DIVERGES from C fts5\n  go:  %q\n  cgo: %q", goOut, cgoOut)
	}
}

// TestFts5CorrelatedMatchJoinForcedOrder is the fts5 analogue of
// anchor_fts_opaque_r17_test.go's TestFts3CorrelatedMatchJoinForcedOrderAdversarial:
// an INNER-joined, two-item FROM where t10's MATCH constraint correlates to
// EXACTLY the other item (x), built so the two candidate nestings (t10
// outer/x inner vs. x outer/t10 inner) produce DIFFERENT group anchors --
// the exact adversarial construction that would catch a naive relaxation.
func TestFts5CorrelatedMatchJoinForcedOrder(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t10 USING fts5(value)`,
		`INSERT INTO t10(value) VALUES ('apple'),('banana'),('cherry')`,
	}
	probe := `SELECT x.tag, t10.value FROM t10 JOIN
                (SELECT 'banana' AS term, 1 AS tag UNION ALL SELECT 'apple', 1) AS x
              WHERE t10 MATCH x.term GROUP BY x.tag`

	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		if err := fts5Exec(t, drv, dsn[drv], setup); err != nil {
			t.Fatalf("setup(%s): %v", drv, err)
		}
	}
	goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], probe)
	cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], probe)
	switch {
	case goErr != nil:
		t.Errorf("this engine declined a query C fts5 answers\n  err: %v\n  cgo: %s", goErr, cgoOut)
	case cgoErr != nil:
		t.Fatalf("oracle rejected the probe: %v", cgoErr)
	case goOut != cgoOut:
		t.Errorf("DIVERGES from C fts5\n  go:  %q\n  cgo: %q", goOut, cgoOut)
	}
}
