package compat

import (
	"fmt"
	"testing"
)

// A subquery whose body holds a LEFT JOIN is flattened into its parent, as C
// flattens it: TPC-DS q93's shape (issue #169), a fact table LEFT JOIN its
// returns and a dimension, grouped by the parent. Rows, their order where no
// ORDER BY fixes it, and the NULL side's columns must all match.
func TestFlattenBodyLeftJoin(t *testing.T) {
	pre := []string{
		"CREATE TABLE ss(item INT, ticket INT, cust INT, qty INT, price REAL)",
		"CREATE TABLE sr(item INT, ticket INT, rqty INT, reason INT)",
		"CREATE TABLE reason(r_sk INT, r_desc TEXT)",
		"CREATE TABLE cu(c_sk INT, name TEXT)",
		"INSERT INTO ss VALUES(1,1,10,3,1.5),(2,1,10,1,2.0),(1,2,11,5,0.5),(3,3,12,2,4.0),(2,4,NULL,7,1.25),(4,5,11,1,9.0),(1,6,13,2,3.0)",
		"INSERT INTO sr VALUES(1,1,1,1),(1,2,2,2),(3,3,2,1),(2,4,7,NULL),(9,9,1,1)",
		"INSERT INTO reason VALUES(1,'damaged'),(2,'late'),(3,'unused')",
		"INSERT INTO cu VALUES(10,'a'),(11,'b'),(12,'c'),(13,'d')",
	}
	sub := "(SELECT ss.item, ss.ticket, cust, CASE WHEN rqty IS NOT NULL THEN (qty - rqty) * price ELSE qty * price END act, rqty, reason " +
		"FROM ss LEFT OUTER JOIN sr ON (sr.item = ss.item AND sr.ticket = ss.ticket)%s) t"
	for i, q := range []string{
		"SELECT cust, sum(act) s FROM " + fmt.Sprintf(sub, ", reason WHERE reason = r_sk AND r_desc = 'damaged'") + " GROUP BY cust ORDER BY s NULLS FIRST, cust NULLS FIRST",
		"SELECT cust, sum(act) FROM " + fmt.Sprintf(sub, "") + " GROUP BY cust ORDER BY 1",
		"SELECT * FROM " + fmt.Sprintf(sub, ""),
		"SELECT cust, act, rqty FROM " + fmt.Sprintf(sub, " WHERE rqty IS NULL"),
		"SELECT cust, act FROM " + fmt.Sprintf(sub, ", reason WHERE reason = r_sk"),
		"SELECT name, act FROM cu, " + fmt.Sprintf(sub, "") + " WHERE cu.c_sk = t.cust",
		"SELECT name, act, r_desc FROM " + fmt.Sprintf(sub, "") + " JOIN cu ON cu.c_sk = t.cust LEFT JOIN reason ON reason.r_sk = t.reason",
		"SELECT count(*), sum(coalesce(rqty, -1)), group_concat(act) FROM " + fmt.Sprintf(sub, "") + " WHERE cust IS NOT NULL",
		"SELECT DISTINCT cust FROM " + fmt.Sprintf(sub, " WHERE reason IS NULL"),
		"SELECT ticket, act FROM " + fmt.Sprintf(sub, "") + " WHERE act > 3 ORDER BY ticket LIMIT 3",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q))
	}
}
