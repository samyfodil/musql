package engine

import (
	"errors"
	"sync/atomic"
)

// ErrInterrupted is a statement stopped by its interrupt flag, C's
// SQLITE_INTERRUPT.
var ErrInterrupted = errors.New("engine: interrupted")

// SetInterrupt makes f this session's interrupt flag: while it is set, a
// running statement stops at its next loop with ErrInterrupted, as
// sqlite3_interrupt stops one in C. The pagers the session builds from now on
// carry it too.
func (db *DB) SetInterrupt(f *atomic.Bool) { db.interrupt = f }

// SetInterrupt is DB.SetInterrupt for the statements run on this pager.
func (p *ReadOnlyPager) SetInterrupt(f *atomic.Bool) { p.interrupt = f }

// interruptFlag is the pager's interrupt flag, or nil.
func (p *ReadOnlyPager) interruptFlag() *atomic.Bool {
	if p == nil {
		return nil
	}
	return p.interrupt
}

// interrupted reports whether the statements run on this pager are
// interrupted.
func (p *ReadOnlyPager) interrupted() bool {
	f := p.interruptFlag()
	return f != nil && f.Load()
}

// sortStop ends a long sort early once its statement is interrupted: every
// comparison after that answers equal, so the sort finishes at once and its
// order, which nothing reads, is left as it is. A nil sortStop never stops.
type sortStop struct {
	f       *atomic.Bool
	n       uint32
	stopped bool
}

func (s *sortStop) now() bool {
	if s == nil {
		return false
	}
	if s.n++; s.n&0xffff == 0 && s.f != nil && s.f.Load() {
		s.stopped = true
	}
	return s.stopped
}

// interruptFlag is the flag that stops this run: its pager's, or else the
// writing session's.
func (m *vdbe) interruptFlag() *atomic.Bool {
	if m.pager != nil && m.pager.interrupt != nil {
		return m.pager.interrupt
	}
	if m.wctx != nil && m.wctx.db != nil {
		return m.wctx.db.interrupt
	}
	return nil
}

// interrupted reports whether this run's statement is interrupted: a kernel
// that runs a whole loop in one instruction asks it as it goes, and stops.
func (m *vdbe) interrupted() bool {
	f := m.interruptFlag()
	return f != nil && f.Load()
}
