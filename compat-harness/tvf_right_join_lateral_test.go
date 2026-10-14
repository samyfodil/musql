package compat

// A table-valued function whose argument reads the left side of a RIGHT or
// FULL JOIN is built again for each left row and once more for the unmatched
// sweep, with the left side NULL-extended; its rows are matched by rowid.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var tvfRightJoinSchema = []string{
	"CREATE TABLE vi(value INTEGER, json TEXT)",
	"INSERT INTO vi VALUES(1,'[1,2]')",
	"CREATE TABLE vi2(value INTEGER, json TEXT)",
	"INSERT INTO vi2 VALUES(1,'[1,2]'),(5,'[5,6,7]')",
	"CREATE TABLE empty(value INTEGER, json TEXT)",
}

func TestTVFRightJoinLateral(t *testing.T) {
	for _, q := range []string{
		"SELECT vi.value, r.value FROM vi RIGHT JOIN json_each(vi.json) AS r ON vi.value=r.value ORDER BY r.value",
		"SELECT vi.value, r.value FROM vi RIGHT JOIN json_each((SELECT vi.json)) AS r ON vi.value=r.value ORDER BY r.value",
		"SELECT v.value, r.value FROM vi2 v FULL JOIN json_each(v.json) AS r ON v.value=r.value ORDER BY 1, 2",
		"SELECT v.value, r.value FROM vi2 v RIGHT JOIN json_each(coalesce(v.json, '[9]')) AS r ON v.value=r.value ORDER BY 1, 2",
		"SELECT v.value, r.value, r.id FROM vi2 v FULL JOIN json_each(coalesce(v.json, '[1,2,3,4,5,6,7,8]')) AS r ON v.value=r.value ORDER BY 1, 2, 3",
		"SELECT count(*) FROM vi2 v RIGHT JOIN json_each(v.json) AS r ON 1",
		"SELECT v.value, r.value FROM empty v RIGHT JOIN json_each(coalesce(v.json, '[3,4]')) AS r ON v.value=r.value ORDER BY 2",
		"SELECT v.value, r.value FROM vi2 v LEFT JOIN json_each((SELECT v.json)) AS r ON v.value=r.value ORDER BY 1, 2",
	} {
		t.Run(q, func(t *testing.T) {
			differ(t, q, append(append([]string{}, tvfRightJoinSchema...), q))
		})
	}
}
