package engine

import (
	"bytes"
	"fmt"
	"math"
)

// vec1NumericType is sqlite3_value_numeric_type with the value it settles on:
// a TEXT holding a number in full becomes that INTEGER or REAL, anything else
// keeps its type.
func vec1NumericType(v Value) Value {
	if v.Typ != Text {
		return v
	}
	isF, i, f, ok := parseFullNumeric(string(v.S))
	if !ok {
		return v
	}
	if isF {
		return Value{Typ: Float, F: f}
	}
	return Value{Typ: Int, I: i}
}

// vec1IntOf reports whether f is "(i64)f == f" in C, for a finite f inside
// int64's range.
func vec1IntOf(f float64) (int64, bool) {
	if f >= -9223372036854775808.0 && f < 9223372036854775808.0 && f == math.Trunc(f) {
		return int64(f), true
	}
	return 0, false
}

// vec1JSONOpt is one key/value of a configuration object, as
// vec1ParseJsonConfig hands it to its callback.
type vec1JSONOpt struct {
	key  string // sqlite3_column_text of json_each's key: "" for NULL
	typ  ValueType
	i    int64   // INTEGER: the value
	f    float64 // INTEGER or REAL: the value
	text string  // sqlite3_column_text of the value
}

// vec1ParseJSONConfig is vec1ParseJsonConfig: "SELECT key, value FROM
// json_each(?)" over z, each row handed to fn with the value's numeric type,
// a REAL that is a whole number counting as an INTEGER. A NULL value (or a
// BLOB) stops it with "vec1: null value in json object"; malformed JSON is
// json_each's own error. z is read as a C string, up to a NUL.
func vec1ParseJSONConfig(z []byte, isNull bool, fn func(o vec1JSONOpt) error) error {
	if isNull {
		return nil
	}
	if i := bytes.IndexByte(z, 0); i >= 0 {
		z = z[:i]
	}
	cur, err := jsonEachTable{name: "json_each"}.Open()
	if err != nil {
		return err
	}
	defer cur.Close()
	if err := cur.Filter(jsonEachHasJSON, "", []Value{{Typ: Text, S: z}}); err != nil {
		return fmt.Errorf("%s", trimEnginePrefix(err.Error()))
	}
	for !cur.Eof() {
		key, err := cur.Column(jsonEachColKey)
		if err != nil {
			return err
		}
		val, err := cur.Column(jsonEachColValue)
		if err != nil {
			return err
		}
		o := vec1JSONOpt{}
		if key.Typ != Null {
			o.key = valueToText(key)
		}
		n := vec1NumericType(val)
		switch n.Typ {
		case Int:
			o.typ, o.i, o.f = Int, n.I, float64(n.I)
		case Float:
			o.typ, o.f = Float, n.F
			if i, ok := vec1IntOf(n.F); ok {
				o.typ, o.i = Int, i
			}
		case Text:
			o.typ = Text
		default:
			return fmt.Errorf("vec1: null value in json object")
		}
		o.text = valueToText(val)
		if err := fn(o); err != nil {
			return err
		}
		if err := cur.Next(); err != nil {
			return fmt.Errorf("%s", trimEnginePrefix(err.Error()))
		}
	}
	return nil
}

// trimEnginePrefix drops the "engine: " this package's errors start with,
// where C's message is wanted verbatim.
func trimEnginePrefix(s string) string {
	for len(s) > 8 && s[:8] == "engine: " {
		s = s[8:]
	}
	return s
}
