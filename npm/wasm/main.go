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
//	musql_query(handle, sqlLen, argsLen) -> outLen: {"columns","types","rows"} or {"error"}
//	musql_exec(handle, sqlLen, argsLen)  -> outLen: {"changes","lastInsertRowid"} or {"error"}
//	musql_convert(toSQLite, srcLen, dstLen) -> outLen: {} or {"error"}
//	musql_split(sqlLen)              -> outLen: {"statements"} or {"error"}
//
// A query's types are its columns' declared types, "" where a column has none.
//
// An argument is a tag byte then its value: 0 NULL, 1 int64 (8 bytes LE),
// 2 float64 (8 bytes LE), 3 text and 4 blob (a 4-byte LE length, then the
// bytes). In a result, an integer outside +-2^53 is {"i":"<decimal>"} and a
// blob is {"b":"<base64>"}, so neither loses anything in JavaScript.
package main

import (
	"context"
	sqldriver "database/sql/driver"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strconv"
	"syscall/js"
	"unicode/utf8"
	"unsafe"

	sqlite "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
)

// conn is the driver's connection itself, not a database/sql pool over it:
// one handle runs its statements in order on one connection, as a SQLite
// connection does, and the pool's bookkeeping was a third of a point lookup.
type conn struct {
	c     *driver.Conn
	stmts map[string]*driver.Stmt
}

// stmtsMax bounds a handle's prepared statements: a page that sends many
// distinct texts drops them all and starts over.
const stmtsMax = 256

// stmt returns q's prepared statement, preparing it on first use. A statement
// keeps its argument buffers between runs, which a fresh one allocates.
func (c *conn) stmt(q string) (*driver.Stmt, error) {
	if st := c.stmts[q]; st != nil {
		return st, nil
	}
	ds, err := c.c.Prepare(q)
	if err != nil {
		return nil, err
	}
	if len(c.stmts) >= stmtsMax {
		c.closeStmts()
	}
	st := ds.(*driver.Stmt)
	c.stmts[q] = st
	return st, nil
}

func (c *conn) closeStmts() {
	for q, st := range c.stmts {
		st.Close()
		delete(c.stmts, q)
	}
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
	dc, err := (&driver.Driver{}).Open(path)
	if err != nil {
		outBuf = appendError(outBuf[:0], err)
		return -int32(len(outBuf)) - 1
	}
	c := dc.(*driver.Conn)
	// Results carry declared types: a page tells a vector column from any
	// other BLOB by its F32_BLOB(n).
	c.ReportDeclTypes(true)
	nextID++
	conns[nextID] = &conn{c: c, stmts: map[string]*driver.Stmt{}}
	return nextID
}

//go:wasmexport musql_close
func musqlClose(h int32) {
	if c := conns[h]; c != nil {
		c.closeStmts()
		c.c.Close()
		delete(conns, h)
	}
}

