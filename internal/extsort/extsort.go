// Package extsort sorts more records than memory holds: records are kept until
// they reach a byte budget, then sorted and written to a temporary file as a
// run, and the runs are merged back in order. The order is a stable sort's --
// by Compare, ties in the order Add saw them -- so a sort that fits in one run
// and one that spills give the same sequence.
package extsort

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"

	"github.com/samyfodil/musql/internal/fsio"
)

// Codec is what a Sorter needs to know about its records.
type Codec[T any] struct {
	// Compare orders two records.
	Compare func(a, b T) int
	// Size is about how much memory a held record takes.
	Size func(T) int
	// Append encodes a record onto dst; Decode reverses it into a record that
	// owns its memory.
	Append func(dst []byte, v T) []byte
	Decode func(b []byte) (T, error)
}

// maxFanIn bounds how many runs one merge reads at once; more are merged in
// passes first.
const maxFanIn = 64

// Sorter collects records and hands them back sorted.
type Sorter[T any] struct {
	c      Codec[T]
	budget int
	recs   []T
	size   int
	f      *TempFile
	runs   []run
	buf    []byte
}

type run struct{ off, end int64 }

// New returns a Sorter that holds about budget bytes of records in memory.
func New[T any](c Codec[T], budget int) *Sorter[T] {
	return &Sorter[T]{c: c, budget: max(budget, 1<<16)}
}

// Add takes a record. The Sorter keeps v until it is written to a run, so v
// must not change afterwards.
func (s *Sorter[T]) Add(v T) error {
	s.recs = append(s.recs, v)
	s.size += s.c.Size(v)
	if s.size >= s.budget {
		return s.spill()
	}
	return nil
}

func (s *Sorter[T]) sortHeld() {
	slices.SortStableFunc(s.recs, s.c.Compare)
}

