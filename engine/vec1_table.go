package engine

// The vec1 virtual table: "CREATE VIRTUAL TABLE t USING vec1(vector, meta...)".
// This file is the table's declaration, its shadow tables and the state a
// connection keeps for it; vec1_write.go is xUpdate and its commands,
// vec1_query.go the reads.
//
// vec1 keeps everything in five ordinary tables beside the virtual one, and so
// does this engine, byte for byte: %_config (key/value), %_base (each row's
// rowid, vector and metadata), %_idx (the index lists, vec1_list.go) with its
// index %_idx_idx, %_model (the model blob) and %_meta (the metadata lists).
// They are real tables in the catalog, which is what keeps a file holding a
// vec1 table interchangeable with C SQLite through the converter, and what
// lets the virtual table hold nothing of its own.
//
// What a connection holds is what vec1's Vec1Tab holds (vec1Tab below): the
// %_config values and the model, loaded at the points vec1 loads them, and the
// writer -- the lists INSERTs build in memory, written to %_idx only when the
// transaction syncs or a savepoint opens or is released. A query in the same
// transaction does not see what is still in the writer, as in C.

import (
	"fmt"
	"strings"
	"sync"
)

func init() {
	RegisterVtabModule("vec1", vec1Module{})
	RegisterVtabModule("vec1cat", vec1CatModule{})
}

// vec1Module is the vec1 module.
type vec1Module struct{}

// The declared columns, after this engine's leading hidden rowid slot (as
// fts5's, vtabSyntheticRowidSlot): vec1 declares
// "CREATE TABLE v1(cmd HIDDEN, arg HIDDEN, distance HIDDEN, <vector> BLOB, <meta>...)".
const (
	vec1ColRowid    = 0
	vec1ColCmd      = 1
	vec1ColArg      = 2
	vec1ColDistance = 3
	vec1ColVector   = 4
	vec1ColMeta     = 5 // the first metadata column

	vec1MaxMetaColumns = 256
)

func (vec1Module) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	st, err := newVec1Store("", args)
	if err != nil {
		return nil, nil, err
	}
	return st.cols, st, nil
}

func (vec1Module) newWritableStore(tableName string, args []string) (vtabStore, error) {
	return newVec1Store(tableName, args)
}

// vec1Store is a vec1 table's vtabStore. It holds the table's declaration
// only: the rows are in the shadow tables, and the connection's state for the
// table in vec1Tab.
type vec1Store struct {
	name  string
	cols  []VtabColumn
	nMeta int
}

