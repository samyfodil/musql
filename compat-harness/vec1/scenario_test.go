package vec1

import (
	"strings"
	"testing"
)

// TestVec1Scenarios replays statement scripts in both engines, each on its
// own pair of databases, comparing every statement's answer or error:
// transactions and savepoints around writes the index has pending, joins and
// subqueries over a nearest-neighbour query (where LIMIT is or is not the
// query's K), OR as IN, 64-bit and negative rowids, and small blocks.
func TestVec1Scenarios(t *testing.T) {
	for name, script := range vec1Scenarios {
		t.Run(name, func(t *testing.T) {
			m, c := engines(t)
			for _, s := range strings.Split(script, "\n") {
				s = strings.TrimSpace(s)
				if s == "" || strings.HasPrefix(s, "--") {
					continue
				}
				same(t, m, c, s)
			}
		})
	}
}

// vec1Fill is 300 rows of 8-dimensional vectors with two metadata columns,
// generated in SQL.
const vec1Fill = `
CREATE VIRTUAL TABLE t USING vec1(vector, cat, score)
INSERT INTO t(rowid, vector, cat, score) WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 300) SELECT i, vec1_from_json('[' || (i%17) || ',' || (i%5) || ',' || (i%3) || ',' || (i%11) || ',' || (i%7) || ',' || (i%2) || ',' || (i%13) || ',' || (i%19) || ']'), char(97 + i%4), i % 10 FROM c
CREATE TABLE o(id INTEGER PRIMARY KEY, label TEXT)
INSERT INTO o WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 300) SELECT i, 'L' || i FROM c WHERE i % 3 <> 0
`

const vec1Q = `vec1_from_json('[3,1,2,4,5,1,6,7]')`

