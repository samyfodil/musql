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

// TestTypesAndCopies: a raft database reads back as raft, and a file
// holding what replication refuses cannot become a cluster database of
// either kind.
func TestTypesAndCopies(t *testing.T) {
	if typeToProto(state.TypeRaft) != v1.DatabaseType_DATABASE_TYPE_RAFT {
		t.Error("a raft database reads back as another type")
	}
	e := newEnv(t, "")
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "trig.musq")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`CREATE TABLE t(id INTEGER PRIMARY KEY)`, `CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	for _, typ := range []string{state.TypeCRDT, state.TypePlain} {
		if err := e.s.checkReplicable(ctx, &state.Database{Type: typ}, path); code(err) != connect.CodeInvalidArgument {
			t.Errorf("a file with a trigger as a %s cluster database: %v", typ, err)
		}
	}
}
