package compat

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestTursoSqltestSplit(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"SELECT 1; SELECT 2;", []string{"SELECT 1;", "SELECT 2;"}},
		{"SELECT ';' ; -- c;\nSELECT [a;b], \"x;y\"", []string{"SELECT ';' ;", "-- c;\nSELECT [a;b], \"x;y\""}},
		{"CREATE TRIGGER r AFTER INSERT ON t BEGIN INSERT INTO u VALUES(1); SELECT CASE WHEN 1 THEN 2 END; END; SELECT 3",
			[]string{"CREATE TRIGGER r AFTER INSERT ON t BEGIN INSERT INTO u VALUES(1); SELECT CASE WHEN 1 THEN 2 END; END;", "SELECT 3"}},
		{"CREATE TEMP TRIGGER r BEFORE DELETE ON t BEGIN SELECT 1; END;BEGIN;COMMIT;", []string{"CREATE TEMP TRIGGER r BEFORE DELETE ON t BEGIN SELECT 1; END;", "BEGIN;", "COMMIT;"}},
		{";; /* only; a comment */ ;", nil},
	} {
		if got := tsSplitStatements(c.in); !slices.Equal(got, c.want) {
			t.Errorf("%q\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}

func TestTursoSqltestParse(t *testing.T) {
	f, err := parseTursoSqltest(`# comment
@database :memory:@database :temp:
@skip-file-if mvcc "x"

setup s {
    CREATE TABLE t(a);
}

@setup s
@cross-check-integrity
@skip-if sqlite "y"
test one {
    SELECT '\{' || '\}' || 1;
}
expect {
    \{1
}
expect @js {
    other
}

@var m { A | b c }
matrix mx {
    SELECT $m;
}

test err {
    SELEC;
}
expect error {
    syntax
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.dbs) != 2 || f.dbs[1].kind != tsDBTemp || len(f.skips) != 1 || f.setups["s"] != "    CREATE TABLE t(a);" {
		t.Fatalf("header: %+v", f)
	}
	one := f.cases[0]
	if one.sql != "SELECT '{' || '}' || 1;" || !one.crossCheck || one.skips[0].cond != "sqlite" || len(one.expect.lines) != 1 || one.expect.lines[0] != "{1" {
		t.Fatalf("test one: %+v %+v", one, one.expect)
	}
	names, sqls := tsExpandMatrix(f.cases[1])
	if !slices.Equal(names, []string{"mx[a]", "mx[b-c]"}) || sqls[1] != "SELECT b c;" {
		t.Fatalf("matrix: %q %q", names, sqls)
	}
	if f.cases[2].expect.kind != "error" || f.cases[2].expect.text != "syntax" {
		t.Fatalf("error expectation: %+v", f.cases[2].expect)
	}
	for _, bad := range []string{"@database :memory:\n@frobnicate\n", "@database :memory:\n@requires warp \"x\"\ntest a {\nSELECT 1;\n}\nexpect {\n1\n}\n", "test a {\nSELECT 1;\n}\nexpect {\n1\n}\n"} {
		if _, err := parseTursoSqltest(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// TestTursoSqltestCorpusParses reports how much of the vendored corpus the
// parser reads; a file it cannot read is skipped by the gate, so this is where
// that number is kept honest.
func TestTursoSqltestCorpusParses(t *testing.T) {
	var files, tests, matrices, snaps, expanded int
	err := filepath.WalkDir(filepath.Join("testdata", "turso-sqltests", "sqlite-sqltests"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".sqltest") {
			return err
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files++
		f, perr := parseTursoSqltest(string(src))
		if perr != nil {
			t.Errorf("%s: %v", p, perr)
			return nil
		}
		for _, c := range f.cases {
			switch c.kind {
			case "test":
				tests++
			case "snapshot":
				snaps++
			case "matrix":
				matrices++
				n, _ := tsExpandMatrix(c)
				expanded += len(n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d files: %d tests, %d snapshots, %d matrices expanding to %d cases", files, tests, snaps, matrices, expanded)
}
