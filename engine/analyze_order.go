package engine

import "sort"

// THE ORDER ANALYZE VISITS TABLES AND INDEXES IN, determined by the sqlite_schema
// table hash iteration order. analyzeDatabase walks the table hash (analyze.c:1403),
// and analyzeOneTable walks a table's index list (newest-first, per build.c).

// analyzeTableOrder returns main's ordinary tables in sqlite_schema hash iteration
// order, for a fresh schema load (sqlite3InitCallback's rowid order).
func (db *DB) analyzeTableOrder() []*tableMeta {
	type obj struct {
		seq  uint64
		name string
		t    *tableMeta
	}
	var objs []obj
	for _, t := range db.tables {
		if !t.isTemp {
			objs = append(objs, obj{t.schemaSeq, t.name, t})
		}
	}
	for _, v := range db.views {
		if !v.isTemp {
			objs = append(objs, obj{v.schemaSeq, v.name, nil})
		}
	}
	for _, vt := range db.vtabs {
		if !vt.isTemp {
			objs = append(objs, obj{vt.schemaSeq, vt.name, nil})
		}
	}
	sort.SliceStable(objs, func(i, j int) bool { return objs[i].seq < objs[j].seq })
	keys := make([]string, 0, len(objs)+1)
	keys = append(keys, "sqlite_master")
	for _, o := range objs {
		keys = append(keys, o.name)
	}
	var out []*tableMeta
	for _, i := range sqliteHashOrder(keys) {
		if i > 0 && objs[i-1].t != nil {
			out = append(out, objs[i-1].t)
		}
	}
	return out
}

// analyzeIndexOrder returns t's index list ordered by schema sequence and
// ON CONFLICT REPLACE status (build.c:4481-4484, 4503-4516).
func (db *DB) analyzeIndexOrder(t *tableMeta) []*indexMeta {
	var out []*indexMeta
	for _, idx := range db.indexes {
		if idx.isTemp == t.isTemp && equalFoldName(idx.table, t.name) {
			out = append(out, idx)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := out[i].onConflict == conflictReplace, out[j].onConflict == conflictReplace
		if ri != rj {
			return rj
		}
		return out[i].schemaSeq > out[j].schemaSeq
	})
	return out
}

// sqliteHashOrder simulates SQLite's Hash iteration order (hash.c)
// after inserting keys in order: rehash occurs at count >= 5 and
// count > 2*bucket_count, resizing to count*3 buckets (capped at 1024 bytes).
func sqliteHashOrder(keys []string) []int {
	n := len(keys)
	h := make([]uint32, n)
	next, prev := make([]int, n), make([]int, n)
	first := -1
	type bucket struct{ count, chain int }
	var ht []bucket
	insert := func(b *bucket, e int) {
		head := -1
		if b != nil {
			if b.count > 0 {
				head = b.chain
			}
			b.count++
			b.chain = e
		}
		if head >= 0 {
			next[e], prev[e] = head, prev[head]
			if prev[head] >= 0 {
				next[prev[head]] = e
			} else {
				first = e
			}
			prev[head] = e
			return
		}
		next[e], prev[e] = first, -1
		if first >= 0 {
			prev[first] = e
		}
		first = e
	}
	for i, k := range keys {
		h[i] = sqliteStrHash(k)
		if count := i + 1; count >= 5 && count > 2*len(ht) {
			size := count * 3
			if size*16 > 1024 {
				size = 1024 / 16
			}
			if size != len(ht) {
				ht = make([]bucket, size)
				e := first
				first = -1
				for e >= 0 {
					nx := next[e]
					insert(&ht[h[e]%uint32(size)], e)
					e = nx
				}
			}
		}
		if ht != nil {
			insert(&ht[h[i]%uint32(len(ht))], i)
		} else {
			insert(nil, i)
		}
	}
	order := make([]int, 0, n)
	for e := first; e >= 0; e = next[e] {
		order = append(order, e)
	}
	return order
}

// sqliteStrHash is hash.c's strHash: Knuth multiplicative hashing over the key
// with each byte's 0x20 (ASCII case) bit masked off.
func sqliteStrHash(z string) uint32 {
	var h uint32
	for i := 0; i < len(z); i++ {
		h += 0xdf & uint32(z[i])
		h *= 0x9e3779b1
	}
	return h
}
