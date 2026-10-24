package server

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqliteconv "github.com/samyfodil/musql/convert/sqlite"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// A crdt database's export is the user's database, in either format: none of
// replication's bookkeeping, and no guard refusing a plain connection's write.
func TestClusterCRDTExportIsPlain(t *testing.T) {
	nodes := newCluster(t, 2)
	acmeEverywhere(t, nodes)
	nodes[0].exec("/db/acme/app", nodes[0].admin,
		"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)", "CREATE INDEX t_v ON t(v)", "CREATE TABLE k(a, b)",
		"INSERT INTO t(v) VALUES ('a'), ('b')", "INSERT INTO k VALUES (1, x'01'), (2.5, NULL)")
	eventually(t, "the rows on node 1", func() bool { return nodes[1].count("app", "k") == 2 })
	ctx := context.Background()
	_, _, dbs := nodes[1].clients()
	dir := t.TempDir()
	for _, f := range []v1.FileFormat{v1.FileFormat_FILE_FORMAT_MUSQL, v1.FileFormat_FILE_FORMAT_SQLITE} {
		out := need(dbs.Export(ctx, auth(nodes[1].admin, &v1.ExportRequest{Org: "acme", Name: "app", Format: f})))(t)
		p := filepath.Join(dir, f.String())
		if err := os.WriteFile(p, out.Msg.Content, 0o600); err != nil {
			t.Fatal(err)
		}
		if f == v1.FileFormat_FILE_FORMAT_SQLITE {
			if err := sqliteconv.Import(p, p+".musq", sqliteconv.ImportOptions{}); err != nil {
				t.Fatal(err)
			}
			p += ".musq"
		}
		dump := dumpFile(t, p)
		if strings.Contains(dump, "_repl_") || !strings.Contains(dump, "CREATE INDEX t_v ON t(v)") || !strings.Contains(dump, "string:b|") || !strings.Contains(dump, "float64:2.5|<nil>:<nil>|") {
			t.Fatalf("%v export:\n%s", f, dump)
		}
		db, err := sql.Open("sqlite", p)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec("INSERT INTO t(v) VALUES ('c')")
		db.Close()
		if err != nil {
			t.Fatalf("a write to the %v export: %v", f, err)
		}
	}
}
