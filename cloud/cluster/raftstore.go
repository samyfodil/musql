package cluster

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"sync"

	"github.com/hashicorp/raft"
)

// raftStore is an election's raft log and stable store in one file, written
// whole on every change: an election's log gains an entry per election or
// membership change and nothing else, so the file stays small. raft needs the
// term and vote on disk, or a restarted node could vote twice in one term.
type raftStore struct {
	path string
	mu   sync.Mutex
	d    raftData
}

type raftData struct {
	Stable map[string][]byte
	Logs   []*raft.Log // ascending, contiguous
}

// errKeyNotFound is the text raft checks for a missing stable key.
var errKeyNotFound = errors.New("not found")

func openRaftStore(path string) (*raftStore, error) {
	s := &raftStore{path: path, d: raftData{Stable: map[string][]byte{}}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.d); err != nil {
		return nil, err
	}
	if s.d.Stable == nil {
		s.d.Stable = map[string][]byte{}
	}
	return s, nil
}

// save writes the file whole: a new file, synced, renamed over the old.
func (s *raftStore) save() error {
	b, err := json.Marshal(s.d)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *raftStore) FirstIndex() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.d.Logs) == 0 {
		return 0, nil
	}
	return s.d.Logs[0].Index, nil
}

func (s *raftStore) LastIndex() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.d.Logs) == 0 {
		return 0, nil
	}
	return s.d.Logs[len(s.d.Logs)-1].Index, nil
}

func (s *raftStore) GetLog(index uint64, log *raft.Log) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.d.Logs) == 0 || index < s.d.Logs[0].Index || index > s.d.Logs[len(s.d.Logs)-1].Index {
		return raft.ErrLogNotFound
	}
	*log = *s.d.Logs[index-s.d.Logs[0].Index]
	return nil
}

func (s *raftStore) StoreLog(log *raft.Log) error { return s.StoreLogs([]*raft.Log{log}) }

// StoreLogs appends logs, replacing any entries at or after the first one's
// index (raft overwrites a follower's conflicting tail).
func (s *raftStore) StoreLogs(logs []*raft.Log) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range logs {
		c := *l
		if n := len(s.d.Logs); n > 0 && c.Index <= s.d.Logs[n-1].Index {
			if c.Index < s.d.Logs[0].Index {
				s.d.Logs = s.d.Logs[:0]
			} else {
				s.d.Logs = s.d.Logs[:c.Index-s.d.Logs[0].Index]
			}
		}
		s.d.Logs = append(s.d.Logs, &c)
	}
	return s.save()
}

func (s *raftStore) DeleteRange(lo, hi uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.d.Logs[:0]
	for _, l := range s.d.Logs {
		if l.Index < lo || l.Index > hi {
			kept = append(kept, l)
		}
	}
	s.d.Logs = kept
	return s.save()
}

func (s *raftStore) Set(key, val []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Stable[string(key)] = append([]byte(nil), val...)
	return s.save()
}

func (s *raftStore) Get(key []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.d.Stable[string(key)]
	if !ok {
		return nil, errKeyNotFound
	}
	return v, nil
}

func (s *raftStore) SetUint64(key []byte, val uint64) error {
	return s.Set(key, binary.BigEndian.AppendUint64(nil, val))
}

func (s *raftStore) GetUint64(key []byte) (uint64, error) {
	v, err := s.Get(key)
	if err != nil {
		return 0, err
	}
	if len(v) != 8 {
		return 0, errors.New("raft store: not a uint64")
	}
	return binary.BigEndian.Uint64(v), nil
}
