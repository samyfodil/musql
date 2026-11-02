package engine

// int64Set is a set of int64 values: open addressing with linear probing over
// a power-of-two table, mixed by mix64. Zero marks an empty slot, so the value
// zero is kept apart.
type int64Set struct {
	slots []int64
	n     int // values in slots
	zero  bool
}

func (s *int64Set) add(v int64) {
	if v == 0 {
		s.zero = true
		return
	}
	if (s.n+1)*2 > len(s.slots) {
		s.grow()
	}
	s.put(v)
}

// put places v, which is not zero, in a table with room for it.
func (s *int64Set) put(v int64) {
	mask := uint64(len(s.slots) - 1)
	for i := mix64(uint64(v)) & mask; ; i = (i + 1) & mask {
		switch s.slots[i] {
		case 0:
			s.slots[i] = v
			s.n++
			return
		case v:
			return
		}
	}
}

func (s *int64Set) grow() {
	old := s.slots
	s.slots = make([]int64, max(16, 2*len(old)))
	s.n = 0
	for _, v := range old {
		if v != 0 {
			s.put(v)
		}
	}
}

func (s *int64Set) addAll(vs []int64) {
	for _, v := range vs {
		s.add(v)
	}
}

func (s *int64Set) union(o *int64Set) {
	if o.zero {
		s.zero = true
	}
	for _, v := range o.slots {
		if v != 0 {
			s.add(v)
		}
	}
}

func (s *int64Set) len() int {
	if s.zero {
		return s.n + 1
	}
	return s.n
}

// each calls fn on every value, stopping when fn returns false; it reports
// whether it went through all of them.
func (s *int64Set) each(fn func(int64) bool) bool {
	if s.zero && !fn(0) {
		return false
	}
	for _, v := range s.slots {
		if v != 0 && !fn(v) {
			return false
		}
	}
	return true
}

// hashIndex maps a hash to a non-zero int by open addressing; zero is no
// entry. It is what a DISTINCT accumulator chains its seen values by
// (aggItem.distinctSeen), where a Go map cost a third of the scan.
type hashIndex struct {
	hs []uint64
	vs []int
	n  int
}

// swap makes h map to v, which is not zero, and returns what it mapped to.
func (t *hashIndex) swap(h uint64, v int) int {
	if (t.n+1)*2 > len(t.hs) {
		t.grow()
	}
	mask := uint64(len(t.hs) - 1)
	for i := h & mask; ; i = (i + 1) & mask {
		switch {
		case t.vs[i] == 0:
			t.hs[i], t.vs[i] = h, v
			t.n++
			return 0
		case t.hs[i] == h:
			old := t.vs[i]
			t.vs[i] = v
			return old
		}
	}
}

func (t *hashIndex) get(h uint64) int {
	if len(t.hs) == 0 {
		return 0
	}
	mask := uint64(len(t.hs) - 1)
	for i := h & mask; t.vs[i] != 0; i = (i + 1) & mask {
		if t.hs[i] == h {
			return t.vs[i]
		}
	}
	return 0
}

func (t *hashIndex) grow() {
	hs, vs := t.hs, t.vs
	t.hs, t.vs = make([]uint64, max(16, 2*len(hs))), make([]int, max(16, 2*len(hs)))
	t.n = 0
	for i, v := range vs {
		if v != 0 {
			t.swap(hs[i], v)
		}
	}
}