// vec1CheckColumnName is vec1CheckColumnName: one token or quoted name, with
// nothing after it, and not "rowid" or "distance".
func vec1CheckColumnName(arg string) error {
	ws := func(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
	z := 0
	for z < len(arg) && ws(arg[z]) {
		z++
	}
	var name []byte
	add := func(c byte) {
		if len(name) < 16 {
			name = append(name, c)
		}
	}
	switch {
	case z < len(arg) && (arg[z] == '"' || arg[z] == '\'' || arg[z] == '`'):
		q := arg[z]
		z++
		for {
			if z >= len(arg) {
				return fmt.Errorf("vec1: unmatched quote character")
			}
			if arg[z] == q {
				z++
				if z >= len(arg) || arg[z] != q {
					break
				}
			}
			add(arg[z])
			z++
		}
	case z < len(arg) && arg[z] == '[':
		z++
		for z < len(arg) && arg[z] != ']' {
			add(arg[z])
			z++
		}
		if z >= len(arg) {
			return fmt.Errorf("vec1: unmatched [ character")
		}
		z++
	default:
		for z < len(arg) && !ws(arg[z]) {
			add(arg[z])
			z++
		}
	}
	for z < len(arg) && ws(arg[z]) {
		z++
	}
	if z < len(arg) {
		return fmt.Errorf("vec1: trailing characters after column name")
	}
	if (len(name) == 5 && strings.EqualFold(string(name), "rowid")) || (len(name) == 8 && strings.EqualFold(string(name), "distance")) {
		return fmt.Errorf("vec1: reserved column name: %s", arg)
	}
	return nil
}

// newVec1Store is vec1CreateConnect's checks and vec1DeclareVtab.
func newVec1Store(name string, args []string) (*vec1Store, error) {
	nMeta := max(len(args)-1, 0)
	if nMeta > vec1MaxMetaColumns {
		return nil, fmt.Errorf("too many meta-data columns (max is %d)", vec1MaxMetaColumns)
	}
	for _, a := range args {
		if err := vec1CheckColumnName(a); err != nil {
			return nil, err
		}
	}
	vector := "vector"
	if len(args) > 0 {
		vector = args[0]
	}
	var decl strings.Builder
	decl.WriteString("CREATE TABLE v1(cmd, arg, distance, ")
	decl.WriteString(vector)
	decl.WriteString(" BLOB")
	for _, a := range args[min(1, len(args)):] {
		decl.WriteString(", ")
		decl.WriteString(a)
	}
	decl.WriteString(")")
	cols, _, _, err := parseCreateTableColumnsAndAutoIndexes(decl.String(), false)
	if err != nil {
		return nil, fmt.Errorf("vec1: %s", strings.TrimPrefix(err.Error(), "engine: "))
	}
	if len(cols) != 4+nMeta {
		return nil, fmt.Errorf("vec1: internal: declared %d columns for %d arguments", len(cols), len(args))
	}
	st := &vec1Store{name: name, nMeta: nMeta}
	st.cols = append(st.cols,
		VtabColumn{Name: "rowid", Type: "INTEGER", Hidden: true},
		VtabColumn{Name: cols[0].Name, Hidden: true, NumericAffinity: true},
		VtabColumn{Name: cols[1].Name, Hidden: true, NumericAffinity: true},
		VtabColumn{Name: cols[2].Name, Hidden: true, NumericAffinity: true},
		VtabColumn{Name: cols[3].Name, Type: "BLOB"})
	for _, c := range cols[4:] {
		st.cols = append(st.cols, VtabColumn{Name: c.Name, Type: c.DeclType})
	}
	return st, nil
}

func (s *vec1Store) vtabColumns() []VtabColumn { return s.cols }

// vtabRows is not how a vec1 table is read or written: vec1_query.go reads
// the shadow tables and vec1_write.go is the write path, both reached ahead
// of every generic caller of this.
func (s *vec1Store) vtabRows() (rowids []int64, rows [][]Value) { return nil, nil }
func (s *vec1Store) vtabClone() vtabStore                       { cp := *s; return &cp }
func (s *vec1Store) vtabLoadRow(rowid int64, record []Value)    {}
func (s *vec1Store) vtabDetach()                                {}

func (s *vec1Store) BestIndex(info *VtabIndexInfo) error { return nil }
func (s *vec1Store) Open() (VtabCursor, error) {
	return nil, fmt.Errorf("%w: vec1: a cursor over a vec1 table outside its own read path", errVDBEUnsupported)
}

func (s *vec1Store) InsertRow(vals []Value) (int64, error) {
	return 0, fmt.Errorf("%w: vec1: write outside its own write path", errVDBEUnsupported)
}
func (s *vec1Store) UpdateRow(oldRowid int64, vals []Value) (int64, error) {
	return 0, fmt.Errorf("%w: vec1: write outside its own write path", errVDBEUnsupported)
}
func (s *vec1Store) DeleteRow(rowid int64) error {
	return fmt.Errorf("%w: vec1: write outside its own write path", errVDBEUnsupported)
}

// vec1ModuleOf reports whether vm is a vec1 table.
func vec1ModuleOf(vm *vtabMeta) (*vec1Store, bool) {
	if vm == nil {
		return nil, false
	}
	st, ok := vm.store.(*vec1Store)
	return st, ok
}

// ---- the shadow tables -------------------------------------------------------

// vec1Suffixes are the shadow tables, in vec1ShadowSchema's order.
var vec1Suffixes = []string{"_config", "_base", "_idx", "_model", "_meta"}

// vec1ShadowDDL is vec1ShadowSchema's statements as C SQLite stores them
// (its "%Q." qualifier is not part of the stored text).
func vec1ShadowDDL(name string, nMeta int) (tables []string, index string) {
	q := strings.ReplaceAll(name, "'", "''")
	var meta strings.Builder
	for i := range nMeta {
		fmt.Fprintf(&meta, ", c%d", i)
	}
	tables = []string{
		"CREATE TABLE '" + q + "_config'(id INTEGER PRIMARY KEY, val ANY)",
		"CREATE TABLE '" + q + "_base'(id INTEGER PRIMARY KEY, vector BLOB" + meta.String() + ")",
		"CREATE TABLE '" + q + "_idx'(id INTEGER PRIMARY KEY, bucket INTEGER, first INTEGER, last INTEGER, val BLOB)",
		"CREATE TABLE '" + q + "_model'(id INTEGER PRIMARY KEY, val BLOB)",
		"CREATE TABLE '" + q + "_meta'(id INTEGER PRIMARY KEY, val BLOB)",
	}
	index = "CREATE INDEX '" + q + "_idx_idx' ON '" + q + "_idx'(bucket, first, last)"
	return tables, index
}

// createShadowTables is xCreate's half of vec1CreateConnect: the five tables
// and the index, in vec1's order (the index right after %_idx), then the
// format version in %_config.
func (vec1Module) createShadowTables(db *DB, name string, st *vec1Store, isTemp bool) error {
	tables, index := vec1ShadowDDL(name, st.nMeta)
	scope := createScope(isTemp)
	var created []string
	undo := func() {
		for _, n := range created {
			if tbl := db.findTableMetaIn(scope, n); tbl != nil {
				db.removeTableAndIndexes(tbl)
			}
		}
	}
	for i, sql := range tables {
		if isTemp {
			sql = "CREATE TEMP TABLE" + strings.TrimPrefix(sql, "CREATE TABLE")
		}
		if err := db.CreateTable(sql); err != nil {
			undo()
			return err
		}
		created = append(created, name+vec1Suffixes[i])
		if vec1Suffixes[i] == "_idx" {
			isql := index
			if isTemp {
				isql = "CREATE INDEX temp." + strings.TrimPrefix(index, "CREATE INDEX ")
			}
			if err := db.CreateIndex(isql); err != nil {
				undo()
				return err
			}
		}
	}
	t := db.vec1Shadow(scope, name)
	return t.put("_config", vec1CfgFmtVersion, []Value{{Typ: Null}, {Typ: Int, I: vec1FmtVersion}})
}

// dropShadowTables is xDestroy's "DROP TABLE IF EXISTS" of each.
func (vec1Module) dropShadowTables(db *DB, name string, isTemp bool) {
	scope := createScope(isTemp)
	for _, suffix := range vec1Suffixes {
		if tbl := db.findTableMetaIn(scope, name+suffix); tbl != nil {
			db.removeTableAndIndexes(tbl)
		}
	}
}

// ---- the connection's state ----------------------------------------------------

// The %_config keys (VEC1_CONFIG_*).
const (
	vec1CfgModel        = 0
	vec1CfgBlocksize    = 1
	vec1CfgNElem        = 2
	vec1CfgBlocksizeMin = 3
	vec1CfgFmtVersion   = 5
)

// vec1Config is Vec1Config.
type vec1Config struct {
	modelVersion int64
	blocksize    int
	blocksizeMin int
	nElem        int
}

// Vec1Connection is the state vec1 keeps per database connection: its
// Vec1TabList (vec1_config()'s nthread and nprobe) and each table's Vec1Tab.
// The driver holds one per connection; an engine-direct DB makes its own.
type Vec1Connection struct {
	mu      sync.Mutex
	nThread int
	nProbe  float64
	tabs    map[string]*vec1Tab
	// gen moves whenever a table's state is dropped (forget), so a writer
	// kept from before knows its vec1Tab may be gone (vtabMeta.vec1w).
	gen uint64
}

// NewVec1Connection is a connection's vec1 state, as initExtension sets it.
func NewVec1Connection() *Vec1Connection {
	return &Vec1Connection{nThread: 1, nProbe: vec1DefaultNProbe, tabs: map[string]*vec1Tab{}}
}

// vec1Tab is Vec1Tab: what the connection caches for one table.
type vec1Tab struct {
	cfg    vec1Config
	mod    *vec1Model
	writer *vec1Writer
	// begun is xBegin having run in this transaction (it loads the config).
	begun bool
	// The qprofile counters vec1cat reports.
	cycles, counts [vec1QNCounter]int64
}

// The qprofile counters (VEC1_QINSTR_*).
const (
	vec1QTotal = iota
	vec1QInit
	vec1QCoarse
	vec1QIdxRead
	vec1QMetaRead
	vec1QMetaScan
	vec1QLUT
	vec1QPQScan
	vec1QFinalSort
	vec1QNCounter
)

func vec1TabKey(schema, name string) string {
	return r33sFoldIdent(schema) + "\x00" + r33sFoldIdent(name)
}

// tab returns the connection's state for schema.name, creating it with no
// model (distance L2, as vec1CreateConnect starts it).
func (c *Vec1Connection) tab(schema, name string) *vec1Tab {
	key := vec1TabKey(schema, name)
	t := c.tabs[key]
	if t == nil {
		t = &vec1Tab{mod: vec1NoModel()}
		c.tabs[key] = t
	}
	return t
}

// forget drops a table's state, as xDisconnect does.
func (c *Vec1Connection) forget(schema, name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tabs, vec1TabKey(schema, name))
	c.gen++
}

