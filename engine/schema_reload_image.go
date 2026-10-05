// This file is the schema reload behind "PRAGMA writable_schema=RESET" and the
// one alter.c schedules after every ALTER TABLE: throw the parsed schema away
// and rebuild it from the catalog.
//
// # What C does
//
// sqlite3ResetAllSchemasOfConnection (build.c:650) only erases; the rebuild
// happens at the next prepare, in sqlite3InitOne (prepare.c:199) calling
// sqlite3InitCallback (prepare.c:96) per sqlite_schema row. The callback
// ignores the type column: it branches on whether the sql text starts with
// "cr" (prepare.c:115-117), re-parses it with db->init.busy set, and takes the
// root page from argv[3] (prepare.c:135; build.c:2673 for a table, 4393 for an
// index; prepare.c:174-184 for an autoindex row, matched to its Index by
// name). pragma.c:1182's RESET arm and alter.c:111's renameReloadSchema both
// schedule it.
//
// # This engine's reload
//
// The live schema is db.tables/indexes/views/triggers, built from a catalog by
// loadSchemaObjects -- the pass OpenWrite runs. The reload re-runs it over the
// edited rows (reloadSchemaFromSegmentCatalog), carrying each surviving table's
// rows across by routing root: the catalog is read as a fresh connection
// would. This file holds the per-row checks: rows C's load refuses
// (wsInsertedRowCorrupt, schemaLoadCorruptRow) and agreement checks that keep
// a reload from answering where C's would not (reloadCatalogTextAgreesWithRow,
// reloadAutoIndexRowsAgree).
//
// # Declined
//
//   - an edited name: C matches an autoindex row to an Index by name
//     (prepare.c:174), so a name matching a different index moves its root;
//     this engine matches sql=NULL rows to derived automatic indexes by
//     position (autoIdxOrder) and would reload a healthy schema where C loads
//     a broken one (fkey1.test 8.2);
//   - an edited type: C branches on the text, loadSchemaObjects on the type
//     column, so a disagreement means different objects;
//   - an inserted row naming storage: this format names storage by directory
//     entry, not page;
//   - a reload inside a transaction unless the cookie moved: C's ROLLBACK
//     leaves reverted rows under the reloaded schema, this engine's restores
//     both.
//
// Root page assignments are schema_reload_rootpage.go's.
package engine

import (
	"fmt"
	"strings"
)

// wsInsertedRowCorrupt applies sqlite3InitCallback's per-row rules
// (prepare.c:145-200) to the catalog rows this session inserted, in rowid
// order, and reports the first the load would refuse: corruptSchema's object
// name and optional detail. A RESET then fails wholesale: every later
// statement is "malformed database schema (%s)" until "PRAGMA
// writable_schema=ON" (clearSchemaCorrupt). e.g. "INSERT INTO sqlite_schema
// DEFAULT VALUES" then RESET.
//
// The arms, in C's order:
//
//   - rootpage NULL (argv[3]==0, prepare.c:163) -- no detail;
//   - a "CR..." text is a CREATE for the parser (the ordinary reload path);
//   - otherwise a NULL name, or non-empty sql that is not a CREATE
//     (prepare.c:197) -- no detail;
//   - otherwise an automatic-index row matched to a live index by name
//     (prepare.c:203); no match is "orphan index".
func (db *DB) wsInsertedRowCorrupt() (obj, detail string, bad bool) {
	for i := 1; i <= db.wsInserted; i++ {
		for _, temp := range []bool{false, true} {
			e := db.wsEdits[wsCatalogKey{ins: i, temp: temp}]
			if e == nil || e.deleted {
				continue
			}
			name := wsInsertedRowText(e, 1)
			if v := e.set[3]; v == nil || v.Typ == Null {
				return name, "", true
			}
			sql := wsInsertedRowText(e, 4)
			if len(sql) >= 2 && equalFoldName(sql[:2], "cr") {
				continue // a CREATE: the ordinary reload parses it
			}
			if e.set[1] == nil || e.set[1].Typ == Null || sql != "" {
				return name, "", true
			}
			if db.findIndexMetaIn(scopeAny, name) == nil {
				return name, "orphan index", true
			}
		}
	}
	return "", "", false
}

// wsInsertedRowText is one column of an inserted catalog row as TEXT, "" for a
// column the INSERT did not give or gave as NULL.
func wsInsertedRowText(e *wsCatalogEdit, col int) string {
	if v := e.set[col]; v != nil && v.Typ != Null {
		return valueText(*v)
	}
	return ""
}

