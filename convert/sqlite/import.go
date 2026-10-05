package sqlite

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/samyfodil/musql/engine"
)

// Import: a SQLite file converted into a musql database.
// Every table's rows and the catalog go through engine.Builder, which derives
// the shape from CREATE TABLE text. This package reads pages; nothing runs SQL.

// ImportOptions tunes Import.
type ImportOptions struct {
	// Force converts a source that fails the integrity check instead of refusing it.
	Force bool
}

// ErrSourceCorrupt is returned for a source that fails integrity_check.
var ErrSourceCorrupt = errors.New("sqlite: the source database fails integrity_check")

// Import converts the SQLite database at src into a musql database at dst.
// A source that fails integrity_check is refused unless opts.Force is set.
// The conversion reads rows and rebuilds from DDL; a corrupt source file would
// become a clean database with wrong answers. The refusal carries C's
// integrity_check answer: structural findings first, then per-row checks.
// Indexes are validated against the converted rows.
func Import(src, dst string, opts ImportOptions) error {
	p, err := openPager(src)
	if err != nil {
		return err
	}
	defer p.Close()
	const maxErr = 100 // integrity_check's default budget (pragma.c's mxErr)
	var found []string
	nStructural := 0
	if !opts.Force {
		res, err := p.checkStructuralIntegrity()
		if err != nil {
			return corruptErr(err)
		}
		if nStructural = min(len(res.Problems), maxErr); nStructural > 0 {
			msg := "*** in database main ***"
			for _, pr := range res.Problems[:nStructural] {
				msg += "\n" + pr.Message
			}
			found = append(found, msg)
		}
	}
	rows, err := p.schema()
	if err == nil {
		err = build(p, rows, dst)
	}
	if err != nil {
		if len(found) > 0 {
			return corrupt(found) // the rows are what the structure says is broken
		}
		return err
	}
	if opts.Force {
		return nil
	}
	if budget := maxErr - nStructural; budget > 0 {
		more, cerr := engine.IntegrityCheck(dst, engine.IntegrityCheckOptions{MaxErrors: budget, StoredIndexEntries: storedIndexEntries(p, rows)})
		if cerr != nil {
			removeDB(dst)
			return corruptErr(cerr)
		}
		found = append(found, more...)
	}
	if len(found) > 0 {
		removeDB(dst)
		return corrupt(found)
	}
	return nil
}

// build writes the converted database: header values, all carried table rows,
// and catalog entries, in source catalog order.
func build(p *pager, rows []schemaRow, dst string) error {
	b, err := engine.NewBuilder(dst)
	if err != nil {
		return err
	}
	defer b.Discard()
	h := p.hdr
	b.SetMeta(engine.Meta{
		SchemaVersion: h.SchemaCookie, UserVersion: h.UserVersion, ApplicationID: h.ApplicationID,
		Encoding: engine.TextEncoding(p.encoding()), PageSize: h.PageSize,
		AutoVacuum:    autoVacuumModeOfHeader(h.LargestRootPage, h.IncrementalVacuum),
		WAL:           h.WriteVersion == 2 && h.ReadVersion == 2,
		ChangeCounter: h.FileChangeCounter, PageCount: p.PageCount(),
	})
	var seqs []sequence
	for _, r := range rows {
		switch {
		case carriedTable(r):
			if err := importTable(p, b, r); err != nil {
				return err
			}
			if strings.EqualFold(r.Name, "sqlite_sequence") {
				if seqs, err = sequencesOf(p, r); err != nil {
					return err
				}
			}
		case carriedObject(r):
			b.Object(r.Type, r.Name, r.TblName, r.SQL, r.Rowid)
		}
	}
	slices.SortStableFunc(seqs, func(a, b sequence) int { return strings.Compare(a.Table, b.Table) })
	for _, s := range seqs {
		b.Sequence(s.Table, s.Seq)
	}
	return b.Finish()
}

