package sqlite

import "fmt"

// schemaRow is one row of a SQLite file's sqlite_schema, as stored.
type schemaRow struct {
	Type, Name, TblName string
	RootPage            uint32
	SQL                 string // "" where the stored sql is NULL
	Rowid               int64
}

// schema reads sqlite_schema (page 1's b-tree) in rowid order.
func (p *pager) schema() ([]schemaRow, error) {
	if p.nPages == 0 {
		return nil, nil // a zero-length file: an empty database
	}
	seq, errFn := p.ScanTable(1)
	var out []schemaRow
	for rowid, v := range seq {
		if len(v) < 5 {
			return nil, fmt.Errorf("sqlite: sqlite_schema row %d has %d columns", rowid, len(v))
		}
		r := schemaRow{Type: string(v[0].S), Name: string(v[1].S), TblName: string(v[2].S), Rowid: int64(rowid)}
		if v[3].Typ == Int {
			r.RootPage = uint32(v[3].I)
		}
		if v[4].Typ == Text {
			r.SQL = string(v[4].S)
		}
		out = append(out, r)
	}
	return out, errFn()
}

// autoVacuumModeOfHeader returns PRAGMA auto_vacuum from header bytes:
// 0 for none, 1 for full, 2 for incremental. Header offset 52 is the largest
// root page number; its zero/non-zero status determines the mode.
func autoVacuumModeOfHeader(largestRoot, incrVacuum uint32) int {
	if largestRoot == 0 {
		return 0
	}
	if incrVacuum == 0 {
		return 1
	}
	return 2
}
