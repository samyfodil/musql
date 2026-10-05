package compat

// Tests that outer join with LIMIT terminates and returns correct results.
// Each case runs under a deadline to catch hangs quickly.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// runDeadline executes statements with a timeout to catch hangs.
func runDeadline(t *testing.T, engine string, stmts []string, d time.Duration) []map[string]any {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stmts-*.json")
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := json.Marshal(stmts)
	f.Write(enc)
	f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, workerBin[engine], f.Name())
	cmd.Env = append(os.Environ(), "COMPAT_DSN="+filepath.Join(t.TempDir(), engine+".db"))
	out, err := cmd.Output()
	if ctx.Err() != nil {
		t.Fatalf("%s worker did not terminate within %s on: %v", engine, d, stmts)
	}
	if err != nil {
		t.Fatalf("%s worker failed: %v", engine, err)
	}
	var res []map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("%s: bad worker output: %v\n%s", engine, err, out)
	}
	return res
}

func TestR27OuterJoinLimitTerminates(t *testing.T) {
	setup := []string{
		"CREATE TABLE ja(a1,a2)",
		"INSERT INTO ja VALUES('a1',1),('a2',2)",
		"CREATE TABLE jb(b1,b2)",
		"INSERT INTO jb VALUES(2,'b0'),(1,'b1'),(2,'b4'),(1,'b5')",
	}
	for _, q := range []string{
		"SELECT ja.a1, jb.b2 FROM ja LEFT JOIN jb ON ja.a2=jb.b1 LIMIT 3",
		"SELECT ja.a1, jb.b2 FROM ja LEFT JOIN jb ON ja.a2=jb.b1 LIMIT 1",
		"SELECT ja.a1, jb.b2 FROM ja LEFT JOIN jb ON ja.a2=jb.b1 LIMIT 2 OFFSET 1",
		"SELECT DISTINCT ja.a1 FROM ja LEFT JOIN jb ON ja.a2=jb.b1 LIMIT 1",
		"SELECT ja.a1, jb.b2 FROM ja RIGHT JOIN jb ON ja.a2=jb.b1 LIMIT 2",
		"SELECT ja.a1, jb.b2 FROM ja FULL JOIN jb ON ja.a2=jb.b1 LIMIT 2",
		"SELECT ja.a1 FROM ja LEFT JOIN jb ON ja.a2=jb.b1 LEFT JOIN jb x ON x.b1=ja.a2 LIMIT 2",
		"SELECT (SELECT ja.a1 FROM ja LEFT JOIN jb ON ja.a2=jb.b1 LIMIT 1)",
		// Control cases: unreachable LIMIT or non-matching join
		"SELECT ja.a1, jb.b2 FROM ja LEFT JOIN jb ON ja.a2=jb.b1 LIMIT 9",
		"SELECT ja.a1, jb.b2 FROM ja LEFT JOIN jb ON ja.a2=99 LIMIT 1",
	} {
		stmts := append(append([]string{}, setup...), q)
		cgo, _ := json.Marshal(runDeadline(t, "cgo", stmts, 30*time.Second))
		mush, _ := json.Marshal(runDeadline(t, "musql", stmts, 30*time.Second))
		if string(cgo) != string(mush) {
			t.Errorf("%s DIVERGES\n  cgo:    %s\n  musql: %s", q, cgo, mush)
		}
	}
}
