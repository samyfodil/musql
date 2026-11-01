package cluster

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStartAndKeys: a node starts under its key's ID and listens where told; a
// peer whose ID or address does not parse, a key file that is not one, and a
// key path it cannot write are refused.
func TestStartAndKeys(t *testing.T) {
	dir := t.TempDir()
	key, err := LoadKey(filepath.Join(dir, "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadKey(filepath.Join(dir, "node.key"))
	if err != nil || !again.Equals(key) {
		t.Fatalf("the key read back: %v", err)
	}
	id, err := PeerID(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, peers := range map[string][]Peer{
		"a peer ID that does not decode": {{ID: "nope"}},
		"an address that does not parse": {{ID: id, Addrs: []string{"not-a-multiaddr"}}},
	} {
		if n, err := Start(key, nil, peers, t.Logf); err == nil {
			n.Close()
			t.Errorf("%s: started", name)
		}
	}
	n, err := Start(key, []string{"/ip4/127.0.0.1/tcp/0"}, nil, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if n.ID() != id || len(n.Addrs()) == 0 {
		t.Fatalf("started as %s on %v, want %s", n.ID(), n.Addrs(), id)
	}

	bad := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(bad, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(bad); err == nil {
		t.Error("read a key file that is not one")
	}
	if _, err := LoadKey(dir); err == nil {
		t.Error("read a directory as a key")
	}
	if _, err := LoadKey(filepath.Join(dir, "none", "node.key")); err == nil {
		t.Error("made a key where it cannot be written")
	}
}
