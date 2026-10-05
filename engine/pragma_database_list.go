package engine

import "path/filepath"

// pragma_database_list returns one row per open database. seq is the aDb index
// in the current list; file is absolute; temp's file is empty.
func (p *ReadOnlyPager) pragmaDatabaseList() (cols []string, rows [][]Value, err error) {
	cols = []string{"seq", "name", "file"}
	row := func(seq int64, name, file string) []Value {
		return []Value{
			{Typ: Int, I: seq},
			{Typ: Text, S: []byte(name)},
			{Typ: Text, S: []byte(file)},
		}
	}
	rows = append(rows, row(0, localSchemaOr(p.localSchema), databaseListFile(p.mainPath, p.inMemory)))
	if p.tempOpen {
		// aDb[1] always; temp database has no file.
		rows = append(rows, row(1, "temp", ""))
	}
	seq := int64(2) // aDb[2] onward, whether or not temp's slot is filled
	for _, ar := range p.attachedReaders {
		if ar.name == "temp" {
			continue // already placed at its own index
		}
		rows = append(rows, row(seq, ar.name, databaseListFile(ar.path, ar.inMemory)))
		seq++
	}
	return cols, rows, nil
}

// databaseListFile returns the "file" cell: empty for in-memory databases,
// otherwise the absolute path.
func databaseListFile(path string, inMemory bool) string {
	if inMemory || path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// SetMainPath records the main database path.
func (p *ReadOnlyPager) SetMainPath(path string) {
	if p != nil {
		p.mainPath = path
	}
}

// SetTempOpen records that the connection's TEMP database is open.
func (p *ReadOnlyPager) SetTempOpen(v bool) {
	if p != nil {
		p.tempOpen = v
	}
}
