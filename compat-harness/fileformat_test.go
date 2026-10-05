package compat

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileFormatInterop verifies SQLite file format compatibility across
// engines in rollback-journal and WAL modes.
func TestFileFormatInterop(t *testing.T) {
	const numRows = 300 // Spans several pages.

	modes := []struct {
		name   string
		pragma string // run before CREATE TABLE; "" = default rollback-journal mode
	}{
		{"rollback-journal", ""},
		{"wal", "PRAGMA journal_mode=WAL"},
	}

	for _, m := range modes {
		m := m
		t.Run(m.name, func(t *testing.T) {
			for _, writer := range engineOrder {
				writer := writer
				t.Run("written-by-"+writer, func(t *testing.T) {
					dir := t.TempDir()
					dsn := filepath.Join(dir, "shared.db")

					writerStmts := buildFileFormatWriterProgram(m.pragma, numRows)
					writerResults := runWithDSN(t, writer, dsn, writerStmts)
					if len(writerResults) == 0 {
						t.Fatalf("writer %s produced no results", writer)
					}
					baseline := writerResults[len(writerResults)-1] // the final SELECT
					baselineJSON, _ := json.Marshal(baseline)
					if kind, _ := baseline["kind"].(string); kind != "rows" {
						t.Fatalf("writer %s: expected final statement to be a row-returning SELECT, got kind=%v (full: %s)", writer, baseline["kind"], baselineJSON)
					}

					for _, reader := range engineOrder {
						reader := reader
						t.Run("read-by-"+reader, func(t *testing.T) {
							// The other engine's file is read through the converter
							// (pathForReader) -- the only door between the formats.
							got := runWithDSN(t, reader, pathForReader(t, reader, dsn), []string{ffSelectSQL})
							if len(got) != 1 {
								t.Fatalf("reader %s: expected exactly 1 result, got %d", reader, len(got))
							}
							gotJSON, _ := json.Marshal(got[0])
							if string(gotJSON) != string(baselineJSON) {
								t.Errorf("[%s/%s writes, %s reads] file-format mismatch\n  writer (%s) saw: %s\n  reader (%s) saw: %s",
									m.name, writer, reader, writer, baselineJSON, reader, gotJSON)
							}
						})
					}
				})
			}
		})
	}
}

// runWithDSN behaves like run() but lets the caller pin an explicit DSN path
// instead of letting each call mint its own fresh temp file -- required here
// so a writer and one or more readers, run as separate worker processes
// (possibly different engines), all operate on the exact same on-disk file.
func runWithDSN(t *testing.T, engine, dsn string, stmts []string) []map[string]any {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stmts-*.json")
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := json.Marshal(stmts)
	f.Write(enc)
	f.Close()

	cmd := exec.Command(workerBin[engine], f.Name())
	cmd.Env = append(os.Environ(), "COMPAT_DSN="+dsn)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s worker failed: %v", engine, err)
	}
	var res []map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("%s: bad worker output: %v\n%s", engine, err, out)
	}
	return res
}

const ffSelectSQL = "SELECT id,i,r,s,b,n,typeof(i),typeof(r),typeof(s),typeof(b),typeof(n) FROM ff ORDER BY id"

// buildFileFormatWriterProgram returns a self-contained statement list that
// creates a table + index and inserts numRows rows spanning the SQLite type
// space (NULL, integers incl. extremes, floats incl. signed zero/very
// small/very large, ascii+unicode text, blobs, and cross-type values in the
// NUMERIC column), then reads everything back (the "baseline" the readers
// must reproduce byte-for-byte). Data is deterministic (not randomized) so
// the expected content is easy to reason about independent of the fuzzer.
func buildFileFormatWriterProgram(pragma string, numRows int) []string {
	var stmts []string
	if pragma != "" {
		stmts = append(stmts, pragma)
	}
	stmts = append(stmts,
		"CREATE TABLE ff(id INTEGER PRIMARY KEY, i INTEGER, r REAL, s TEXT, b BLOB, n NUMERIC)",
		"CREATE INDEX idx_ff_i ON ff(i)",
		"BEGIN",
	)

	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "eta-Ω", "漢字テスト", "emoji-☃-🎉", "", "plain"}
	for k := 0; k < numRows; k++ {
		var iVal, rVal, nVal string
		switch k % 7 {
		case 0:
			iVal = "NULL"
		case 1:
			iVal = "9223372036854775807"
		case 2:
			iVal = "-9223372036854775808"
		default:
			iVal = fmt.Sprintf("%d", k*31-500)
		}
		switch k % 5 {
		case 0:
			rVal = "0.0"
		case 1:
			rVal = "-0.0"
		case 2:
			rVal = "1e308"
		case 3:
			rVal = "1e-308"
		default:
			rVal = fmt.Sprintf("%d.%d", k, (k*7)%1000)
		}
		w := words[k%len(words)]
		sVal := "'" + strings.ReplaceAll(fmt.Sprintf("%s-%d", w, k), "'", "''") + "'"
		blob := make([]byte, k%13)
		for bi := range blob {
			blob[bi] = byte((k + bi) % 256)
		}
		bVal := "x'" + hex.EncodeToString(blob) + "'"
		switch k % 4 {
		case 0:
			nVal = fmt.Sprintf("%d", k)
		case 1:
			nVal = "'" + fmt.Sprintf("num-%d", k) + "'"
		case 2:
			nVal = "NULL"
		default:
			nVal = fmt.Sprintf("%d.%d", k, k%10)
		}
		stmts = append(stmts, fmt.Sprintf("INSERT INTO ff(id,i,r,s,b,n) VALUES(%d,%s,%s,%s,%s,%s)", k+1, iVal, rVal, sVal, bVal, nVal))
	}
	stmts = append(stmts, "COMMIT", ffSelectSQL)
	return stmts
}
