package engine

import "fmt"

// qualifiedTableName is the name sqlite3LocateTable reports for a missing table
// (build.c): "db.name" when the statement qualified it, else the bare name.
func qualifiedTableName(schema, table string) string {
	if schema == "" {
		return table
	}
	return schema + "." + table
}

// relabelMissingTable restates the resolver's bare "no such table: t" with the
// database qualifier the FROM item was written with.
func relabelMissingTable(it FromItem, err error) error {
	if err != nil && it.Schema != "" && err.Error() == "engine: no such table: "+it.Table {
		return fmt.Errorf("engine: no such table: %s", qualifiedTableName(it.Schema, it.Table))
	}
	return err
}