var vec1Scenarios = map[string]string{
	"transactions": vec1Fill + `
INSERT INTO t(cmd, arg) VALUES('rebuild', '{index:"flat", distance:"l2"}')
SELECT rowid, distance FROM t(` + vec1Q + `, 5)
BEGIN
INSERT INTO t(rowid, vector, cat, score) VALUES(1000, ` + vec1Q + `, 'z', 0)
SELECT rowid, distance FROM t(` + vec1Q + `, 5)
SELECT rowid, cat FROM t WHERE rowid = 1000
SAVEPOINT s1
INSERT INTO t(rowid, vector, cat, score) VALUES(1001, vec1_from_json('[3,1,2,4,5,1,6,8]'), 'z', 1)
SELECT rowid, distance FROM t(` + vec1Q + `, 5)
DELETE FROM t WHERE rowid = 1000
SELECT rowid, distance FROM t(` + vec1Q + `, 5)
ROLLBACK TO s1
SELECT rowid, distance FROM t(` + vec1Q + `, 5)
RELEASE s1
SELECT rowid, distance FROM t(` + vec1Q + `, 5)
COMMIT
SELECT rowid, distance FROM t(` + vec1Q + `, 5)
BEGIN
DELETE FROM t WHERE rowid < 50
UPDATE t SET score = 99 WHERE rowid BETWEEN 50 AND 60
SELECT rowid, distance, score FROM t(` + vec1Q + `, 8)
ROLLBACK
SELECT rowid, distance, score FROM t(` + vec1Q + `, 8)
SELECT count(*) FROM t
BEGIN
UPDATE t SET vector = ` + vec1Q + ` WHERE rowid = 7
SELECT rowid, distance FROM t(` + vec1Q + `, 3)
COMMIT
SELECT rowid, distance FROM t(` + vec1Q + `, 3)
SELECT * FROM t_config
SELECT id, bucket, first, last, length(val) FROM t_idx ORDER BY id
PRAGMA integrity_check
`,
	"limits": vec1Fill + `
INSERT INTO t(cmd, arg) VALUES('rebuild', '{index:"flat", distance:"l2"}')
SELECT rowid, distance FROM t(` + vec1Q + `) LIMIT 4
SELECT rowid, distance FROM t(` + vec1Q + `) LIMIT 4 OFFSET 2
SELECT rowid, distance FROM t(` + vec1Q + `) WHERE cat = 'a' LIMIT 4
SELECT rowid, distance FROM t(` + vec1Q + `) WHERE cat = 'a' OR cat = 'b' LIMIT 6
SELECT rowid, distance FROM t(` + vec1Q + `, 20) WHERE cat IN ('c', 'd') AND score > 3
SELECT rowid, distance FROM t(` + vec1Q + `, 20) WHERE score BETWEEN 2 AND 4
SELECT rowid, distance FROM t(` + vec1Q + `, 20) WHERE cat LIKE 'b%'
SELECT rowid, distance FROM t(` + vec1Q + `, 20) WHERE cat GLOB '[ab]'
SELECT rowid, distance FROM t(` + vec1Q + `, 20) WHERE likely(score = 3)
SELECT rowid, distance FROM t(` + vec1Q + `) ORDER BY distance LIMIT 3
SELECT rowid, distance FROM t(` + vec1Q + `, 9) ORDER BY rowid DESC LIMIT 3
SELECT count(*), max(distance) FROM t(` + vec1Q + `, 25)
SELECT cat, count(*) FROM t(` + vec1Q + `, 40) GROUP BY cat
SELECT DISTINCT cat FROM t(` + vec1Q + `, 12)
SELECT * FROM (SELECT rowid, distance FROM t(` + vec1Q + `) LIMIT 4)
SELECT rowid FROM t(` + vec1Q + `, 6) WHERE rowid IN (SELECT id FROM o)
SELECT t.rowid, o.label, t.distance FROM t(` + vec1Q + `, 10) AS t JOIN o ON o.id = t.rowid
SELECT t.rowid, o.label FROM t(` + vec1Q + `, 10) AS t LEFT JOIN o ON o.id = t.rowid ORDER BY t.rowid
SELECT o.id, (SELECT count(*) FROM t(` + vec1Q + `, 30) WHERE cat = 'a') FROM o WHERE o.id < 3
SELECT rowid, distance FROM t(` + vec1Q + `, 5) UNION ALL SELECT rowid, distance FROM t(vec1_from_json('[0,0,0,0,0,0,0,0]'), 2)
WITH n AS (SELECT rowid, distance FROM t(` + vec1Q + `, 8)) SELECT rowid FROM n WHERE distance < 40
SELECT rowid, distance FROM t(` + vec1Q + `, '{k:4, streaming:1}') LIMIT 10
`,
	"rowids": `
CREATE VIRTUAL TABLE r USING vec1(vector, tag)
INSERT INTO r(rowid, vector, tag) VALUES(-5, vec1_from_json('[1,2]'), 'neg')
INSERT INTO r(rowid, vector, tag) VALUES(1099511627776, vec1_from_json('[1,3]'), 'big')
INSERT INTO r(vector, tag) VALUES(vec1_from_json('[5,5]'), 'auto')
INSERT INTO r(rowid, vector, tag) VALUES(9223372036854775807, vec1_from_json('[2,2]'), 'max')
INSERT INTO r(rowid, vector, tag) VALUES(-9223372036854775808, vec1_from_json('[0,2]'), 'min')
INSERT INTO r(rowid, vector, tag) VALUES(3, vec1_from_json('[1,1]'), 'small')
SELECT rowid, tag FROM r ORDER BY rowid
-- With the largest rowid taken, a new one is chosen at random: compare
-- only that nothing was overwritten.
INSERT INTO r(vector, tag) VALUES(vec1_from_json('[6,6]'), 'random')
SELECT count(*), sum(tag = 'min'), sum(tag = 'random'), sum(rowid > 0) FROM r
DELETE FROM r WHERE tag = 'random'
SELECT rowid, tag FROM r ORDER BY rowid
INSERT INTO r(cmd, arg) VALUES('rebuild', '{index:"flat", distance:"l2"}')
SELECT rowid, distance, tag FROM r(vec1_from_json('[1,2]'), 10)
SELECT id, bucket, first, last, hex(val) FROM r_idx ORDER BY id
DELETE FROM r WHERE rowid = 1099511627776
SELECT rowid, distance FROM r(vec1_from_json('[1,2]'), 10)
INSERT INTO r(rowid, vector, tag) VALUES(1099511627777, vec1_from_json('[1,2]'), 'big2')
SELECT rowid, distance FROM r(vec1_from_json('[1,2]'), 10)
SELECT id, bucket, first, last, hex(val) FROM r_idx ORDER BY id
UPDATE r SET tag = 'up' WHERE rowid = -5
SELECT rowid, tag FROM r WHERE rowid = -5
PRAGMA integrity_check
`,
	"blocks": vec1Fill + `
INSERT INTO t(cmd, arg) VALUES('blocksize', 512)
INSERT INTO t(cmd, arg) VALUES('blocksize-min', 256)
SELECT * FROM t_config
INSERT INTO t(cmd, arg) VALUES('rebuild', '{index:"flat", distance:"l2"}')
SELECT id, bucket, first, last, length(val) FROM t_idx ORDER BY id
DELETE FROM t WHERE rowid % 4 = 1
SELECT id, bucket, first, last, hex(val) FROM t_idx ORDER BY id
INSERT INTO t(rowid, vector, cat, score) WITH RECURSIVE c(i) AS (SELECT 301 UNION ALL SELECT i+1 FROM c WHERE i < 360) SELECT i, vec1_from_json('[' || (i%3) || ',1,1,1,1,1,1,' || (i%9) || ']'), 'n', 0 FROM c
SELECT id, bucket, first, last, hex(val) FROM t_idx ORDER BY id
DELETE FROM t WHERE rowid > 100 AND rowid < 250
SELECT id, bucket, first, last, hex(val) FROM t_idx ORDER BY id
SELECT rowid, distance FROM t(` + vec1Q + `, 12)
SELECT rowid, distance FROM t(` + vec1Q + `, 12) WHERE cat = 'n'
INSERT INTO t(cmd, arg) VALUES('rebuild', NULL)
SELECT id, bucket, first, last, hex(val) FROM t_idx ORDER BY id
SELECT id, hex(val) FROM t_meta ORDER BY id
PRAGMA integrity_check
`,
}