// reloadCatalogTextAgreesWithRow reports whether every catalog row with CREATE
// text names, in that text, the same kind and name its type/name columns
// claim. sqlite3InitCallback takes both from the text (prepare.c:115-117);
// loadSchemaObjects takes them from the columns. A direct write separates
// them -- "UPDATE sqlite_schema SET sql='CREATE TABLE zzz(a,b)' WHERE
// name='t'" makes C hold a table zzz -- so a disagreement declines.
func reloadCatalogTextAgreesWithRow(rows []SchemaRow) bool {
	for i := range rows {
		sr := &rows[i]
		if strings.TrimSpace(sr.SQL) == "" {
			continue // an autoindex row; reloadAutoIndexRowsAgree owns those
		}
		toks, err := lex(sr.SQL)
		if err != nil || len(toks) == 0 {
			return false
		}
		kind := ""
		for _, t := range toks {
			if t.kind != tkIdent || t.quoted {
				break
			}
			switch t.upper() {
			case "TABLE", "INDEX", "VIEW", "TRIGGER":
				kind = strings.ToLower(t.upper())
			}
			if kind != "" {
				break
			}
		}
		if kind != sr.Type {
			return false
		}
		name, ok := ddlObjectName(toks)
		if !ok || !equalFoldName(name, sr.Name) {
			return false
		}
	}
	return true
}

// reloadAutoIndexRowsAgree reports whether every automatic index the reload
// derived has its own sql=NULL catalog row, and vice versa.
//
// loadSchemaObjects checks only the rows-to-indexes direction (a missing
// constraint). An extra derived index cannot occur in a file either engine
// wrote, but a reload can produce one:
//
//	CREATE TABLE t(a,b); INSERT INTO t VALUES(1,1),(1,2);
//	PRAGMA writable_schema=ON;
//	UPDATE sqlite_schema SET sql='CREATE TABLE t(a UNIQUE,b)' WHERE name='t';
//	PRAGMA writable_schema=RESET;
//	SELECT type,name FROM sqlite_schema     -- C: table|t only
//
// C builds that Index in memory with tnum left at the table's own root (only
// an autoindex row corrects it, prepare.c:174-184), so it reads the table's
// b-tree -- something this engine cannot represent. Declined. The converse,
// a sql=NULL row with no derived index, is "orphan index" (prepare.c:176)
// either way.
func (db *DB) reloadAutoIndexRowsAgree(rows []SchemaRow) bool {
	type key struct {
		name string
		temp bool
	}
	want := map[key]int{}
	for i := range rows {
		if rows[i].Type == "index" && rows[i].SQL == "" {
			want[key{r33sFoldIdent(rows[i].TblName), rows[i].Temp}]++
		}
	}
	got := map[key]int{}
	for _, ix := range db.indexes {
		if ix.sql == "" && !ix.isTablePK {
			got[key{r33sFoldIdent(ix.table), ix.isTemp}]++
		}
	}
	if len(want) != len(got) {
		return false
	}
	for k, n := range want {
		if got[k] != n {
			return false
		}
	}
	return true
}

// noteReloadedFromImage marks every table the reload just installed as one
// this session has WRITTEN to (tableMeta.rowsMutatedThisSession).
//
// It is what keeps the reload from destroying data. A catalog rewrite reloads
// every table it can from the file (segment_ddl.go), and every
// tableMeta this reload produces is fresh, so its rowsMutatedThisSession starts
// false however much this session had already inserted. Left alone, a session
// that inserted into an existing table, then edited its sqlite_master row, then
// RESET, would have those inserts replaced at commit by the file's rows.
func (db *DB) noteReloadedFromImage() {
	for _, tbl := range db.tables {
		tbl.rowsMutatedThisSession, tbl.rowsWrittenSinceCommit = true, true
	}
}

// reloadSchemaForALTER is alter.c:111's renameReloadSchema for this engine: an
// ALTER TABLE run while a direct sqlite_master write is outstanding must see
// the edited definitions, since C's ALTER rewrites the catalog rows
// (sqlite_rename_table, alter.c:216-240) and reloads from them. With t1 edited
// to "CREATE TABLE t1(a NOT NULL,b)" and t1a to "CREATE UNIQUE INDEX t1a ON
// t1(a)" (pragma.test pragma-3.20):
//
//	ALTER TABLE t1 RENAME TO t1x
//	SELECT type,name,sql FROM sqlite_schema
//	  table  t1x  CREATE TABLE "t1x"(a NOT NULL,b)
//	  index  t1a  CREATE UNIQUE INDEX t1a ON "t1x"(a)
//
// Reloading first and then renaming gives the same rows, since the rename
// rewrites each object's stored sql. It reports whether the reload ran.
func (db *DB) reloadSchemaForALTER() bool {
	// The segment format's own whole-catalog reload -- the one RESET runs.
	return db.reloadSchemaFromSegmentCatalog(false)
}

