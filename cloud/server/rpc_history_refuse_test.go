package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// TestHistoryRPCRefusals: the snapshot, restore and usage calls refuse a
// database that is not there, a restore asked both in place and into a new
// name, a new name that is bad or taken, and a usage range that ends before it
// starts.
func TestHistoryRPCRefusals(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()

	if _, err := dbs.ListSnapshots(ctx, auth(e.admin, &v1.ListSnapshotsRequest{Org: "acme", Name: "nope"})); code(err) != connect.CodeNotFound {
		t.Errorf("snapshots of no database: %v", err)
	}
	if _, err := dbs.CreateSnapshot(ctx, auth(e.admin, &v1.CreateSnapshotRequest{Org: "acme", Name: "nope"})); code(err) != connect.CodeNotFound {
		t.Errorf("a snapshot of no database: %v", err)
	}
	for name, tc := range map[string]struct {
		req  *v1.RestoreRequest
		want connect.Code
	}{
		"no such database":      {&v1.RestoreRequest{Org: "acme", Name: "nope"}, connect.CodeNotFound},
		"in place and renamed":  {&v1.RestoreRequest{Org: "acme", Name: "app", InPlace: true, NewName: "x"}, connect.CodeInvalidArgument},
		"a bad new name":        {&v1.RestoreRequest{Org: "acme", Name: "app", NewName: "Bad Name"}, connect.CodeInvalidArgument},
		"a new name that is in": {&v1.RestoreRequest{Org: "acme", Name: "app", NewName: "app"}, connect.CodeAlreadyExists},
	} {
		if _, err := dbs.Restore(ctx, auth(e.admin, tc.req)); code(err) != tc.want {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	now := time.Now()
	if _, err := dbs.Usage(ctx, auth(e.admin, &v1.UsageRequest{Org: "acme", From: timestamppb.New(now), To: timestamppb.New(now.Add(-2 * time.Hour))})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("a range that ends before it starts: %v", err)
	}
	if _, err := dbs.Usage(ctx, auth(e.admin, &v1.UsageRequest{Org: "acme", Name: "nope"})); code(err) != connect.CodeNotFound {
		t.Errorf("usage of no database: %v", err)
	}
}
