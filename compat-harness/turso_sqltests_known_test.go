package compat

import (
	"strings"
	"sync/atomic"
)

// tsKnownEntry is a deferred divergence from C SQLite: a test key pattern
// ("<file>::<database>::<test>", '*' matches any run of characters) and the
// dated reason it is not fixed yet. A test matching an entry is reported as
// skipped with that reason instead of failing; an entry that matches no
// diverging test in a whole-corpus run fails the run, so the list cannot go
// stale the way an unmeasured one would.
type tsKnownEntry struct {
	pattern string
	reason  string
	hits    atomic.Int64
	// cBug marks C SQLite 3.53.3 as the one that is wrong (tsKnownCBug):
	// musql's answer, which must be Turso's recorded one, is not a
	// divergence.
	cBug bool
}

// tsKnownDivergences is the list, filled by turso_sqltests_known_data_test.go.
// Every wrong answer on it is here on purpose: the fix is larger than this
// change, or C SQLite itself is the odd one out.
var tsKnownDivergences = []*tsKnownEntry{}

// tsKnownCBug registers patterns where C SQLite answers wrong and musql
// answers right: a test matching one passes only while musql satisfies
// Turso's expect block, and is counted as "c-bug", not as a divergence.
func tsKnownCBug(reason string, patterns ...string) {
	for _, p := range patterns {
		tsKnownDivergences = append(tsKnownDivergences, &tsKnownEntry{pattern: p, reason: reason, cBug: true})
	}
}

// tsKnownGroup registers patterns that share one dated reason.
func tsKnownGroup(reason string, patterns ...string) {
	for _, p := range patterns {
		tsKnownDivergences = append(tsKnownDivergences, &tsKnownEntry{pattern: p, reason: reason})
	}
}

func tsGlobMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// tsKnownFor returns the entry naming key, counting the hit.
func tsKnownFor(key string) *tsKnownEntry {
	for _, e := range tsKnownDivergences {
		if tsGlobMatch(e.pattern, key) {
			e.hits.Add(1)
			return e
		}
	}
	return nil
}
