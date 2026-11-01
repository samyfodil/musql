package server

import (
	"context"
	"testing"
	"time"
)

// TestRestoreInPlaceUndoes: an in-place restore to before the history is
// refused, and one whose new file cannot be recorded goes back to the old file,
// which is still served as it was.
func TestRestoreInPlaceUndoes(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	e.exec("/db/acme/app", e.admin, "CREATE TABLE t(a)", "INSERT INTO t VALUES (1)")
	o := must(e.s.store.OrgBySlug(ctx, "acme"))
	d := must(e.s.store.Database(ctx, o.ID, "app"))
	if err := e.s.restoreInPlace(ctx, d, time.Unix(1, 0)); err == nil {
		t.Error("restored to before the history")
	}
	restore := e.breakTable("databases")
	err := e.s.restoreInPlace(ctx, d, time.Now())
	restore()
	if err == nil {
		t.Fatal("restored with a record that could not be updated")
	}
	if got := e.exec("/db/acme/app", e.admin, "SELECT count(*) FROM t"); got[0].Response.Result.Rows[0][0].Value != "1" {
		t.Fatalf("after the undone restore: %+v", got)
	}
}
