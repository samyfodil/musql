package server

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/samyfodil/musql/engine"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/store"
)

func TestMain(m *testing.M) {
	engine.Configure(engine.WithCommitHook(store.OnCommit))
	engine.RegisterFTS5()
	os.Exit(m.Run())
}

// dumpFile is a database file's whole state: schema text, then every table's
// rows in rowid order.
func dumpFile(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var b strings.Builder
	q := func(query string) {
		rows, err := db.Query(query)
		if err != nil && strings.Contains(err.Error(), "unsupported") {
			fmt.Fprintf(&b, "-- declined: %s\n", query) // an fts5 segment layout the engine will not show
			return
		}
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				if bs, ok := v.([]byte); ok {
					v = fmt.Sprintf("x'%x'", bs)
				}
				fmt.Fprintf(&b, "%T:%v|", v, v)
			}
			b.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	q(`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY type, name`)
	var tables []string
	wr := map[string]bool{}
	rows, err := db.Query(`SELECT name, wr FROM pragma_table_list WHERE schema = 'main' AND type IN ('table', 'shadow') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		var w bool
		if err := rows.Scan(&n, &w); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
		wr[n] = w
	}
	rows.Close()
	for _, tb := range tables {
		fmt.Fprintf(&b, "-- %s\n", tb)
		if wr[tb] {
			q(`SELECT * FROM "` + tb + `" ORDER BY 1`)
		} else {
			q(`SELECT rowid, * FROM "` + tb + `" ORDER BY rowid`)
		}
	}
	return b.String()
}

// exportDump exports a database and dumps the file.
func (e *env) exportDump(org, name string) string {
	e.t.Helper()
	_, _, dbs := e.clients()
	out := must(dbs.Export(context.Background(), auth(e.admin, &v1.ExportRequest{Org: org, Name: name})))
	p := filepath.Join(e.t.TempDir(), "export.musq")
	if err := os.WriteFile(p, out.Msg.Content, 0o600); err != nil {
		e.t.Fatal(err)
	}
	return dumpFile(e.t, p)
}

// exec runs statements over Hrana and fails on any error.
func (e *env) exec(path, tok string, stmts ...string) []result {
	e.t.Helper()
	status, res := e.pipeline(path, tok, stmts...)
	if status != 200 {
		e.t.Fatalf("%v: HTTP %d", stmts, status)
	}
	for i, r := range res {
		if r.Type != "ok" {
			e.t.Fatalf("%s: %s", stmts[i], r.Error.Message)
		}
	}
	return res
}

type histPoint struct {
	at   time.Time
	want string
}

// mark records the database's state now, between two commits.
func (e *env) mark(org, name string) histPoint {
	e.t.Helper()
	time.Sleep(5 * time.Millisecond)
	p := histPoint{at: time.Now(), want: e.exportDump(org, name)}
	time.Sleep(5 * time.Millisecond)
	return p
}

func TestRestoreIsExact(t *testing.T) {
	e := newEnv(t, "snapshots:\n  every: \"0\"\n")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	tok := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app"}))).Msg.Token
	const app = "/db/acme/app"

	var points []histPoint
	e.exec(app, tok, `CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT NOT NULL, b REAL)`, `CREATE TABLE k(x, y)`)
	points = append(points, e.mark("acme", "app"))
	var ins []string
	for i := range 30 {
		ins = append(ins, fmt.Sprintf(`INSERT INTO t(id, a, b) VALUES(%d, 'r%d', %d.5)`, i+1, i, i), fmt.Sprintf(`INSERT INTO k VALUES(%d, x'%02x')`, i, i))
	}
	e.exec(app, tok, ins...)
	points = append(points, e.mark("acme", "app"))
	must(dbs.CreateSnapshot(ctx, auth(e.admin, &v1.CreateSnapshotRequest{Org: "acme", Name: "app"})))
	e.exec(app, tok, `CREATE INDEX t_a ON t(a)`, `UPDATE t SET a = a || '!' WHERE id % 3 = 0`, `DELETE FROM k WHERE x % 4 = 1`)
	points = append(points, e.mark("acme", "app"))
	e.exec(app, tok, `ALTER TABLE t ADD COLUMN c INTEGER DEFAULT 7`, `UPDATE t SET c = id * 2 WHERE id < 5`, `INSERT INTO k VALUES('late', NULL)`)
	points = append(points, e.mark("acme", "app"))
	e.s.snapshotPass(ctx, time.Now())
	// What replication could not host: a trigger, FTS5, WITHOUT ROWID, and
	// rowids left as the engine assigned them.
	e.exec(app, tok, `CREATE TABLE log(id INTEGER PRIMARY KEY AUTOINCREMENT, what)`,
		`CREATE TRIGGER t_del AFTER DELETE ON t BEGIN INSERT INTO log(what) VALUES ('del ' || old.id); END`,
		`CREATE VIRTUAL TABLE ft USING fts5(body)`, `INSERT INTO ft(body) VALUES ('apple pie'), ('pear')`,
		`CREATE TABLE wr(k TEXT PRIMARY KEY, v) WITHOUT ROWID`, `INSERT INTO wr VALUES ('b', 1), ('a', 2)`,
		`DELETE FROM k WHERE x = 'late'`, `INSERT INTO k VALUES ('again', 1)`)
	points = append(points, e.mark("acme", "app"))
	e.exec(app, tok, `ALTER TABLE k RENAME TO k2`, `DROP INDEX t_a`, `DELETE FROM t WHERE id > 25`, `CREATE VIEW v AS SELECT id, a FROM t`)
	points = append(points, e.mark("acme", "app"))

	for i := range points {
		for j := range i {
			if points[i].want == points[j].want {
				t.Fatalf("points %d and %d hold the same state", j, i)
			}
		}
	}
	if !strings.Contains(points[3].want, "int64:7|") || !strings.Contains(points[3].want, "x'1c'") {
		t.Fatalf("point 3 lacks its rows:\n%s", points[3].want)
	}
	for i, p := range points {
		name := fmt.Sprintf("r%d", i)
		r := must(dbs.Restore(ctx, auth(e.admin, &v1.RestoreRequest{Org: "acme", Name: "app", At: timestamppb.New(p.at), NewName: name})))
		if r.Msg.Database.Parent == "" {
			t.Fatalf("a restore has no parent: %+v", r.Msg.Database)
		}
		if got := e.exportDump("acme", name); got != p.want {
			t.Fatalf("restore to point %d differs:\n got %s\nwant %s", i, got, p.want)
		}
	}

	// Branches: now, at a time, and schema only.
	now := e.exportDump("acme", "app")
	must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "b-now", Source: &v1.CreateDatabaseRequest_BranchOf{BranchOf: "app"}})))
	if got := e.exportDump("acme", "b-now"); got != now {
		t.Fatalf("branch differs:\n got %s\nwant %s", got, now)
	}
	must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "b-2", Source: &v1.CreateDatabaseRequest_BranchOf{BranchOf: "app"}, BranchAt: timestamppb.New(points[2].at)})))
	if got := e.exportDump("acme", "b-2"); got != points[2].want {
		t.Fatalf("branch at point 2 differs:\n got %s\nwant %s", got, points[2].want)
	}
	must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "b-schema", Source: &v1.CreateDatabaseRequest_BranchOf{BranchOf: "app"}, SchemaOnly: true})))
	schema := e.exportDump("acme", "b-schema")
	if head := strings.SplitN(now, "-- ", 2)[0]; !strings.HasPrefix(schema, head) {
		t.Fatalf("schema-only branch's schema:\n%s\nwant:\n%s", schema, head)
	}
	for _, tb := range []string{"t", "k2", "log", "wr"} {
		sec := schema[strings.Index(schema, "-- "+tb+"\n"):]
		if lines := strings.SplitN(sec, "\n", 3); len(lines) > 1 && lines[1] != "" && !strings.HasPrefix(lines[1], "-- ") {
			t.Fatalf("schema-only branch has rows in %s:\n%s", tb, sec)
		}
	}

	// The restored database is live: it takes writes and has a history.
	rtok := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "r1"}))).Msg.Token
	e.exec("/db/acme/r1", rtok, `INSERT INTO t(id, a) VALUES(1000, 'new')`)
	if sns := must(dbs.ListSnapshots(ctx, auth(e.admin, &v1.ListSnapshotsRequest{Org: "acme", Name: "r1"}))).Msg.Snapshots; len(sns) != 1 {
		t.Fatalf("a restored database has %d snapshots, want its first", len(sns))
	}

	// In place: the database becomes point 1, open streams end, and the
	// history it had is still reachable.
	before := time.Now()
	time.Sleep(5 * time.Millisecond)
	must(dbs.Restore(ctx, auth(e.admin, &v1.RestoreRequest{Org: "acme", Name: "app", At: timestamppb.New(points[1].at), InPlace: true})))
	if got := e.exportDump("acme", "app"); got != points[1].want {
		t.Fatalf("in-place restore differs:\n got %s\nwant %s", got, points[1].want)
	}
	res := e.exec(app, tok, `SELECT count(*) FROM t`)
	if v := res[0].Response.Result.Rows[0][0].Value; v != "30" {
		t.Fatalf("after an in-place restore, %v rows", v)
	}
	e.exec(app, tok, `DELETE FROM t WHERE id = 1`)
	after := e.mark("acme", "app")
	for name, p := range map[string]histPoint{"old3": points[3], "old-end": {at: before, want: now}, "new": after} {
		must(dbs.Restore(ctx, auth(e.admin, &v1.RestoreRequest{Org: "acme", Name: "app", At: timestamppb.New(p.at), NewName: name})))
		if got := e.exportDump("acme", name); got != p.want {
			t.Fatalf("restore %s after an in-place restore differs:\n got %s\nwant %s", name, got, p.want)
		}
	}

	// Declined: before the history starts, and in the future.
	for _, at := range []time.Time{points[0].at.Add(-time.Hour), time.Now().Add(time.Hour)} {
		_, err := dbs.Restore(ctx, auth(e.admin, &v1.RestoreRequest{Org: "acme", Name: "app", At: timestamppb.New(at), NewName: "nope"}))
		if code(err) != connect.CodeFailedPrecondition {
			t.Fatalf("restore to %v: %v", at, err)
		}
	}
}

func TestSnapshotPrune(t *testing.T) {
	e := newEnv(t, "snapshots:\n  every: \"0\"\n  keep: \"1h\"\n")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	tok := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app"}))).Msg.Token
	e.exec("/db/acme/app", tok, `CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`)
	p1 := e.mark("acme", "app")
	must(dbs.CreateSnapshot(ctx, auth(e.admin, &v1.CreateSnapshotRequest{Org: "acme", Name: "app"})))
	e.exec("/db/acme/app", tok, `INSERT INTO t VALUES(2)`)
	p2 := e.mark("acme", "app")
	e.exec("/db/acme/app", tok, `INSERT INTO t VALUES(3)`)
	// Two hours on, everything but the newest base is past keep.
	e.s.snapshotPass(ctx, time.Now().Add(2*time.Hour))
	sns := must(dbs.ListSnapshots(ctx, auth(e.admin, &v1.ListSnapshotsRequest{Org: "acme", Name: "app"}))).Msg.Snapshots
	if len(sns) != 1 {
		t.Fatalf("%d snapshots kept, want the new one", len(sns))
	}
	for _, p := range []histPoint{p1, p2} {
		_, err := dbs.Restore(ctx, auth(e.admin, &v1.RestoreRequest{Org: "acme", Name: "app", At: timestamppb.New(p.at), NewName: "x"}))
		if code(err) != connect.CodeFailedPrecondition {
			t.Fatalf("restore behind the oldest snapshot: %v", err)
		}
	}
	files, err := filepath.Glob(filepath.Join(e.s.cfg.DataDir, "history", "*", "*", "base-*.musq"))
	if err != nil || len(files) != 1 {
		t.Fatalf("snapshot files: %v %v", files, err)
	}
}