// vec1NoModel is the model of a table never given one.
func vec1NoModel() *vec1Model {
	return &vec1Model{hdr: vec1Header{distance: vec1DistL2}}
}

// vec1Conn is the DB's connection state, made on first use.
func (db *DB) vec1Conn() *Vec1Connection {
	if db.vec1Conns == nil {
		db.vec1Conns = NewVec1Connection()
	}
	return db.vec1Conns
}

// SetVec1Connection gives this session the connection's vec1 state.
func (db *DB) SetVec1Connection(c *Vec1Connection) { db.vec1Conns = c }

// SetVec1Connection gives this pager the connection's vec1 state.
func (p *ReadOnlyPager) SetVec1Connection(c *Vec1Connection) { p.vec1Conns = c }

// vec1Conn is the pager's connection state: the session's, or a private one
// for a pager made without a connection.
func (p *ReadOnlyPager) vec1Conn() *Vec1Connection {
	if p.vec1Conns == nil {
		p.vec1Conns = NewVec1Connection()
	}
	return p.vec1Conns
}

// ---- reading the config and the model -----------------------------------------

// vec1Reader is the shadow-table access a load needs, from the write session
// (vec1DBShadow) or a read pager (vec1PagerShadow).
type vec1Reader interface {
	get(suffix string, rowid int64) ([]Value, bool, error)
	scan(suffix string, fn func(rowid int64, row []Value) (bool, error)) error
}

