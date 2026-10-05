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

// This file tests foreign key parent probes against the oracle.
// Tests verify that the parent lookup finds the correct candidates and
// handles affinity conversions correctly.
func TestFKParentProbeMatchesOracle(t *testing.T) {
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

	setup := []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY, name TEXT COLLATE NOCASE UNIQUE, a, b, UNIQUE(a, b))`,
		`CREATE TABLE c(id INTEGER PRIMARY KEY, pid REFERENCES p(id), pname TEXT REFERENCES p(name), x, y, FOREIGN KEY(x, y) REFERENCES p(a, b))`,
	}
	for i := 1; i <= 12; i++ {
		setup = append(setup, fmt.Sprintf(`INSERT INTO p VALUES(%d, 'N%d', %d, 'k%d')`, i, i, i%4, i))
	}
	for _, s := range setup {
		if _, err := mq.Exec(s); err != nil {
			t.Fatalf("musql %s: %v", s, err)
		}
		if _, err := c.Exec(s); err != nil {
			t.Fatalf("oracle %s: %v", s, err)
		}
	}

	rng := rand.New(rand.NewSource(928))
	pid := func() string {
		n := 1 + rng.Intn(15)
		return []string{fmt.Sprint(n), fmt.Sprintf("'%d'", n), fmt.Sprintf("%d.0", n), "NULL", "' 3'", "'x'"}[rng.Intn(6)]
	}
	pname := func() string {
		n := 1 + rng.Intn(15)
		return []string{fmt.Sprintf("'N%d'", n), fmt.Sprintf("'n%d'", n), fmt.Sprintf("'N%d '", n), "NULL"}[rng.Intn(4)]
	}
	xy := func() (string, string) {
		n := 1 + rng.Intn(15)
		x := []string{fmt.Sprint(n % 4), fmt.Sprintf("'%d'", n%4), fmt.Sprintf("%d.0", n%4), "NULL"}[rng.Intn(4)]
		return x, fmt.Sprintf("'k%d'", n)
	}
	stmt := func() string {
		switch k := rng.Intn(10); {
		case k < 5:
			x, y := xy()
			return fmt.Sprintf("INSERT INTO c(pid, pname, x, y) VALUES(%s, %s, %s, %s)", pid(), pname(), x, y)
		case k < 7:
			x, y := xy()
			return fmt.Sprintf("UPDATE c SET pid = %s, pname = %s, x = %s, y = %s WHERE id %% 4 = %d", pid(), pname(), x, y, rng.Intn(4))
		case k < 8:
			return fmt.Sprintf("DELETE FROM p WHERE id = %d", 1+rng.Intn(15))
		case k < 9:
			n := 1 + rng.Intn(15)
			return fmt.Sprintf("INSERT OR REPLACE INTO p VALUES(%d, 'n%d', %d, 'k%d')", n, n, n%4, n)
		}
		return fmt.Sprintf("DELETE FROM c WHERE id %% 5 = %d", rng.Intn(5))
	}
	dump := func(db *sql.DB) string {
		var sb strings.Builder
		for _, q := range []string{
			`SELECT id, quote(name), quote(a), quote(b) FROM p ORDER BY id`,
			`SELECT id, quote(pid) || '|' || quote(pname), quote(x), quote(y) FROM c ORDER BY id`,
		} {
			rows, err := db.Query(q)
			if err != nil {
				return "error: " + err.Error()
			}
			for rows.Next() {
				var id int64
				var a, b, cc string
				rows.Scan(&id, &a, &b, &cc)
				fmt.Fprintf(&sb, "%d|%s|%s|%s\n", id, a, b, cc)
			}
			rows.Close()
			sb.WriteString("--\n")
		}
		return sb.String()
	}
	accepted := 0
	for i := 0; i < 2000; i++ {
		s := stmt()
		_, merr := mq.Exec(s)
		_, cerr := c.Exec(s)
		if (merr == nil) != (cerr == nil) {
			t.Fatalf("stmt %d %s\n  musql: %v\n  oracle: %v", i, s, merr, cerr)
		}
		if cerr == nil && strings.HasPrefix(s, "INSERT INTO c") {
			accepted++
		}
	}
	if m, o := dump(mq), dump(c); m != o {
		t.Fatalf("final tables differ\n--- musql\n%s--- oracle\n%s", m, o)
	}
	// The generator must actually reach both answers, or it proves nothing.
	if accepted == 0 {
		t.Fatal("no child insert was accepted: the generator never reached the found-parent case")
	}
}
