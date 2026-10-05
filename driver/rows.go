package driver

import (
	"database/sql/driver"
	"fmt"
	"io"
	"time"

	"github.com/samyfodil/musql/engine"
)

// sqliteTimestampFormat is the TEXT layout for binding time.Time arguments,
// matching mattn/go-sqlite3's default format for byte-for-byte round-trips.
const sqliteTimestampFormat = "2006-01-02 15:04:05.999999999-07:00"

// driverValueToEngine converts a driver.Value into engine.Value.
// bool binds as INTEGER 0/1, time.Time as TEXT with sqliteTimestampFormat.
func driverValueToEngine(v driver.Value) (engine.Value, error) {
	switch x := v.(type) {
	case nil:
		return engine.Value{Typ: engine.Null}, nil
	case int64:
		return engine.Value{Typ: engine.Int, I: x}, nil
	case float64:
		return engine.Value{Typ: engine.Float, F: x}, nil
	// The kinds Conn.CheckNamedValue lets through unconverted.
	case int:
		return engine.Value{Typ: engine.Int, I: int64(x)}, nil
	case int32:
		return engine.Value{Typ: engine.Int, I: int64(x)}, nil
	case int16:
		return engine.Value{Typ: engine.Int, I: int64(x)}, nil
	case int8:
		return engine.Value{Typ: engine.Int, I: int64(x)}, nil
	case float32:
		return engine.Value{Typ: engine.Float, F: float64(x)}, nil
	case bool:
		i := int64(0)
		if x {
			i = 1
		}
		return engine.Value{Typ: engine.Int, I: i}, nil
	case []byte:
		return engine.Value{Typ: engine.Blob, S: append([]byte(nil), x...)}, nil
	case string:
		return engine.Value{Typ: engine.Text, S: []byte(x)}, nil
	case time.Time:
		var buf [64]byte
		b := x.AppendFormat(buf[:0], sqliteTimestampFormat)
		return engine.Value{Typ: engine.Text, S: append([]byte(nil), b...)}, nil
	default:
		return engine.Value{}, fmt.Errorf("driver: unsupported bind argument type %T", v)
	}
}

// engineValueToDriver converts engine.Value into driver.Value:
// Null->nil, Int->int64, Float->float64, Text->string, Blob->[]byte (fresh copy).
// Columns are not converted to time.Time; callers must parse timestamp strings.
func engineValueToDriver(v engine.Value) driver.Value {
	switch v.Typ {
	case engine.Null:
		return nil
	case engine.Int:
		return v.I
	case engine.Float:
		return v.F
	case engine.Text:
		return string(v.S)
	case engine.Blob:
		b := make([]byte, len(v.S))
		copy(b, v.S)
		return b
	default:
		return nil
	}
}

// Rows implements driver.Rows over a fully materialized query result.
type Rows struct {
	cols []string
	rows [][]engine.Value
	pos  int

	// timeCols and loc are set for connections whose DSN asks for time coercion.
	// timeCols[i] true means column i's DECLARED type should convert to time.Time.
	timeCols []bool
	loc      *time.Location

	// declTypes are the result columns' declared types, when the connection
	// asked for them (Conn.ReportDeclTypes); "" for a column with none.
	declTypes []string
}

var _ driver.Rows = (*Rows)(nil)

// Columns implements driver.Rows.
func (r *Rows) Columns() []string { return r.cols }

// ColumnTypeDatabaseTypeName implements driver.RowsColumnTypeDatabaseTypeName:
// the column's declared type, "" when it has none or the connection did not ask
// for them (Conn.ReportDeclTypes).
func (r *Rows) ColumnTypeDatabaseTypeName(i int) string {
	if i < len(r.declTypes) {
		return r.declTypes[i]
	}
	return ""
}

// Close implements driver.Rows. This is a no-op since the pager is already closed.
func (r *Rows) Close() error { return nil }

// Next implements driver.Rows: io.EOF once every row has been delivered.
func (r *Rows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	row := r.rows[r.pos]
	r.pos++
	for i, v := range row {
		if i < len(r.timeCols) && r.timeCols[i] {
			if tv, ok := declTypeTimeValue(v, r.loc); ok {
				dest[i] = tv
				continue
			}
		}
		dest[i] = engineValueToDriver(v)
	}
	return nil
}

// execResult implements driver.Result over the (rowsAffected, lastInsertID)
// pair engine.DB.ExecArgs returns (see that method's doc comment for exactly
// when lastInsertID changes vs. stays at its prior value).
type execResult struct {
	rowsAffected int64
	lastInsertID int64
}

var _ driver.Result = (*execResult)(nil)

func (r *execResult) LastInsertId() (int64, error) { return r.lastInsertID, nil }
func (r *execResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }
