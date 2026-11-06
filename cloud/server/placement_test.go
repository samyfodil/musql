package server

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/state"
)

// TestTypesAndCopies: a raft database reads back as raft; a file holding a
// virtual table cannot become a cluster database of either kind, and one
// holding a trigger only a regular one.
func TestTypesAndCopies(t *testing.T) {
	if typeToProto(state.TypeRaft) != v1.DatabaseType_DATABASE_TYPE_RAFT {
		t.Error("a raft database reads back as another type")
	}
	e := newEnv(t, "")
	ctx := context.Background()
	file := func(name string, stmts ...string) string {
		path := filepath.Join(t.TempDir(), name)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, q := range stmts {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		return path
	}
	for _, typ := range []string{state.TypeCRDT, state.TypePlain} {
		path := file("vt.musq", `CREATE VIRTUAL TABLE f USING fts4(body)`)
		if err := e.s.checkReplicable(ctx, &state.Database{Type: typ}, path); code(err) != connect.CodeInvalidArgument {
			t.Errorf("a file with a virtual table as a %s cluster database: %v", typ, err)
		}
		path = file("trig.musq", `CREATE TABLE t(id INTEGER PRIMARY KEY)`, `CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END`)
		err := e.s.checkReplicable(ctx, &state.Database{Type: typ}, path)
		if typ == state.TypeCRDT && code(err) != connect.CodeInvalidArgument || typ == state.TypePlain && err != nil {
			t.Errorf("a file with a trigger as a %s cluster database: %v", typ, err)
		}
	}
}
