package server

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/tursodatabase/libsql-client-go/libsql"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

func TestLibsqlClientUsesReturnedURL(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	tok := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app"}))).Msg.Token
	got := must(dbs.Get(ctx, auth(e.admin, &v1.GetDatabaseRequest{Org: "acme", Name: "app"}))).Msg.Database.Url
	if !strings.HasSuffix(got, "/db/acme/app/") {
		t.Fatalf("url %q has no trailing slash", got)
	}
	for _, u := range []string{got, strings.TrimSuffix(got, "/")} {
		conn, err := libsql.NewConnector(u, libsql.WithAuthToken(tok))
		if err != nil {
			t.Fatal(err)
		}
		db := sql.OpenDB(conn)
		if _, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS t(a)"); err != nil {
			db.Close()
			t.Fatalf("%s: %v", u, err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (1)"); err != nil {
			db.Close()
			t.Fatalf("%s: %v", u, err)
		}
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil || n < 1 {
			db.Close()
			t.Fatalf("%s: %d %v", u, n, err)
		}
		db.Close()
	}
}
