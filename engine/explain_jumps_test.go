package engine

import (
	"go/ast"
	goparser "go/parser"
	"go/printer"
	gotoken "go/token"
	"strings"
	"testing"
)

// TestExplainJumpTablesMatchTheVDBE: the opcodes a compound's listing moves
// the jump of (explainRelocate) are exactly the ones whose case in the VDBE's
// run loop sets pc, read from vdbe.go itself, so a new jumping opcode cannot
// be listed with its arm-local target.
func TestExplainJumpTablesMatchTheVDBE(t *testing.T) {
	fs := gotoken.NewFileSet()
	f, err := goparser.ParseFile(fs, "vdbe.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	byName := opCodesByIdent(t, fs)
	jumps := map[OpCode]map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		var ops []OpCode
		for _, e := range cc.List {
			if id, ok := e.(*ast.Ident); ok {
				if op, known := byName[id.Name]; known {
					ops = append(ops, op)
				}
			}
		}
		for _, st := range cc.Body {
			ast.Inspect(st, func(m ast.Node) bool {
				as, ok := m.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for i, l := range as.Lhs {
					if id, ok := l.(*ast.Ident); ok && id.Name == "pc" && i < len(as.Rhs) {
						var b strings.Builder
						printer.Fprint(&b, fs, as.Rhs[i])
						for _, op := range ops {
							if jumps[op] == nil {
								jumps[op] = map[string]bool{}
							}
							jumps[op][b.String()] = true
						}
					}
				}
				return true
			})
		}
		return true
	})
	for _, op := range byName {
		to := jumps[op]
		p2 := explainJumpsP2(Instruction{Op: op})
		p3 := explainJumpsP3(op)
		computed := explainJumpsComputed(op)
		switch {
		case op == OpReturn:
			// Back to the address its OpGosub stored as the program ran.
		case len(to) == 1 && to["op.P2"]:
			if !p2 || p3 || computed {
				t.Errorf("%s jumps to P2 in the VDBE; the listing's tables say p2=%v p3=%v computed=%v", op, p2, p3, computed)
			}
		case len(to) == 1 && to["op.P3"]:
			if p2 || !p3 || computed {
				t.Errorf("%s jumps to P3 in the VDBE; the listing's tables say p2=%v p3=%v computed=%v", op, p2, p3, computed)
			}
		case len(to) > 0 && to["target"] && explainJumpsP2(Instruction{Op: op, P5: p5StoreP2}) != p2:
			// A comparison jumps to P2 unless it stores its result there.
			if p3 || computed {
				t.Errorf("%s jumps to P2 in the VDBE unless it stores; the listing's tables say p3=%v computed=%v", op, p3, computed)
			}
		case len(to) > 0:
			if p2 || p3 || !computed {
				t.Errorf("%s jumps to %v in the VDBE; the listing's tables say p2=%v p3=%v computed=%v", op, to, p2, p3, computed)
			}
		default:
			if p2 || p3 || computed {
				t.Errorf("%s never jumps in the VDBE; the listing's tables say p2=%v p3=%v computed=%v", op, p2, p3, computed)
			}
		}
	}
}

// opCodesByIdent is every opcode by its Go name, read off vdbe_op.go's iota
// block.
func opCodesByIdent(t *testing.T, fs *gotoken.FileSet) map[string]OpCode {
	f, err := goparser.ParseFile(fs, "vdbe_op.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]OpCode{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != gotoken.CONST || len(g.Specs) == 0 {
			continue
		}
		first := g.Specs[0].(*ast.ValueSpec)
		if len(first.Names) != 1 || first.Names[0].Name != "OpInit" {
			continue
		}
		for i, sp := range g.Specs {
			vs := sp.(*ast.ValueSpec)
			if len(vs.Names) != 1 || (i > 0 && len(vs.Values) > 0) {
				t.Fatalf("vdbe_op.go: %v is not one more iota step", vs.Names)
			}
			out[vs.Names[0].Name] = OpCode(i)
		}
	}
	if out["OpGoto"] != OpGoto || out["OpRightJoinSweepNext"] != OpRightJoinSweepNext {
		t.Fatal("vdbe_op.go's opcode block did not read as the compiled one")
	}
	return out
}
