package libm

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoUnguardedProducts keeps every floating-point product that can reach
// an addition or a subtraction converted with float64(). The Go compiler may
// fuse x*y+z into one FMA wherever it likes, and on arm64 it does; an
// error-free transformation or an exact subtraction is no longer exact once
// one of its operands is a product it fused, and glibc's plain build and
// CORE-MATH's analysis are both of unfused arithmetic. madd and math.FMA are
// the only fused operations, and only their multiplicands may be raw.
//
// A product counts where it is an operand of + or - (or += and -=), the
// addend of madd or math.FMA, an argument of a function of this package, or
// a result; a variable counts where a raw product is ever assigned to it.
func TestNoUnguardedProducts(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := conf.Check("libm", fset, files, info)
	if err != nil {
		t.Fatal(err)
	}
	l := &fusionLint{t: t, fset: fset, info: info, pkg: pkg}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				l.check(fd)
			}
		}
	}
}

type fusionLint struct {
	t    *testing.T
	fset *token.FileSet
	info *types.Info
	pkg  *types.Package
	prod map[types.Object]bool
}

func (l *fusionLint) isFloat(e ast.Expr) bool {
	tv, ok := l.info.Types[e]
	if !ok {
		return false
	}
	b, ok := tv.Type.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsFloat != 0
}

func (l *fusionLint) obj(id *ast.Ident) types.Object {
	if o := l.info.Defs[id]; o != nil {
		return o
	}
	return l.info.Uses[id]
}

// raw is whether e, through parentheses and negations, is a floating-point
// multiplication not converted with float64(), or a variable such a product
// is assigned to.
func (l *fusionLint) raw(e ast.Expr) bool {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.UnaryExpr:
			if x.Op != token.SUB {
				return false
			}
			e = x.X
		case *ast.BinaryExpr:
			return x.Op == token.MUL && l.isFloat(x)
		case *ast.Ident:
			return l.prod[l.obj(x)]
		default:
			return false
		}
	}
}

func (l *fusionLint) check(fd *ast.FuncDecl) {
	l.prod = map[types.Object]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if s.Tok == token.MUL_ASSIGN && l.isFloat(s.Lhs[0]) {
				if id, ok := s.Lhs[0].(*ast.Ident); ok {
					l.prod[l.obj(id)] = true
				}
			}
			if len(s.Lhs) == len(s.Rhs) {
				for i, r := range s.Rhs {
					if id, ok := s.Lhs[i].(*ast.Ident); ok && l.raw(r) {
						l.prod[l.obj(id)] = true
					}
				}
			}
		case *ast.ValueSpec:
			for i, v := range s.Values {
				if i < len(s.Names) && l.raw(v) {
					l.prod[l.obj(s.Names[i])] = true
				}
			}
		}
		return true
	})
	flag := func(e ast.Expr, why string) {
		if l.raw(e) {
			l.t.Errorf("%s: %s: an unguarded product %s", l.fset.Position(e.Pos()), fd.Name.Name, why)
		}
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.BinaryExpr:
			if (e.Op == token.ADD || e.Op == token.SUB) && l.isFloat(e) {
				flag(e.X, "is added")
				flag(e.Y, "is added")
			}
		case *ast.AssignStmt:
			if (e.Tok == token.ADD_ASSIGN || e.Tok == token.SUB_ASSIGN) && l.isFloat(e.Lhs[0]) {
				flag(e.Lhs[0], "is added to")
				flag(e.Rhs[0], "is added")
			}
		case *ast.CallExpr:
			switch fn := l.callee(e); {
			case fn == "math.FMA" || fn == "madd":
				flag(e.Args[2], "is an FMA's addend")
			case fn != "" && !strings.Contains(fn, ".") && !bitFuncs[fn]:
				for _, a := range e.Args {
					flag(a, "is passed to "+fn)
				}
			}
		case *ast.ReturnStmt:
			if fd.Name.IsExported() {
				// What an exported function returns is the engine's to
				// store, not to add.
				break
			}
			for _, r := range e.Results {
				flag(r, "is returned")
			}
			if len(e.Results) == 0 && fd.Type.Results != nil {
				for _, fl := range fd.Type.Results.List {
					for _, nm := range fl.Names {
						if l.prod[l.obj(nm)] {
							l.t.Errorf("%s: %s: an unguarded product %s is returned", l.fset.Position(e.Pos()), fd.Name.Name, nm.Name)
						}
					}
				}
			}
		}
		return true
	})
}

// bitFuncs only reinterpret their argument's bits, which nothing can fuse.
var bitFuncs = map[string]bool{"asuint64": true, "top12": true, "top16": true, "highWord": true}

// callee is "madd" or the name of another function of this package, "pkg.F"
// for another package's, and "" for anything else (a conversion, a builtin,
// a function value).
func (l *fusionLint) callee(c *ast.CallExpr) string {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		if fn, ok := l.obj(f).(*types.Func); ok && fn.Pkg() == l.pkg {
			return fn.Name()
		}
	case *ast.SelectorExpr:
		if fn, ok := l.obj(f.Sel).(*types.Func); ok && fn.Pkg() != nil {
			if fn.Pkg() == l.pkg {
				return fn.Name()
			}
			return fn.Pkg().Name() + "." + fn.Name()
		}
	}
	return ""
}