//go:wasmexport musql_query
func musqlQuery(h int32, sqlLen, argsLen uint32) uint32 {
	c, q, args, err := request(h, sqlLen, argsLen)
	if err == nil {
		var rows [][]sqldriver.Value
		var cols, types []string
		if rows, cols, types, err = c.query(q, args); err == nil {
			outBuf = appendRows(outBuf[:0], cols, types, rows)
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
		var res sqldriver.Result
		var st *driver.Stmt
		if st, err = c.stmt(q); err == nil {
			res, err = st.ExecContext(context.Background(), args)
		}
		if err == nil {
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

// musql_convert converts the file at the inbuf's first srcLen bytes (a path)
// into the next dstLen: a SQLite file into a musql database, or with toSQLite
// set, a musql database into a SQLite file. The package moves SQLite files in
// and out of the browser through it, so they never meet the engine.
//
//go:wasmexport musql_convert
func musqlConvert(toSQLite int32, srcLen, dstLen uint32) uint32 {
	var err error
	if uint64(srcLen)+uint64(dstLen) > uint64(len(inBuf)) {
		err = errors.New("musql: request past its buffer")
	} else {
		src, dst := string(inBuf[:srcLen]), string(inBuf[srcLen:srcLen+dstLen])
		if toSQLite != 0 {
			err = sqlite.Export(src, dst, 0)
		} else {
			err = sqlite.Import(src, dst, sqlite.ImportOptions{})
		}
	}
	if err != nil {
		outBuf = appendError(outBuf[:0], err)
	} else {
		outBuf = append(outBuf[:0], "{}"...)
	}
	return uint32(len(outBuf))
}

var errClosed = errors.New("musql: the database is closed")

func request(h int32, sqlLen, argsLen uint32) (*conn, string, []sqldriver.NamedValue, error) {
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

// query runs q and reads every row.
func (c *conn) query(q string, args []sqldriver.NamedValue) ([][]sqldriver.Value, []string, []string, error) {
	st, err := c.stmt(q)
	if err != nil {
		return nil, nil, nil, err
	}
	rs, err := st.QueryContext(context.Background(), args)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rs.Close()
	rows := rs.(*driver.Rows)
	cols := rows.Columns()
	types := make([]string, len(cols))
	for i := range types {
		types[i] = rows.ColumnTypeDatabaseTypeName(i)
	}
	var out [][]sqldriver.Value
	for {
		vals := make([]sqldriver.Value, len(cols))
		if err := rows.Next(vals); err == io.EOF {
			break
		} else if err != nil {
			return nil, nil, nil, err
		}
		out = append(out, vals)
	}
	return out, cols, types, nil
}

var errArg = errors.New("musql: malformed argument buffer")

func decodeArgs(b []byte) ([]sqldriver.NamedValue, error) {
	var out []sqldriver.NamedValue
	add := func(v any) { out = append(out, sqldriver.NamedValue{Ordinal: len(out) + 1, Value: v}) }
	for len(b) > 0 {
		tag := b[0]
		b = b[1:]
		switch tag {
		case 0:
			add(nil)
		case 1, 2:
			if len(b) < 8 {
				return nil, errArg
			}
			u := binary.LittleEndian.Uint64(b)
			b = b[8:]
			if tag == 1 {
				add(int64(u))
			} else {
				add(math.Float64frombits(u))
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
				add(string(b[:n]))
			} else {
				add(append([]byte{}, b[:n]...))
			}
			b = b[n:]
		default:
			return nil, errArg
		}
	}
	return out, nil
}

func appendRows(b []byte, cols, types []string, rows [][]sqldriver.Value) []byte {
	b = appendStrings(append(b, `{"columns":`...), cols)
	b = appendStrings(append(b, `,"types":`...), types)
	b = append(b, `,"rows":[`...)
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

// appendStrings writes ss as a JSON array of strings.
func appendStrings(b []byte, ss []string) []byte {
	b = append(b, '[')
	for i, s := range ss {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendString(b, s)
	}
	return append(b, ']')
}

// musql_split splits the inbuf's first sqlLen bytes into its statements, as
// the engine does (a trigger body is one statement with the CREATE TRIGGER).
//
//go:wasmexport musql_split
func musqlSplit(sqlLen uint32) uint32 {
	stmts, err := engine.SplitStatements(string(inBuf[:sqlLen]))
	if err != nil {
		outBuf = appendError(outBuf[:0], err)
		return uint32(len(outBuf))
	}
	outBuf = append(appendStrings(append(outBuf[:0], `{"statements":`...), stmts), '}')
	return uint32(len(outBuf))
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
	// fts5 is opt-in in the engine (RegisterFTS5) only so the default test
	// oracle, built without it, agrees; a page has every module.
	engine.RegisterFTS5()
	// The package waits on this before its first call: the runtime is up and
	// the exports may be called.
	if ready := js.Global().Get("__musqlReady"); ready.Type() == js.TypeFunction {
		ready.Invoke()
	}
	select {}
}
