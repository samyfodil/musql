package driver_test

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	_ "github.com/samyfodil/musql/driver"
)

// A complete program: open (and create) a database, create a table, insert
// rows with parameters, and read them back.
func Example() {
	dir, err := os.MkdirTemp("", "musql-example")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := sql.Open("sqlite", filepath.Join(dir, "app.musq"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE people (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		log.Fatal(err)
	}
	for _, name := range []string{"John", "Ann"} {
		if _, err := db.Exec(`INSERT INTO people (name) VALUES (?)`, name); err != nil {
			log.Fatal(err)
		}
	}

	var name string
	err = db.QueryRow(`SELECT name FROM people WHERE id = ?`, 1).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		log.Fatal("no such person")
	} else if err != nil {
		log.Fatal(err)
	}
	fmt.Println("id 1:", name)

	rows, err := db.Query(`SELECT id, name FROM people ORDER BY name`)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			log.Fatal(err)
		}
		fmt.Println(id, name)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
	// Output:
	// id 1: John
	// 2 Ann
	// 1 John
}
