package server

import (
	"context"
	"testing"
	"time"

	"github.com/samyfodil/musql/cloud/state"
)

// TestSnapshotPass: a pass snapshots a database changed since its last base,
// passes over a record whose file is not here and a crdt database placed
// elsewhere, and takes nothing from a store it cannot read.
func TestSnapshotPass(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	o := must(e.s.store.OrgBySlug(ctx, "acme"))
	g := must(e.s.store.Group(ctx, o.ID, "default"))
	for _, d := range []*state.Database{
		{OrgID: o.ID, Name: "nofile", GroupID: g.ID},
		{OrgID: o.ID, Name: "elsewhere", GroupID: g.ID, Type: state.TypeCRDT},
	} {
		if err := e.s.store.CreateDatabase(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	h := must(e.s.history(must(e.s.store.Database(ctx, o.ID, "app"))))
	before := len(h.Snapshots())
	e.exec("/db/acme/app", e.admin, "CREATE TABLE t(a)")
	e.s.snapshotPass(ctx, time.Now())
	if n := len(h.Snapshots()); n != before+1 {
		t.Fatalf("%d snapshots after a change, want %d", n, before+1)
	}
	e.exec("/db/acme/app", e.admin, "INSERT INTO t VALUES (1)")
	restore := e.breakTable("databases")
	defer restore()
	e.s.snapshotPass(ctx, time.Now())
	if n := len(h.Snapshots()); n != before+1 {
		t.Fatalf("a pass over a store it cannot read took a snapshot: %d", n)
	}
}
