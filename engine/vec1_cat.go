package engine

// vec1cat, the eponymous table listing a connection's vec1 tables:
// "SELECT * FROM vec1cat" gives each one's database, name, model (as JSON)
// and query profile (vec1catColumnMethod).
//
// vec1 walks its connection's list of connected tables, newest first, after
// connecting every vec1 table in every attached database. A table connected
// before the one created after it is the case that order depends on, which
// this does not track: the tables are given newest-created first.

import (
	"fmt"
	"strings"
)

type vec1CatModule struct{}

func (vec1CatModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	return []VtabColumn{{Name: "database"}, {Name: "name"}, {Name: "model"}, {Name: "qprofile"}}, vec1CatTable{}, nil
}

// vec1CatTable answers from the database (vec1CatRows), so it never opens a
// cursor.
type vec1CatTable struct{}

func (vec1CatTable) BestIndex(info *VtabIndexInfo) error { return nil }
func (vec1CatTable) Open() (VtabCursor, error) {
	return nil, fmt.Errorf("vec1cat: no cursor")
}

// vec1CatRows is vec1cat's rows.
func (p *ReadOnlyPager) vec1CatRows() ([][]Value, []int64, error) {
	schema, err := p.Schema()
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, r := range schema {
		if r.Type != "table" || r.Temp || !isCreateVirtualTableSQL(r.SQL) {
			continue
		}
		_, module, _, _, perr := parseCreateVirtualTableStmt(r.SQL)
		if perr == nil && strings.EqualFold(module, "vec1") {
			names = append(names, r.Name)
		}
	}
	c := p.vec1Conn()
	c.mu.Lock()
	defer c.mu.Unlock()
	var rows [][]Value
	var rowids []int64
	for i := len(names) - 1; i >= 0; i-- {
		name := names[i]
		t := c.tab("main", name)
		model, err := vec1CatModel(t, p.vec1Shadow(name))
		if err != nil {
			return nil, nil, err
		}
		rows = append(rows, []Value{
			{Typ: Text, S: []byte("main")},
			{Typ: Text, S: []byte(name)},
			model,
			{Typ: Text, S: []byte(vec1CatProfile(t))},
		})
		rowids = append(rowids, int64(len(rows)))
	}
	return rows, rowids, nil
}

// vec1CatModel is vec1cat's model column, with the config loaded first.
func vec1CatModel(t *vec1Tab, sh *vec1PagerShadow) (Value, error) {
	if err := t.loadConfig(sh); err != nil {
		return Value{}, err
	}
	h := t.mod.hdr
	dist := "cos"
	if h.distance == vec1DistL2 {
		dist = "l2"
	}
	if h.nCodebook == 0 && h.nBucket == 0 {
		index := "none"
		if h.flags&vec1ModelIndex != 0 {
			index = "flat"
		}
		return Value{Typ: Text, S: []byte(fmt.Sprintf("{\"index\": \"%s\", \"distance\": \"%s\"}", index, dist))}, nil
	}
	quant := "none"
	if h.nCodebook != 0 {
		switch {
		case h.flags&vec1ModelBitQuant != 0:
			quant = "bq"
		case h.flags&vec1ModelRotate != 0:
			quant = "opq"
		default:
			quant = "pq"
		}
	}
	tf := func(b bool) string {
		if b {
			return "true"
		}
		return "false"
	}
	return Value{Typ: Text, S: []byte(fmt.Sprintf("{\"index\": \"trained\", \"distance\": \"%s\", \"nbucket\": %d, \"quantizer\": \"%s\", \"codesize\": %d, \"residual\": %s, \"wht\": %s}",
		dist, int32(h.nBucket), quant, int32(h.nCodebook), tf(h.flags&vec1ModelResidual != 0), tf(h.flags&vec1ModelWHT != 0)))}, nil
}

// vec1CatProfile is vec1cat's qprofile column: each counter's count and the
// time it took, in nanoseconds where vec1 counts CPU cycles.
func vec1CatProfile(t *vec1Tab) string {
	names := []string{"total", "init", "coarse", "idxread", "metaread", "metascan", "lut", "pqscan", "finalsort"}
	var b strings.Builder
	b.WriteString("{")
	for i, n := range names {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "\n  \"%s\": {\"cnt\": %d, \"cycle\": %d}", n, t.counts[i], t.cycles[i])
	}
	b.WriteString("\n}")
	return b.String()
}
