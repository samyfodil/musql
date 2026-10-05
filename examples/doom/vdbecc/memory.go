package vdbecc

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/samyfodil/musql/engine"
)

// Memory is a compiled program's C address space: globals from GlobalsBase, the
// stack at the top growing down. The program reaches it only through the
// functions below, which its OpFunction instructions hold directly (P4), so an
// access is one VDBE instruction and one Go call -- where Turso's vdbecc keeps
// the bytes in a table row and decodes them with BlobRead and SQL functions.
// An address outside it is an error, which halts the program.
type Memory struct {
	B []byte

	fns memFuncs
}

type memFuncs struct {
	load     [9][2]*engine.ScalarFunction // [width][signed]
	loadF32  *engine.ScalarFunction
	loadF64  *engine.ScalarFunction
	store    [9]*engine.ScalarFunction // [width]
	storeF32 *engine.ScalarFunction
	storeF64 *engine.ScalarFunction
	copyN    [9]*engine.ScalarFunction // [width]: (dst, src)
	memmove  *engine.ScalarFunction    // (dst, src, n)
	memset   *engine.ScalarFunction    // (dst, byte, n)
	readBlob *engine.ScalarFunction    // (addr, n): the bytes, as a BLOB
}

func intArg(v engine.Value) int64 {
	switch v.Typ {
	case engine.Int:
		return v.I
	case engine.Float:
		return int64(v.F)
	}
	return 0 // NULL: a register never written yet (see the reentrancy spill)
}

func floatArg(v engine.Value) float64 {
	switch v.Typ {
	case engine.Float:
		return v.F
	case engine.Int:
		return float64(v.I)
	}
	return 0
}

func (m *Memory) span(addr, n int64) ([]byte, error) {
	if addr < 0 || n < 0 || addr > int64(len(m.B))-n {
		return nil, fmt.Errorf("vdbecc: memory access [%d, %d) outside the %d-byte address space", addr, addr+n, len(m.B))
	}
	return m.B[addr : addr+n], nil
}

func newMemory(b []byte) *Memory {
	m := &Memory{B: b}
	f := &m.fns
	fn := func(name string, narg int, body func([]engine.Value) (engine.Value, error)) *engine.ScalarFunction {
		return &engine.ScalarFunction{Name: name, NArg: narg, Fn: body}
	}
	iv := func(n int64) engine.Value { return engine.Value{Typ: engine.Int, I: n} }
	for _, w := range []int{1, 2, 4, 8} {
		for signed := range 2 {
			f.load[w][signed] = fn(fmt.Sprintf("load%d", w), 1, func(a []engine.Value) (engine.Value, error) {
				p, err := m.span(intArg(a[0]), int64(w))
				if err != nil {
					return engine.Value{}, err
				}
				switch w {
				case 1:
					if signed == 1 {
						return iv(int64(int8(p[0]))), nil
					}
					return iv(int64(p[0])), nil
				case 2:
					v := binary.LittleEndian.Uint16(p)
					if signed == 1 {
						return iv(int64(int16(v))), nil
					}
					return iv(int64(v)), nil
				case 4:
					v := binary.LittleEndian.Uint32(p)
					if signed == 1 {
						return iv(int64(int32(v))), nil
					}
					return iv(int64(v)), nil
				}
				return iv(int64(binary.LittleEndian.Uint64(p))), nil
			})
		}
		f.store[w] = fn(fmt.Sprintf("store%d", w), 2, func(a []engine.Value) (engine.Value, error) {
			p, err := m.span(intArg(a[0]), int64(w))
			if err != nil {
				return engine.Value{}, err
			}
			v := uint64(intArg(a[1]))
			switch w {
			case 1:
				p[0] = byte(v)
			case 2:
				binary.LittleEndian.PutUint16(p, uint16(v))
			case 4:
				binary.LittleEndian.PutUint32(p, uint32(v))
			default:
				binary.LittleEndian.PutUint64(p, v)
			}
			return engine.Value{}, nil
		})
		f.copyN[w] = fn(fmt.Sprintf("copy%d", w), 2, func(a []engine.Value) (engine.Value, error) {
			d, err := m.span(intArg(a[0]), int64(w))
			if err != nil {
				return engine.Value{}, err
			}
			s, err := m.span(intArg(a[1]), int64(w))
			if err != nil {
				return engine.Value{}, err
			}
			copy(d, s)
			return engine.Value{}, nil
		})
	}
	f.loadF32 = fn("loadf32", 1, func(a []engine.Value) (engine.Value, error) {
		p, err := m.span(intArg(a[0]), 4)
		if err != nil {
			return engine.Value{}, err
		}
		return engine.Value{Typ: engine.Float, F: float64(math.Float32frombits(binary.LittleEndian.Uint32(p)))}, nil
	})
	f.loadF64 = fn("loadf64", 1, func(a []engine.Value) (engine.Value, error) {
		p, err := m.span(intArg(a[0]), 8)
		if err != nil {
			return engine.Value{}, err
		}
		return engine.Value{Typ: engine.Float, F: math.Float64frombits(binary.LittleEndian.Uint64(p))}, nil
	})
	f.storeF32 = fn("storef32", 2, func(a []engine.Value) (engine.Value, error) {
		p, err := m.span(intArg(a[0]), 4)
		if err != nil {
			return engine.Value{}, err
		}
		binary.LittleEndian.PutUint32(p, math.Float32bits(float32(floatArg(a[1]))))
		return engine.Value{}, nil
	})
	f.storeF64 = fn("storef64", 2, func(a []engine.Value) (engine.Value, error) {
		p, err := m.span(intArg(a[0]), 8)
		if err != nil {
			return engine.Value{}, err
		}
		binary.LittleEndian.PutUint64(p, math.Float64bits(floatArg(a[1])))
		return engine.Value{}, nil
	})
	f.memmove = fn("memmove", 3, func(a []engine.Value) (engine.Value, error) {
		n := intArg(a[2])
		d, err := m.span(intArg(a[0]), n)
		if err != nil {
			return engine.Value{}, err
		}
		s, err := m.span(intArg(a[1]), n)
		if err != nil {
			return engine.Value{}, err
		}
		copy(d, s) // copy is memmove: overlap is fine
		return engine.Value{}, nil
	})
	f.memset = fn("memset", 3, func(a []engine.Value) (engine.Value, error) {
		d, err := m.span(intArg(a[0]), intArg(a[2]))
		if err != nil {
			return engine.Value{}, err
		}
		b := byte(intArg(a[1]))
		for i := range d {
			d[i] = b
		}
		return engine.Value{}, nil
	})
	f.readBlob = fn("readblob", 2, func(a []engine.Value) (engine.Value, error) {
		p, err := m.span(intArg(a[0]), intArg(a[1]))
		if err != nil {
			return engine.Value{}, err
		}
		return engine.Value{Typ: engine.Blob, S: append([]byte(nil), p...)}, nil
	})
	return m
}
