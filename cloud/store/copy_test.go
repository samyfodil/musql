package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samyfodil/musql/engine"
)

// TestKeepFile: a file put back behind its log is kept as it is and logged as
// a new base, and the history before it still restores.
func TestKeepFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.musq")
	hdir := filepath.Join(dir, "history")
	db := openDB(t, path)
	exec(t, db, `CREATE TABLE t(a INTEGER PRIMARY KEY, b)`)
	h, err := Open(path, hdir, KeepFile())
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `INSERT INTO t(b) VALUES (1)`)
	saved := filepath.Join(dir, "saved")
	if err := os.Mkdir(saved, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"", ".delta"} {
		if err := copyFile(path+s, filepath.Join(saved, "db.musq"+s), -1); err != nil {
			t.Fatal(err)
		}
	}
	old := dump(t, path)
	exec(t, db, `INSERT INTO t(b) VALUES (2)`)
	newer := dump(t, path)
	mid := time.Now()
	db.Close()
	h.Close()
	engine.RemoveDatabaseFiles(path)
	for _, s := range []string{"", ".delta"} {
		if err := copyFile(filepath.Join(saved, "db.musq"+s), path+s, -1); err != nil {
			t.Fatal(err)
		}
	}
	h, err = Open(path, hdir, KeepFile())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if got := dump(t, path); got != old {
		t.Fatalf("the file moved: want:\n%s\ngot:\n%s", old, got)
	}
	for _, c := range []struct {
		at   time.Time
		want string
	}{{mid, newer}, {time.Now(), old}} {
		dst := filepath.Join(t.TempDir(), "r.musq")
		if err := h.Restore(c.at, dst); err != nil {
			t.Fatal(err)
		}
		if got := dump(t, dst); got != c.want {
			t.Errorf("restore to %v: want:\n%s\ngot:\n%s", c.at, c.want, got)
		}
	}
}

// TestOpenLogCopy: a copy of the log alone, given the snapshot files
// RestoreFiles names, restores every point as the history does.
func TestOpenLogCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.musq")
	hdir := filepath.Join(dir, "history")
	db := openDB(t, path)
	defer db.Close()
	exec(t, db, `CREATE TABLE t(a INTEGER PRIMARY KEY, b)`)
	h, err := Open(path, hdir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var points []point
	for i := range 6 {
		exec(t, db, `INSERT INTO t(b) VALUES (?)`, i)
		if i == 3 {
			exec(t, db, `CREATE INDEX tb ON t(b)`)
		}
		if err := h.Sync(); err != nil {
			t.Fatal(err)
		}
		points = append(points, point{time.Now(), dump(t, path)})
		time.Sleep(2 * time.Millisecond)
	}
	f := h.Files()
	if len(f.Snapshots) < 2 {
		t.Fatalf("snapshot files %v: want the CREATE INDEX's base too", f.Snapshots)
	}
	for _, p := range points {
		cdir := t.TempDir()
		if err := copyFile(filepath.Join(hdir, LogName), filepath.Join(cdir, LogName), f.LogEnd); err != nil {
			t.Fatal(err)
		}
		c, err := OpenLog(cdir)
		if err != nil {
			t.Fatal(err)
		}
		names, err := c.RestoreFiles(p.at)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if err := copyFile(filepath.Join(hdir, n), filepath.Join(cdir, n), -1); err != nil {
				t.Fatal(err)
			}
		}
		dst := filepath.Join(t.TempDir(), "r.musq")
		if err := c.Restore(p.at, dst); err != nil {
			t.Fatal(err)
		}
		c.Close()
		if got := dump(t, dst); got != p.dump {
			t.Errorf("restore to %v: want:\n%s\ngot:\n%s", p.at, p.dump, got)
		}
	}
}
