package cluster

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"

	_ "github.com/samyfodil/musql/driver"
)

// raftStore is an election's raft log and stable store: a musql database,
// raft.musq, synced at every commit. A group's log carries its durable
// databases' commits; raft needs the term and vote on disk too, or a
// restarted node could vote twice in one term.
type raftStore struct {
	db *sql.DB
}

// errKeyNotFound is the text raft checks for a missing stable key.
var errKeyNotFound = errors.New("not found")

func openRaftStore(dir string) (*raftStore, error) {
	db, err := sql.Open("sqlite", filepath.Join(dir, "raft.musq"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &raftStore{db: db}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS logs(idx INTEGER PRIMARY KEY, term INTEGER NOT NULL, type INTEGER NOT NULL, data BLOB, ext BLOB, appended INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS stable(k BLOB PRIMARY KEY, v BLOB NOT NULL)`); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.adoptJSON(filepath.Join(dir, "raft.json")); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// adoptJSON moves the state an election kept in raft.json, before its log
// carried data, into the database.
func (s *raftStore) adoptJSON(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var d struct {
		Stable map[string][]byte
		Logs   []*raft.Log
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range d.Stable {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO stable(k, v) VALUES (?, ?)`, []byte(k), v); err != nil {
			return err
		}
	}
	if err := storeLogs(tx, d.Logs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return os.Remove(path)
}

func (s *raftStore) Close() error { return s.db.Close() }

func (s *raftStore) FirstIndex() (uint64, error) {
	var i uint64
	err := s.db.QueryRow(`SELECT coalesce(min(idx), 0) FROM logs`).Scan(&i)
	return i, err
}

func (s *raftStore) LastIndex() (uint64, error) {
	var i uint64
	err := s.db.QueryRow(`SELECT coalesce(max(idx), 0) FROM logs`).Scan(&i)
	return i, err
}

func (s *raftStore) GetLog(index uint64, log *raft.Log) error {
	var typ uint8
	var appended int64
	err := s.db.QueryRow(`SELECT term, type, data, ext, appended FROM logs WHERE idx = ?`, index).Scan(&log.Term, &typ, &log.Data, &log.Extensions, &appended)
	if errors.Is(err, sql.ErrNoRows) {
		return raft.ErrLogNotFound
	}
	if err != nil {
		return err
	}
	log.Index, log.Type, log.AppendedAt = index, raft.LogType(typ), time.Unix(0, appended)
	return nil
}

func (s *raftStore) StoreLog(log *raft.Log) error { return s.StoreLogs([]*raft.Log{log}) }

// StoreLogs appends logs, replacing any entries at or after the first one's
// index (raft overwrites a follower's conflicting tail).
func (s *raftStore) StoreLogs(logs []*raft.Log) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := storeLogs(tx, logs); err != nil {
		return err
	}
	return tx.Commit()
}

func storeLogs(tx *sql.Tx, logs []*raft.Log) error {
	if len(logs) == 0 {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM logs WHERE idx >= ?`, logs[0].Index); err != nil {
		return err
	}
	for _, l := range logs {
		if _, err := tx.Exec(`INSERT INTO logs(idx, term, type, data, ext, appended) VALUES (?, ?, ?, ?, ?, ?)`,
			l.Index, l.Term, uint8(l.Type), l.Data, l.Extensions, l.AppendedAt.UnixNano()); err != nil {
			return err
		}
	}
	return nil
}

func (s *raftStore) DeleteRange(lo, hi uint64) error {
	_, err := s.db.Exec(`DELETE FROM logs WHERE idx BETWEEN ? AND ?`, lo, hi)
	return err
}

func (s *raftStore) Set(key, val []byte) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO stable(k, v) VALUES (?, ?)`, key, val)
	return err
}

func (s *raftStore) Get(key []byte) ([]byte, error) {
	var v []byte
	err := s.db.QueryRow(`SELECT v FROM stable WHERE k = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errKeyNotFound
	}
	return v, err
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
