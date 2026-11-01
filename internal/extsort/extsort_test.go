package extsort

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"slices"
	"testing"
)

type rec struct {
	key, seq uint64
	pad      []byte
}

var recCodec = Codec[rec]{
	Compare: func(a, b rec) int {
		switch {
		case a.key < b.key:
			return -1
		case a.key > b.key:
			return 1
		}
		return 0
	},
	Size: func(r rec) int { return 40 + len(r.pad) },
	Append: func(dst []byte, r rec) []byte {
		dst = binary.AppendUvarint(dst, r.key)
		dst = binary.AppendUvarint(dst, r.seq)
		return append(dst, r.pad...)
	},
	Decode: func(b []byte) (rec, error) {
		k, n := binary.Uvarint(b)
		if n <= 0 {
			return rec{}, errors.New("bad key")
		}
		s, m := binary.Uvarint(b[n:])
		if m <= 0 {
			return rec{}, errors.New("bad seq")
		}
		return rec{key: k, seq: s, pad: slices.Clone(b[n+m:])}, nil
	},
}

// TestSorterMatchesStableSort holds the merged order to slices.SortStableFunc's
// across one run, a few, and more than one merge reads at once.
func TestSorterMatchesStableSort(t *testing.T) {
	for _, n := range []int{0, 1, 500, 5000, 120000} {
		r := rand.New(rand.NewPCG(uint64(n), 7))
		var in []rec
		s := New(recCodec, 1)
		for i := range n {
			v := rec{key: r.Uint64N(uint64(n/4 + 1)), seq: uint64(i), pad: make([]byte, r.IntN(40))}
			in = append(in, v)
			if err := s.Add(v); err != nil {
				t.Fatal(err)
			}
		}
		want := slices.Clone(in)
		slices.SortStableFunc(want, recCodec.Compare)
		var got []rec
		if err := s.Each(func(v rec) error { got = append(got, v); return nil }); err != nil {
			t.Fatal(err)
		}
		if n >= 5000 && len(s.runs) < 2 {
			t.Fatalf("n=%d: expected the sort to spill, it held %d runs", n, len(s.runs))
		}
		if len(got) != len(want) {
			t.Fatalf("n=%d: got %d records, want %d", n, len(got), len(want))
		}
		for i := range want {
			if got[i].key != want[i].key || got[i].seq != want[i].seq || len(got[i].pad) != len(want[i].pad) {
				t.Fatalf("n=%d: record %d is %d/%d, want %d/%d", n, i, got[i].key, got[i].seq, want[i].key, want[i].seq)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSorterStopsAtCallbackError is Each stopping at fn's first error.
func TestSorterStopsAtCallbackError(t *testing.T) {
	s := New(recCodec, 1)
	defer s.Close()
	for i := range 3000 {
		if err := s.Add(rec{key: uint64(i % 17), seq: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	stop := errors.New("stop")
	seen := 0
	err := s.Each(func(rec) error {
		seen++
		if seen == 10 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || seen != 10 {
		t.Fatalf("Each = %v after %d records, want stop after 10", err, seen)
	}
}

func TestSorterHeldCallbackError(t *testing.T) {
	s := New(recCodec, 1<<20)
	defer s.Close()
	for i := range 10 {
		if err := s.Add(rec{key: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	stop := errors.New("stop")
	if err := s.Each(func(rec) error { return stop }); !errors.Is(err, stop) || len(s.runs) != 0 {
		t.Fatalf("Each = %v with %d runs, want stop from the held records", err, len(s.runs))
	}
}

// TestSorterMergesALoneRun spills every record, so 65 runs merge in a pass of
// 64 and a group of one.
func TestSorterMergesALoneRun(t *testing.T) {
	c := recCodec
	c.Size = func(rec) int { return 1 << 16 }
	s := New(c, 1)
	defer s.Close()
	const n = maxFanIn + 1
	for i := range n {
		if err := s.Add(rec{key: uint64(n - i), seq: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.runs) != n {
		t.Fatalf("%d runs, want %d", len(s.runs), n)
	}
	var got []uint64
	if err := s.Each(func(r rec) error { got = append(got, r.key); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != n || !slices.IsSorted(got) {
		t.Fatalf("got %v", got)
	}
}

func TestSorterDecodeError(t *testing.T) {
	c := recCodec
	bad := errors.New("bad record")
	c.Decode = func([]byte) (rec, error) { return rec{}, bad }
	s := New(c, 1)
	defer s.Close()
	for i := range 3000 {
		if err := s.Add(rec{key: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Each(func(rec) error { return nil }); !errors.Is(err, bad) {
		t.Fatalf("Each = %v, want the decode error", err)
	}
}

func TestReadRecord(t *testing.T) {
	long := binary.AppendUvarint(nil, 1<<32)
	for _, tc := range []struct {
		name string
		in   []byte
		want error
	}{
		{"clean end", nil, io.EOF},
		{"torn length", []byte{0x80}, io.ErrUnexpectedEOF},
		{"length out of range", long, nil},
		{"no body", []byte{5}, io.ErrUnexpectedEOF},
		{"short body", []byte{5, 1, 2}, io.ErrUnexpectedEOF},
	} {
		_, err := ReadRecord(bufio.NewReader(bytes.NewReader(tc.in)), nil)
		if tc.want == nil {
			if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("%s: %v, want a range error", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	got, err := ReadRecord(bufio.NewReader(bytes.NewReader([]byte{3, 'a', 'b', 'c'})), nil)
	if err != nil || string(got) != "abc" {
		t.Fatalf("ReadRecord = %q, %v", got, err)
	}
}

func TestTempFileReadPastEnd(t *testing.T) {
	f, err := NewTempFile()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := f.Writer()
	w.WriteString("abc")
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 8)
	if n, err := f.ReadAt(b, 0); n != 3 || err != io.EOF {
		t.Fatalf("ReadAt = %d, %v, want 3 bytes and io.EOF", n, err)
	}
}
