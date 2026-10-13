//go:build mattn

package main

import (
	"database/sql"

	_ "github.com/mattn/go-sqlite3"
)

const engine = "c-sqlite"

func openDB(path string) (*sql.DB, error) { return sql.Open("sqlite3", path) }
