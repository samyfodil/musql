package driver

import (
	"database/sql/driver"
	"math"
	"reflect"
	"testing"
	"time"
)

type namedInt int

// CheckNamedValue plus driverValueToEngine must bind exactly what database/sql's
// default converter plus driverValueToEngine binds, or return ErrSkip so the
// default runs.
func TestCheckNamedValueMatchesDefault(t *testing.T) {
	ts := time.Date(2026, 9, 29, 1, 2, 3, 4, time.UTC)
	n := 7
	for _, v := range []any{
		nil, int64(-3), 2.5, true, "s", []byte("b"), ts,
		int(math.MinInt64), int(-5), int32(math.MaxInt32), int16(-9), int8(127),
		float32(0.1), float32(math.Inf(1)),
		uint(3), uint64(math.MaxUint64), namedInt(4), &n,
	} {
		nv := driver.NamedValue{Ordinal: 1, Value: v}
		err := (&Conn{}).CheckNamedValue(&nv)
		if err == driver.ErrSkip {
			continue
		}
		got, gerr := driverValueToEngine(nv.Value)
		dv, derr := driver.DefaultParameterConverter.ConvertValue(v)
		if derr != nil {
			t.Errorf("%T(%v): accepted, but the default refuses it: %v", v, v, derr)
			continue
		}
		want, werr := driverValueToEngine(dv)
		if err != nil || gerr != nil || werr != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%T(%v): got %#v, %v, %v; default %#v, %v", v, v, got, err, gerr, want, werr)
		}
	}
	for _, v := range []any{uint(3), namedInt(4), &n} {
		nv := driver.NamedValue{Ordinal: 1, Value: v}
		if err := (&Conn{}).CheckNamedValue(&nv); err != driver.ErrSkip {
			t.Errorf("%T: got %v, want ErrSkip so the default converts it", v, err)
		}
	}
}
