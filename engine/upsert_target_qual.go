package engine

// validateUpsertTargetQualifiers checks the "table." (and "schema.") written
// before an ON CONFLICT target column names the INSERT's own table: the target
// is resolved as an ordinary expression against it (upsert.c:119), so a
// qualifier that names anything else is "no such column: q.col". With an
// "AS alias" on the target, only the alias names it.
func validateUpsertTargetQualifiers(stmt *insertStmt, tbl *tableMeta, up *upsertClause) error {
	for i, quals := range up.targetQuals {
		if len(quals) == 0 || i >= len(up.targetCols) {
			continue
		}
		name := stmt.table
		if stmt.alias != "" {
			name = stmt.alias
		}
		ok := equalFoldName(quals[len(quals)-1], name)
		if ok && len(quals) == 2 {
			sch := quals[0]
			ok = equalFoldName(sch, "main") && !tbl.isTemp || equalFoldName(sch, "temp") && tbl.isTemp ||
				stmt.schema != "" && equalFoldName(sch, stmt.schema)
		}
		if !ok {
			written := quals[0]
			if len(quals) == 2 {
				written += "." + quals[1]
			}
			return semanticf("engine: no such column: %s.%s", written, up.targetCols[i])
		}
	}
	return nil
}
