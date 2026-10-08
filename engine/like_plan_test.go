package engine

import "testing"

// TestLikePlanMatchesLikeMatch: the byte matcher must agree with likeMatch on
// every pattern it accepts, for every text, under both case rules.
func TestLikePlanMatchesLikeMatch(t *testing.T) {
	patterns := []string{"", "%", "%%", "a", "A", "abc", "a%", "%a", "%a%", "a%a", "a%b%c", "%ab%ab%", "%-7-payload",
		"x1%", "%12%3%", "ab%%cd", "%ABC", "abc\x00zz", "%\x00", "a_c", "é%", "%Z%", "%a%a%a%", "aa%aa"}
	texts := []string{"", "a", "A", "abc", "ABC", "aBc", "abcabc", "ab", "aab", "baa", "a-7-payload", "row-3-7-payload",
		"x10", "X1", "123", "1x2y3", "abxxcd", "ab cd", "é", "éa", "a\x00bc", "abc\x00", "\xff\xfea", "a\xffa", "aaaaa", "zZz", "abab", "aba"}
	checked := 0
	for _, p := range patterns {
		lp := newLikePlan([]byte(p))
		if !lp.ok {
			continue
		}
		for _, x := range texts {
			for _, cs := range []bool{false, true} {
				got, want := lp.match([]byte(x), cs), likeMatch(p, x, cs)
				if got != want {
					t.Errorf("%q LIKE %q (case-sensitive %v): plan %v, likeMatch %v", x, p, cs, got, want)
				}
				checked++
			}
		}
	}
	if checked < 500 {
		t.Fatalf("only %d comparisons: the plan rejected too many patterns to test anything", checked)
	}
}
