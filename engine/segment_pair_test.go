package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// segPair tests the same SQL two ways: fast paths enabled and with peepholes disabled.
type segPair struct {
	t    *testing.T
	path string
}

func newSegPair(t *testing.T, stmts ...string) *segPair {
	t.Helper()
	p := &segPair{t: t, path: filepath.Join(t.TempDir(), "seg.musq")}
	buildDB(t, p.path, stmts...)
	execDB(t, p.path, `VACUUM`)
	return p
}

func (p *segPair) delta(stmts ...string) {
	p.t.Helper()
	execDB(p.t, p.path, stmts...)
}

func (p *segPair) ask(q string, args []Value) ([]string, [][]Value, error) {
	p.t.Helper()
	n, err := OpenWrite(p.path)
	if err != nil {
		p.t.Fatal(err)
	}
	defer n.Discard()
	return n.Query(q, args)
}

// fast answers q as production does.
func (p *segPair) fast(q string, args ...Value) ([]string, [][]Value, error) {
	p.t.Helper()
	return p.ask(q, args)
}

// plain answers q with no segment fast path.
func (p *segPair) plain(q string, args ...Value) ([]string, [][]Value, error) {
	p.t.Helper()
	segPeepholesOffForTest = true
	defer func() { segPeepholesOffForTest = false }()
	return p.ask(q, args)
}

// rows is ask that fails the test on an error.
func (p *segPair) mustFast(q string, args ...Value) [][]Value {
	p.t.Helper()
	_, rows, err := p.fast(q, args...)
	if err != nil {
		p.t.Fatalf("%s: %v", q, err)
	}
	return rows
}

func (p *segPair) mustPlain(q string, args ...Value) [][]Value {
	p.t.Helper()
	_, rows, err := p.plain(q, args...)
	if err != nil {
		p.t.Fatalf("%s (plain): %v", q, err)
	}
	return rows
}

// typedRows renders rows one string per row, each value with its TYPE, so an
// integer 1 and the text "1" differ.
func typedRows(rows [][]Value) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		s := ""
		for _, v := range r {
			s += fmt.Sprintf("%d:", v.Typ)
			switch v.Typ {
			case Int:
				s += fmt.Sprintf("%d|", v.I)
			case Float:
				s += fmt.Sprintf("%v|", v.F)
			case Text, Blob:
				s += fmt.Sprintf("%q|", v.S)
			default:
				s += "null|"
			}
		}
		out = append(out, s)
	}
	return out
}
