package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samyfodil/musql/hrana"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

func TestNormalizeSQL(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT * FROM t WHERE a = 1 AND b = 'x''y'":        "SELECT * FROM t WHERE a = ? AND b = ?",
		"insert into t2 values (1.5e-3, x'00ff', -4);  ":    "insert into t2 values (?, ?, -?)",
		"SELECT \"col 1\", t3.c2 FROM [t 3]\n\tWHERE id=?1": "SELECT \"col 1\", t3.c2 FROM [t 3] WHERE id=?1",
	} {
		if got := normalizeSQL(in); got != want {
			t.Errorf("normalizeSQL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUsage(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	tok := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app"}))).Msg.Token
	e.exec("/db/acme/app", tok, `CREATE TABLE t(a)`, `INSERT INTO t VALUES (1)`, `INSERT INTO t VALUES (2)`, `INSERT INTO t VALUES (3)`, `SELECT a FROM t`)
	e.pipeline("/db/acme/app", tok, `SELECT * FROM nope`)
	must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "two"})))
	tok2 := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "two"}))).Msg.Token
	e.exec("/db/acme/two", tok2, `SELECT 1`)

	u := must(dbs.Usage(ctx, auth(e.admin, &v1.UsageRequest{Org: "acme", Name: "app"}))).Msg
	if len(u.Points) != 1 {
		t.Fatalf("%d points", len(u.Points))
	}
	p := u.Points[0]
	if p.Statements != 6 || p.RowsWritten != 3 || p.RowsRead != 3 || p.Errors != 1 || p.BytesStored == 0 || p.P95Ms <= 0 {
		t.Fatalf("usage %+v", p)
	}
	org := must(dbs.Usage(ctx, auth(e.admin, &v1.UsageRequest{Org: "acme"}))).Msg
	if len(org.Points) != 1 || org.Points[0].Statements != 7 || len(org.Databases) != 2 || org.Databases[0].Name != "app" {
		t.Fatalf("org usage %+v", org)
	}
	st := must(dbs.Stats(ctx, auth(e.admin, &v1.StatsRequest{Org: "acme", Name: "app"}))).Msg
	found := false
	for _, q := range st.TopQueries {
		if q.Sql == "INSERT INTO t VALUES (?)" {
			found = q.Count == 3 && q.RowsWritten == 3
		}
	}
	if !found {
		t.Fatalf("top queries %+v", st.TopQueries)
	}
	// Counts survive a restart: flushed at Close, read back from the state.
	if err := e.s.meter.flush(ctx); err != nil {
		t.Fatal(err)
	}
	again := must(dbs.Usage(ctx, auth(e.admin, &v1.UsageRequest{Org: "acme", Name: "app", From: nil, To: nil}))).Msg
	if again.Points[0].Statements != 6 {
		t.Fatalf("usage counted twice: %+v", again.Points[0])
	}
}

// BenchmarkObserver is what metering adds to one statement: the observer
// alone, and a statement through the Hrana stream with and without it.
func BenchmarkObserver(b *testing.B) {
	m := newMeter(nil, func(string) int64 { return 0 })
	obs := m.observer("db", "org")
	st := hrana.Stmt{SQL: "SELECT a, b FROM t WHERE id = 42", Duration: 37 * time.Microsecond, RowsRead: 1}
	b.Run("observer", func(b *testing.B) {
		for b.Loop() {
			obs(st)
		}
	})
	b.Run("observer-parallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				obs(st)
			}
		})
	})
}

// BenchmarkHranaStatement times a point read through Hrana's HTTP pipeline,
// with and without the meter, so the meter's share is visible.
func BenchmarkHranaStatement(b *testing.B) {
	for _, metered := range []bool{false, true} {
		name := "plain"
		if metered {
			name = "metered"
		}
		b.Run(name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "b.musq")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, a, b); INSERT INTO t VALUES (42, 'x', 1.5)`); err != nil {
				b.Fatal(err)
			}
			var opts []hrana.Option
			if metered {
				opts = append(opts, hrana.WithObserver(newMeter(nil, func(string) int64 { return 0 }).observer("db", "org")))
			}
			benchPipeline(b, hrana.NewServer(db, opts...))
		})
	}
}

// benchPipeline runs one statement per request on one stream, carrying the
// baton, so each iteration is a statement and not a stream setup.
func benchPipeline(b *testing.B, srv *hrana.Server) {
	defer srv.Close()
	baton := "null"
	for b.Loop() {
		body := `{"baton":` + baton + `,"requests":[{"type":"execute","stmt":{"sql":"SELECT a, b FROM t WHERE id = 42"}}]}`
		req := httptest.NewRequest("POST", "/v2/pipeline", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		var out struct {
			Baton string `json:"baton"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Baton == "" {
			b.Fatalf("%d %s", w.Code, w.Body)
		}
		baton = `"` + out.Baton + `"`
	}
}
