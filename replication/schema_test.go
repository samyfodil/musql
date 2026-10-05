package replication

import (
	"context"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func TestInitSchema(t *testing.T) {
	db, err := openApplyDB(":memory:")
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// First call should succeed
	if err := InitSchema(ctx, db); err != nil {
		t.Fatalf("InitSchema failed: %v", err)
	}

	// Second call should succeed (idempotent)
	if err := InitSchema(ctx, db); err != nil {
		t.Fatalf("InitSchema (second call) failed: %v", err)
	}
}

func TestInitSchemaTablesExist(t *testing.T) {
	db, err := openApplyDB(":memory:")
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	if err := InitSchema(ctx, db); err != nil {
		t.Fatalf("InitSchema failed: %v", err)
	}

	// Check that the three tables exist in sqlite_master
	tables := []string{"_repl_meta", "_repl_oplog", "_repl_clock"}
	for _, tableName := range tables {
		var count int
		err := db.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?",
			tableName,
		).Scan(&count)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected 1 table %q, found %d", tableName, count)
		}
	}
}

func TestInitSchemaColumnStructure(t *testing.T) {
	db, err := openApplyDB(":memory:")
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	if err := InitSchema(ctx, db); err != nil {
		t.Fatalf("InitSchema failed: %v", err)
	}

	tests := []struct {
		table   string
		columns []string
	}{
		{
			table:   "_repl_meta",
			columns: []string{"k", "v"},
		},
		{
			table:   "_repl_oplog",
			columns: []string{"site", "seq", "hlc", "tbl", "pk", "op", "cells"},
		},
		{
			table:   "_repl_clock",
			columns: []string{"tbl", "pk", "col", "hlc", "site", "val"},
		},
	}

	for _, tt := range tests {
		rows, err := db.QueryContext(ctx, "PRAGMA table_info("+tt.table+")")
		if err != nil {
			t.Fatalf("PRAGMA table_info failed for %q: %v", tt.table, err)
		}
		defer rows.Close()

		var actualCols []string
		for rows.Next() {
			var cid int
			var name string
			var typ string
			var notnull int
			var dfltValue interface{}
			var pk int

			if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk); err != nil {
				t.Fatalf("scan failed: %v", err)
			}
			actualCols = append(actualCols, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows error for %q: %v", tt.table, err)
		}

		if len(actualCols) != len(tt.columns) {
			t.Fatalf("table %q: expected %d columns, found %d: %v",
				tt.table, len(tt.columns), len(actualCols), actualCols)
		}
		for i, col := range tt.columns {
			if actualCols[i] != col {
				t.Fatalf("table %q column %d: expected %q, found %q",
					tt.table, i, col, actualCols[i])
			}
		}
	}
}
