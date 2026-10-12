package engine

import "iter"

// changeChunk is how many RowChanges one chunk of a changeLog holds (about
// 550 KB). keptChunks is how many emptied chunks a log keeps for its next
// transaction, so a transaction of up to 65,536 rows logs without allocating.
const (
	changeChunk = 4096
	keptChunks  = 16
)

// changeLog is the session's row-change log: a list of fixed-size chunks.
//
// It was one slice that grew by doubling, and a transaction of tens of
// thousands of rows copied it at every doubling -- about 440 bytes allocated
// per row logged, a fifth of what an INSERT loop allocated in all. A chunk is
// never copied; the log only ever adds one.
type changeLog struct {
	chunks [][]RowChange // every chunk full but the last
	n      int
	spare  [][]RowChange // emptied chunks kept for reuse, each len 0 and cap changeChunk
}

func (l *changeLog) len() int { return l.n }

// push appends c and returns where it was stored.
func (l *changeLog) push(c RowChange) *RowChange {
	k := len(l.chunks) - 1
	if k < 0 || len(l.chunks[k]) == changeChunk {
		var ch []RowChange
		if s := len(l.spare); s > 0 {
			ch, l.spare = l.spare[s-1], l.spare[:s-1]
		} else {
			ch = make([]RowChange, 0, changeChunk)
		}
		l.chunks = append(l.chunks, ch)
		k++
	}
	l.chunks[k] = append(l.chunks[k], c)
	l.n++
	return &l.chunks[k][len(l.chunks[k])-1]
}

// last is the newest entry; the log must not be empty.
func (l *changeLog) last() *RowChange {
	c := l.chunks[len(l.chunks)-1]
	return &c[len(c)-1]
}

// truncate drops every entry from index n on, clearing them so they pin no row.
func (l *changeLog) truncate(n int) {
	for l.n > n {
		k := len(l.chunks) - 1
		c := l.chunks[k]
		drop := min(len(c), l.n-n)
		clear(c[len(c)-drop:])
		c = c[:len(c)-drop]
		l.n -= drop
		if len(c) == 0 {
			l.chunks[k] = nil
			l.chunks = l.chunks[:k]
			l.keep(c)
		} else {
			l.chunks[k] = c
		}
	}
}

// keep files an emptied chunk away for reuse, if there is room.
func (l *changeLog) keep(c []RowChange) {
	if len(l.spare) < keptChunks {
		l.spare = append(l.spare, c[:0])
	}
}

// reset empties the log, keeping up to keptChunks chunks for the next
// transaction. Entries are cleared, so the log pins no row.
func (l *changeLog) reset() {
	for i, c := range l.chunks {
		clear(c)
		l.keep(c)
		l.chunks[i] = nil
	}
	l.chunks = l.chunks[:0]
	l.n = 0
}

// all yields every entry in order, by pointer.
func (l *changeLog) all() iter.Seq[*RowChange] {
	return func(yield func(*RowChange) bool) {
		for _, c := range l.chunks {
			for i := range c {
				if !yield(&c[i]) {
					return
				}
			}
		}
	}
}

// ptrSeq is all() over a plain slice.
func ptrSeq(s []RowChange) iter.Seq[*RowChange] {
	return func(yield func(*RowChange) bool) {
		for i := range s {
			if !yield(&s[i]) {
				return
			}
		}
	}
}

// flat is the entries from index from on, as one slice. It is a copy.
func (l *changeLog) flat(from int) []RowChange {
	if from >= l.n {
		return nil
	}
	out := make([]RowChange, 0, l.n-from)
	for i := from; i < l.n; {
		c := l.chunks[i/changeChunk]
		off := i % changeChunk
		out = append(out, c[off:]...)
		i += len(c) - off
	}
	return out
}

// compact keeps the entries keep returns true for, in order, and hands each of
// the others to drop.
func (l *changeLog) compact(keep func(*RowChange) bool, drop func(RowChange)) {
	found := false
	for c := range l.all() {
		if !keep(c) {
			found = true
			break
		}
	}
	if !found {
		return
	}
	var kept changeLog
	for c := range l.all() {
		if keep(c) {
			kept.push(*c)
		} else {
			drop(*c)
		}
	}
	l.reset()
	*l = kept
}
