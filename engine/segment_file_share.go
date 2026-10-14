package engine

import "sync"

// ONE MAPPING OF A SEGMENT FILE PER PROCESS.
//
// Every connection held its own session, and every session mapped the file
// itself: twelve readers of one 10M-row file were twelve mappings, and each
// took its own page fault the first time it touched each page -- 130,000 of
// them over 600,000 point reads on macOS, most of a read's tail. One mapping
// shared by all of them faults each page once.
//
// Sharing is sound because a segment file's bytes never change: a rewrite
// writes a new file and renames it over the path, so a new inode is a new
// mapping. The key is the file's identity AND its size and mtime, so a file
// changed in place -- which this engine does not do to a file in use -- is not
// mistaken for the one already mapped. Each OpenSegmentFile gets a handle of
// its own over the shared mapping, and the mapping is released when the last
// handle closes; segments are parsed per handle, as before.

// segFileKey identifies a segment file's bytes: its device and inode, and its
// size and mtime.
type segFileKey struct {
	dev, ino    uint64
	size, mtime int64
}

// sharedSegFile is one mapping and how many handles hold it.
type sharedSegFile struct {
	key  segFileKey
	base *SegmentFile // never handed out: each opener gets a copy
	refs int
}

var sharedSegFiles = struct {
	sync.Mutex
	m map[segFileKey]*sharedSegFile
}{m: map[segFileKey]*sharedSegFile{}}

// handle is a new handle over s; the caller holds sharedSegFiles' lock.
func (s *sharedSegFile) handle() *SegmentFile {
	s.refs++
	h := *s.base
	h.shared, h.closed = s, false
	return &h
}

// sharedSegFileFor is a new handle over the mapping of key, or nil.
func sharedSegFileFor(key segFileKey) *SegmentFile {
	sharedSegFiles.Lock()
	defer sharedSegFiles.Unlock()
	if s := sharedSegFiles.m[key]; s != nil {
		return s.handle()
	}
	return nil
}

// shareSegFile registers f, just opened, as key's mapping and returns a handle
// over it -- or, when another opener registered one first, a handle over that
// one, closing f's.
func shareSegFile(key segFileKey, f *SegmentFile) *SegmentFile {
	sharedSegFiles.Lock()
	defer sharedSegFiles.Unlock()
	s := sharedSegFiles.m[key]
	if s != nil {
		f.m.Close()
	} else {
		s = &sharedSegFile{key: key, base: f}
		sharedSegFiles.m[key] = s
	}
	return s.handle()
}

// release lets go of one handle's hold, unmapping with the last.
func (s *sharedSegFile) release() error {
	sharedSegFiles.Lock()
	defer sharedSegFiles.Unlock()
	if s.refs--; s.refs > 0 {
		return nil
	}
	if sharedSegFiles.m[s.key] == s {
		delete(sharedSegFiles.m, s.key)
	}
	return s.base.m.Close()
}
