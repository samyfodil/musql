package compat

import (
	"fmt"
	"testing"
)

// ALTER TABLE ... ADD takes a column definition or a CHECK constraint, and a
// column's name is an nm, which PRIMARY, FOREIGN, UNIQUE and the other
// reserved keywords never fall back to: "ADD PRIMARY KEY (x)" is the syntax
// error C reports, not a table constraint (issue #184).
func TestAlterAddReservedKeywordShapes(t *testing.T) {
	pre := []string{"CREATE TABLE t(a INT, b TEXT)", "INSERT INTO t VALUES(1,'x')"}
	for i, q := range []string{
		"ALTER TABLE t ADD PRIMARY KEY (a)",
		"ALTER TABLE t ADD primary key (a)",
		"ALTER TABLE t ADD FOREIGN KEY fk1 (a) REFERENCES t(a)",
		"ALTER TABLE t ADD UNIQUE (a)",
		"ALTER TABLE t ADD COLUMN PRIMARY INT",
		"ALTER TABLE t ADD COLUMN \"primary\" INT",
		"ALTER TABLE t ADD COLUMN [unique] TEXT",
		"ALTER TABLE t ADD COLUMN key INT",
		"ALTER TABLE t ADD CHECK (a > 0)",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q, "SELECT sql FROM sqlite_schema WHERE name = 't'"))
	}
}
