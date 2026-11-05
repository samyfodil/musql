package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/samyfodil/musql/replication"
)

// groupLog applies a group's raft log on this node: each entry is a commit of
// one of the group's durable databases, under its ID, handed to that
// database's Quorum.
type groupLog struct {
	s     *Server
	group string
	stop  chan struct{}
	once  sync.Once
}

func newGroupLog(s *Server, group string) *groupLog {
	return &groupLog{s: s, group: group, stop: make(chan struct{})}
}

var errStopped = errors.New("the group's election closed")

func (g *groupLog) Apply(id string, entry []byte) error {
	q, err := g.ready(id, nil)
	if err != nil || q == nil {
		return err
	}
	return q.Apply(context.Background(), entry)
}

// ready waits until database id is open here, past its first sync and
// holding what want names, and returns its Quorum; nil when the database is
// gone. An entry is never skipped for a database this node has yet to open:
// the node that proposed it may stop before any other node's copy has it.
func (g *groupLog) ready(id string, want replication.VersionVector) (*replication.Quorum, error) {
	var asked time.Time
	for {
		if q := g.open(id); q != nil && q.Covers(want) {
			return q, nil
		}
		if time.Since(asked) >= time.Second {
			asked = time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			gone, err := g.s.store.DatabaseGone(ctx, id)
			cancel()
			if err == nil && gone {
				return nil, nil
			}
		}
		select {
		case <-g.stop:
			return nil, errStopped
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// open is id's Quorum when it is open here and past its first sync.
func (g *groupLog) open(id string) *replication.Quorum {
	g.s.cl.mu.Lock()
	defer g.s.cl.mu.Unlock()
	c := g.s.cl.dbs[id]
	if c == nil || c.quorum == nil || (c.syncing && !replication.Synced(c.db)) {
		return nil
	}
	return c.quorum
}

// Snapshot is what each of the group's durable databases here holds. It
// waits for their first syncs: until then a copy holds less than the log
// applied.
func (g *groupLog) Snapshot() ([]byte, error) {
	g.s.cl.mu.Lock()
	qs := map[string]*replication.Quorum{}
	for id, c := range g.s.cl.dbs {
		if c.group != g.group || c.quorum == nil {
			continue
		}
		if c.syncing && !replication.Synced(c.db) {
			g.s.cl.mu.Unlock()
			return nil, errors.New("a database of the group is in its first sync")
		}
		qs[id] = c.quorum
	}
	g.s.cl.mu.Unlock()
	held := map[string]replication.VersionVector{}
	for id, q := range qs {
		held[id] = q.Frontier()
	}
	return json.Marshal(held)
}

// Restore waits until this node's copies hold what a member's did at its
// snapshot, which replication's anti-entropy brings from the other nodes.
func (g *groupLog) Restore(b []byte) error {
	var held map[string]replication.VersionVector
	if err := json.Unmarshal(b, &held); err != nil {
		return err
	}
	for id, vv := range held {
		if _, err := g.ready(id, vv); err != nil {
			return err
		}
	}
	return nil
}

func (g *groupLog) Stop() { g.once.Do(func() { close(g.stop) }) }
