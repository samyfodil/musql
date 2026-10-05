// This file tests which column declarations may carry AUTOINCREMENT.
// The declared type must be exactly "INTEGER"; constraints do not affect the type.
package compat

import (
	"fmt"
	"testing"
)

// autoIncColumnDefs tests valid and invalid AUTOINCREMENT declarations.
var autoIncColumnDefs = []string{
	// accepted -- the type is INTEGER and the rest is constraints
	`id INTEGER PRIMARY KEY AUTOINCREMENT`,
	`id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT`,
	`id INTEGER NULL PRIMARY KEY AUTOINCREMENT`,
	`id INTEGER DEFAULT 5 PRIMARY KEY AUTOINCREMENT`,
	`id INTEGER UNIQUE PRIMARY KEY AUTOINCREMENT`,
	`id INTEGER CONSTRAINT cx NOT NULL PRIMARY KEY AUTOINCREMENT`,
	`id INTEGER COLLATE NOCASE PRIMARY KEY AUTOINCREMENT`,
	`id INTEGER CHECK(id>0) PRIMARY KEY AUTOINCREMENT`,
	`id INTEGER NOT NULL UNIQUE DEFAULT 1 PRIMARY KEY AUTOINCREMENT`,
	`id integer not null primary key autoincrement`,
	`id INTEGER NOT NULL PRIMARY KEY ASC AUTOINCREMENT`,
	// refused -- the declared type is not exactly INTEGER
	`id INT NOT NULL PRIMARY KEY AUTOINCREMENT`,
	`id BIGINT NOT NULL PRIMARY KEY AUTOINCREMENT`,
	`id INTEGER(10) NOT NULL PRIMARY KEY AUTOINCREMENT`,
	`id UNSIGNED BIG INT NOT NULL PRIMARY KEY AUTOINCREMENT`,
	`id TEXT NOT NULL PRIMARY KEY AUTOINCREMENT`,
	`id PRIMARY KEY AUTOINCREMENT`,
	`id INTEGER NOT NULL PRIMARY KEY DESC AUTOINCREMENT`,
	`id INTEGER NOT NULL AUTOINCREMENT`,
}

func TestAutoIncrementDeclaredType(t *testing.T) {
	for i, def := range autoIncColumnDefs {
		tn := fmt.Sprintf("ai%d", i)
		differ(t, def, []string{
			fmt.Sprintf("CREATE TABLE %s(%s)", tn, def),
			fmt.Sprintf("INSERT INTO %s DEFAULT VALUES", tn),
			fmt.Sprintf("INSERT INTO %s DEFAULT VALUES", tn),
			fmt.Sprintf("SELECT id FROM %s ORDER BY id", tn),
			fmt.Sprintf("SELECT name, seq FROM sqlite_sequence WHERE name='%s'", tn),
			fmt.Sprintf("SELECT type FROM pragma_table_info('%s') WHERE name='id'", tn),
			fmt.Sprintf("SELECT sql FROM sqlite_master WHERE name='%s'", tn),
		})
	}
}

// TestAutoIncrementRealWorldSchema tests a real-world schema with AUTOINCREMENT.
func TestAutoIncrementRealWorldSchema(t *testing.T) {
	differ(t, "real-world AUTOINCREMENT schema", []string{
		`CREATE TABLE account(id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,` +
			`t_name TEXT NOT NULL,t_number TEXT NOT NULL DEFAULT '',` +
			`t_close VARCHAR(1) DEFAULT 'N' CHECK (t_close IN ('Y', 'N')))`,
		`CREATE TABLE category(id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,` +
			`t_name TEXT NOT NULL DEFAULT '' CHECK (t_name NOT LIKE '% > %'),` +
			`t_bookmarked VARCHAR(1) NOT NULL DEFAULT 'N' CHECK (t_bookmarked IN ('Y','N')))`,
		`INSERT INTO account(t_name) VALUES('one')`,
		`INSERT INTO account(t_name) VALUES('two')`,
		`INSERT INTO category(t_name) VALUES('cat')`,
		`INSERT INTO account(t_name,t_close) VALUES('bad','X')`,
		`SELECT id,t_name,t_close FROM account ORDER BY id`,
		`SELECT id,t_name FROM category ORDER BY id`,
		`SELECT name,seq FROM sqlite_sequence ORDER BY name`,
		`DELETE FROM account`,
		`INSERT INTO account(t_name) VALUES('after-delete')`,
		`SELECT id FROM account`,
		`SELECT name,seq FROM sqlite_sequence ORDER BY name`,
	})
}
