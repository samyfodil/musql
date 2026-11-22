package state

import "testing"

// TestRetryableRemoteBusy: the master's busy refusal reaches a node as the
// driver's text, without the engine's prefix, and is retried like a local one.
func TestRetryableRemoteBusy(t *testing.T) {
	for msg, want := range map[string]bool{
		"database is locked (SQLITE_BUSY): another connection committed while this transaction was open": true,
		"replication: this node is not the writer":                                                       true,
		"no such table: members": false,
	} {
		if got := retryable(&errRemote{msg}); got != want {
			t.Errorf("retryable(%q) = %v, want %v", msg, got, want)
		}
	}
}
