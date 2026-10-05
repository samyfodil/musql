package compat

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
)

// exportedForOracle returns the path of a .db holding ExportSQLite's output for
// a segment database, so the oracle can query what musql wrote. Cached per path
// to avoid repeated exports.
func exportedForOracle(t *testing.T, path string) string {
	t.Helper()
	if exportedForOracleCache == nil {
		exportedForOracleCache = map[string]string{}
	}
	key := exportCacheKey(path)
	if out, ok := exportedForOracleCache[key]; ok {
		return out
	}
	// A path that is ALREADY a .db needs no conversion, and asking for one is not
	// a mistake: several fixtures here are built by the oracle itself, and a helper
	// that wraps every oracle open (index_write_test.go's openCgo) sees both. So
	// this is "hand me a path the oracle can read", and a non-segment file is
	// already that.
	if !isSegmentFilePath(path) {
		return path
	}
	out := filepath.Join(t.TempDir(), "exported-for-oracle.db")
	if err := sqliteconv.Export(path, out, 0); err != nil {
		t.Fatalf("ExportSQLite(%s): %v -- the oracle cannot be asked about this engine's file directly", path, err)
	}
	exportedForOracleCache[key] = out
	return out
}

// exportedForOracleCache caches exports per database to avoid repeated conversions.
var exportedForOracleCache map[string]string

// exportCacheKey is a cache key based on path and file contents.
func exportCacheKey(path string) string {
	return fileContentKey(path, path+".delta")
}

// fileContentKey is a cache key based on path and file contents.
func fileContentKey(path string, files ...string) string {
	h := sha256.New()
	for _, p := range append([]string{path}, files...) {
		if b, err := os.ReadFile(p); err == nil {
			h.Write(b)
		}
		h.Write([]byte{0})
	}
	return path + "\x00" + string(h.Sum(nil))
}


// importedForMusql returns the path of a segment database holding ImportSQLite's
// output for a SQLite file, cached per path.
func importedForMusql(t *testing.T, path string) string {
	t.Helper()
	if importedForMusqlCache == nil {
		importedForMusqlCache = map[string]string{}
	}
	key := fileContentKey(path, path+"-wal")
	if out, ok := importedForMusqlCache[key]; ok {
		return out
	}
	// Pass through segment files and non-existent/empty paths.
	if isSegmentFilePath(path) {
		return path
	}
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		return path
	}
	out := filepath.Join(t.TempDir(), "imported-for-musql.musq")
	if err := sqliteconv.Import(path, out, sqliteconv.ImportOptions{}); err != nil {
		t.Fatalf("ImportSQLite(%s): %v -- musql cannot be asked about the oracle's file directly", path, err)
	}
	importedForMusqlCache[key] = out
	return out
}

var importedForMusqlCache map[string]string

// pathsForBothEngines returns paths for the two engines to query, converting as needed.
// writer is "sqlite" (segment file) or "sqlite3" (.db).
func pathsForBothEngines(t *testing.T, writer, dsn string) (goPath, cgoPath string) {
	t.Helper()
	if writer == "sqlite3" {
		return importedForMusql(t, dsn), dsn
	}
	return dsn, exportedForOracle(t, dsn)
}

// musqlPathFor and oraclePathFor return the cached path for each engine.
func musqlPathFor(t *testing.T, writer, dsn string) string {
	t.Helper()
	goPath, _ := pathsForBothEngines(t, writer, dsn)
	return goPath
}

func oraclePathFor(t *testing.T, writer, dsn string) string {
	t.Helper()
	_, cgoPath := pathsForBothEngines(t, writer, dsn)
	return cgoPath
}

// pathForReader returns the path the reader engine should open, converting if needed.
func pathForReader(t *testing.T, reader, path string) string {
	t.Helper()
	if writerEngineName(reader) == "sqlite3" {
		return exportedForOracle(t, path)
	}
	return importedForMusql(t, path)
}

// writerEngineName normalizes engine name spellings.
func writerEngineName(writer string) string {
	if writer == "cgo" || writer == "sqlite3" {
		return "sqlite3"
	}
	return "sqlite"
}

// isSegmentFilePath reports whether path holds a segment database, by its magic.
// A file that is absent or unreadable is not one.
func isSegmentFilePath(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, rerr := f.ReadAt(magic[:], 0); rerr != nil {
		return false
	}
	return string(magic[:]) == "MQSF"
}

// importRefusal reports why an import was refused by returning integrity_check
// findings, or the error string if the import succeeded.
func importRefusal(t *testing.T, path string) (findings string, refused bool) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "refused-import.musq")
	err := sqliteconv.Import(path, out, sqliteconv.ImportOptions{})
	if err == nil {
		return "", false
	}
	if !errors.Is(err, sqliteconv.ErrSourceCorrupt) {
		t.Fatalf("ImportSQLite(%s) failed, but not as a corrupt source: %v", path, err)
	}
	if ferr := sqliteconv.Import(path, filepath.Join(t.TempDir(), "forced.musq"), sqliteconv.ImportOptions{Force: true}); ferr != nil {
		t.Errorf("a forced import of the refused source failed: %v", ferr)
	}
	msg := err.Error()
	if i := strings.Index(msg, "\n"); i >= 0 {
		return msg[i+1:], true
	}
	return "", true
}
