package engine

// The order TEMP triggers on a non-temp table fire in is the order of C's
// temp schema trigger hash (sqlite3TriggerList, trigger.c, walks
// pTmpSchema->trigHash and prepends each match). tempTrigHash reproduces that
// hash (hash.c) over trigger names: its element list, and once it has grown
// buckets, which bucket each name falls in, so the walk is C's walk.

// tempTrigHashSoftLimit is SQLITE_MALLOC_SOFT_LIMIT over sizeof(struct _ht)
// (16 bytes on a 64-bit build): rehash never asks for more buckets.
const tempTrigHashMaxBuckets = 1024 / 16

// tempTrigHash is one Hash keyed by trigger name. unknown is set once this
// session did something to the temp schema the emulation does not follow;
// the order is then not claimed (checkTempTriggerOrder). The zero value is
// the empty hash a new session starts with.
type tempTrigHash struct {
	unknown bool
	htsize  int
	elems   []tempTrigElem // the element list, first to last
	counts  []int          // per bucket, when htsize > 0
}

type tempTrigElem struct {
	key string
	h   uint32
}

// strHash is hash.c's strHash: case is masked off ASCII letters.
func tempTrigStrHash(z string) uint32 {
	var h uint32
	for i := 0; i < len(z); i++ {
		h += uint32(z[i] & 0xdf)
		h *= 0x9e3779b1
	}
	return h
}

func (t *tempTrigHash) clone() tempTrigHash {
	return tempTrigHash{unknown: t.unknown, htsize: t.htsize,
		elems: append([]tempTrigElem(nil), t.elems...), counts: append([]int(nil), t.counts...)}
}

// insertElement is hash.c's insertElement: ahead of the bucket's first
// element when it has one, else at the head of the list.
func (t *tempTrigHash) insertElement(e tempTrigElem) {
	if t.htsize > 0 {
		b := int(e.h % uint32(t.htsize))
		if t.counts[b] > 0 {
			for i, x := range t.elems {
				if int(x.h%uint32(t.htsize)) == b {
					t.elems = append(t.elems[:i], append([]tempTrigElem{e}, t.elems[i:]...)...)
					t.counts[b]++
					return
				}
			}
		}
		t.counts[b]++
	}
	t.elems = append([]tempTrigElem{e}, t.elems...)
}

// insert is sqlite3HashInsert of a new key.
func (t *tempTrigHash) insert(key string) {
	e := tempTrigElem{key: key, h: tempTrigStrHash(key)}
	n := len(t.elems) + 1
	if n >= 5 && n > 2*t.htsize {
		size := min(n*3, tempTrigHashMaxBuckets)
		if size != t.htsize {
			old := t.elems
			t.htsize, t.elems, t.counts = size, nil, make([]int, size)
			for _, x := range old {
				t.insertElement(x)
			}
		}
	}
	t.insertElement(e)
}

// remove is sqlite3HashInsert with no data: removeElement, which clears the
// hash, buckets and all, once it is empty.
func (t *tempTrigHash) remove(key string) {
	for i, x := range t.elems {
		if equalFoldName(x.key, key) {
			t.elems = append(t.elems[:i], t.elems[i+1:]...)
			if t.htsize > 0 {
				t.counts[int(x.h%uint32(t.htsize))]--
			}
			break
		}
	}
	if len(t.elems) == 0 {
		t.htsize, t.elems, t.counts = 0, nil, nil
	}
}

// rebuildTempTrigHash is the hash a reload of the temp schema builds: empty, then every
// TEMP trigger inserted in sqlite_temp_master's rowid order (sqlite3InitOne),
// which is creation order.
func (db *DB) rebuildTempTrigHash() {
	db.tempTrigHash = tempTrigHash{}
	for _, tr := range db.triggers {
		if tr.isTemp {
			db.tempTrigHash.insert(tr.name)
		}
	}
}

// tempTrigOrder is the TEMP triggers keep accepts, in the order C prepends
// them onto a non-temp table's trigger list -- the reverse of the hash walk --
// and ok when the hash is known and holds exactly the session's TEMP triggers.
func (db *DB) tempTrigOrder(keep func(*triggerMeta) bool) ([]*triggerMeta, bool) {
	h := &db.tempTrigHash
	if h.unknown {
		return nil, false
	}
	byName := map[string]*triggerMeta{}
	for _, tr := range db.triggers {
		if tr.isTemp {
			k := r33sFoldIdent(tr.name)
			if _, dup := byName[k]; dup {
				return nil, false
			}
			byName[k] = tr
		}
	}
	if len(byName) != len(h.elems) {
		return nil, false
	}
	var out []*triggerMeta
	for i := len(h.elems) - 1; i >= 0; i-- {
		tr, ok := byName[r33sFoldIdent(h.elems[i].key)]
		if !ok {
			return nil, false
		}
		if keep(tr) {
			out = append(out, tr)
		}
	}
	return out, true
}
