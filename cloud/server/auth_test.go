package server

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"github.com/samyfodil/musql/hrana"

	"github.com/samyfodil/musql/cloud/state"
)

// TestAuthStateOutOfReach: a token check whose state lookup fails for any
// reason but a missing record, as when the state's master is being elected,
// refuses the client as one to retry, not as unauthorized: a database's
// Hrana check fails with hrana.ErrUnavailable (HTTP 503) and an RPC with
// CodeUnavailable. A record that is not there still denies.
func TestAuthStateOutOfReach(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.musq"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st}
	if _, err := s.hranaAuth("no-org", "no-db")("tok"); !errors.Is(err, errNoAccess) {
		t.Fatalf("a missing org: %v", err)
	}
	if _, err := s.caller(context.Background(), http.Header{"Authorization": {"Bearer " + state.PlatformTokenPrefix + "nope"}}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("an unknown platform token: %v", err)
	}

	st.Close()
	if _, err := s.hranaAuth("no-org", "no-db")("tok"); !errors.Is(err, hrana.ErrUnavailable) {
		t.Fatalf("Hrana with the state out of reach: %v", err)
	}
	if _, err := s.caller(context.Background(), http.Header{"Authorization": {"Bearer " + state.PlatformTokenPrefix + "nope"}}); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("an RPC with the state out of reach: %v", err)
	}
}
