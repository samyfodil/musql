//go:build musql

package main

import (
	"database/sql"

	_ "github.com/samyfodil/musql/driver"
)

const engine = "musql"

func openDB(path string) (*sql.DB, error) { return sql.Open("sqlite", path) }
