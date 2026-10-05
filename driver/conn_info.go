package driver

import "github.com/samyfodil/musql/engine"

// IsAutocommit reports whether the connection is outside an explicit
// transaction, sqlite3_get_autocommit.
func (c *Conn) IsAutocommit() bool {
	return c.tx == nil && (c.ndb == nil || !c.ndb.InTransaction())
}

// ReportDeclTypes makes this connection's query results carry each column's
// declared type (sql.ColumnType.DatabaseTypeName). Off by default: it parses and
// resolves every query a second time.
func (c *Conn) ReportDeclTypes(on bool) { c.reportDeclTypes = on }

func (c *Conn) reportedDeclTypes(pager *engine.ReadOnlyPager, sqlText string, cols []string) []string {
	if !c.reportDeclTypes || pager == nil || len(cols) == 0 {
		return nil
	}
	types, ok := pager.ResultDeclTypes(sqlText)
	if !ok || len(types) != len(cols) {
		return nil
	}
	return types
}

// ChangeState is sqlite3_changes, sqlite3_total_changes and
// sqlite3_last_insert_rowid as the last statement left them.
func (c *Conn) ChangeState() (changes, totalChanges, lastInsertRowid int64) {
	return c.changes, c.totalChanges, c.lastInsertRowid
}
