package engine

import (
	"errors"
	"testing"
)

func TestIntegrityValuesRefuseMalformed(t *testing.T) {
	enc := appendIntegrityValues(nil, []Value{{Typ: Int, I: -7}, {Typ: Text, S: []byte("x")}})
	for _, n := range []int{0, 1, 3, 5, len(enc) - 1} {
		if _, err := decodeIntegrityValues(enc[:n]); !errors.Is(err, errIntegrityRecord) {
			t.Errorf("cut at %d of %d: %v, want errIntegrityRecord", n, len(enc), err)
		}
	}
}

func TestIntegrityRowidName(t *testing.T) {
	taken := []columnInfo{{Name: "rowid"}, {Name: "_ROWID_"}, {Name: "oid"}}
	if got := integrityRowidName(&tableMeta{cols: taken, ipkIndex: -1}); got != "" {
		t.Errorf("every spelling taken, no INTEGER PRIMARY KEY: %q", got)
	}
	withPK := append(taken, columnInfo{Name: "id"})
	if got := integrityRowidName(&tableMeta{cols: withPK, ipkIndex: 3}); got != quoteIdent("id") {
		t.Errorf("every spelling taken: %q, want the INTEGER PRIMARY KEY's name", got)
	}
	if got := integrityRowidName(&tableMeta{cols: taken[:1], ipkIndex: -1}); got != "_rowid_" {
		t.Errorf("rowid taken: %q, want _rowid_", got)
	}
}
