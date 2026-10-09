//go:build js && wasm

// Command wasm is the WebAssembly module the npm package ships (built into
// npm/musql.wasm by npm/build.sh): musql over an in-memory filesystem, called from JavaScript through
// the module's own exports. JavaScript writes a statement and its arguments
// into a buffer in linear memory and calls musql_exec or musql_query; the
// answer comes back as JSON in another. No syscall/js on that path.
//
// The exports, all over the buffers musql_inbuf and musql_outbuf:
//
//	musql_open(pathLen)              -> handle > 0, or -(n+1): an n-byte {"error"} in the outbuf
//	musql_close(handle)
//	musql_query(handle, sqlLen, argsLen) -> outLen: {"columns","rows"} or {"error"}
//	musql_exec(handle, sqlLen, argsLen)  -> outLen: {"changes","lastInsertRowid"} or {"error"}
//
// An argument is a tag byte then its value: 0 NULL, 1 int64 (8 bytes LE),
// 2 float64 (8 bytes LE), 3 text and 4 blob (a 4-byte LE length, then the
// bytes). In a result, an integer outside +-2^53 is {"i":"<decimal>"} and a
// blob is {"b":"<base64>"}, so neither loses anything in JavaScript.
package main

import (
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"syscall/js"
	"unicode/utf8"
	"unsafe"

	"github.com/samyfodil/musql/driver"
)

type conn struct {
	db    *sql.DB
	stmts map[string]*sql.Stmt
}

var (
	conns  = map[int32]*conn{}
	nextID int32
	inBuf  []byte
	outBuf []byte
)

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

//go:wasmexport musql_outbuf
func musqlOutBuf() uint32 {
	if len(outBuf) == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(&outBuf[0])))
}

//go:wasmexport musql_open
func musqlOpen(pathLen uint32) int32 {
	path := string(inBuf[:pathLen])
	db, err := sql.Open(driver.DriverName, path)
	if err == nil {
		err = db.Ping()
	}
	if err != nil {
		outBuf = appendError(outBuf[:0], err)
		return -int32(len(outBuf)) - 1
	}
	// One connection: the statements of one handle run in order, as a
	// SQLite connection's do, and see each other's writes.
	db.SetMaxOpenConns(1)
	nextID++
	conns[nextID] = &conn{db: db, stmts: map[string]*sql.Stmt{}}
	return nextID
}

//go:wasmexport musql_close
func musqlClose(h int32) {
	if c := conns[h]; c != nil {
		for _, st := range c.stmts {
			st.Close()
		}
		c.db.Close()
		delete(conns, h)
	}
}

//go:wasmexport musql_query
func musqlQuery(h int32, sqlLen, argsLen uint32) uint32 {
	c, q, args, err := request(h, sqlLen, argsLen)
	if err == nil {
		var rows [][]any
		var cols []string
		if rows, cols, err = c.query(q, args); err == nil {
			outBuf = appendRows(outBuf[:0], cols, rows)
			return uint32(len(outBuf))
		}
	}
	outBuf = appendError(outBuf[:0], err)
	return uint32(len(outBuf))
}

//go:wasmexport musql_exec
func musqlExec(h int32, sqlLen, argsLen uint32) uint32 {
	c, q, args, err := request(h, sqlLen, argsLen)
	if err == nil {
		var res sql.Result
		if res, err = c.db.Exec(q, args...); err == nil {
			n, _ := res.RowsAffected()
			id, _ := res.LastInsertId()
			b := append(outBuf[:0], `{"changes":`...)
			b = appendInt(b, n)
			b = append(b, `,"lastInsertRowid":`...)
			b = appendInt(b, id)
			outBuf = append(b, '}')
			return uint32(len(outBuf))
		}
	}
	outBuf = appendError(outBuf[:0], err)
	return uint32(len(outBuf))
}

var errClosed = errors.New("musql: the database is closed")

func request(h int32, sqlLen, argsLen uint32) (*conn, string, []any, error) {
	c := conns[h]
	if c == nil {
		return nil, "", nil, errClosed
	}
	if uint64(sqlLen)+uint64(argsLen) > uint64(len(inBuf)) {
		return nil, "", nil, errors.New("musql: request past its buffer")
	}
	q := string(inBuf[:sqlLen])
	args, err := decodeArgs(inBuf[sqlLen : sqlLen+argsLen])
	return c, q, args, err
}

