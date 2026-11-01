package store

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// record is one log record of kind holding pl.
func record(kind byte, pl []byte) []byte {
	body := make([]byte, hdrLen-4, hdrLen-4+len(pl))
	body[0] = kind
	binary.LittleEndian.PutUint64(body[1:], 1)
	binary.LittleEndian.PutUint64(body[9:], uint64(time.Now().UnixNano()))
	body = append(body, pl...)
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(body)))
	out = append(out, body...)
	return binary.LittleEndian.AppendUint32(out, crc32.Checksum(body, crcTable))
}

// TestOpenRefusesABrokenLog: a log whose record is cut short, of a kind it
// does not know, or that does not start with a base is refused at open.
func TestOpenRefusesABrokenLog(t *testing.T) {
	for name, log := range map[string][]byte{
		"a short batch record":    record(kindBatch, []byte{1, 2, 3}),
		"a base record cut short": record(kindBase, make([]byte, 10)),
		"a kind it does not know": record(99, nil),
		"no base first":           record(kindBatch, make([]byte, 8)),
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "log"), log, 0o600); err != nil {
			t.Fatal(err)
		}
		if h, err := Open(filepath.Join(dir, "db.musq"), dir); err == nil {
			h.Close()
			t.Errorf("%s: opened", name)
		}
	}
}

// TestHistoryRefusals: a damaged snapshot is not restored from; a closed
// history takes no snapshot and follows no new path; and a history does not
// take over a database another history follows.
func TestHistoryRefusals(t *testing.T) {
	dir := t.TempDir()
	path, other := filepath.Join(dir, "db.musq"), filepath.Join(dir, "other.musq")
	for _, p := range []string{path, other} {
		db := openDB(t, p)
		exec(t, db, `CREATE TABLE t(a)`)
		db.Close()
	}
	h, err := Open(path, filepath.Join(dir, "history"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	o, err := Open(other, filepath.Join(dir, "history-other"))
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err := h.Retarget(other); err == nil {
		t.Error("followed a database another history follows")
	}

	musq, _ := h.baseFiles(h.Snapshots()[0].Seq)
	b, err := os.ReadFile(musq)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	if err := os.WriteFile(musq, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.Restore(time.Now(), filepath.Join(dir, "restored.musq")); err == nil {
		t.Error("restored from a damaged snapshot")
	}

	h.Close()
	if _, err := h.Snapshot(true); err == nil {
		t.Error("a closed history took a snapshot")
	}
	if err := h.Retarget(filepath.Join(dir, "moved.musq")); err == nil {
		t.Error("a closed history followed a new path")
	}
}

// TestSchemaOnlyRefuses: a schema-only copy needs a database to read and a
// place to write that is free.
func TestSchemaOnlyRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.musq")
	db := openDB(t, path)
	exec(t, db, `CREATE TABLE t(a)`)
	db.Close()
	ctx := context.Background()
	if err := SchemaOnly(ctx, path, path); err == nil {
		t.Fatal("wrote a schema over a file already there")
	}
	if err := SchemaOnly(ctx, t.TempDir(), filepath.Join(t.TempDir(), "out.musq")); err == nil {
		t.Error("read the schema of a directory")
	}
	if err := SchemaOnly(ctx, path, filepath.Join(t.TempDir(), "none", "out.musq")); err == nil {
		t.Error("wrote a schema into no directory")
	}
}

// TestOpenRefusesWhatItCannotUse: a history will not open twice for one
// database, over a log that is a directory, or in a directory that is a file;
// and a record larger than the reader's first buffer reads back.
func TestOpenRefusesWhatItCannotUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.musq")
	db := openDB(t, path)
	defer db.Close()
	exec(t, db, `CREATE TABLE t(b)`)
	h, err := Open(path, filepath.Join(dir, "history"))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := Open(path, filepath.Join(dir, "history-again")); err == nil {
		again.Close()
		t.Error("a second history of one database opened")
	}
	exec(t, db, `INSERT INTO t VALUES (randomblob(100000))`)
	if err := h.Sync(); err != nil {
		t.Fatal(err)
	}
	h.Close()
	if h, err = Open(path, filepath.Join(dir, "history")); err != nil {
		t.Fatalf("a log with a large record: %v", err)
	}
	h.Close()

	logDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(logDir, "log"), 0o700); err != nil {
		t.Fatal(err)
	}
	if h, err := Open(path, logDir); err == nil {
		h.Close()
		t.Error("opened a log that is a directory")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if h, err := Open(path, file); err == nil {
		h.Close()
		t.Error("opened a history in a file")
	}
}
