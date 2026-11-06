package state

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samyfodil/musql/hrana"
	"github.com/samyfodil/musql/replication"
)

// TestFollowerStore: a follower's Store runs every call on the master's
// state, over Hrana: what it writes the master holds, it reads its own writes
// back at once, a transaction commits or rolls back there, and the master's
// errors keep their meaning. With no master, its own copy answers reads,
// unless it has yet to sync once.
func TestFollowerStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.musq")
	writer := func() bool { return true }
	db, err := replication.Open(ctx, path, replication.Leader(writer))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	master, err := OpenReplicated(db, "m", &Master{Here: writer})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	srv := httptest.NewServer(hrana.NewServer(db))
	t.Cleanup(srv.Close)

	// The follower's own copy has the schema and no rows: anything it answers
	// came from the master. It writes there only while it starts, as a
	// cluster node does.
	var electing, starting, unsynced atomic.Bool
	starting.Store(true)
	fdb, err := replication.Open(ctx, filepath.Join(t.TempDir(), "state.musq"), replication.Leader(starting.Load))
	if err != nil {
		t.Fatal(err)
	}
	url := func() (string, error) {
		if electing.Load() {
			return "", ErrNoMaster
		}
		return srv.URL + "/v3-protobuf/pipeline", nil
	}
	local := func() bool { return !unsynced.Load() }
	follower, err := OpenReplicated(fdb, "f", &Master{Here: starting.Load, Client: srv.Client(), URL: url, Local: local})
	if err != nil {
		fdb.Close()
		t.Fatal(err)
	}
	starting.Store(false)
	t.Cleanup(func() { follower.Close() })

	u, err := follower.CreateUser(ctx, "a@example.com", "A", HashPassword("p"), false)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := follower.UserByEmail(ctx, "a@example.com"); err != nil || got.ID != u.ID {
		t.Fatalf("the follower does not read its own write: %v %v", got, err)
	}
	if got, err := master.UserByEmail(ctx, "a@example.com"); err != nil || got.ID != u.ID {
		t.Fatalf("the master does not hold the follower's write: %v %v", got, err)
	}
	if _, err := follower.CreateUser(ctx, "a@example.com", "Again", HashPassword("p"), false); !errors.Is(err, ErrExists) {
		t.Fatalf("a second user with one email: %v, want ErrExists", err)
	}
	// A transaction: CreateOrg writes the org, its default group and the
	// membership together.
	o, err := follower.CreateOrg(ctx, "shop", "Shop", u.ID, "fra")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := master.Group(ctx, o.ID, "default"); err != nil {
		t.Fatalf("the org's group is not on the master: %v", err)
	}
	// One that fails rolls back on the master, and the connection is usable
	// after.
	if _, err := follower.CreateOrg(ctx, "shop", "Shop again", u.ID, "fra"); !errors.Is(err, ErrExists) {
		t.Fatalf("a second org with one slug: %v, want ErrExists", err)
	}
	orgs, err := master.AllOrgs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var shops int
	for _, o := range orgs {
		if o.Slug == "shop" || o.Name == "Shop again" {
			shops++
		}
	}
	if shops != 1 {
		t.Fatalf("%d orgs for one slug on the master", shops)
	}
	if _, err := follower.OrgBySlug(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	// No master: reads answer from this node's copy, and a write waits for a
	// master rather than failing at once.
	electing.Store(true)
	if _, err := follower.OrgBySlug(ctx, "shop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("with no master, the follower's own copy answers: %v, want ErrNotFound (it holds no rows)", err)
	}
	// A copy that has yet to sync once answers nothing: its "not found" could
	// be a record it has not received.
	unsynced.Store(true)
	if _, err := follower.OrgBySlug(ctx, "shop"); !errors.Is(err, ErrNoMaster) {
		t.Fatalf("with no master, an unsynced copy: %v, want ErrNoMaster", err)
	}
	unsynced.Store(false)
	start := time.Now()
	go func() {
		time.Sleep(300 * time.Millisecond)
		electing.Store(false)
	}()
	if _, err := follower.CreateOrg(ctx, "later", "Later", u.ID, "fra"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("the write did not wait for a master")
	}
	// The master stops before the election notices: a read answers from
	// this node's copy instead of failing.
	srv.Close()
	if _, err := follower.OrgBySlug(ctx, "shop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("with the master gone, the follower's own copy answers: %v, want ErrNotFound (it holds no rows)", err)
	}
	unsynced.Store(true)
	if _, err := follower.OrgBySlug(ctx, "shop"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("with the master gone, an unsynced copy: %v, want the master's absence", err)
	}
}