// schemaLoadCorruptRow applies sqlite3InitCallback's per-row rules
// (prepare.c:145-200) to a catalog as read, reporting the first row the load
// refuses. It is wsInsertedRowCorrupt's counterpart for rows already in the
// file, which every later connection sees. C fails the whole load until
// SQLITE_WriteSchema is set (corruptSchema's silent arm, prepare.c:45-46), so
// callers check it only while writable_schema is off.
//
// The orphan-index arm (prepare.c:203) is not applied here: it matches by name,
// and a name this loader derives differently from C would make a healthy
// database corrupt. It stays on the overlay path, where every row is this
// session's own insert.
func schemaLoadCorruptRow(vals [][]Value) (obj, detail string, bad bool) {
	for _, r := range vals {
		if len(r) != wsCatalogColumnCount {
			continue
		}
		name := ""
		if r[1].Typ != Null {
			name = valueText(r[1])
		}
		if r[3].Typ == Null { // argv[3]==0, prepare.c:163
			return corruptSchemaObj(name), "", true
		}
		sql := ""
		if r[4].Typ != Null {
			sql = valueText(r[4])
		}
		if len(sql) >= 2 && equalFoldName(sql[:2], "cr") {
			continue // a CREATE: the parser's business, not this check's
		}
		if r[1].Typ == Null || sql != "" { // prepare.c:197
			return corruptSchemaObj(name), "", true
		}
		// The row has a name, a rootpage and no SQL, which in a healthy catalog
		// means an AUTOMATIC index -- one a UNIQUE or PRIMARY KEY constraint
		// created, whose Index the CREATE TABLE row already built. C looks that
		// Index up BY NAME (prepare.c:203) and calls a miss an orphan index.
		//
		// This applies only the half that cannot misfire: a name C SQLite
		// never gives an automatic index is one no CREATE TABLE can have built,
		// so the lookup provably fails. Checking the other half needs this
		// loader's own automatic-index naming to agree with C's for every table
		// shape, and being WRONG there would turn a healthy database into a
		// corrupt one -- the one direction this check must never take.
		if !strings.HasPrefix(strings.ToLower(name), "sqlite_autoindex_") {
			return corruptSchemaObj(name), "orphan index", true
		}
	}
	return "", "", false
}

// corruptSchemaObj is prepare.c:49's `azObj[1] ? azObj[1] : "?"`.
func corruptSchemaObj(name string) string {
	if name == "" {
		return "?"
	}
	return name
}

// schemaCorruptVerdict caches what schemaLoadCorruptRefuses found, keyed by the
// schema cookie -- C's own key (sqlite3ReadSchema re-reads only when it
// moves). Every statement that can add a malformed catalog row changes the
// schema and so the cookie; a direct sqlite_schema write short-circuits above
// this. gen, bumped by every commit, is a second key: caching a clean answer
// is the dangerous direction.
type schemaCorruptVerdict struct {
	valid  bool
	cookie uint32
	gen    uint64
	obj    string
	detail string
	bad    bool
}

// schemaLoadCorruptRefuses is schemaLoadCorruptRow as a read applies it: a
// catalog this pager cannot load is "malformed database schema (%s)" for every
// statement needing the schema, as on a fresh C connection. PRAGMA is exempt:
// "PRAGMA writable_schema=ON" lifts it (corruptSchema's silent arm,
// prepare.c:45-46).
func (p *ReadOnlyPager) schemaLoadCorruptRefuses(sqlText string) error {
	if p == nil || p.writableSchema {
		return nil
	}
	if verb, ok := LeadingStatementVerb(sqlText); ok && strings.EqualFold(verb, "PRAGMA") {
		return nil
	}
	if c := p.schemaCorrupt; c != nil && c.valid &&
		c.cookie == p.meta.schemaCookie && c.gen == p.schemaCorruptGen {
		if !c.bad {
			return nil
		}
		return schemaCorruptError(c.obj, c.detail)
	}
	// EVERY database's catalog, not just this one's: sqlite3ReadSchema reads
	// them all (prepare.c:470's loop over db->aDb), so a malformed row in the
	// TEMP database fails a query against MAIN exactly as one in main does --
	// measured, and it is the whole reason "INSERT INTO sqlite_temp_schema
	// DEFAULT VALUES; PRAGMA writable_schema=RESET" then breaks "SELECT ... FROM
	// sqlite_schema".
	obj, detail, bad := "", "", false
	for _, src := range p.schemaLoadCatalogs() {
		rows, err := rawCatalogRows(src)
		if err != nil {
			continue // an unreadable catalog is its own error, raised where it is read
		}
		if obj, detail, bad = schemaLoadCorruptRow(rows); bad {
			break
		}
	}
	if c := p.schemaCorrupt; c != nil {
		c.valid, c.cookie, c.gen = true, p.meta.schemaCookie, p.schemaCorruptGen
		c.obj, c.detail, c.bad = obj, detail, bad
	}
	if !bad {
		return nil
	}
	return schemaCorruptError(obj, detail)
}

// schemaCorruptError is the message C SQLite gives, built in one place so the
// cached verdict above and the fresh scan below it cannot drift apart.
func schemaCorruptError(obj, detail string) error {
	if detail == "" {
		return fmt.Errorf("malformed database schema (%s)", obj)
	}
	return fmt.Errorf("malformed database schema (%s) - %s", obj, detail)
}

// schemaLoadCatalogs is every database a statement on p can see: p itself and
// each reader wired onto it (the TEMP database among them). See
// schemaLoadCorruptRefuses.
func (p *ReadOnlyPager) schemaLoadCatalogs() []*ReadOnlyPager {
	out := make([]*ReadOnlyPager, 0, 1+len(p.attachedReaders))
	out = append(out, p)
	for i := range p.attachedReaders {
		if rp := p.attachedReaders[i].pager; rp != nil {
			out = append(out, rp)
		}
	}
	return out
}
