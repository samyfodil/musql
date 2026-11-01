package replication

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	c.openWith("a", []Option{WithSyncInterval(time.Hour)}, imageDDL...) // no round prunes a's ops
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
	lo, _ := RowidRange(m.Site)
	clash := fmt.Sprintf("b#r%d", lo>>32)
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
		"a site on the image's range":  {m, clash, modeCRDT, nil, true},
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

func TestDurableFilesReportFailures(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeDurable(dir); err == nil {
		t.Error("removed a directory that is not empty")
	}
	if _, err := os.Stat("/dev/full"); err == nil {
		if err := writeDurable("/dev/full", []byte("x")); err == nil {
			t.Error("a write to a full device succeeded")
		}
	}
}

// TestKeepMarkerLeavesAMovedInstall: an install that failed with the database
// no longer as it was keeps its marker, for Open to finish it.
func TestKeepMarkerLeavesAMovedInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.db")
	pending, err := keepMarker(path, filepath.Join(dir, "x.db"+stagedInfix+"1"), 7, errors.New("install failed"))
	if !pending || err == nil || !strings.Contains(err.Error(), "next opened") {
		t.Fatalf("pending %v, %v", pending, err)
	}
}

// TestImageStateRefusesBrokenBookkeeping: an image whose bookkeeping tables are
// missing or hold what is not a number is refused, not half read.
func TestImageStateRefusesBrokenBookkeeping(t *testing.T) {
	ctx := context.Background()
	meta := `CREATE TABLE _repl_meta(k TEXT PRIMARY KEY, v BLOB)`
	floors := `CREATE TABLE _repl_floors(site TEXT PRIMARY KEY, seq INTEGER)`
	for name, ddl := range map[string][]string{
		"no meta":                   nil,
		"no floors":                 {meta},
		"a floor that is no number": {meta, floors, `INSERT INTO _repl_floors VALUES('a', 'x')`},
		"no ranges":                 {meta, floors},
		"a range that is no number": {meta, floors, `CREATE TABLE _repl_ranges(lo INTEGER, site TEXT)`, `INSERT INTO _repl_ranges VALUES('x', 'a')`},
	} {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "image.db"))
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range ddl {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		var m ImageManifest
		if err := imageState(ctx, db, &m); err == nil {
			t.Errorf("%s: read", name)
		}
		db.Close()
	}
}

func TestImageNeedsADatabaseFile(t *testing.T) {
	ctx := context.Background()
	db, err := openApplyDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	s, err := OpenStore(ctx, db, "mem")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Image(ctx, imageRequest(bookkeepingLayout)); err == nil {
		t.Fatal("an image of a database with no file")
	}
}
