//go:build js

package main

import (
	"encoding/binary"
	"math"
	"unsafe"
)

// The query fast path: functions the module exports, called by JS directly
// (musql.js's musqlQueryFast) with the statement and its arguments in linear
// memory. querySync goes through syscall/js, whose every call reads its
// arguments as js.Values -- each with a finalizer the runtime must register --
// which was a measurable share of a point lookup. Nothing here makes a
// js.Value. JS calls in only between events, as it does querySync, so the
// same rule holds: nothing on this path may block.

// inBuf holds the statement then its encoded arguments; kept referenced, so its
// address stays valid (Go's collector does not move objects).
var inBuf []byte

// musqlInBuf returns the address of an input buffer of at least n bytes.
//
//go:wasmexport musql_inbuf
func musqlInBuf(n uint32) uint32 {
	if cap(inBuf) < int(n) {
		inBuf = make([]byte, n)
	}
	inBuf = inBuf[:n]
	if n == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(&inBuf[0])))
}

// musqlQuery runs the statement in inBuf[:sqlLen] with the arguments encoded
// after it, and returns the length of the JSON answer musql_outbuf points at.
// An argument is a tag byte -- 0 NULL, 1 int64, 2 float64, 3 text -- then
// 8 little-endian bytes for a number, or a 4-byte length and UTF-8 for text.
//
//go:wasmexport musql_query
func musqlQuery(sqlLen, argsLen uint32) uint32 {
	in := inBuf
	sql := string(in[:sqlLen])
	args, err := decodeArgs(in[sqlLen : sqlLen+argsLen])
	var rows [][]any
	var cols []string
	if err == nil {
		rows, cols, err = queryRows(sql, args...)
	}
	jsonBuf = appendQueryJSON(jsonBuf[:0], rows, cols, err)
	return uint32(len(jsonBuf))
}

// musqlOutBuf returns the address of the last answer musql_query wrote.
//
//go:wasmexport musql_outbuf
func musqlOutBuf() uint32 {
	if len(jsonBuf) == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(&jsonBuf[0])))
}

type argErr string

func (e argErr) Error() string { return string(e) }

func decodeArgs(b []byte) ([]any, error) {
	var out []any
	for len(b) > 0 {
		tag := b[0]
		b = b[1:]
		switch tag {
		case 0:
			out = append(out, nil)
		case 1, 2:
			if len(b) < 8 {
				return nil, argErr("musql: truncated argument")
			}
			u := binary.LittleEndian.Uint64(b)
			b = b[8:]
			if tag == 1 {
				out = append(out, int64(u))
			} else {
				out = append(out, math.Float64frombits(u))
			}
		case 3:
			if len(b) < 4 {
				return nil, argErr("musql: truncated argument")
			}
			n := binary.LittleEndian.Uint32(b)
			b = b[4:]
			if uint32(len(b)) < n {
				return nil, argErr("musql: truncated argument")
			}
			out = append(out, string(b[:n]))
			b = b[n:]
		default:
			return nil, argErr("musql: unknown argument tag")
		}
	}
	return out, nil
}
