package libsql

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestKNN compares distance-ordered top-k queries with libSQL's answers, row
// for row: both distances, either argument order, ASC and DESC, OFFSETs, ties
// (duplicate vectors), a zero vector (a NULL distance), and rows changed after
// the table was compacted (the log musql merges).
func TestKNN(t *testing.T) {
	m, l := engines(t)
	const n, d = 3000, 24
	r := rand.New(rand.NewPCG(3, 4))
	setup := []string{fmt.Sprintf("CREATE TABLE docs(id INTEGER PRIMARY KEY, tag TEXT, emb F32_BLOB(%d))", d)}
	for i := 1; i <= n; i++ {
		v := randVec(r, d)
		if i%97 == 0 {
			v = fmt.Sprintf("[%s1]", repeat("1,", d-1)) // duplicates: ties
		}
		if i == 1234 {
			v = fmt.Sprintf("[%s0]", repeat("0,", d-1)) // zero: NULL cosine
		}
		setup = append(setup, fmt.Sprintf("INSERT INTO docs VALUES (%d, 't%d', vector('%s'))", i, i%7, v))
	}
	setup = append(setup, "VACUUM",
		"DELETE FROM docs WHERE id % 50 = 0",
		fmt.Sprintf("UPDATE docs SET emb = vector('%s') WHERE id %% 61 = 0", randVec(r, d)),
		fmt.Sprintf("INSERT INTO docs VALUES (99999, 'new', vector('%s'))", randVec(r, d)))
	for _, s := range setup {
		if _, err := m.Exec(s); err != nil {
			t.Fatal(s, err)
		}
		if _, err := l.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}
	q := blob(r, d)
	qtext := randVec(r, d)
	var qs []string
	for _, f := range []string{"vector_distance_cos(emb, ?)", "vector_distance_cos(?, emb)", "vector_distance_l2(emb, ?)", "vector_distance_l2(?, emb)"} {
		for _, tail := range []string{"LIMIT 10", "LIMIT 1", "DESC LIMIT 15", "LIMIT 20 OFFSET 7", "DESC LIMIT 5 OFFSET 3", "LIMIT 5000"} {
			qs = append(qs, fmt.Sprintf("SELECT id, tag FROM docs ORDER BY %s %s", f, tail))
		}
	}
	engine.VecBoundedServedForTest()
	// The spellings libSQL applications use: the query inline, through an
	// encoder, and the distance returned alongside.
	inline := []string{
		fmt.Sprintf("SELECT id FROM docs ORDER BY vector_distance_cos(emb, vector('%s')) LIMIT 10", qtext),
		fmt.Sprintf("SELECT id, vector_distance_cos(emb, vector32('%s')) AS d FROM docs ORDER BY d LIMIT 10", qtext),
		fmt.Sprintf("SELECT id, tag, vector_distance_l2(vector('%s'), emb) AS d FROM docs ORDER BY d DESC LIMIT 7 OFFSET 2", qtext),
		"SELECT vector_distance_cos(emb, vector32(?)) AS d, id FROM docs ORDER BY d LIMIT 10",
	}
	qs = append(qs, inline...)
	for _, s := range qs {
		arg := []any{q}
		if !strings.Contains(s, "?") {
			arg = nil
		}
		s0, d0 := engine.SegFilterCountersForTest()
		got := answerArgs(m, s, arg...)
		s1, d1 := engine.SegFilterCountersForTest()
		if s1-s0 != 1 || d1 != d0 {
			t.Errorf("%s: the columnar top-k did not serve (served +%d, declined +%d)", s, s1-s0, d1-d0)
		}
		if want := answerArgs(l, s, arg...); got != want {
			t.Errorf("%s\n musql:  %.300s\n libsql: %.300s", s, got, want)
		}
	}
	// The int8 bounds answered most of them (the small-LIMIT ones, once the
	// first search built the sidecar), and every answer above still matched.
	if n := engine.VecBoundedServedForTest(); n < int64(len(qs)/2) {
		t.Errorf("the int8 bounds answered %d of %d searches", n, len(qs))
	}
}

func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}
