package vec1

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

// vecBlob is a vector in vec1's format: float32s in machine order (little
// endian on every platform this runs on).
func vecBlob(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

// randomVecs is n random vectors, components in [-1,1).
func randomVecs(r *rand.Rand, n, dims int) [][]float32 {
	out := make([][]float32, n)
	for i := range out {
		v := make([]float32, dims)
		for j := range v {
			v[j] = float32(r.Float64()*2 - 1)
		}
		out[i] = v
	}
	return out
}

// clusteredVecs is n vectors around k random centres.
func clusteredVecs(r *rand.Rand, n, dims, k int) [][]float32 {
	centres := randomVecs(r, k, dims)
	out := make([][]float32, n)
	for i := range out {
		c := centres[r.IntN(k)]
		v := make([]float32, dims)
		for j := range v {
			v[j] = c[j] + float32(r.NormFloat64()*0.1)
		}
		out[i] = v
	}
	return out
}

// exec runs one statement in both engines and fails unless both answer the
// same.
func same(t *testing.T, m, c *sql.DB, q string, args ...any) string {
	t.Helper()
	got, want := render(m, q, args...), render(c, q, args...)
	if got != want {
		t.Fatalf("%s\n--- musql\n%s--- C\n%s", q, clip(got), clip(want))
	}
	return want
}

func clip(s string) string {
	if len(s) > 3000 {
		return s[:3000] + "...\n"
	}
	return s
}

// shadowsSame compares every shadow table of t byte for byte.
func shadowsSame(t *testing.T, m, c *sql.DB, name string) {
	t.Helper()
	for _, q := range []string{
		"SELECT id, quote(val) FROM %s_config ORDER BY id",
		"SELECT * FROM %s_base ORDER BY id",
		"SELECT id, bucket, first, last, hex(val) FROM %s_idx ORDER BY id",
		"SELECT id, hex(val) FROM %s_meta ORDER BY id",
		"SELECT id, length(val) FROM %s_model ORDER BY id",
	} {
		same(t, m, c, fmt.Sprintf(q, name))
	}
}

// load fills a vec1 table in both engines with the same rows: rowid i+1,
// the vector, and metadata columns derived from i of mixed types.
func load(t *testing.T, m, c *sql.DB, name string, vecs [][]float32, nMeta int) {
	t.Helper()
	for _, db := range []*sql.DB{m, c} {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		cols := "rowid, vector"
		ph := "?, ?"
		for j := range nMeta {
			cols += fmt.Sprintf(", m%d", j)
			ph += ", ?"
		}
		st, err := tx.Prepare(fmt.Sprintf("INSERT INTO %s(%s) VALUES(%s)", name, cols, ph))
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range vecs {
			args := []any{i + 1, vecBlob(v)}
			for j := range nMeta {
				args = append(args, metaValue(i, j))
			}
			if _, err := st.Exec(args...); err != nil {
				t.Fatal(err)
			}
		}
		st.Close()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

// metaValue is metadata column j of row i: column 0 a small integer, 1 a
// larger one or NULL, 2 a REAL, 3 TEXT, others mixed.
func metaValue(i, j int) any {
	switch j {
	case 0:
		return i % 7
	case 1:
		if i%5 == 0 {
			return nil
		}
		return i * 1000
	case 2:
		return float64(i%11) / 4
	case 3:
		return fmt.Sprintf("t%02d", i%13)
	}
	switch i % 4 {
	case 0:
		return i
	case 1:
		return "s"
	case 2:
		return 2.5
	}
	return nil
}

// trainModel trains a model in the oracle over table name's vectors.
func trainModel(t *testing.T, c *sql.DB, name, cfg string) []byte {
	t.Helper()
	var blob []byte
	if err := c.QueryRow(fmt.Sprintf("SELECT vec1_train(vector, ?) FROM %s", name), cfg).Scan(&blob); err != nil {
		t.Fatalf("train %s: %v", cfg, err)
	}
	return blob
}

// TestVec1Exact compares answers over every index kind vec1 builds, with one
// model trained by the oracle and given to both: the shadow tables after a
// rebuild, after inserts, deletes and updates, and the nearest-neighbour
// queries -- rows, order and distances to the bit -- with and without
// metadata filters, streaming, and LIMIT as K.
func TestVec1Exact(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	const dims = 24
	vecs := clusteredVecs(r, 1500, dims, 12)
	queries := randomVecs(r, 6, dims)
	configs := []struct {
		name  string
		model string // a JSON model, or a vec1_train config when train is set
		train bool
	}{
		{"none", "", false},
		{"flatl2", `{index:"flat", distance:"l2"}`, false},
		{"flatcos", `{index:"flat", distance:"cos"}`, false},
		{"nonecos", `{index:"none", distance:"cos"}`, false},
		{"ivf", `{nbucket: 16}`, true},
		{"ivfcos", `{nbucket: 16, distance:"cos"}`, true},
		{"pq", `{codesize: 6, quantizer:"pq"}`, true},
		{"ivfpq", `{nbucket: 8, codesize: 6, quantizer:"pq"}`, true},
		{"ivfpqnores", `{nbucket: 8, codesize: 5, quantizer:"pq", residual: false}`, true},
		{"ivfpqcos", `{nbucket: 8, codesize: 8, quantizer:"pq", distance:"cos"}`, true},
		{"opq", `{nbucket: 8, codesize: 6, quantizer:"opq", nopq_round: 2}`, true},
		{"opqcos", `{nbucket: 4, codesize: 4, quantizer:"opq", nopq_round: 2, distance:"cos"}`, true},
		{"bq", `{quantizer:"bq"}`, true},
		{"ivfbq", `{nbucket: 8, quantizer:"bq"}`, true},
		{"ivfbqnores", `{nbucket: 8, quantizer:"bq", residual:false}`, true},
		{"ivfbqhalf", `{nbucket: 8, quantizer:"bq", codesize: 2}`, true},
		{"ivfbqcos", `{nbucket: 4, quantizer:"bq", distance:"cos"}`, true},
		{"pqwht", `{nbucket: 4, codesize: 6, quantizer:"pq", wht: true}`, true},
	}
	for _, cf := range configs {
		t.Run(cf.name, func(t *testing.T) {
			m, c := engines(t)
			const nMeta = 5
			same(t, m, c, "CREATE VIRTUAL TABLE v USING vec1(vector, m0, m1, m2, m3, m4)")
			load(t, m, c, "v", vecs[:1000], nMeta)
			var model any
			if cf.train {
				model = trainModel(t, c, "v", cf.model)
			} else if cf.model != "" {
				model = cf.model
			}
			if model != nil {
				same(t, m, c, "INSERT INTO v(cmd, arg) VALUES('rebuild', ?)", model)
			}
			shadowsSame(t, m, c, "v")
			check := func() {
				for qi, q := range queries {
					qb := vecBlob(q)
					same(t, m, c, "SELECT rowid, distance, m0, m3 FROM v(?, 10)", qb)
					same(t, m, c, "SELECT rowid, distance FROM v(?, '{k:20, nprobe:3}')", qb)
					same(t, m, c, "SELECT rowid, distance FROM v(?, '{k:7, nprobe:0.5}') WHERE m0 = 3", qb)
					same(t, m, c, "SELECT rowid, distance FROM v(?, 15) WHERE m1 > 400000 AND m2 <= 1.5", qb)
					same(t, m, c, "SELECT rowid, distance FROM v(?, 15) WHERE m1 IS NULL", qb)
					same(t, m, c, "SELECT rowid, distance FROM v(?, 15) WHERE m3 IN ('t01', 't05', 't09')", qb)
					same(t, m, c, "SELECT rowid, distance FROM v(?, 12) WHERE m4 IS NOT NULL AND m0 < 4", qb)
					same(t, m, c, "SELECT rowid, distance FROM v(?) WHERE m0 >= 2 LIMIT 9", qb)
					same(t, m, c, "SELECT rowid, distance FROM v(?, '{k:5, streaming:1, nprobe:2}') LIMIT 40", qb)
					same(t, m, c, "SELECT rowid, distance FROM v(?, '{k:30, nprobe_slack:0.2, nprobe:1.0}')", qb)
					same(t, m, c, "SELECT count(*) FROM v(?, 1000)", qb)
					if qi == 0 {
						same(t, m, c, "SELECT rowid, vector, distance FROM v(?, 3)", qb)
					}
				}
			}
			check()
			// Writes after the index exists: through the pending lists and
			// into the shadow tables.
			load(t, m, c, "v", vecs[:0], nMeta)
			for _, db := range []*sql.DB{m, c} {
				for i := 1000; i < 1200; i++ {
					if _, err := db.Exec("INSERT INTO v(rowid, vector, m0, m1, m2, m3, m4) VALUES(?, ?, ?, ?, ?, ?, ?)",
						i+1, vecBlob(vecs[i]), metaValue(i, 0), metaValue(i, 1), metaValue(i, 2), metaValue(i, 3), metaValue(i, 4)); err != nil {
						t.Fatal(err)
					}
				}
			}
			shadowsSame(t, m, c, "v")
			same(t, m, c, "DELETE FROM v WHERE rowid % 3 = 0 AND rowid < 600")
			same(t, m, c, "DELETE FROM v WHERE rowid = 1100")
			same(t, m, c, "UPDATE v SET m0 = 99 WHERE rowid BETWEEN 700 AND 720")
			same(t, m, c, "UPDATE v SET vector = ? WHERE rowid = 5", vecBlob(vecs[1300]))
			shadowsSame(t, m, c, "v")
			check()
			same(t, m, c, "INSERT INTO v(cmd, arg) VALUES('rebuild', NULL)")
			shadowsSame(t, m, c, "v")
			check()
			same(t, m, c, "PRAGMA integrity_check")
			same(t, m, c, "SELECT database, name, model FROM vec1cat")
		})
	}
}

// TestVec1Statements compares DDL, commands, errors and the scalar
// functions statement by statement.
func TestVec1Statements(t *testing.T) {
	m, c := engines(t)
	for _, s := range strings.Split(vec1Statements, "\n") {
		s = strings.TrimSpace(s)
		if s == "" || strings.HasPrefix(s, "--") {
			continue
		}
		same(t, m, c, s)
	}
}

const vec1Statements = `
SELECT vec1_info()
SELECT vec1_to_json(vec1_from_json('[1, -2.5, 3e10, 1e-40, 0.1]'))
SELECT hex(vec1_from_json('[ 1,2 ,3]')), hex(vec1_from_json('[0x10, -inf, nan]')), hex(vec1_from_json('[1e400,-0]'))
SELECT vec1_from_json('[]')
SELECT vec1_from_json('[1 2]')
SELECT vec1_from_json('1,2')
SELECT vec1_from_json(NULL), vec1_from_json(5)
SELECT vec1_to_json(x''), vec1_to_json(x'01')
SELECT vec1_to_json('abcd'), vec1_to_json_i(x'01000000ffffffff'), vec1_to_json_f(x'0000803f')
SELECT vec1_l2_distance(x'0000803f00000040', x'0000000000000000'), vec1_cos_distance(x'0000803f00000040', x'00000040000080bf')
SELECT vec1_l2_distance(x'0000803f', x'00000000ff'), vec1_cos_distance('ab', 'cd')
SELECT vec1_l2_distance(x'', x''), vec1_cos_distance(x'', x'')
SELECT vec1_config('nthread'), vec1_config('nprobe')
SELECT vec1_config('nthread', 4), vec1_config('nthread')
SELECT vec1_config('nthread', 0)
SELECT vec1_config('nthread', 200)
SELECT vec1_config('nprobe', 0.25), vec1_config('nprobe', 3), vec1_config('nprobe')
SELECT vec1_config('nprobe', 0)
SELECT vec1_config('nprobe', 'abc')
SELECT vec1_config('bogus')
SELECT vec1_config(NULL)
SELECT vec1_config('nthread', 1), vec1_config('nprobe', 0.05)
CREATE VIRTUAL TABLE t1 USING vec1
CREATE VIRTUAL TABLE t2 USING vec1()
CREATE VIRTUAL TABLE t3 USING vec1(emb, "a b", [c], 'd', ` + "`e`" + `)
CREATE VIRTUAL TABLE t4 USING vec1(emb, rowid)
CREATE VIRTUAL TABLE t5 USING vec1(emb, distance)
CREATE VIRTUAL TABLE t6 USING vec1(emb, a INTEGER)
CREATE VIRTUAL TABLE t7 USING vec1(emb, a, a)
CREATE VIRTUAL TABLE t8 USING vec1(emb, cmd)
CREATE VIRTUAL TABLE t10 USING vec1(RowId)
SELECT type, name, tbl_name, sql FROM sqlite_schema ORDER BY rowid
PRAGMA table_xinfo(t3)
PRAGMA table_info(t1)
INSERT INTO t1 VALUES(x'0000803f00000040')
INSERT INTO t1(vector) VALUES(x'0000803f')
INSERT INTO t1(vector) VALUES(x'0000803f000000400000')
INSERT INTO t1(vector) VALUES('abcd')
INSERT INTO t1(vector) VALUES(NULL)
INSERT INTO t1(vector) VALUES(12)
INSERT INTO t1(vector) VALUES(1.5)
INSERT INTO t1(vector) VALUES(x'0000c07f00000040')
INSERT INTO t1(rowid, vector) VALUES(10, x'000080400000a040')
INSERT INTO t1(rowid, vector) VALUES(10, x'000080400000a040')
INSERT OR IGNORE INTO t1(rowid, vector) VALUES(10, x'000080400000a040')
SELECT rowid, hex(vector), distance FROM t1
SELECT * FROM t1_config
SELECT rowid, distance FROM t1(x'0000803f00000040', 5)
SELECT rowid, distance FROM t1(x'0000803f00000040', '5')
SELECT rowid, distance FROM t1(x'0000803f00000040', 5.0)
SELECT rowid, distance FROM t1(x'0000803f00000040', 5.5)
SELECT rowid, distance FROM t1(x'0000803f00000040', -1)
SELECT rowid, distance FROM t1(x'0000803f00000040', 33554433)
SELECT rowid, distance FROM t1(x'0000803f00000040', NULL)
SELECT rowid, distance FROM t1(x'0000803f00000040', '{k:2}')
SELECT rowid, distance FROM t1(x'0000803f00000040', '{K:2, nprobe:0}')
SELECT rowid, distance FROM t1(x'0000803f00000040', '{K:2, nprobe:"x"}')
SELECT rowid, distance FROM t1(x'0000803f00000040', '{K:"2"}')
SELECT rowid, distance FROM t1(x'0000803f00000040', '{K:2.0}')
SELECT rowid, distance FROM t1(x'0000803f00000040', '{bogus:1}')
SELECT rowid, distance FROM t1(x'0000803f00000040', '{K:null}')
SELECT rowid, distance FROM t1(x'0000803f00000040', '{K:')
SELECT rowid, distance FROM t1(x'0000803f00000040', '[1,2]')
SELECT rowid, distance FROM t1(x'0000803f', 5)
SELECT rowid, distance FROM t1(x'0000c07f00000040', 5)
SELECT rowid, distance FROM t1(x'0000803f00000040')
SELECT rowid, distance FROM t1(x'0000803f00000040') LIMIT 1
SELECT rowid, distance FROM t1(x'0000803f00000040') LIMIT 0
SELECT rowid, distance FROM t1(x'0000803f00000040') LIMIT -1
SELECT rowid, distance FROM t1(x'0000803f00000040') ORDER BY rowid LIMIT 1
SELECT rowid, distance FROM t1(x'0000803f00000040', 9) ORDER BY rowid LIMIT 1
SELECT rowid, distance FROM t1(x'0000803f00000040', 9) LIMIT 1
SELECT rowid, distance FROM t1(x'0000803f00000040', 0)
SELECT rowid, distance FROM t1 WHERE cmd = x'0000803f00000040' AND arg = 3
SELECT rowid, distance FROM t1(x'0000803f00000040', 3) WHERE rowid > 1
SELECT count(*) FROM t1
SELECT rowid FROM t1 WHERE rowid = 10
SELECT rowid FROM t1 WHERE rowid = '10'
INSERT INTO t1(cmd, arg) VALUES('nope', 1)
INSERT INTO t1(cmd, arg) VALUES('blocksize', 100)
INSERT INTO t1(cmd, arg) VALUES('blocksize', '2048')
INSERT INTO t1(cmd, arg) VALUES('blocksize', 2048.0)
INSERT INTO t1(cmd, arg) VALUES('BlockSize-Min', 4096)
INSERT INTO t1(cmd, vector) VALUES('blocksize', 65536)
SELECT * FROM t1_config
INSERT INTO t1(cmd, arg) VALUES('rebuild', '{index:"bogus"}')
INSERT INTO t1(cmd, arg) VALUES('rebuild', '{index:"flat", distance:3}')
INSERT INTO t1(cmd, arg) VALUES('rebuild', '{index:"flat", extra:1}')
INSERT INTO t1(cmd, arg) VALUES('rebuild', '{}')
INSERT INTO t1(cmd, arg) VALUES('rebuild', 'not json')
INSERT INTO t1(cmd, arg) VALUES('rebuild', x'00')
INSERT INTO t1(cmd, arg) VALUES('rebuild', x'000000040000000100000003000000000000000000000001')
INSERT INTO t1(cmd, arg) VALUES('rebuild', x'000000090000000100000002000000000000000000000001')
INSERT INTO t1(cmd, arg) VALUES('rebuild', x'00000004000000010000000200000000000000000000000100')
INSERT INTO t1(cmd, arg) VALUES('rebuild', 5)
INSERT INTO t1(cmd, arg) VALUES('rebuild', '{index:"flat"}')
SELECT rowid, distance FROM t1(x'0000803f00000040', 5)
SELECT database, name, model FROM vec1cat ORDER BY name
INSERT INTO t1(cmd, arg) VALUES('rebuild', '{index:"none"}')
SELECT rowid, hex(vector) FROM t1
PRAGMA integrity_check
DROP TABLE t1
SELECT name FROM sqlite_schema ORDER BY rowid
SELECT vec1_train(x'0000803f')
SELECT vec1_train(NULL)
SELECT vec1_train(x'0000803f', '{bogus:1}')
SELECT vec1_train(x'0000803f', '{nbucket:3}')
SELECT vec1_train(x'0000803f', '{codesize:300}')
SELECT vec1_train(x'0000803f', '{nbucket:-1}')
SELECT vec1_train(x'0000803f', '{quantizer:"x"}')
SELECT vec1_train(x'0000803f', '{residual:2}')
SELECT vec1_train(x'0000803f', '{index:"x"}')
SELECT vec1_train(x'0000803f', '{nopq_round:0}')
SELECT vec1_train(x'0000803f', '{nthread:0}')
SELECT vec1_train(x'0000803f', '{distance:"l1"}')
SELECT vec1_train(x'0000803f', 'bad json')
SELECT hex(vec1_train(x'0000803f00000040'))
SELECT hex(vec1_train(x'0000803f00000040', '{distance:"cos"}'))
SELECT vec1_train(x'0000803f00000040', '{nbucket:2}')
SELECT vec1_train(x'0000803f00000040', '{codesize:1}')
SELECT vec1_train(x) FROM (SELECT 1 AS x WHERE 0)
`
