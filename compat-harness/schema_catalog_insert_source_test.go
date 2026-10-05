// Tests reading a schema catalog as an INSERT ... SELECT source.
// Covers all catalog spellings and whether a temp object exists.
package compat

import (
	"fmt"
	"strings"
	"testing"
)

// scisCatalogs are the spellings that name a schema catalog as a FROM source.
var scisCatalogs = []string{
	`sqlite_master`,
	`sqlite_schema`,
	`main.sqlite_master`,
	`temp.sqlite_master`,
	`sqlite_temp_master`,
	`sqlite_temp_schema`,
}

func TestSchemaCatalogAsInsertSource(t *testing.T) {
	for _, cat := range scisCatalogs {
		for _, target := range []string{"mainlist", "templist"} {
			for _, withTemp := range []bool{false, true} {
				name := fmt.Sprintf("%s -> %s (temp=%v)", cat, target, withTemp)
				stmts := []string{
					`CREATE TABLE m1(x,y,z)`,
					`CREATE INDEX m1x ON m1(x)`,
					`INSERT INTO m1 VALUES('a','b','c')`,
					`CREATE TABLE mainlist(a,b,c)`,
					`CREATE TEMP TABLE templist(a,b,c)`,
				}
				if withTemp {
					// A temp TABLE and a temp INDEX, so the temp catalog has
					// rows of two kinds to tell apart from main's.
					stmts = append(stmts,
						`CREATE TEMP TABLE tmp1(p,q)`,
						`CREATE INDEX temp.tmp1p ON tmp1(p)`)
				}
				stmts = append(stmts,
					fmt.Sprintf(`INSERT INTO %s SELECT type,name,tbl_name FROM %s`, target, cat),
					// The CONTENT is the assertion: which catalog answered, and
					// with which rows. Ordered so the compare is total.
					fmt.Sprintf(`SELECT a,b,c FROM %s ORDER BY a,b,c`, target),
					fmt.Sprintf(`SELECT count(*) FROM %s`, target))
				differ(t, name, stmts)
			}
		}
	}
}

// TestSchemaCatalogAsInsertSourceShapes tests various statement shapes.
func TestSchemaCatalogAsInsertSourceShapes(t *testing.T) {
	base := []string{
		`CREATE TABLE m1(x,y,z)`,
		`INSERT INTO m1 VALUES('table','m1','m1')`,
		`CREATE TABLE lst(a,b,c)`,
		`CREATE TEMP TABLE tmp1(p,q)`,
	}
	for _, s := range []string{
		`INSERT INTO lst SELECT type,name,tbl_name FROM sqlite_master WHERE name!='lst'`,
		`INSERT INTO lst SELECT type,name,tbl_name FROM sqlite_master UNION ALL SELECT type,name,tbl_name FROM sqlite_temp_master`,
		`INSERT INTO lst SELECT s.type,s.name,m1.y FROM sqlite_master s JOIN m1 ON m1.y=s.name`,
		`INSERT INTO lst WITH c AS (SELECT type,name,tbl_name FROM sqlite_master) SELECT * FROM c`,
		`INSERT INTO lst SELECT type,name,(SELECT count(*) FROM sqlite_master) FROM sqlite_master`,
		`CREATE TABLE ctas AS SELECT type,name,tbl_name FROM sqlite_master`,
		`CREATE TEMP TABLE ctastmp AS SELECT type,name,tbl_name FROM sqlite_temp_master`,
	} {
		stmts := append(append([]string{}, base...), s)
		read := `SELECT a,b,c FROM lst ORDER BY a,b,c`
		if strings.HasPrefix(s, "CREATE") {
			tbl := "ctas"
			if strings.Contains(s, "ctastmp") {
				tbl = "ctastmp"
			}
			read = fmt.Sprintf(`SELECT type,name,tbl_name FROM %s ORDER BY 1,2,3`, tbl)
			stmts = append(stmts, fmt.Sprintf(`SELECT sql FROM sqlite_master WHERE name='%s'`, tbl))
		}
		stmts = append(stmts, read)
		differ(t, s, stmts)
	}
}
