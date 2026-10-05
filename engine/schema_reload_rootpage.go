// This file is the rootpage half of "PRAGMA writable_schema=RESET"'s schema
// reload: what happens when a direct sqlite_master write assigned the rootpage
// column rather than the sql text.
//
// A rootpage edit falls into three cases decided in segment_schema_write.go:
//  1. The value names the object's own current root: a no-op.
//  2. The value names a different live table: an alias to the same b-tree
//     under two names. For an autoindex row the load is refused instead, which
//     sets the latch below.
//  3. Anything else is declined.
//
// The latch is a connection-level state that persists across statements until
// "PRAGMA writable_schema=ON" clears it.
package engine

import (
	"fmt"
	"strings"
)

// schemaCorruptErr is the error every statement gets while this connection is
// latched, or nil if the schema is loaded.
func (db *DB) schemaCorruptErr() error {
	if db == nil || db.wsSchemaCorruptObj == "" {
		return nil
	}
	// Append " - <detail>" only when a detail is provided.
	if db.wsSchemaCorruptDetail == "" {
		return fmt.Errorf("malformed database schema (%s)", db.wsSchemaCorruptObj)
	}
	return fmt.Errorf("malformed database schema (%s) - %s", db.wsSchemaCorruptObj, db.wsSchemaCorruptDetail)
}

// latchSchemaCorrupt records the corrupted object name and optional detail.
func (db *DB) latchSchemaCorrupt(obj, detail string) {
	if obj == "" {
		obj = "?"
	}
	db.wsSchemaCorruptObj, db.wsSchemaCorruptDetail = obj, detail
}

// clearSchemaCorrupt lifts the latch. Called by "PRAGMA writable_schema = ON".
func (db *DB) clearSchemaCorrupt() { db.wsSchemaCorruptObj, db.wsSchemaCorruptDetail = "", "" }

// SchemaCorrupt reports the latch. The driver needs this to carry the latch
// across statement sessions, since each new session must know about the error.
func (db *DB) SchemaCorrupt() (obj, detail string) {
	if db == nil {
		return "", ""
	}
	return db.wsSchemaCorruptObj, db.wsSchemaCorruptDetail
}

// SetSchemaCorrupt seeds it, the mirror of SchemaCorrupt.
func (db *DB) SetSchemaCorrupt(obj, detail string) {
	db.wsSchemaCorruptObj, db.wsSchemaCorruptDetail = obj, detail
}

// SchemaCorruptRefuses applies the latch for the driver.
func SchemaCorruptRefuses(obj, detail, sqlText string) error {
	if obj == "" {
		return nil
	}
	if verb, ok := LeadingStatementVerb(sqlText); ok && strings.EqualFold(verb, "PRAGMA") {
		return nil
	}
	return (&DB{wsSchemaCorruptObj: obj, wsSchemaCorruptDetail: detail}).schemaCorruptErr()
}

// schemaCorruptRefusesStatement refuses statements while a failed schema load
// is outstanding, except for PRAGMA which may lift the latch.
func (db *DB) schemaCorruptRefusesStatement(sqlText string) error {
	if db == nil || db.wsSchemaCorruptObj == "" {
		return nil
	}
	if verb, ok := LeadingStatementVerb(sqlText); ok && strings.EqualFold(verb, "PRAGMA") {
		return nil
	}
	return db.schemaCorruptErr()
}
