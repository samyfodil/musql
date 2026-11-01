package sqlite

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// importCase is a C SQLite file damaged in one way, and what Import says.
type importCase struct {
	name  string
	stmts []string
	// damage edits the exported file; nil leaves it as it is.
	damage func(t *testing.T, path string)
}

const importCasePageSize = 512

func importCaseRows(extra ...string) []string {
	return append([]string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c TEXT COLLATE NOCASE, d INT)`,
		`CREATE INDEX t_b ON t(b)`,
		`CREATE INDEX t_c ON t(c)`,
		`CREATE UNIQUE INDEX t_d ON t(d)`,
		`INSERT INTO t(b, c, d) SELECT printf('key%04d', value), printf('Name%04d', value), value*3 FROM generate_series(1, 600)`,
	}, extra...)
}

// indexPages is every page of the index b-tree rooted at root.
func indexPages(t *testing.T, p *pager, root uint32) []uint32 {
	t.Helper()
	page, err := p.page(root)
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := parseIdxPageHeader(page, root)
	if err != nil {
		t.Fatal(err)
	}
	out := []uint32{root}
	if hdr.pageType != pageTypeIndexInterior {
		return out
	}
	for i := range int(hdr.numCells) {
		off, err := hdr.cellOffset(page, i)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, indexPages(t, p, binary.BigEndian.Uint32(page[off:off+4]))...)
	}
	return append(out, indexPages(t, p, hdr.rightmost)...)
}

// patchIndex replaces from with to, same length, wherever the index named
// index holds it.
func patchIndex(index, from, to string) func(t *testing.T, path string) {
	return func(t *testing.T, path string) {
		t.Helper()
		p, err := openPager(path)
		if err != nil {
			t.Fatal(err)
		}
		pages := indexPages(t, p, rootOf(t, p, index))
		type patch struct{ off int64 }
		var patches []patch
		for _, pgno := range pages {
			page, err := p.page(pgno)
			if err != nil {
				t.Fatal(err)
			}
			if at := bytes.Index(page, []byte(from)); at >= 0 {
				patches = append(patches, patch{int64(pgno-1)*importCasePageSize + int64(at)})
			}
		}
		p.Close()
		if len(patches) == 0 {
			t.Fatalf("%s holds no %q", index, from)
		}
		for _, pt := range patches {
			writeAt(t, path, pt.off, []byte(to))
		}
	}
}

// swapIndexCells swaps two cell pointers of the index's first leaf page,
// which puts two entries out of order and leaves every byte of both.
func swapIndexCells(index string, i, j int) func(t *testing.T, path string) {
	return func(t *testing.T, path string) {
		t.Helper()
		p, err := openPager(path)
		if err != nil {
			t.Fatal(err)
		}
		var leaf uint32
		var hdr idxPageHeader
		for _, pgno := range indexPages(t, p, rootOf(t, p, index)) {
			page, err := p.page(pgno)
			if err != nil {
				t.Fatal(err)
			}
			if hdr, err = parseIdxPageHeader(page, pgno); err == nil && hdr.pageType == pageTypeIndexLeaf {
				leaf = pgno
				break
			}
		}
		page, _ := p.page(leaf)
		a, b := hdr.headerSize+2*i, hdr.headerSize+2*j
		pa, pb := bytes.Clone(page[a:a+2]), bytes.Clone(page[b:b+2])
		p.Close()
		base := int64(leaf-1) * importCasePageSize
		writeAt(t, path, base+int64(a), pb)
		writeAt(t, path, base+int64(b), pa)
	}
}

var importCases = []importCase{
	{name: "clean", stmts: importCaseRows()},
	// A key spelled differently in the index than in its row: the row is
	// missing from it, and the entry sorts out of place.
	{name: "renamed-key", stmts: importCaseRows(), damage: patchIndex("t_b", "key0123", "kez0123")},
	// Another case under NOCASE: the entry is found, its value differs.
	{name: "case-of-key", stmts: importCaseRows(), damage: patchIndex("t_c", "Name0042", "NAME0042")},
	{name: "swapped-cells", stmts: importCaseRows(), damage: swapIndexCells("t_b", 1, 5)},
	{name: "swapped-adjacent", stmts: importCaseRows(), damage: swapIndexCells("t_c", 2, 3)},
	// Two rows share a key of a UNIQUE index, which the stored copy agrees
	// with.
	{name: "duplicate-unique", stmts: importCaseRows(
		`UPDATE t SET b = 'key0007' WHERE a IN (8, 9, 400)`,
		`PRAGMA writable_schema=ON`,
		`INSERT INTO sqlite_master VALUES('index','u_b','t',0,'CREATE UNIQUE INDEX u_b ON t(b)')`,
	)},
	{name: "check-and-notnull", stmts: importCaseRows(
		`UPDATE t SET b = NULL WHERE a % 50 = 0`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_master SET sql='CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT NOT NULL, c TEXT COLLATE NOCASE, d INT CHECK (d % 9 <> 0))' WHERE name='t'`,
	)},
	{name: "check-and-renamed-key", stmts: importCaseRows(
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_master SET sql='CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c TEXT COLLATE NOCASE, d INT CHECK (d % 4 <> 0))' WHERE name='t'`,
	), damage: patchIndex("t_b", "key0123", "kez0123")},
}

// runImportCase builds the case's file and returns Import's answer.
func runImportCase(t *testing.T, c importCase) string {
	t.Helper()
	dir := t.TempDir()
	src, db, dst := filepath.Join(dir, "src.musq"), filepath.Join(dir, "x.db"), filepath.Join(dir, "dst.musq")
	buildDB(t, src, c.stmts...)
	if err := Export(src, db, importCasePageSize); err != nil {
		t.Fatal(err)
	}
	if c.damage != nil {
		c.damage(t, db)
	}
	if err := Import(db, dst, ImportOptions{}); err != nil {
		return err.Error()
	}
	return ""
}

// TestImportFindsWhatItFound holds Import's refusals to what it answered
// before its check was made to stream: these answers are the in-memory
// check's, recorded at the commit before.
func TestImportFindsWhatItFound(t *testing.T) {
	for _, c := range importCases {
		got := runImportCase(t, c)
		want, ok := importGolden[c.name]
		if !ok {
			t.Errorf("%s: no recorded answer; got %q", c.name, got)
			continue
		}
		if got != want {
			t.Errorf("%s:\ngot  %q\nwant %q", c.name, got, want)
		}
	}
}

var importGolden = map[string]string{
	"clean":            "",
	"renamed-key":      refused("row 123 missing from index t_b", "row 124 missing from index t_b"),
	"case-of-key":      refused("row 42 values differ from index t_c"),
	"swapped-cells":    refused("row 2 missing from index t_b", "row 3 missing from index t_b", "row 6 missing from index t_b"),
	"swapped-adjacent": refused("row 3 missing from index t_c"),
	"duplicate-unique": refused("non-unique entry in index u_b", "non-unique entry in index u_b", "non-unique entry in index u_b"),
	"check-and-notnull": refused(firstFindings(func(a int) (out []string) {
		if a%50 == 0 {
			out = append(out, "NULL value in t.b")
		}
		if a%3 == 0 {
			out = append(out, "CHECK constraint failed in t")
		}
		return out
	})...),
	"check-and-renamed-key": refused(firstFindings(func(a int) (out []string) {
		if a%4 == 0 {
			out = append(out, "CHECK constraint failed in t")
		}
		if a == 123 || a == 124 {
			out = append(out, fmt.Sprintf("row %d missing from index t_b", a))
		}
		return out
	})...),
}

func refused(findings ...string) string {
	return "sqlite: the source database fails integrity_check (pass ImportOptions{Force: true} to convert it anyway):\n" + strings.Join(findings, "\n")
}

// firstFindings is the first 100 of the findings rows 1, 2, ... hold, the
// cap Import checks with.
func firstFindings(row func(a int) []string) []string {
	var out []string
	for a := 1; len(out) < 100; a++ {
		out = append(out, row(a)...)
	}
	return out[:100]
}