// storedIndexEntries returns a function that reads an index's entries from the
// source b-tree in b-tree order. Returns false if no b-tree or walk fails.
func storedIndexEntries(p *pager, rows []schemaRow) func(string) ([][]Value, bool) {
	roots := map[string]uint32{}
	for _, r := range rows {
		if r.Type == "index" && r.RootPage != 0 {
			roots[strings.ToLower(r.Name)] = r.RootPage
		}
	}
	return func(index string) ([][]Value, bool) {
		root, ok := roots[strings.ToLower(index)]
		if !ok {
			return nil, false
		}
		var recs [][]Value
		seq, errFn := p.ScanWithoutRowidRows(root)
		for _, rec := range seq {
			recs = append(recs, slices.Clone(rec))
		}
		return recs, errFn() == nil
	}
}

func removeDB(path string) {
	os.Remove(path)
	os.Remove(path + ".delta")
}

func corruptErr(err error) error {
	return fmt.Errorf("%w: %v (pass ImportOptions{Force: true} to convert it anyway)", ErrSourceCorrupt, err)
}

func corrupt(found []string) error {
	return fmt.Errorf("%w (pass ImportOptions{Force: true} to convert it anyway):\n%s", ErrSourceCorrupt, strings.Join(found, "\n"))
}

// carriedTable reports whether r is a table whose rows are carried: every
// table with a b-tree except the engine's "sqlite_" tables, but including
// sqlite_sequence and sqlite_stat tables (which C reads as ordinary tables).
func carriedTable(r schemaRow) bool {
	if r.Type != "table" || r.RootPage == 0 {
		return false
	}
	name := strings.ToLower(r.Name)
	return !strings.HasPrefix(name, "sqlite_") || name == "sqlite_sequence" || strings.HasPrefix(name, "sqlite_stat")
}

// carriedObject reports whether r is a catalog entry with no rows of its own
// that the engine must know about: index, view, trigger, or virtual table.
// Automatic indexes and other "sqlite_" entries are recreated by table creation.
func carriedObject(r schemaRow) bool {
	switch r.Type {
	case "index", "view", "trigger":
	case "table":
		if r.RootPage != 0 {
			return false
		}
	default:
		return false
	}
	return strings.TrimSpace(r.SQL) != "" && !strings.HasPrefix(r.Name, "sqlite_")
}

// importTable imports one table's stored rows. A rowid table's record is
// already stored (INTEGER PRIMARY KEY is NULL in it, rowid carries it); a
// WITHOUT ROWID table's primary key is reordered to declared order.
func importTable(p *pager, b *engine.Builder, r schemaRow) error {
	t, err := b.Table(r.Name, r.SQL, r.Rowid)
	if err != nil {
		return err
	}
	shape := t.Shape()
	var aerr error
	if shape.WithoutRowid {
		sp := storedPositions(shape)
		seq, errFn := p.ScanWithoutRowidRows(r.RootPage)
		for _, rec := range seq {
			if aerr = t.Add(0, storedFromWR(shape, sp, rec)); aerr != nil {
				break
			}
		}
		if err := errFn(); err != nil {
			return fmt.Errorf("scanning %s: %w", r.Name, err)
		}
	} else {
		seq, errFn := p.ScanTable(r.RootPage)
		for rid, vals := range seq {
			if aerr = t.Add(int64(rid), slices.Clone(vals)); aerr != nil {
				break
			}
		}
		if err := errFn(); err != nil {
			return fmt.Errorf("scanning %s: %w", r.Name, err)
		}
	}
	return aerr
}

// sequencesOf reads sqlite_sequence rows (name, seq pairs) from the b-tree.
func sequencesOf(p *pager, r schemaRow) ([]sequence, error) {
	var out []sequence
	seq, errFn := p.ScanTable(r.RootPage)
	for _, v := range seq {
		if len(v) == 2 && v[0].Typ == Text {
			out = append(out, sequence{Table: string(v[0].S), Seq: intOf(v[1])})
		}
	}
	return out, errFn()
}

// sequence is one sqlite_sequence row.
type sequence struct {
	Table string
	Seq   int64
}

func intOf(v Value) int64 {
	switch v.Typ {
	case Int:
		return v.I
	case Float:
		return int64(v.F)
	}
	return 0
}
