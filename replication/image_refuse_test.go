package replication

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestStageImageChecksTheManifest: a body shorter, longer or other than its
// manifest says, or a delta larger than the image, is refused and leaves
// nothing behind; one that matches lands as a segment file and its delta.
func TestStageImageChecksTheManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.musq")
	body := []byte("segmentbytesdelta")
	sum := crc32.Checksum(body, castagnoli)
	good := ImageManifest{Size: int64(len(body)), DeltaSize: 5, Checksum: sum}
	staged, err := stageImage(path, good, io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	seg, _ := os.ReadFile(staged)
	delta, _ := os.ReadFile(staged + deltaSuffix)
	if string(seg) != "segmentbytes" || string(delta) != "delta" {
		t.Fatalf("staged %q and %q", seg, delta)
	}
	engine.RemoveDatabaseFiles(staged)
	for name, tc := range map[string]struct {
		m    ImageManifest
		body []byte
	}{
		"a delta past the image": {ImageManifest{Size: 4, DeltaSize: 5, Checksum: sum}, body},
		"a short body":           {good, body[:10]},
		"a long body":            {good, append(slices.Clone(body), 'x')},
		"another checksum":       {ImageManifest{Size: good.Size, DeltaSize: 5, Checksum: sum + 1}, body},
	} {
		if _, err := stageImage(path, tc.m, io.NopCloser(bytes.NewReader(tc.body))); err == nil {
			t.Errorf("%s: staged", name)
		}
	}
	if l := leftovers(t, path); len(l) > 0 {
		t.Fatalf("left beside the database: %v", l)
	}
}

// TestAdoptImageRefuses: an image whose contents are not what its manifest
// says, that carries this node's own site, that replicates in another mode or
// that holds another op at one of this node's seqs is not adopted.
func TestAdoptImageRefuses(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t)
	c.open("a", imageDDL...)
	c.exec("a", `INSERT INTO t VALUES(1, 'one')`)
	st := syncerOf(t, c.dbs["a"]).store
	m, body, err := st.Image(ctx, imageRequest(bookkeepingLayout))
	if err != nil {
		t.Fatal(err)
	}
	staged, err := stageImage(filepath.Join(c.dir, "b.db"), m, body)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.RemoveDatabaseFiles(staged)
	ops, err := st.OpsSince(ctx, m.Site, 0, 10)
	if err != nil || len(ops) == 0 {
		t.Fatalf("a's ops: %v, %v", ops, err)
	}
	other := ops[0]
	other.HLC++
	otherSite := m
	otherSite.Site = "zz"
	for name, tc := range map[string]struct {
		m          ImageManifest
		site, mode string
		own        []Op
		refused    bool
	}{
		"another site in the manifest": {otherSite, "b", modeCRDT, nil, false},
		"its own site":                 {m, m.Site, modeCRDT, nil, false},
		"another mode":                 {m, "b", modeLeader, nil, true},
		"another op at a seq":          {m, "b", modeCRDT, []Op{other}, true},
	} {
		err := adoptImage(ctx, staged, tc.m, tc.site, tc.mode, tc.own)
		if err == nil || errors.Is(err, ErrImageRefused) != tc.refused {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSyncedNeedsReplication(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "plain.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if Synced(db) {
		t.Fatal("a database Open did not return is synced")
	}
	c := newCluster(t)
	c.open("a", imageDDL...)
	if Synced(c.dbs["a"]) {
		t.Fatal("a node that reached no peer is synced")
	}
}

func TestLayoutAndMainFile(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := mainFile(ctx, db); err == nil {
		t.Error("an in-memory database has a main file")
	}
	if _, err := db.Exec(`CREATE TABLE _repl_meta(k TEXT PRIMARY KEY, v BLOB)`); err != nil {
		t.Fatal(err)
	}
	if l, err := layoutOf(ctx, db); err != nil || l != "1" {
		t.Errorf("layout of a database that names none: %q, %v", l, err)
	}
}

func TestPlainCopyRefuses(t *testing.T) {
	dir := t.TempDir()
	if err := PlainCopy(context.Background(), filepath.Join(dir, "none.db"), filepath.Join(dir, "out.db")); err == nil {
		t.Error("copied a missing database")
	}
	c := newCluster(t)
	c.open("a", imageDDL...)
	existing := filepath.Join(dir, "exists.db")
	if err := os.WriteFile(existing, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PlainCopy(context.Background(), filepath.Join(c.dir, "a.db"), existing); err == nil {
		t.Error("copied over an existing file")
	}
}
