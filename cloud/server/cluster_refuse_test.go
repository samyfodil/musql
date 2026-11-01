package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/cloud/cluster"
)

// TestClusteredStateRefusals: a cluster node will not start with a node key it
// cannot read, a peer whose ID does not parse, or a state it cannot open.
func TestClusteredStateRefusals(t *testing.T) {
	ctx := context.Background()
	key, err := cluster.LoadKey(filepath.Join(t.TempDir(), "peer.key"))
	if err != nil {
		t.Fatal(err)
	}
	peer, err := cluster.PeerID(key)
	if err != nil {
		t.Fatal(err)
	}
	cfgAt := func(dir, peerID string) *Config {
		t.Helper()
		cfg, err := ParseConfig([]byte(fmt.Sprintf(`
data_dir: %s
public_url: http://cloud.test
node:
  name: n0
  location: fra
  p2p_listen: [/ip4/127.0.0.1/tcp/0]
peers:
  - {name: n1, location: iad, peer_id: %s, addrs: [/ip4/127.0.0.1/tcp/1]}
`, dir, peerID)))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	keyDir, stateDir := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(keyDir, "node.key"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(stateDir, "state.musq"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]*Config{
		"a node key that is a directory": cfgAt(keyDir, peer),
		"a peer ID that does not parse":  cfgAt(t.TempDir(), "nope"),
		"a state that is a directory":    cfgAt(stateDir, peer),
	} {
		if s, err := New(ctx, cfg, nil); err == nil {
			s.Close()
			t.Errorf("%s: started", name)
		}
	}
}
