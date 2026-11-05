package state

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/samyfodil/musql/replication"
)

// openClusterState opens a replicated state for node on net, a master: two
// of them are two masters overlapping around an election.
func openClusterState(t *testing.T, net *replication.MemNetwork, node string) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.musq")
	master := func() bool { return true }
	db, err := replication.Open(context.Background(), path, replication.Leader(master), replication.WithSyncInterval(50*time.Millisecond),
		replication.WithTransport(func(ctx context.Context, s *replication.Syncer) (replication.Transport, error) {
			return net.AddNode(replication.PeerID(node), s.OpSource()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenReplicated(db, node, &Master{Here: master})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func eventually(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func orgSlugs(t *testing.T, s *Store) []string {
	t.Helper()
	orgs, err := s.AllOrgs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, o := range orgs {
		if !o.Personal {
			out = append(out, o.Slug)
		}
	}
	sort.Strings(out)
	return out
}

func TestClusterStateConflictingCreates(t *testing.T) {
	ctx := context.Background()
	net := replication.NewMemNetwork()
	a := openClusterState(t, net, "a")
	b := openClusterState(t, net, "b")
	net.Partition("a", "b", true)

	ua, err := a.CreateUser(ctx, "Same@Example.com", "From A", HashPassword("pa"), false)
	if err != nil {
		t.Fatal(err)
	}
	ub, err := b.CreateUser(ctx, "same@example.com", "From B", HashPassword("pb"), false)
	if err != nil {
		t.Fatal(err)
	}
	if ua.ID != ub.ID {
		t.Fatalf("one email, two user IDs: %s %s", ua.ID, ub.ID)
	}
	oa, err := a.CreateOrg(ctx, "shop", "Shop A", ua.ID, "fra")
	if err != nil {
		t.Fatal(err)
	}
	ob, err := b.CreateOrg(ctx, "shop", "Shop B", ub.ID, "iad")
	if err != nil {
		t.Fatal(err)
	}
	net.Partition("a", "b", false)

	// Replication holds one of the two out; resolving on both nodes brings it
	// back renamed, the same name everywhere.
	want := func(s *Store) bool {
		got := orgSlugs(t, s)
		return len(got) == 2 && got[0] == "shop" && (got[1] == "shop-"+oa.ID[:6] || got[1] == "shop-"+ob.ID[:6])
	}
	eventually(t, "the held org to come back renamed on both nodes", func() bool {
		for _, s := range []*Store{a, b} {
			if _, err := s.ResolveHeld(ctx); err != nil {
				t.Fatal(err)
			}
		}
		return want(a) && want(b)
	})
	if ga, gb := orgSlugs(t, a), orgSlugs(t, b); ga[1] != gb[1] {
		t.Fatalf("nodes disagree: %v %v", ga, gb)
	}
	eventually(t, "one user on both nodes", func() bool {
		la, errA := a.ListUsers(ctx)
		lb, errB := b.ListUsers(ctx)
		return errA == nil && errB == nil && len(la) == 1 && len(lb) == 1 && la[0].Name == lb[0].Name
	})
}

func TestClusterStateUsageIsPerNode(t *testing.T) {
	ctx := context.Background()
	net := replication.NewMemNetwork()
	a := openClusterState(t, net, "a")
	b := openClusterState(t, net, "b")
	hour := time.Unix(3600*1000, 0).UTC()
	for _, s := range []*Store{a, b} {
		if err := s.AddUsage(ctx, []*UsageHour{{DBID: "d", OrgID: "o", Hour: hour, Statements: 3}}, []*QueryUsage{{DBID: "d", Hour: hour, SQL: "SELECT ?", Count: 2}}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "both nodes' usage summed", func() bool {
		for _, s := range []*Store{a, b} {
			h, err := s.UsageHours(ctx, "o", "", hour, hour.Add(time.Hour))
			if err != nil || len(h) != 1 || h[0].Statements != 6 {
				return false
			}
			q, err := s.TopQueries(ctx, "d", hour, hour.Add(time.Hour), 10)
			if err != nil || len(q) != 1 || q[0].Count != 4 {
				return false
			}
		}
		return true
	})
}
