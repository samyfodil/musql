package replication

import "testing"

// fkDSN turns foreign keys on for every connection a node opens, as a DSN does
// for any other database.
const fkDSN = "?_pragma=foreign_keys(1)"

// TestForeignKeysDoNotActOnApply: foreign keys on in the DSN enforce for users
// but not on the apply handle, where a peer's UPDATE, applied as a DELETE and
// an INSERT, would cascade to children the peer never touched.
func TestForeignKeysDoNotActOnApply(t *testing.T) {
	for _, tc := range []struct {
		name, onDelete, write, want string
	}{
		{"an update of the parent keeps its child", "ON DELETE CASCADE", `UPDATE parent SET name = 'renamed' WHERE id = 1`, "10|1|child\n"},
		{"a delete cascades to the child", "ON DELETE CASCADE", `DELETE FROM parent WHERE id = 1`, ""},
		{"a delete sets the child's key to NULL", "ON DELETE SET NULL", `DELETE FROM parent WHERE id = 1`, "10|NULL|child\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t)
			c.query = fkDSN
			schema := []string{
				`CREATE TABLE parent(id INTEGER PRIMARY KEY, name TEXT)`,
				`CREATE TABLE child(id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id) ` + tc.onDelete + `, note TEXT)`,
			}
			for _, site := range []string{"a", "b", "c"} {
				c.open(site, schema...)
			}
			c.exec("a", `INSERT INTO parent VALUES (1, 'p')`, `INSERT INTO child VALUES (10, 1, 'child')`)
			const children, parents = `SELECT id, pid, note FROM child ORDER BY id`, `SELECT id, name FROM parent ORDER BY id`
			c.converge("10|1|child\n", children, parents)

			c.exec("a", tc.write)
			c.converge(tc.want, children, parents)
		})
	}
}

// TestForeignKeysDoNotBlockAJoiningNode: a node that joins empty is given the
// tables and the rows together. With enforcement on its apply handle a foreign
// key refused them ("no such table: main.parent"), and it never got either.
func TestForeignKeysDoNotBlockAJoiningNode(t *testing.T) {
	c := newCluster(t)
	c.query = fkDSN
	c.open("a",
		`CREATE TABLE parent(id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE child(id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id) ON DELETE CASCADE, note TEXT)`,
		`INSERT INTO parent VALUES (1, 'p')`, `INSERT INTO child VALUES (10, 1, 'child')`)
	c.open("b")
	c.converge("10|1|child\n", `SELECT id, pid, note FROM child ORDER BY id`, `SELECT id, name FROM parent ORDER BY id`)
}