// vec1Col is a shadow row's column: the INTEGER PRIMARY KEY is stored NULL
// and every other column is at its position.
func vec1Col(row []Value, i int) Value {
	if i < len(row) {
		return row[i]
	}
	return Value{Typ: Null}
}

// vec1Errorf is an error a module method left in the vtab's zErrMsg.
func vec1Errorf(format string, args ...any) error { return fmt.Errorf(format, args...) }

// loadConfig is vec1LoadConfig: %_config read into t.cfg, the model reloaded
// when its version moved. Every key is read even after an error, and the last
// message wins, as in C.
func (t *vec1Tab) loadConfig(r vec1Reader) error {
	t.cfg.blocksize = vec1BlocksizeDefault
	t.cfg.blocksizeMin = vec1BlocksizeMinDefault
	t.cfg.nElem = 0
	var failed bool
	var lastMsg error
	note := func(err error) {
		failed = true
		lastMsg = err
	}
	err := r.scan("_config", func(id int64, row []Value) (bool, error) {
		val := vec1Col(row, 1)
		switch id {
		case vec1CfgModel:
			v := valueToInt64Trunc(val)
			if t.cfg.modelVersion != v {
				if lerr := t.loadModel(r); lerr != nil {
					note(lerr)
				} else {
					t.cfg.modelVersion = v
				}
			}
		case vec1CfgBlocksize:
			t.cfg.blocksize = min(max(int(int32(valueToInt64Trunc(val))), vec1BlocksizeMin), vec1BlocksizeMax)
		case vec1CfgBlocksizeMin:
			t.cfg.blocksizeMin = min(max(int(int32(valueToInt64Trunc(val))), vec1BlocksizeMin), vec1BlocksizeMax)
		case vec1CfgNElem:
			t.cfg.nElem = int(int32(valueToInt64Trunc(val)))
		case vec1CfgFmtVersion:
			v := int(int32(valueToInt64Trunc(val)))
			if v < vec1FmtVersion {
				note(fmt.Errorf("unrecognized index version"))
			} else if v > vec1FmtVersion {
				note(fmt.Errorf("unsupported index version - requires vec1 %d.%d", v/1000, v%1000))
			}
		default:
			note(fmt.Errorf("unrecognized config entry: %d", int32(id)))
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	if failed {
		return lastMsg
	}
	if t.mod.nElem() != 0 && t.cfg.nElem != t.mod.nElem() {
		msg := fmt.Errorf("model/data vector size mismatch: %d/%d", t.mod.nElem(), t.cfg.nElem)
		t.unloadModel()
		return msg
	}
	return nil
}

// loadIfRequired is vec1LoadConfigIfRequired: the config is taken as not
// loaded while nElem is 0.
func (t *vec1Tab) loadIfRequired(r vec1Reader) error {
	if t.cfg.nElem == 0 {
		if err := t.loadConfig(r); err != nil {
			t.cfg.nElem = 0
			return err
		}
	}
	return nil
}

// loadModel is vec1LoadModel: %_model's blob decoded. A missing row is an
// error, as sqlite3_blob_open's is.
func (t *vec1Tab) loadModel(r vec1Reader) error {
	t.unloadModel()
	row, ok, err := r.get("_model", 1)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("SQL logic error")
	}
	blob := vec1Col(row, 1)
	b := blob.S
	if blob.Typ != Blob && blob.Typ != Text {
		b = []byte(valueToText(blob))
	}
	m, err := vec1DecodeModel(append([]byte(nil), b...))
	if err != nil {
		return err
	}
	t.mod = m
	return nil
}

// unloadModel is vec1UnloadModel: no model, distance 0, version 0.
func (t *vec1Tab) unloadModel() {
	t.mod = &vec1Model{}
	t.cfg.modelVersion = 0
}

// tmpLen is the length of the table's transform buffers, vec1LoadConfig's
// nTmpVec.
func (t *vec1Tab) tmpLen() int {
	return max(t.mod.nElem(), t.mod.nCodeElem*t.mod.nCodebook())
}