func (s *Sorter[T]) spill() error {
	s.sortHeld()
	if s.f == nil {
		f, err := NewTempFile()
		if err != nil {
			return err
		}
		s.f = f
	}
	off := s.f.Size()
	w := s.f.Writer()
	for _, r := range s.recs {
		if err := s.write(w, r); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	s.runs = append(s.runs, run{off, s.f.Size()})
	clear(s.recs)
	s.recs, s.size = s.recs[:0], 0
	return nil
}

func (s *Sorter[T]) write(w *bufio.Writer, v T) error {
	s.buf = s.c.Append(s.buf[:0], v)
	var n [binary.MaxVarintLen64]byte
	if _, err := w.Write(n[:binary.PutUvarint(n[:], uint64(len(s.buf)))]); err != nil {
		return err
	}
	_, err := w.Write(s.buf)
	return err
}

// Each calls fn with every record, in order, and stops at fn's first error.
// It is called once; Close still releases the Sorter after it.
func (s *Sorter[T]) Each(fn func(T) error) error {
	if len(s.runs) == 0 {
		s.sortHeld()
		for i, r := range s.recs {
			var zero T
			s.recs[i] = zero
			if err := fn(r); err != nil {
				return err
			}
		}
		s.recs = nil
		return nil
	}
	if len(s.recs) > 0 {
		if err := s.spill(); err != nil {
			return err
		}
	}
	s.recs = nil
	for len(s.runs) > maxFanIn {
		var next []run
		for i := 0; i < len(s.runs); i += maxFanIn {
			group := s.runs[i:min(i+maxFanIn, len(s.runs))]
			if len(group) == 1 {
				next = append(next, group[0])
				continue
			}
			off := s.f.Size()
			w := s.f.Writer()
			if err := s.merge(group, func(v T) error { return s.write(w, v) }); err != nil {
				return err
			}
			if err := w.Flush(); err != nil {
				return err
			}
			next = append(next, run{off, s.f.Size()})
		}
		s.runs = next
	}
	return s.merge(s.runs, fn)
}

// merge reads runs together, smallest record first, the earlier run first
// among equals: runs hold consecutive stretches of what Add saw, so that is
// the stable order.
func (s *Sorter[T]) merge(runs []run, fn func(T) error) error {
	bufSize := min(max(s.budget/(2*len(runs)), 4<<10), 1<<20)
	h := make([]*cursor[T], 0, len(runs))
	for i, r := range runs {
		c := &cursor[T]{r: bufio.NewReaderSize(io.NewSectionReader(s.f, r.off, r.end-r.off), bufSize), i: i}
		ok, err := c.next(s.c.Decode)
		if err != nil {
			return err
		}
		if ok {
			h = append(h, c)
		}
	}
	less := func(a, b *cursor[T]) bool {
		if c := s.c.Compare(a.v, b.v); c != 0 {
			return c < 0
		}
		return a.i < b.i
	}
	for i := len(h)/2 - 1; i >= 0; i-- {
		down(h, i, less)
	}
	for len(h) > 0 {
		c := h[0]
		if err := fn(c.v); err != nil {
			return err
		}
		ok, err := c.next(s.c.Decode)
		if err != nil {
			return err
		}
		if !ok {
			h[0] = h[len(h)-1]
			h = h[:len(h)-1]
		}
		if len(h) > 0 {
			down(h, 0, less)
		}
	}
	return nil
}

func down[T any](h []*cursor[T], i int, less func(a, b *cursor[T]) bool) {
	for {
		l := 2*i + 1
		if l >= len(h) {
			return
		}
		m := l
		if r := l + 1; r < len(h) && less(h[r], h[l]) {
			m = r
		}
		if !less(h[m], h[i]) {
			return
		}
		h[i], h[m] = h[m], h[i]
		i = m
	}
}

type cursor[T any] struct {
	r   *bufio.Reader
	i   int
	v   T
	buf []byte
}

func (c *cursor[T]) next(decode func([]byte) (T, error)) (bool, error) {
	var err error
	c.buf, err = ReadRecord(c.r, c.buf)
	if err == io.EOF {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	c.v, err = decode(c.buf)
	return err == nil, err
}

// ReadRecord reads one length-prefixed record into buf, which it grows as
// needed: io.EOF at a clean end, io.ErrUnexpectedEOF inside a record.
func ReadRecord(r *bufio.Reader, buf []byte) ([]byte, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		if err == io.EOF {
			return buf, io.EOF
		}
		return buf, unexpected(err)
	}
	if n > math.MaxInt32 {
		return buf, errors.New("extsort: record length out of range")
	}
	buf = slices.Grow(buf[:0], int(n))[:n]
	if _, err := io.ReadFull(r, buf); err != nil {
		return buf, unexpected(err)
	}
	return buf, nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Close releases the Sorter's records and file.
func (s *Sorter[T]) Close() error {
	s.recs = nil
	if s.f == nil {
		return nil
	}
	f := s.f
	s.f, s.runs = nil, nil
	return f.Close()
}

// TempFile is a scratch file with no name where the platform allows: what a
// spill writes to and reads back.
type TempFile struct {
	f    *os.File
	name string // set while the file still has its name
	size int64
	w    *bufio.Writer
}

// NewTempFile creates a scratch file in the system temporary directory.
func NewTempFile() (*TempFile, error) {
	f, err := os.CreateTemp("", "musql-sort-*")
	if err != nil {
		return nil, fmt.Errorf("extsort: %w", err)
	}
	t := &TempFile{f: f}
	if os.Remove(f.Name()) != nil {
		t.name = f.Name()
	}
	return t, nil
}

// Size is how many bytes have been written.
func (t *TempFile) Size() int64 { return t.size }

// Writer appends at the end of the file; its Flush writes them.
func (t *TempFile) Writer() *bufio.Writer {
	if t.w == nil {
		t.w = bufio.NewWriterSize(appender{t}, 64<<10)
	} else {
		t.w.Reset(appender{t})
	}
	return t.w
}

type appender struct{ t *TempFile }

func (a appender) Write(b []byte) (int, error) {
	n, err := fsio.WriteAt(a.t.f, b, a.t.size)
	a.t.size += int64(n)
	return n, err
}

// ReadAt reads written bytes.
func (t *TempFile) ReadAt(b []byte, off int64) (int, error) {
	n, err := fsio.ReadAt(t.f, b, off)
	if err == nil && n < len(b) {
		err = io.EOF
	}
	return n, err
}

// Close closes the file and removes it.
func (t *TempFile) Close() error {
	err := t.f.Close()
	if t.name != "" {
		os.Remove(t.name)
	}
	return err
}
