package replication

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// opsOnly is an OpSource that serves no image.
type opsOnly struct{}

func (opsOnly) LocalVV() VersionVector                         { return VersionVector{} }
func (opsOnly) OpsSince(string, uint64, int) ([]Op, error)     { return nil, nil }
func (opsOnly) SnapshotPage(int, []byte) (SnapshotPage, error) { return SnapshotPage{}, nil }
func (opsOnly) ExchangeAcks(Acks) Acks                         { return nil }
func (opsOnly) ReceiveOp(PeerID, Op)                           {}

func TestMemTransportGetImageRefusals(t *testing.T) {
	ctx := context.Background()
	net := NewMemNetwork()
	net.AddNode("a", opsOnly{})
	b := net.AddNode("b", opsOnly{}).(Imager)
	req := imageRequest(bookkeepingLayout)
	if _, _, err := b.GetImage(ctx, "nobody", req); err == nil {
		t.Error("a missing peer sent an image")
	}
	if _, _, err := b.GetImage(ctx, "a", req); !errors.Is(err, ErrImageRefused) {
		t.Errorf("a peer that serves none: %v, want ErrImageRefused", err)
	}
	net.Partition("a", "b", true)
	if _, _, err := b.GetImage(ctx, "a", req); err == nil {
		t.Error("a partitioned peer sent an image")
	}
}

func TestDecodeRecRefusesMalformed(t *testing.T) {
	rec, _ := encodeRec(map[string]clockCell{"a": {hlc: 5, site: "s", val: []byte("x")}, presenceCol: {hlc: 5, site: "s", val: []byte{1}}})
	if _, err := decodeRec(rec); err != nil {
		t.Fatalf("a record as written: %v", err)
	}
	for n := range len(rec) {
		if _, err := decodeRec(rec[:n]); !errors.Is(err, errBadRec) {
			t.Fatalf("cut at %d of %d: %v, want errBadRec", n, len(rec), err)
		}
	}
}

func TestSnapshotPageRefusesAnUnknownPart(t *testing.T) {
	c := newCluster(t)
	c.openWith("a", []Option{WithSyncInterval(time.Hour)}, `CREATE TABLE t(id INTEGER PRIMARY KEY)`)
	if _, err := syncerOf(t, c.dbs["a"]).store.SnapshotPage(context.Background(), 99, nil); err == nil {
		t.Fatal("served an unknown snapshot part")
	}
}

func TestPlainCopyIntoAMissingDirectory(t *testing.T) {
	c := newCluster(t)
	c.openWith("a", []Option{WithSyncInterval(time.Hour)}, `CREATE TABLE t(id INTEGER PRIMARY KEY)`)
	dst := filepath.Join(t.TempDir(), "missing", "out.db")
	if err := PlainCopy(context.Background(), filepath.Join(c.dir, "a.db"), dst); err == nil {
		t.Fatal("copied into a directory that does not exist")
	}
}
