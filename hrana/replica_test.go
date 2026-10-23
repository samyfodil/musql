package hrana

import (
	"database/sql"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestReplicaRoutes: a replica runs reads itself and hands writes,
// transactions and the primary's batons to the primary. The two databases
// differ, so each answer shows where it ran.
func TestReplicaRoutes(t *testing.T) {
	open := func(name, dsn string) *sql.DB {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), name)+dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	pdb := open("p.musq", "")
	for _, q := range []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES('primary')`} {
		if _, err := pdb.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	rdir := t.TempDir()
	w, err := sql.Open("sqlite", filepath.Join(rdir, "r.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES('replica')`} {
		if _, err := w.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	rdb, err := sql.Open("sqlite", filepath.Join(rdir, "r.musq")+"?_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	primary := NewServer(pdb)
	defer primary.Close()
	readOnly := func(q string) bool {
		return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(q)), "SELECT")
	}
	replica := NewServer(rdb, WithPrimary(primary, readOnly))
	defer replica.Close()
	hs := httptest.NewServer(replica)
	defer hs.Close()

	exec := func(sql string) string { return fmt.Sprintf(`{"type":"execute","stmt":{"sql":%q}}`, sql) }
	pipe := func(baton any, reqs ...string) map[string]any {
		b := "null"
		if s, ok := baton.(string); ok {
			b = fmt.Sprintf("%q", s)
		}
		return postJSON(t, hs.URL, "/v3/pipeline", `{"baton":`+b+`,"requests":[`+strings.Join(reqs, ",")+`]}`)
	}
	value := func(out map[string]any, i int) string {
		t.Helper()
		res := out["results"].([]any)[i].(map[string]any)
		if res["type"] != "ok" {
			t.Fatalf("result %d: %v", i, res)
		}
		rows := res["response"].(map[string]any)["result"].(map[string]any)["rows"].([]any)
		var vals []string
		for _, r := range rows {
			vals = append(vals, r.([]any)[0].(map[string]any)["value"].(string))
		}
		return strings.Join(vals, ",")
	}

	out := pipe(nil, exec(`SELECT a FROM t ORDER BY a`))
	if v := value(out, 0); v != "replica" {
		t.Fatalf("a read ran at %q, want the replica", v)
	}
	local := out["baton"]
	// A write on the replica's stream goes to the primary, without the
	// replica's baton.
	out = pipe(local, exec(`INSERT INTO t VALUES('written')`), exec(`SELECT a FROM t ORDER BY a`))
	if v := value(out, 1); v != "primary,written" {
		t.Fatalf("after a write: %q", v)
	}
	// The primary's baton stays with the primary, reads included.
	out = pipe(out["baton"], exec(`SELECT a FROM t ORDER BY a`), `{"type":"close"}`)
	if v := value(out, 0); v != "primary,written" {
		t.Fatalf("on the primary's stream: %q", v)
	}
	// A transaction goes whole.
	out = pipe(nil, exec(`BEGIN`), exec(`SELECT count(*) || '' FROM t`), exec(`ROLLBACK`), `{"type":"close"}`)
	if v := value(out, 1); v != "2" {
		t.Fatalf("in a transaction: %q", v)
	}
	// A batch of reads stays.
	resp := postJSON(t, hs.URL, "/v3/pipeline", `{"requests":[{"type":"batch","batch":{"steps":[{"stmt":{"sql":"SELECT a FROM t"}}]}},{"type":"close"}]}`)
	if s := fmt.Sprint(resp["results"]); !strings.Contains(s, "replica") {
		t.Fatalf("batch read: %s", s)
	}
}
