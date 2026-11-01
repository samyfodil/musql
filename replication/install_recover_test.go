package replication

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestFinishInstallRefuses: finishing an install fails where its marker cannot
// be read, the database it names is gone, or the staged image cannot be
// installed; durable writes and removes fail in a directory that is not there.
func TestFinishInstallRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.musq")
	if err := os.Mkdir(path+markerSuffix, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := finishInstall(path); err == nil {
		t.Error("a marker that is a directory was read")
	}
	if err := os.Remove(path + markerSuffix); err != nil {
		t.Fatal(err)
	}

	staged := path + stagedInfix + "x"
	s, err := engine.Create(staged)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(`CREATE TABLE t(a)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(`INSERT INTO t VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := writeDurable(path+markerSuffix, []byte(filepath.Base(staged))); err != nil {
		t.Fatal(err)
	}
	if err := finishInstall(path); err == nil {
		t.Error("finished an install over a database that is not there")
	}

	s, err = engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(`CREATE TABLE u(a)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := finishInstall(path); err == nil {
		t.Error("installed a staged image that still has a delta")
	}

	missing := filepath.Join(dir, "missing", "f")
	if err := writeDurable(missing, []byte("x")); err == nil {
		t.Error("wrote into a directory that is not there")
	}
	if err := removeDurable(missing); err == nil {
		t.Error("synced a directory that is not there")
	}
}
