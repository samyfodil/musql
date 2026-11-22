package hrana

import (
	"fmt"
	"slices"
	"strings"

	musqldriver "github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
)

// Access is what a token grants a client.
type Access struct {
	ReadOnly bool
	// NewAuthorizer, when set, makes the authorizer asked about every
	// statement the client runs, one for each connection it runs on.
	NewAuthorizer func() musqldriver.Authorizer
}

func (a Access) authorizer() musqldriver.Authorizer {
	if a.NewAuthorizer == nil {
		return nil
	}
	return a.NewAuthorizer()
}

// WithAccess is WithAuth for a check that also limits what the client may
// do, statement by statement.
func WithAccess(check func(token string) (Access, error)) Option {
	return func(c *config) { c.auth = check }
}

// setAuthorizer installs fn on the stream's connection; nil removes it.
func (s *stream) setAuthorizer(fn musqldriver.Authorizer) {
	s.conn.Raw(func(dc any) error {
		dc.(*musqldriver.Conn).SetAuthorizer(fn)
		return nil
	})
}

// A Permission grants actions on tables: one entry of a Turso token's "perm"
// claim. No tables means every table.
type Permission struct {
	Tables  []string `json:"t,omitempty"`
	Actions []string `json:"a"`
}

// The actions a Permission names, Turso's.
const (
	DataRead     = "data_read"
	DataAdd      = "data_add"
	DataUpdate   = "data_update"
	DataDelete   = "data_delete"
	SchemaAdd    = "schema_add"
	SchemaUpdate = "schema_update"
	SchemaDelete = "schema_delete"
)

// Actions are the names a Permission may use.
var Actions = []string{DataRead, DataAdd, DataUpdate, DataDelete, SchemaAdd, SchemaUpdate, SchemaDelete}

// ValidatePermissions refuses an action name that is not one of Actions.
func ValidatePermissions(ps []Permission) error {
	for _, p := range ps {
		for _, a := range p.Actions {
			if !slices.Contains(Actions, a) {
				return fmt.Errorf("unknown action %q (want one of %s)", a, strings.Join(Actions, ", "))
			}
		}
	}
	return nil
}

// introspectionPragmas are the pragmas that only describe the schema, which
// any token may run.
var introspectionPragmas = map[string]bool{
	"table_info": true, "table_xinfo": true, "table_list": true, "index_list": true,
	"index_info": true, "index_xinfo": true, "foreign_key_list": true, "database_list": true,
	"collation_list": true, "function_list": true, "module_list": true, "pragma_list": true,
	"compile_options": true,
}

// PermissionAuthorizer makes the authorizer, for one connection, that lets a statement do only what
// ps grant, as Turso enforces a token's permissions: a column read needs
// data_read on its table, INSERT data_add, UPDATE data_update, DELETE
// data_delete, CREATE schema_add, ALTER schema_update and DROP schema_delete
// on the table the object belongs to. The schema tables a statement reads or
// rewrites along the way need nothing, nor do SELECT itself, functions and
// transactions. A pragma that sets something, ANALYZE and REINDEX need
// schema_update; ATTACH and DETACH are refused.
func PermissionAuthorizer(ps []Permission) musqldriver.Authorizer {
	ps = slices.Clone(ps)
	allows := func(action, table string) bool {
		for _, p := range ps {
			if !slices.Contains(p.Actions, action) {
				continue
			}
			if len(p.Tables) == 0 {
				return true
			}
			for _, t := range p.Tables {
				if table != "" && strings.EqualFold(t, table) {
					return true
				}
			}
		}
		return false
	}
	var dropped, indexed string // the object the previous call dropped or indexed
	return func(action int, arg1, arg2, db, trigger string) int {
		prevDropped, prevIndexed := dropped, indexed
		dropped, indexed = "", ""
		ok := false
		switch action {
		case engine.AuthSelect, engine.AuthFunction, engine.AuthRecursive,
			engine.AuthTransaction, engine.AuthSavepoint:
			ok = true
		case engine.AuthRead:
			ok = schemaTable(arg1) || allows(DataRead, arg1)
		case engine.AuthInsert:
			ok = schemaTable(arg1) || allows(DataAdd, arg1)
		case engine.AuthUpdate:
			ok = schemaTable(arg1) || allows(DataUpdate, arg1)
		case engine.AuthDelete:
			ok = schemaTable(arg1) || strings.EqualFold(arg1, prevDropped) || allows(DataDelete, arg1)
		case engine.AuthCreateTable, engine.AuthCreateTempTable, engine.AuthCreateView,
			engine.AuthCreateTempView, engine.AuthCreateVtable:
			ok = allows(SchemaAdd, arg1)
		case engine.AuthCreateIndex, engine.AuthCreateTempIndex:
			if ok = allows(SchemaAdd, arg2); ok {
				indexed = arg1
			}
		case engine.AuthCreateTrigger, engine.AuthCreateTempTrigger:
			ok = allows(SchemaAdd, arg2)
		case engine.AuthDropTable, engine.AuthDropTempTable, engine.AuthDropView,
			engine.AuthDropTempView, engine.AuthDropVtable:
			if ok = allows(SchemaDelete, arg1); ok {
				dropped = arg1
			}
		case engine.AuthDropIndex, engine.AuthDropTempIndex, engine.AuthDropTrigger,
			engine.AuthDropTempTrigger:
			ok = allows(SchemaDelete, arg2)
		case engine.AuthAlterTable:
			ok = allows(SchemaUpdate, arg2)
		case engine.AuthAnalyze:
			ok = allows(SchemaUpdate, arg1)
		case engine.AuthReindex:
			ok = arg1 != "" && strings.EqualFold(arg1, prevIndexed) || allows(SchemaUpdate, "")
		case engine.AuthPragma:
			ok = arg2 == "" || introspectionPragmas[strings.ToLower(arg1)] || allows(SchemaUpdate, "")
		}
		if ok {
			return engine.AuthOK
		}
		return engine.AuthDeny
	}
}

// schemaTable is a table SQLite keeps for itself, which statements read and
// write as a side effect of what they do.
func schemaTable(name string) bool {
	return len(name) >= 7 && strings.EqualFold(name[:7], "sqlite_")
}