// query runs q through its cached prepared statement and reads every row.
func (c *conn) query(q string, args []any) ([][]any, []string, error) {
	st := c.stmts[q]
	if st == nil {
		var err error
		if st, err = c.db.Prepare(q); err != nil {
			return nil, nil, err
		}
		c.stmts[q] = st
	}
	rows, err := st.Query(args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		out = append(out, vals)
	}
	return out, cols, rows.Err()
}

var errArg = errors.New("musql: malformed argument buffer")

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
				return nil, errArg
			}
			u := binary.LittleEndian.Uint64(b)
			b = b[8:]
			if tag == 1 {
				out = append(out, int64(u))
			} else {
				out = append(out, math.Float64frombits(u))
			}
		case 3, 4:
			if len(b) < 4 {
				return nil, errArg
			}
			n := binary.LittleEndian.Uint32(b)
			b = b[4:]
			if uint32(len(b)) < n {
				return nil, errArg
			}
			if tag == 3 {
				out = append(out, string(b[:n]))
			} else {
				out = append(out, append([]byte{}, b[:n]...))
			}
			b = b[n:]
		default:
			return nil, errArg
		}
	}
	return out, nil
}

func appendRows(b []byte, cols []string, rows [][]any) []byte {
	b = append(b, `{"columns":[`...)
	for i, c := range cols {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendString(b, c)
	}
	b = append(b, `],"rows":[`...)
	for i, r := range rows {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '[')
		for j, v := range r {
			if j > 0 {
				b = append(b, ',')
			}
			b = appendValue(b, v)
		}
		b = append(b, ']')
	}
	return append(b, "]}"...)
}

func appendError(b []byte, err error) []byte {
	return append(appendString(append(b, `{"error":`...), err.Error()), '}')
}

// appendInt writes an integer JSON numbers carry exactly, and anything past
// +-2^53 as {"i":"<decimal>"} for the package to turn into a BigInt.
func appendInt(b []byte, v int64) []byte {
	if v >= -(1<<53) && v <= 1<<53 {
		return strconv.AppendInt(b, v, 10)
	}
	b = append(b, `{"i":"`...)
	b = strconv.AppendInt(b, v, 10)
	return append(b, `"}`...)
}

func appendValue(b []byte, v any) []byte {
	switch x := v.(type) {
	case int64:
		return appendInt(b, x)
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			// JSON has no Infinity; SQLite has no NaN, and returns Inf for
			// an overflowing REAL.
			if x > 0 {
				return append(b, `{"f":"Infinity"}`...)
			}
			return append(b, `{"f":"-Infinity"}`...)
		}
		return strconv.AppendFloat(b, x, 'g', -1, 64)
	case string:
		return appendString(b, x)
	case []byte:
		b = append(b, `{"b":"`...)
		b = base64.StdEncoding.AppendEncode(b, x)
		return append(b, `"}`...)
	case bool:
		return strconv.AppendBool(b, x)
	case nil:
		return append(b, "null"...)
	}
	return append(b, "null"...)
}

// appendString quotes s by JSON's rules: control characters as \u00XX,
// invalid UTF-8 as U+FFFD.
func appendString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' && c < utf8.RuneSelf {
			b = append(b, c)
			i++
			continue
		}
		if c < utf8.RuneSelf {
			switch c {
			case '"', '\\':
				b = append(b, '\\', c)
			case '\n':
				b = append(b, '\\', 'n')
			case '\t':
				b = append(b, '\\', 't')
			case '\r':
				b = append(b, '\\', 'r')
			default:
				b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			b = append(b, "\\ufffd"...)
		} else {
			b = append(b, s[i:i+n]...)
		}
		i += n
	}
	return append(b, '"')
}

func main() {
	// The package waits on this before its first call: the runtime is up and
	// the exports may be called.
	if ready := js.Global().Get("__musqlReady"); ready.Type() == js.TypeFunction {
		ready.Invoke()
	}
	select {}
}
