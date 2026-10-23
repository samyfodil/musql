package engine

// CommitInfo is a database file's committed state, as WithCommitHook and
// CommitState report it.
//
// The state is the segment file at Path plus the first DeltaEnd bytes of its
// delta (Path + ".delta"): every batch up to the last committed one. Those
// bytes are never rewritten while the segment file stands -- an append only
// overwrites what lies past them -- and the segment file is never written in
// place, only replaced by rename. A copy of the two is the database.
type CommitInfo struct {
	Path string
	// Counter is the file's change counter: one more per commit.
	Counter uint32
	// DeltaEnd is 0 when there is no delta.
	DeltaEnd int64
}

var commitHook func(CommitInfo)

// CommitState calls fn with path's committed state, holding the shared lock so
// no commit lands until fn returns.
func CommitState(path string, fn func(CommitInfo) error) error {
	return withSegmentReadLock(path, func() error {
		info, err := commitStateUnlocked(path)
		if err != nil {
			return err
		}
		return fn(info)
	})
}

func commitStateUnlocked(path string) (CommitInfo, error) {
	f, err := OpenSegmentFile(path)
	if err != nil {
		return CommitInfo{}, err
	}
	baseCtr, basePages := f.srcChangeCounter, f.srcPageCount
	f.Close()
	m, data, err := mapSegDelta(segDeltaPath(path))
	if err != nil {
		return CommitInfo{}, err
	}
	if m != nil {
		defer m.Close()
	}
	info := CommitInfo{Path: path, Counter: baseCtr}
	if len(data) < segDeltaHdrSize {
		return info, nil // absent, or too short to hold a batch: the file stands alone
	}
	st, err := replaySegDelta(data, baseCtr, basePages, nil)
	if err != nil {
		return CommitInfo{}, err
	}
	info.Counter, info.DeltaEnd = st.endCtr, st.bytes
	return info, nil
}

// ReplaceState runs fn holding path's exclusive write lock, as a commit does,
// for a caller that changes the file pair itself: a replica appending the
// delta bytes its primary committed, or renaming in a new segment file. No
// reader sees the pair while fn runs, and every session re-checks the files
// at its next statement. fn must leave the pair a committed state; path need
// not exist yet.
func ReplaceState(path string, fn func() error) error {
	return withSegmentWriteLock(path, func() error {
		done, err := segChangeBegin(path)
		if err != nil {
			return err
		}
		defer done()
		return fn()
	})
}
