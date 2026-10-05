// This file tests correlated subqueries in WHERE clauses of aggregate or
// GROUP BY queries.
package compat

import "testing"

const aggCorrSetup = `CREATE TABLE artist(artistid INTEGER PRIMARY KEY, artistname TEXT)`

func TestAggregateCorrelatedWhere(t *testing.T) {
	differ(t, "whole-table aggregate with a correlated WHERE", []string{
		aggCorrSetup,
		`CREATE TABLE track(trackid INTEGER, trackname TEXT, trackartist INTEGER)`,
		`INSERT INTO artist VALUES(1,'a'),(2,'b')`,
		`INSERT INTO track VALUES(1,'t1',1),(2,'t2',NULL),(3,'t3',9),(4,'t4',2),(5,'t5',1)`,
		// e_fkey.test's own statement, and the pieces it is built from.
		`SELECT count(*) FROM track WHERE NOT ( trackartist IS NULL OR EXISTS(SELECT 1 FROM artist WHERE artistid=trackartist) )`,
		`SELECT count(*) FROM track WHERE EXISTS(SELECT 1 FROM artist WHERE artistid=trackartist)`,
		`SELECT count(*) FROM track WHERE NOT EXISTS(SELECT 1 FROM artist WHERE artistid=trackartist)`,
		// Every aggregate flavour, so a per-row miss shows up as a WRONG VALUE
		// and not only as a wrong count.
		`SELECT sum(trackid) FROM track WHERE EXISTS(SELECT 1 FROM artist WHERE artistid=trackartist)`,
		`SELECT min(trackid), max(trackid) FROM track WHERE EXISTS(SELECT 1 FROM artist WHERE artistid=trackartist)`,
		`SELECT group_concat(trackname,'-') FROM track WHERE EXISTS(SELECT 1 FROM artist WHERE artistid=trackartist)`,
		`SELECT count(DISTINCT trackartist) FROM track WHERE EXISTS(SELECT 1 FROM artist WHERE artistid=trackartist)`,
		// A correlated SCALAR subquery, and one inside a comparison.
		`SELECT count(*) FROM track WHERE (SELECT count(*) FROM artist WHERE artistid=trackartist)>0`,
		`SELECT count(*) FROM track WHERE trackartist = (SELECT artistid FROM artist WHERE artistid=trackartist)`,
		`SELECT count(*) FROM track WHERE trackartist IN (SELECT artistid FROM artist WHERE artistid=trackartist)`,
		// An UNcorrelated subquery in the same position, which always worked.
		`SELECT count(*) FROM track WHERE trackartist IN (SELECT artistid FROM artist)`,
		// ...and a HAVING alongside it.
		`SELECT count(*) FROM track WHERE EXISTS(SELECT 1 FROM artist WHERE artistid=trackartist) HAVING count(*)>1`,
		`SELECT count(*) FROM track WHERE EXISTS(SELECT 1 FROM artist WHERE artistid=trackartist) HAVING count(*)>99`,
	})
}

func TestGroupByCorrelatedWhere(t *testing.T) {
	differ(t, "GROUP BY with a correlated WHERE", []string{
		aggCorrSetup,
		`CREATE TABLE track(trackid INTEGER, trackname TEXT, trackartist INTEGER)`,
		`INSERT INTO artist VALUES(1,'a'),(2,'b')`,
		`INSERT INTO track VALUES(1,'t1',1),(2,'t2',NULL),(3,'t3',9),(4,'t4',2),(5,'t5',1)`,
		`SELECT trackartist, count(*) FROM track
		   WHERE EXISTS(SELECT 1 FROM artist WHERE artistid=trackartist)
		   GROUP BY trackartist ORDER BY trackartist`,
		`SELECT trackartist, sum(trackid) FROM track
		   WHERE NOT EXISTS(SELECT 1 FROM artist WHERE artistid=trackartist)
		   GROUP BY trackartist ORDER BY trackartist`,
		// A grouping key that is itself an expression, plus HAVING.
		`SELECT trackartist IS NULL AS g, count(*) FROM track
		   WHERE (SELECT count(*) FROM artist WHERE artistid=trackartist)=0
		   GROUP BY g HAVING count(*)>0 ORDER BY g`,
		// ORDER BY forces the sorted grouping path rather than the hash one.
		`SELECT trackartist, count(*) FROM track
		   WHERE EXISTS(SELECT 1 FROM artist WHERE artistid=trackartist)
		   GROUP BY trackartist ORDER BY count(*) DESC, trackartist`,
	})
}

func TestAggregateCorrelatedWhereOverAJoin(t *testing.T) {
	// A join makes predicate PUSHDOWN choose at which level each conjunct is
	// tested. A correlated subquery pushed to a level whose cursors are not all
	// positioned would read the wrong row -- so the answers, not just the
	// acceptance, are what this compares.
	differ(t, "aggregate + correlated WHERE over a join", []string{
		aggCorrSetup,
		`CREATE TABLE track(trackid INTEGER, trackname TEXT, trackartist INTEGER)`,
		`CREATE TABLE label(lid INTEGER, trackid INTEGER, tag TEXT)`,
		`INSERT INTO artist VALUES(1,'a'),(2,'b')`,
		`INSERT INTO track VALUES(1,'t1',1),(2,'t2',NULL),(3,'t3',9),(4,'t4',2),(5,'t5',1)`,
		`INSERT INTO label VALUES(10,1,'x'),(11,1,'y'),(12,3,'z'),(13,4,'w'),(14,9,'q')`,
		`SELECT count(*) FROM track, label
		   WHERE track.trackid=label.trackid
		     AND EXISTS(SELECT 1 FROM artist WHERE artistid=track.trackartist)`,
		`SELECT count(*) FROM track JOIN label ON track.trackid=label.trackid
		   WHERE EXISTS(SELECT 1 FROM artist WHERE artistid=track.trackartist)`,
		`SELECT count(*) FROM track LEFT JOIN label ON track.trackid=label.trackid
		   WHERE EXISTS(SELECT 1 FROM artist WHERE artistid=track.trackartist)`,
		`SELECT label.tag, count(*) FROM track, label
		   WHERE track.trackid=label.trackid
		     AND EXISTS(SELECT 1 FROM artist WHERE artistid=track.trackartist)
		   GROUP BY label.tag ORDER BY label.tag`,
		// The correlated reference names the INNER source of the join.
		`SELECT count(*) FROM track, label
		   WHERE track.trackid=label.trackid
		     AND EXISTS(SELECT 1 FROM artist WHERE artistid=label.lid-9)`,
		// Both sources correlated from one subquery.
		`SELECT count(*) FROM track, label
		   WHERE EXISTS(SELECT 1 FROM artist WHERE artistid=track.trackartist AND artistid=label.lid-9)`,
	})
}
