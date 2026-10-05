package compat

// Gate for tclCommentedOut: a TCL comment line is not a statement the source
// ever ran, so the miner must not replay it -- and the rule must stay narrow
// enough that a '#' which is NOT opening a comment still mines normally.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestMinerR25CommentedOutRule(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		mined []string
	}{{
		// The plain form, as descidx1.test spells it.
		name:  "commented db eval is not mined",
		src:   "#db eval {\n  PRAGMA legacy_file_format=OFF\n}\n",
		mined: nil,
	}, {
		// walcrash.test comments out a whole enclosing construct.
		name:  "commented enclosing construct is not mined",
		src:   "#   do_test walcrash-3.4 {\n#     execsql { SELECT 1 }\n#   }\n",
		mined: nil,
	}, {
		name:  "leading whitespace before the # still counts",
		src:   "    \t# execsql { SELECT 1 }\n",
		mined: nil,
	}, {
		// The narrowness that matters: a '#' that is part of an ENCLOSING TCL
		// word, not a comment opener, must not suppress a live statement.
		name:  "a mid-line # does not suppress a live statement",
		src:   "puts \"#\" ; execsql { SELECT 1 }\n",
		mined: []string{"SELECT 1"},
	}, {
		name:  "a # inside the SQL itself is irrelevant",
		src:   "execsql { SELECT '#' }\n",
		mined: []string{"SELECT '#'"},
	}, {
		// A commented-out block must not swallow the NEXT line's real one --
		// the failure mode the bare-word fix already had to defend against.
		name:  "the following live statement is still mined",
		src:   "# execsql { SELECT 1 }\nexecsql { SELECT 2 }\n",
		mined: []string{"SELECT 2"},
	}, {
		name:  "an ordinary statement is unaffected",
		src:   "execsql { SELECT 3 }\n",
		mined: []string{"SELECT 3"},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractTCLSQL(c.src)
			if len(got) != len(c.mined) {
				t.Fatalf("mined %d statements %q, want %d %q", len(got), got, len(c.mined), c.mined)
			}
			for i := range got {
				if strings.TrimSpace(got[i]) != c.mined[i] {
					t.Fatalf("statement %d = %q, want %q", i, got[i], c.mined[i])
				}
			}
		})
	}
}

// TestMinerR25NoCommentedBlocksSurvive is the corpus-wide half: after the rule,
// no mined block may start on a comment line anywhere in the vendored corpus.
// It also reports the measured size of what the rule removes, so a later change
// that quietly widens or narrows it is visible.
func TestMinerR25NoCommentedBlocksSurvive(t *testing.T) {
	ents, err := os.ReadDir(tclCorpusDir)
	if err != nil {
		t.Skipf("no corpus: %v", err)
	}
	var files []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".test") {
			files = append(files, filepath.Join(tclCorpusDir, e.Name()))
		}
	}
	sort.Strings(files)

	suppressedBlocks, suppressedStmts := 0, 0
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, blk := range extractTCLBlocks(src) {
			if tclCommentedOut(src, blk.pos) {
				t.Errorf("%s: mined a block from a comment line at offset %d: %q",
					filepath.Base(path), blk.pos, blk.stmts[0])
			}
		}
		// Count what the rule removes, by re-mining every keyword match that it
		// suppresses and asking how many statements that block would have had.
		for _, m := range tclKeywordRegex.FindAllStringIndex(src, -1) {
			if !tclCommentedOut(src, m[0]) {
				continue
			}
			block, ok, _ := findNextBraceGroup(src, m[1])
			if !ok || tclDisqualified(block) {
				continue
			}
			n := 0
			for _, stmt := range splitTopLevelStatements(strings.TrimSpace(block)) {
				if stmt = strings.TrimSpace(stmt); stmt != "" && !tclHasBindParams(stmt) {
					n++
				}
			}
			if n > 0 {
				suppressedBlocks++
				suppressedStmts += n
			}
		}
	}
	t.Logf("comment rule suppresses %d blocks / %d statements", suppressedBlocks, suppressedStmts)
	if suppressedBlocks == 0 {
		t.Fatal("the corpus is known to contain commented-out SQL-embedding constructs; " +
			"suppressing none means the rule stopped working")
	}
}
