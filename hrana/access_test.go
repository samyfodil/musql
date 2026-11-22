package hrana

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	musqldriver "github.com/samyfodil/musql/driver"
)

// permServer serves one file whose tokens name their permissions:
// "big-read" reads big, "comments" reads everything and adds to comments,
// and "all" may do anything.
func permServer(t *testing.T) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "p.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE big(id INTEGER PRIMARY KEY, v)`,
		`CREATE TABLE comments(id INTEGER PRIMARY KEY, body)`,
		`INSERT INTO big VALUES (1, 'a'), (2, 'b')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	tokens := map[string][]Permission{
		"big-read":    {{Tables: []string{"big"}, Actions: []string{DataRead}}},
		"big-update":  {{Tables: []string{"big"}, Actions: []string{DataUpdate, DataDelete}}},
		"comments":    {{Actions: []string{DataRead}}, {Tables: []string{"comments"}, Actions: []string{DataAdd}}},
		"schema-big":  {{Tables: []string{"big"}, Actions: []string{SchemaUpdate, SchemaDelete}}},
		"schema-add":  {{Tables: []string{"fresh"}, Actions: []string{SchemaAdd}}},
		"everything":  {{Actions: Actions}},
		"full-access": nil,
	}
	srv := NewServer(db, WithAccess(func(tok string) (Access, error) {
		ps, ok := tokens[tok]
		if !ok {
			return Access{}, errors.New("bad token")
		}
		if ps == nil {
			return Access{}, nil
		}
		return Access{NewAuthorizer: func() musqldriver.Authorizer { return PermissionAuthorizer(ps) }}, nil
	}))
	hs := httptest.NewServer(srv)
	t.Cleanup(func() { hs.Close(); srv.Close(); db.Close() })
	return hs.URL
}

func TestPermissions(t *testing.T) {
	url := permServer(t)
	for _, c := range []struct {
		token, sql string
		refused    bool
	}{
		{"big-read", `SELECT * FROM big`, false},
		{"big-read", `SELECT count(*) FROM big`, false},
		{"big-read", `SELECT name FROM sqlite_schema`, false},
		{"big-read", `SELECT * FROM comments`, true},
		{"big-read", `INSERT INTO big VALUES (3, 'c')`, true},
		{"big-read", `CREATE TABLE fresh(x)`, true},
		{"comments", `INSERT INTO comments(body) VALUES ('hi')`, false},
		{"comments", `SELECT * FROM big JOIN comments`, false},
		{"comments", `INSERT INTO big VALUES (3, 'c')`, true},
		{"comments", `UPDATE comments SET body = 'x'`, true},
		// An UPDATE or DELETE with a WHERE reads the table too.
		{"big-update", `UPDATE big SET v = 'z'`, false},
		{"big-update", `UPDATE big SET v = 'y' WHERE id = 1`, true},
		{"big-update", `DELETE FROM big WHERE id = 2`, true},
		{"schema-big", `ALTER TABLE big ADD COLUMN w`, false},
		{"schema-big", `ALTER TABLE comments ADD COLUMN w`, true},
		{"schema-big", `CREATE INDEX bv ON big(v)`, true},
		{"schema-add", `CREATE TABLE fresh(x)`, false},
		{"schema-add", `CREATE TABLE other(x)`, true},
		{"schema-add", `DROP TABLE fresh`, true},
		{"schema-big", `DROP TABLE big`, false},
		{"everything", `ATTACH ':memory:' AS aux`, true},
		{"everything", `CREATE TABLE big(id INTEGER PRIMARY KEY, v)`, false},
		{"everything", `PRAGMA table_info(big)`, false},
		{"big-read", `PRAGMA user_version = 3`, true},
		{"full-access", `INSERT INTO comments(body) VALUES ('any')`, false},
	} {
		status, out := post(t, url, c.token, exec1(c.sql))
		if status != http.StatusOK {
			t.Fatalf("%s %s: status %d", c.token, c.sql, status)
		}
		refused := strings.Contains(out, `"SQLITE_AUTH"`)
		if refused != c.refused || !refused && strings.Contains(out, `"error"`) {
			t.Fatalf("%s %s: %s", c.token, c.sql, out)
		}
	}
}

func TestPermissionsOnWebSocket(t *testing.T) {
	url := permServer(t)
	r, err := wsSession(t, url, nil, `{"type":"hello","jwt":"comments"}`, openStream,
		execMsg("2", "INSERT INTO comments(body) VALUES ('ws')"), execMsg("3", "DELETE FROM comments"))
	if err != nil || len(r) != 4 {
		t.Fatalf("%v %q", err, r)
	}
	if !strings.Contains(r[2], "response_ok") || !strings.Contains(r[3], "SQLITE_AUTH") {
		t.Fatalf("replies %q", r)
	}
}

// A connection a limited token ran on goes back to the pool without its
// limits.
func TestPermissionsLeaveThePool(t *testing.T) {
	url := permServer(t)
	for range 3 {
		if _, out := post(t, url, "big-read", exec1("INSERT INTO comments(body) VALUES (1)")); !strings.Contains(out, "SQLITE_AUTH") {
			t.Fatalf("limited token wrote: %s", out)
		}
		if _, out := post(t, url, "full-access", exec1("INSERT INTO comments(body) VALUES (1)")); strings.Contains(out, `"error"`) {
			t.Fatalf("full token refused: %s", out)
		}
	}
}

func TestValidatePermissions(t *testing.T) {
	if err := ValidatePermissions([]Permission{{Actions: []string{DataRead, SchemaAdd}}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePermissions([]Permission{{Actions: []string{"data_insert"}}}); err == nil {
		t.Fatal("an unknown action was accepted")
	}
}
