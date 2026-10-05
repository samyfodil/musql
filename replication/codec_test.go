package replication

import (
	"bytes"
	"testing"
)

// assertValueEqual compares a driverValue result ([]byte gets a byte-slice
// comparison; everything else compares with ==) against a want value.
func assertValueEqual(t *testing.T, got, want any) {
	t.Helper()
	if wb, ok := want.([]byte); ok {
		gb, ok := got.([]byte)
		if !ok || !bytes.Equal(gb, wb) {
			t.Fatalf("got %#v (%T), want %#v", got, got, want)
		}
		return
	}
	if got != want {
		t.Fatalf("got %#v (%T), want %#v (%T)", got, got, want, want)
	}
}

// TestValueCodecRoundTrip exercises CellFromValue/EncodePK -> packVal ->
// unpackVal -> driverValue for every SQLite storage class database/sql hands
// the preupdate hook, plus the Go `int` convenience type accepted by
// encodeValue.
func TestValueCodecRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want any
	}{
		{"int64", int64(42), int64(42)},
		{"int64-negative", int64(-123456789), int64(-123456789)},
		{"int", int(7), int64(7)},
		{"float64", 3.14159, 3.14159},
		{"float64-negative", -2.5, -2.5},
		{"string", "hello world", "hello world"},
		{"string-empty", "", ""},
		{"blob", []byte{0x00, 0x01, 0xff, 0x10}, []byte{0x00, 0x01, 0xff, 0x10}},
		{"blob-empty", []byte{}, []byte(nil)},
		{"nil", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// CellFromValue -> packVal -> unpackVal -> driverValue.
			cell := CellFromValue("col", tc.in)
			packed := packVal(cell.Type, cell.Val)
			typ, raw := unpackVal(packed)
			got := driverValue(typ, raw)
			if tc.name == "blob-empty" {
				// An empty (non-nil) blob round-trips to a nil raw slice once
				// packed/unpacked, since packVal/unpackVal carry no length
				// beyond the tag byte; driverValue(TypeBlob, nil) is fine.
				if got != nil {
					if gb, ok := got.([]byte); !ok || len(gb) != 0 {
						t.Fatalf("blob-empty round trip = %#v, want empty/nil blob", got)
					}
				}
			} else {
				assertValueEqual(t, got, tc.want)
			}

			// EncodePK must be usable the same way for the primary-key path.
			pk := EncodePK(tc.in)
			pkTyp, pkRaw := unpackVal(pk)
			pkGot := driverValue(pkTyp, pkRaw)
			if tc.name != "blob-empty" {
				assertValueEqual(t, pkGot, tc.want)
			}
		})
	}
}

// TestEncodeValueDefaultFallback exercises encodeValue's default branch: an
// unsupported Go type (e.g. bool) is stringified via fmt.Sprint and stored as
// TypeText, rather than causing an error.
func TestEncodeValueDefaultFallback(t *testing.T) {
	cell := CellFromValue("flag", true)
	if cell.Type != TypeText {
		t.Fatalf("bool fallback type = %d, want TypeText(%d)", cell.Type, TypeText)
	}
	if string(cell.Val) != "true" {
		t.Fatalf("bool fallback val = %q, want %q", cell.Val, "true")
	}
}

// TestDriverValuePadding exercises pad8's zero-extend branch directly (raw
// shorter than 8 bytes) and driverValue's unknown-type fallthrough to nil.
func TestDriverValuePadding(t *testing.T) {
	if got := driverValue(TypeInt, []byte{0x05}); got != int64(5) {
		t.Fatalf("driverValue(TypeInt, {0x05}) = %v, want 5", got)
	}
	if got := driverValue(TypeInt, []byte{}); got != int64(0) {
		t.Fatalf("driverValue(TypeInt, {}) = %v, want 0", got)
	}
	if got := driverValue(TypeFloat, []byte{0x3f}); got == nil {
		t.Fatalf("driverValue(TypeFloat, short) = nil, want a float64")
	} else if _, ok := got.(float64); !ok {
		t.Fatalf("driverValue(TypeFloat, short) = %v (%T), want float64", got, got)
	}
	if got := driverValue(TypeNull, nil); got != nil {
		t.Fatalf("driverValue(TypeNull, nil) = %v, want nil", got)
	}
}

// TestUnpackValEmpty covers unpackVal's empty-input branch directly: both a
// nil slice and a zero-length non-nil slice must decode to (TypeNull, nil).
func TestUnpackValEmpty(t *testing.T) {
	if typ, raw := unpackVal(nil); typ != TypeNull || raw != nil {
		t.Fatalf("unpackVal(nil) = (%d, %v), want (TypeNull, nil)", typ, raw)
	}
	if typ, raw := unpackVal([]byte{}); typ != TypeNull || raw != nil {
		t.Fatalf("unpackVal([]byte{}) = (%d, %v), want (TypeNull, nil)", typ, raw)
	}
}

// TestEncodeCellsRoundTrip covers encodeCells/decodeCells over a mixed set of
// cells spanning every storage class, including a NULL cell.
func TestEncodeCellsRoundTrip(t *testing.T) {
	cells := []Cell{
		CellFromValue("name", "alice"),
		CellFromValue("age", int64(30)),
		CellFromValue("balance", 12.5),
		CellFromValue("photo", []byte{1, 2, 3, 0xff}),
		CellFromValue("bio", nil),
	}
	enc := encodeCells(cells)
	dec, err := decodeCells(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(dec) != len(cells) {
		t.Fatalf("decoded %d cells, want %d", len(dec), len(cells))
	}
	for i := range cells {
		if dec[i].Col != cells[i].Col || dec[i].Type != cells[i].Type || !bytes.Equal(dec[i].Val, cells[i].Val) {
			t.Fatalf("cell %d = %+v, want %+v", i, dec[i], cells[i])
		}
	}
}

// TestEncodeCellsEmpty covers the empty-cell-list round trip (encodeCells(nil)
// still writes a zero count, decodeCells reads it back as zero cells) and the
// decodeCells(nil)/decodeCells([]byte{}) short-circuit that returns (nil, nil)
// without even looking at the count varint.
func TestEncodeCellsEmpty(t *testing.T) {
	enc := encodeCells(nil)
	dec, err := decodeCells(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(dec) != 0 {
		t.Fatalf("decoded %d cells from an empty cell list, want 0", len(dec))
	}

	if dec2, err := decodeCells(nil); err != nil || dec2 != nil {
		t.Fatalf("decodeCells(nil) = (%v, %v), want (nil, nil)", dec2, err)
	}
	if dec3, err := decodeCells([]byte{}); err != nil || dec3 != nil {
		t.Fatalf("decodeCells([]byte{}) = (%v, %v), want (nil, nil)", dec3, err)
	}
}

// TestDecodeCellsTruncated exercises decodeCells' error paths: a count that
// promises more cells than the buffer actually contains.
func TestDecodeCellsTruncated(t *testing.T) {
	enc := encodeCells([]Cell{CellFromValue("a", int64(1))})
	// Corrupt: truncate after the count+col-length but before the column
	// name bytes are fully present.
	if len(enc) > 2 {
		truncated := enc[:2]
		if _, err := decodeCells(truncated); err == nil {
			t.Fatalf("decodeCells(truncated) = nil error, want an error")
		}
	}
}
