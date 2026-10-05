package engine

// White-box hooks for the external engine_test package (the SQL-level tests
// live there because they open a database/sql connection through driver,
// which imports this package). Test-only: this file is never built into the
// shipped package.

// IndexNamesForTest returns the names of every index registered on this
// session, in registration order.
// IndexNamesForTest on a segment-backed session, which is what the engine's own tests
// hold now: the same list, from the same place.
func (n *Session) IndexNamesForTest() []string { return n.DB.IndexNamesForTest() }

func (db *DB) IndexNamesForTest() []string {
	out := make([]string, len(db.indexes))
	for i, idx := range db.indexes {
		out[i] = idx.name
	}
	return out
}

// FTS5TokenizeForTest exposes the fts5 tokenizer with its default (unicode61)
// configuration.
func FTS5TokenizeForTest(text string) []string { return (*fts5Tokenizer)(nil).tokenize(text) }

// CompareValuesForTest exposes the value comparator.
func CompareValuesForTest(a, b Value) int { return compareValues(a, b) }
