package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	_ "github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
)

// SchemaOnly creates dst, a new database holding src's schema and none of its
// rows: src's sqlite_schema statements in the order they were made, less the
// engine's own tables and virtual tables' shadow tables, which their virtual
// table creates again.
func SchemaOnly(ctx context.Context, src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("store: %s exists", dst)
	}
	from, err := sql.Open("sqlite", src)
	if err != nil {
		return err
	}
	defer from.Close()
	shadow := map[string]bool{}
	rows, err := from.QueryContext(ctx, `SELECT name FROM pragma_table_list WHERE schema = 'main' AND type = 'shadow'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		shadow[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = from.QueryContext(ctx, `SELECT name, sql FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY rowid`)
	if err != nil {
		return err
	}
	var stmts []string
	for rows.Next() {
		var n, q string
		if err := rows.Scan(&n, &q); err != nil {
			rows.Close()
			return err
		}
		if !shadow[n] {
			stmts = append(stmts, q)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := engine.CreateFile(dst); err != nil {
		return err
	}
	to, err := sql.Open("sqlite", dst)
	if err != nil {
		return err
	}
	defer to.Close()
	tx, err := to.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: recreating %q: %w", q, err)
		}
	}
	return tx.Commit()
}
