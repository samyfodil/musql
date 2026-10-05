package vdbecc

import (
	"bytes"
	"fmt"
	"slices"
)

// LinkModules joins translation units the way a static linker does: each
// unit's static symbols and named types get a unit-unique suffix, a global's
// tentative (all-zero) definition gives way to a real one, and a name some unit
// defines stops being external.
func LinkModules(mods []*Module) (*Module, error) {
	out := &Module{Types: map[string]Type{}}
	vars := map[string]*Var{}
	funcs := map[string]bool{}
	for unit, m := range mods {
		local := map[string]string{}
		for _, f := range m.Funcs {
			if f.Local {
				local[f.Name] = fmt.Sprintf("%s.u%d", f.Name, unit)
			}
		}
		for _, v := range m.Vars {
			if v.Local {
				local[v.Name] = fmt.Sprintf("%s.u%d", v.Name, unit)
			}
		}
		r := renamer{local: local, typeSuffix: fmt.Sprintf(".t%d", unit)}
		for name, t := range m.Types {
			out.Types[name+r.typeSuffix] = r.typ(t)
		}
		for _, f := range m.Funcs {
			r.fn(f)
			if funcs[f.Name] {
				return nil, fmt.Errorf("@%s is defined twice", f.Name)
			}
			funcs[f.Name] = true
			out.Funcs = append(out.Funcs, f)
		}
		for _, v := range m.Vars {
			v.Name = r.name(v.Name)
			for i := range v.Fixups {
				r.value(&v.Fixups[i].Ref)
			}
			prev, seen := vars[v.Name]
			switch {
			case !seen:
				vars[v.Name] = v
				out.Vars = append(out.Vars, v)
			case tentative(prev) && (!tentative(v) || len(v.Init) > len(prev.Init)):
				*prev = *v
			case tentative(v):
			default:
				return nil, fmt.Errorf("@%s is initialized twice", v.Name)
			}
		}
		out.External = append(out.External, m.External...)
	}
	slices.Sort(out.External)
	out.External = slices.DeleteFunc(slices.Compact(out.External), func(n string) bool { return funcs[n] })
	return out, nil
}

func tentative(v *Var) bool {
	return len(v.Fixups) == 0 && bytes.Count(v.Init, []byte{0}) == len(v.Init)
}

// renamer rewrites one unit's symbol and type names in place.
type renamer struct {
	local      map[string]string
	typeSuffix string
}

func (r renamer) name(n string) string {
	if to, ok := r.local[n]; ok {
		return to
	}
	return n
}

func (r renamer) typ(t Type) Type {
	switch t := t.(type) {
	case NamedType:
		return NamedType{t.Name + r.typeSuffix}
	case ArrayType:
		return ArrayType{t.Len, r.typ(t.Elem)}
	case StructType:
		fs := make([]Type, len(t.Fields))
		for i, f := range t.Fields {
			fs[i] = r.typ(f)
		}
		return StructType{fs, t.Packed}
	}
	return t
}

func (r renamer) value(v *Value) {
	switch x := (*v).(type) {
	case GlobalRef:
		*v = GlobalRef{r.name(x.Name)}
	case GlobalPlus:
		*v = GlobalPlus{r.name(x.Name), r.typ(x.Base), x.Indices}
	}
}

func (r renamer) fn(f *Func) {
	f.Name = r.name(f.Name)
	f.Result = r.typ(f.Result)
	for i := range f.Params {
		f.Params[i].Type = r.typ(f.Params[i].Type)
	}
	for _, b := range f.Blocks {
		for _, in := range b.Body {
			in.Operands(r.value)
			switch in := in.(type) {
			case *BinInstr:
				in.Type = r.typ(in.Type)
			case *CmpInstr:
				in.Type = r.typ(in.Type)
			case *ConvInstr:
				in.From, in.To = r.typ(in.From), r.typ(in.To)
			case *AllocaInstr:
				in.Type = r.typ(in.Type)
			case *LoadInstr:
				in.Type = r.typ(in.Type)
			case *StoreInstr:
				in.Type = r.typ(in.Type)
			case *GEPInstr:
				in.Base = r.typ(in.Base)
			case *VaArgInstr:
				in.Type = r.typ(in.Type)
			case *CallInstr:
				in.Callee = r.name(in.Callee)
			}
		}
		b.End.Operands(r.value)
	}
}
