//go:build ((amd64 || arm64) && (unix || windows)) || js

package jit

import (
	"math"
	"regexp"
	"strconv"
	"testing"
)

// TestVMRealText: under VFText a TEXT operand spelled [-]digits.digits, its
// digits under 2^53 and at most 22 after the point, reads as the REAL it
// spells; every other TEXT exits, writing nothing.
func TestVMRealText(t *testing.T) {
	if !Available || !VMReal {
		t.Skip("no REAL arms here")
	}
	plain := regexp.MustCompile(`^-?[0-9]*\.[0-9]*$`)
	spells := func(s string) (float64, bool) {
		u := s
		if u != "" && u[0] == '-' {
			u = u[1:]
		}
		if !plain.MatchString(s) || len(u) < 2 {
			return 0, false
		}
		dot := 0
		for dot < len(u) && u[dot] != '.' {
			dot++
		}
		if len(u)-dot-1 > 22 {
			return 0, false
		}
		d, err := strconv.ParseUint(u[:dot]+u[dot+1:], 10, 64)
		if err != nil || d >= 1<<53 {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	texts := []string{"-5.00", "12.25", "-0.00", "1.", ".5", "-.5", "0.1", "1234567.89",
		"9007199254740991.", "9007199254740992.", "1.0000000000000000000001", "1.00000000000000000000001",
		"12", "-", ".", "-.", "", " 1.5", "1.5 ", "1.5e2", "1..5", "+1.5", "1,5", "١.٥", "00012.3400"}
	others := []tvalue{treal(1.25), tint(3), tint(-1 << 62), {Typ: tvNull}, {Typ: tvText, S: []byte("2.5")}}
	ops := []struct {
		op VOp
		fn func(b, a float64) float64
	}{
		{VFAdd, func(b, a float64) float64 { return b + a }},
		{VFSub, func(b, a float64) float64 { return b - a }},
		{VFMul, func(b, a float64) float64 { return b * a }},
		{VFDiv, func(b, a float64) float64 { return b / a }},
	}
	num := func(v tvalue) (float64, bool) {
		switch v.Typ {
		case tvReal:
			return v.F, true
		case tvInt:
			return float64(v.I), true
		case tvText:
			return spells(string(v.S))
		}
		return 0, false
	}
	for _, o := range ops {
		for _, s := range texts {
			for _, other := range others {
				txt := tvalue{Typ: tvText, S: []byte(s)}
				for _, pair := range [][2]tvalue{{txt, other}, {other, txt}} {
					b, av := pair[0], pair[1]
					regs := []tvalue{av, b, tint(77)}
					args := &ProgArgs{}
					runVM(t, []VInsn{{Op: o.op, A: 0, B: 1, C: 2, Imm: VFText}, {Op: VExit}}, nil, regs, args, 0)
					bf, bok := num(b)
					af, aok := num(av)
					exit := !bok || !aok || (o.op == VFDiv && af == 0)
					var want float64
					if !exit {
						want = o.fn(bf, af)
						exit = want != want
					}
					if exit {
						if args.PC != 0 || !same(regs[2], tint(77)) {
							t.Errorf("op %d (%q, %q): pc %d r[2] %+v, want an exit writing nothing", o.op, b.S, av.S, args.PC, regs[2])
						}
						continue
					}
					if args.PC != 1 || regs[2].Typ != tvReal || math.Float64bits(regs[2].F) != math.Float64bits(want) || regs[2].I != 0 {
						t.Errorf("op %d (%+v, %+v): pc %d r[2] %+v, want %v", o.op, b, av, args.PC, regs[2], want)
					}
				}
			}
		}
	}
	// Without VFText a TEXT exits as before.
	regs := []tvalue{treal(1), {Typ: tvText, S: []byte("2.5")}, tint(77)}
	args := &ProgArgs{}
	runVM(t, []VInsn{{Op: VFAdd, A: 0, B: 1, C: 2}, {Op: VExit}}, nil, regs, args, 0)
	if args.PC != 0 || !same(regs[2], tint(77)) {
		t.Errorf("no VFText: pc %d r[2] %+v, want an exit", args.PC, regs[2])
	}
}
