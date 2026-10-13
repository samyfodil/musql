package engine

// ctxHasSameTable reports whether tables already holds a scope for the same
// table as s: the same name and the same column names in the same order.
func ctxHasSameTable(tables []tableScope, s *tableScope) bool {
	for i := range tables {
		t := &tables[i]
		if !equalFoldName(t.name, s.name) || len(t.cols) != len(s.cols) {
			continue
		}
		same := true
		for j := range t.cols {
			if t.cols[j].Name != s.cols[j].Name {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}
