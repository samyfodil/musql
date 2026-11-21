package server

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/tursodatabase/libsql-client-go/libsql"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// A token limited per table and action, as Turso's fine-grained permissions:
// one that reads big reads big and the schema and nothing else; UPDATE and
// DELETE with a WHERE need data_read as well.
func TestTokenPermissions(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	url := must(dbs.Get(ctx, auth(e.admin, &v1.GetDatabaseRequest{Org: "acme", Name: "app"}))).Msg.Database.Url
	open := func(perms ...*v1.TokenPermission) *sql.DB {
		tok := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app", Permissions: perms}))).Msg.Token
		conn, err := libsql.NewConnector(url, libsql.WithAuthToken(tok))
		if err != nil {
			t.Fatal(err)
		}
		db := sql.OpenDB(conn)
		t.Cleanup(func() { db.Close() })
		return db
	}
	full := open()
	for _, s := range []string{
		`CREATE TABLE big(id INTEGER PRIMARY KEY, v)`,
		`CREATE TABLE comments(id INTEGER PRIMARY KEY, body)`,
		`INSERT INTO big VALUES (1, 'a')`,
	} {
		if _, err := full.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(db *sql.DB, q string) bool {
		t.Helper()
		_, err := db.ExecContext(ctx, q)
		if err != nil && !strings.Contains(err.Error(), "not authorized") && !strings.Contains(err.Error(), "prohibited") {
			t.Fatalf("%s: %v", q, err)
		}
		return err != nil
	}

	read := open(&v1.TokenPermission{Tables: []string{"big"}, Actions: []string{"data_read"}})
	var n int
	if err := read.QueryRowContext(ctx, `SELECT count(*) FROM big`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("read big: %d %v", n, err)
	}
	if err := read.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema`).Scan(&n); err != nil || n < 2 {
		t.Fatalf("read the schema: %d %v", n, err)
	}
	if !refused(read, `INSERT INTO big VALUES (2, 'b')`) || !refused(read, `CREATE TABLE more(x)`) || !refused(read, `SELECT * FROM comments`) {
		t.Fatal("a data_read token wrote or read another table")
	}

	app := open(&v1.TokenPermission{Actions: []string{"data_read"}},
		&v1.TokenPermission{Tables: []string{"comments"}, Actions: []string{"data_add"}})
	if refused(app, `INSERT INTO comments(body) VALUES ('hi')`) || !refused(app, `INSERT INTO big VALUES (3, 'c')`) {
		t.Fatal("all:data_read comments:data_add")
	}

	upd := open(&v1.TokenPermission{Tables: []string{"big"}, Actions: []string{"data_update", "data_delete"}})
	if refused(upd, `UPDATE big SET v = 'z'`) || !refused(upd, `UPDATE big SET v = 'y' WHERE id = 1`) || !refused(upd, `DELETE FROM big WHERE id = 1`) {
		t.Fatal("UPDATE and DELETE with a WHERE without data_read")
	}

	schema := open(&v1.TokenPermission{Tables: []string{"big"}, Actions: []string{"schema_update"}},
		&v1.TokenPermission{Tables: []string{"fresh"}, Actions: []string{"schema_add", "schema_delete"}})
	if refused(schema, `ALTER TABLE big ADD COLUMN w`) || !refused(schema, `DROP TABLE big`) ||
		refused(schema, `CREATE TABLE fresh(x)`) || refused(schema, `DROP TABLE fresh`) {
		t.Fatal("schema_add, schema_update, schema_delete")
	}

	_, err := dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app",
		Permissions: []*v1.TokenPermission{{Actions: []string{"data_insert"}}}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("an unknown action was accepted: %v", err)
	}
}
