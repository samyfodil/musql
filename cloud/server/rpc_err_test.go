package server

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	"github.com/samyfodil/musql/cloud/cluster"
	"github.com/samyfodil/musql/cloud/state"
	"github.com/samyfodil/musql/replication"
)

// TestRPCErrCodes: an error that is the cluster's for the moment is
// Unavailable, which a client retries; a write the old leader may have
// committed is Unknown, which it must not retry blindly; the rest keep their
// codes (#129).
func TestRPCErrCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want connect.Code
	}{
		{state.ErrNotFound, connect.CodeNotFound},
		{state.ErrExists, connect.CodeAlreadyExists},
		{state.ErrNoMaster, connect.CodeUnavailable},
		{fmt.Errorf("lookup: %w", state.ErrNoMaster), connect.CodeUnavailable},
		{errNoMaster, connect.CodeUnavailable},
		{fmt.Errorf("%w: %v", replication.ErrNotWriter, cluster.ErrNotLeader), connect.CodeUnavailable},
		{fmt.Errorf("propose: %w", cluster.ErrNotLeader), connect.CodeUnavailable},
		{fmt.Errorf("propose: %w", cluster.ErrOutcomeUnknown), connect.CodeUnknown},
		{errors.New("disk on fire"), connect.CodeInternal},
	} {
		if got := connect.CodeOf(rpcErr(tc.err)); got != tc.want {
			t.Errorf("%v: %v, want %v", tc.err, got, tc.want)
		}
	}
}
