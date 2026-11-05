package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/replication"
)

// TestImport: a state's records join another state: a record already there
// by its key stays, one whose name is taken comes in renamed, everything
// under it comes along, and importing again writes nothing.
func TestImport(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "old.musq")
	old, err := Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	u, err := old.CreateUser(ctx, "a@example.com", "A", HashPassword("p"), true)
	if err != nil {
		t.Fatal(err)
	}
	shop, err := old.CreateOrg(ctx, "shop", "Old shop", u.ID, "home")
	if err != nil {
		t.Fatal(err)
	}
	g, err := old.Group(ctx, shop.ID, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := old.CreateDatabase(ctx, &Database{ID: NewID(), OrgID: shop.ID, Name: "app", GroupID: g.ID, Type: TypePlain}); err != nil {
		t.Fatal(err)
	}
	old.Close()

	db, err := replication.Open(ctx, filepath.Join(t.TempDir(), "state.musq"), replication.Leader(func() bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	st, err := OpenReplicated(db, "n", nil)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// The same user, and another org already called shop.
	if _, err := st.CreateUser(ctx, "a@example.com", "A here", HashPassword("q"), true); err != nil {
		t.Fatal(err)
	}
	here, err := st.CreateOrg(ctx, "shop", "Shop here", u.ID, "fra")
	if err != nil {
		t.Fatal(err)
	}

	src, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	n, err := st.Import(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("nothing imported")
	}
	if o, err := st.OrgBySlug(ctx, "shop"); err != nil || o.ID != here.ID {
		t.Fatalf("shop is %v, %v: the org here should keep its slug", o, err)
	}
	moved, err := st.OrgBySlug(ctx, "shop-"+shop.ID[:6])
	if err != nil || moved.ID != shop.ID {
		t.Fatalf("the imported shop: %v, %v", moved, err)
	}
	if d, err := st.Database(ctx, shop.ID, "app"); err != nil || d.GroupID != g.ID {
		t.Fatalf("the imported database: %v, %v", d, err)
	}
	if got, err := st.GroupByID(ctx, g.ID); err != nil || len(got.Locations) != 1 || got.Locations[0] != "home" {
		t.Fatalf("the imported group: %+v, %v", got, err)
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Name != "A here" {
		t.Fatalf("users after the import: %+v, want the one here alone", users)
	}
	if n, err := st.Import(ctx, src); err != nil || n != 0 {
		t.Fatalf("a second import wrote %d rows: %v", n, err)
	}
}
