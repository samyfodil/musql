package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/driver"
)

// Tests the transaction undo log against C SQLite. Covers all transaction
// verbs, savepoints, and rollbacks, mixed with DML on indexed and unique columns.
// After each statement, the whole table and all indexes are compared to C SQLite.
func TestTxnUndoLogMatchesOracle(t *testing.T) {
	for seed := int64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { txnUndoRun(t, seed) })
	}
}

func txnUndoRun(t *testing.T, seed int64) {
	dir := t.TempDir()
	mq, err := sql.Open(driver.DriverName, filepath.Join(dir, "m.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer mq.Close()
	mq.SetMaxOpenConns(1)
	c, err := sql.Open("sqlite3", filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetMaxOpenConns(1)
	for _, s := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, u TEXT UNIQUE, v INTEGER, w TEXT)`,
		`CREATE INDEX tv ON t(v)`,
	} {
		for _, db := range []*sql.DB{mq, c} {
			if _, err := db.Exec(s); err != nil {
				t.Fatal(s, err)
			}
		}
	}
	rng := rand.New(rand.NewSource(seed))
	inTxn, sps := false, 0
	stmt := func() string {
		switch k := rng.Intn(24); {
		case k < 8:
			or := []string{"", "OR IGNORE ", "OR REPLACE "}[rng.Intn(3)]
			return fmt.Sprintf("INSERT %sINTO t(id,u,v,w) VALUES(%d,'u%d',%d,'w%d')", or, 1+rng.Intn(60), rng.Intn(40), rng.Intn(8), rng.Intn(5))
		case k < 11:
			return fmt.Sprintf("UPDATE OR IGNORE t SET v = v + %d, u = u || 'x' WHERE id %% 5 = %d", 1+rng.Intn(3), rng.Intn(5))
		case k < 13:
			return fmt.Sprintf("DELETE FROM t WHERE v = %d", rng.Intn(8))
		case k < 14:
			return "DELETE FROM t" // the store's clear
		case k < 16:
			if !inTxn {
				inTxn = true
				return "BEGIN"
			}
			sps++
			return fmt.Sprintf("SAVEPOINT s%d", sps)
		case k < 19:
			if sps > 0 {
				return fmt.Sprintf("ROLLBACK TO s%d", 1+rng.Intn(sps)) // repeatable, keeps the savepoint
			}
			return "SELECT 1"
		case k < 20:
			if sps > 0 {
				n := 1 + rng.Intn(sps)
				s := fmt.Sprintf("RELEASE s%d", n)
				sps = n - 1
				return s
			}
			return "SELECT 1"
		case k < 22:
			if inTxn {
				inTxn, sps = false, 0
				return "ROLLBACK"
			}
			return "SELECT 1"
		}
		if inTxn {
			inTxn, sps = false, 0
			return "COMMIT"
		}
		return "SELECT 1"
	}
	dump := func(db *sql.DB) string {
		var sb strings.Builder
		for _, q := range []string{
			`SELECT id, u, v, w FROM t ORDER BY id`,
			`SELECT id FROM t WHERE v = 3 ORDER BY id`,  // through tv
			`SELECT id FROM t WHERE u = 'u7' ORDER BY id`, // through the UNIQUE index
		} {
			rs, err := db.Query(q)
			if err != nil {
				return "error: " + err.Error()
			}
			cols, _ := rs.Columns()
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			for rs.Next() {
				rs.Scan(ptrs...)
				fmt.Fprintf(&sb, "%v\n", vals)
			}
			rs.Close()
			sb.WriteString("--\n")
		}
		return sb.String()
	}
	for i := 0; i < 700; i++ {
		s := stmt()
		_, merr := mq.Exec(s)
		_, cerr := c.Exec(s)
		if (merr == nil) != (cerr == nil) {
			t.Fatalf("stmt %d %s\n  musql: %v\n  oracle: %v", i, s, merr, cerr)
		}
		if m, o := dump(mq), dump(c); m != o {
			t.Fatalf("after stmt %d (%s) the databases differ\n--- musql\n%s--- oracle\n%s", i, s, m, o)
		}
	}
}
