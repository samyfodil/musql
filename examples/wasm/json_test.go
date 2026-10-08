//go:build js && wasm

package main

import (
	"encoding/json"
	"math"
	"testing"
)

// appendJSONValue must produce what encoding/json does for every type
// queryRows returns, including strings JSON escapes differently from Go.
func TestAppendJSONValueMatchesEncodingJSON(t *testing.T) {
	vals := []any{
		int64(0), int64(-1), int64(math.MaxInt64), int64(math.MinInt64),
		1.5, -0.25, 1e300, 5e-324, 1e21, 123456789.0,
		"", "plain", `quote " and \ backslash`, "line\nbreak\ttab\rcr",
		"\x00\x01\x1f\x7f", "héllo, 世界 🙂", "bad \xff utf8 \xc3", true, false, nil,
	}
	for _, v := range vals {
		got := string(appendJSONValue(nil, v))
		want, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			// Both must at least decode to the same value.
			var g, w any
			if json.Unmarshal([]byte(got), &g) != nil || json.Unmarshal(want, &w) != nil || g != w {
				t.Errorf("%#v: got %s, encoding/json %s", v, got, want)
			}
		}
	}
}
