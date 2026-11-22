package state

import (
	"context"
	"errors"
	"testing"
)

func TestSoftDeleteAndRestore(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "a@example.com", "A", HashPassword("pw"), false)
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(ctx, "acme", "Acme", u.ID, "fra")
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGroup(ctx, org.ID, "eu", "fra", "")
	if err != nil {
		t.Fatal(err)
	}
	d := &Database{OrgID: org.ID, Name: "app", GroupID: g.ID}
	if err := s.CreateDatabase(ctx, d); err != nil {
		t.Fatal(err)
	}

	on, off := true, false
	if err := s.UpdateDatabase(ctx, d.ID, &on, nil, &on); err != nil {
		t.Fatal(err)
	}
	got, err := s.DatabaseByID(ctx, d.ID)
	if err != nil || !got.DeleteProtection || got.BlockReads || !got.BlockWrites {
		t.Fatalf("after update: %+v %v", got, err)
	}
	if err := s.SoftDeleteDatabase(ctx, d.ID); !errors.Is(err, ErrProtected) {
		t.Fatalf("delete a protected database: %v", err)
	}
	if _, err := s.DeleteGroup(ctx, g.ID); !errors.Is(err, ErrProtected) {
		t.Fatalf("delete a group holding a protected database: %v", err)
	}
	if err := s.UpdateDatabase(ctx, d.ID, &off, nil, nil); err != nil {
		t.Fatal(err)
	}

	if err := s.SoftDeleteDatabase(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DatabaseByID(ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted database is still found: %v", err)
	}
	// Its name is free at once.
	reuse := &Database{OrgID: org.ID, Name: "app", GroupID: g.ID}
	if err := s.CreateDatabase(ctx, reuse); err != nil {
		t.Fatalf("reuse a deleted database's name: %v", err)
	}
	del, err := s.DeletedDatabases(ctx, org.ID)
	if err != nil || len(del) != 1 || del[0].ID != d.ID || del[0].DeletedAt.IsZero() || !del[0].BlockWrites {
		t.Fatalf("deleted: %+v %v", del, err)
	}
	if _, err := s.RestoreDatabase(ctx, d.ID, ""); !errors.Is(err, ErrExists) {
		t.Fatalf("restore under a taken name: %v", err)
	}
	r, err := s.RestoreDatabase(ctx, d.ID, "app-old")
	if err != nil || r.Name != "app-old" || r.TokenGen != got.TokenGen+1 || !r.BlockWrites {
		t.Fatalf("restored: %+v %v", r, err)
	}
	if del, _ := s.DeletedDatabases(ctx, ""); len(del) != 0 {
		t.Fatalf("still listed as deleted: %+v", del)
	}

	// A group goes with its databases and comes back with them.
	if err := s.SetGroupProtection(ctx, g.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteGroup(ctx, g.ID); !errors.Is(err, ErrProtected) {
		t.Fatalf("delete a protected group: %v", err)
	}
	if err := s.SetGroupProtection(ctx, g.ID, false); err != nil {
		t.Fatal(err)
	}
	dbs, err := s.DeleteGroup(ctx, g.ID)
	if err != nil || len(dbs) != 2 {
		t.Fatalf("delete group: %v %v", dbs, err)
	}
	if _, err := s.GroupByID(ctx, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted group is still found: %v", err)
	}
	if _, err := s.RestoreDatabase(ctx, reuse.ID, ""); !errors.Is(err, ErrGroupDeleted) {
		t.Fatalf("restore a database whose group is deleted: %v", err)
	}
	gs, err := s.DeletedGroups(ctx, org.ID)
	if err != nil || len(gs) != 1 || gs[0].ID != g.ID || len(gs[0].Locations) != 1 {
		t.Fatalf("deleted groups: %+v %v", gs, err)
	}
	// A database made meanwhile under one of their names keeps it.
	g2, err := s.CreateGroup(ctx, org.ID, "eu", "fra", "")
	if err != nil {
		t.Fatalf("reuse a deleted group's name: %v", err)
	}
	if err := s.CreateDatabase(ctx, &Database{OrgID: org.ID, Name: "app", GroupID: g2.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RestoreGroup(ctx, g.ID, ""); !errors.Is(err, ErrExists) {
		t.Fatalf("restore a group under a taken name: %v", err)
	}
	rg, rdbs, err := s.RestoreGroup(ctx, g.ID, "eu-old")
	if err != nil || rg.Name != "eu-old" || len(rg.Locations) != 1 || len(rdbs) != 2 {
		t.Fatalf("restored group: %+v %v %v", rg, rdbs, err)
	}
	names := map[string]bool{}
	for _, d := range rdbs {
		names[d.Name] = true
	}
	if !names["app-old"] || !names["app-"+reuse.ID[:6]] {
		t.Fatalf("restored databases: %v", names)
	}

	if _, err := s.DeleteGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	for _, d := range rdbs {
		if err := s.PurgeDeletedDatabase(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PurgeDeletedGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeletedGroupByID(ctx, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purged group: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM group_locations WHERE group_id = ?`, g.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a purged group's locations: %d %v", n, err)
	}
}

// TestExpiredStaysDeleted: once a sweep marks a deleted database or group
// past its retention, it is never restored, and a restore that came first
// makes the mark miss, so the sweep keeps the files.
func TestExpiredStaysDeleted(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "a@example.com", "A", HashPassword("pw"), false)
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(ctx, "acme", "Acme", u.ID, "fra")
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGroup(ctx, org.ID, "eu", "fra", "")
	if err != nil {
		t.Fatal(err)
	}
	a := &Database{OrgID: org.ID, Name: "a", GroupID: g.ID}
	b := &Database{OrgID: org.ID, Name: "b", GroupID: g.ID}
	for _, d := range []*Database{a, b} {
		if err := s.CreateDatabase(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.SoftDeleteDatabase(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ExpireDeletedDatabase(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if d, err := s.DeletedDatabaseByID(ctx, a.ID); err != nil || !d.Expired {
		t.Fatalf("expired: %+v %v", d, err)
	}
	if _, err := s.RestoreDatabase(ctx, a.ID, ""); !errors.Is(err, ErrExpired) {
		t.Fatalf("restore an expired database: %v", err)
	}

	if err := s.SoftDeleteDatabase(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreDatabase(ctx, b.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ExpireDeletedDatabase(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expire a database restored first: %v", err)
	}

	// A group whose database expired comes back with none of them.
	if _, err := s.DeleteGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ExpireDeletedDatabase(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RestoreGroup(ctx, g.ID, ""); !errors.Is(err, ErrExpired) {
		t.Fatalf("restore a group holding an expired database: %v", err)
	}
	if _, err := s.DeletedGroupByID(ctx, g.ID); err != nil {
		t.Fatalf("a refused group restore moved the group: %v", err)
	}
	if err := s.ExpireDeletedGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RestoreGroup(ctx, g.ID, ""); !errors.Is(err, ErrExpired) {
		t.Fatalf("restore an expired group: %v", err)
	}
}
