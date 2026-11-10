package cluster

import (
	"errors"
	"testing"

	"github.com/hashicorp/raft"
)

// TestProposeErr: a proposal raft turned away before its log is one that did
// not happen and may be made again on the new leader; one raft lost the
// leadership with may have committed (#129).
func TestProposeErr(t *testing.T) {
	for _, tc := range []struct {
		raft error
		want error
	}{
		{raft.ErrNotLeader, ErrNotLeader},
		{raft.ErrLeadershipTransferInProgress, ErrNotLeader},
		{raft.ErrEnqueueTimeout, ErrNotLeader},
		{raft.ErrLeadershipLost, ErrOutcomeUnknown},
	} {
		if got := proposeErr(tc.raft); !errors.Is(got, tc.want) {
			t.Errorf("%v: %v, want %v", tc.raft, got, tc.want)
		}
	}
	if got := proposeErr(raft.ErrRaftShutdown); errors.Is(got, ErrNotLeader) || errors.Is(got, ErrOutcomeUnknown) {
		t.Errorf("a shutdown: %v, want it as raft gave it", got)
	}
}
