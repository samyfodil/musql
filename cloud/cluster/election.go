package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	gostream "github.com/libp2p/go-libp2p-gostream"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// Election picks one node of a group to write: a raft group over libp2p whose
// log carries nothing but raft's own entries. The leader is the master; the
// data replicates by replication's Leader mode, not through this log.
type Election struct {
	r    *raft.Raft
	tr   *raft.NetworkTransport
	st   *raftStore
	self raft.ServerID
	// settle is how long a new leader waits before it writes, so the commits
	// the previous master made reach it first (replication's anti-entropy).
	settle time.Duration

	mu    sync.Mutex
	since time.Time // when this node became leader; zero when it is not
	done  chan struct{}
	once  sync.Once
	err   error
}

// ElectionTiming sets raft's timeouts; zero fields keep raft's defaults.
type ElectionTiming struct {
	Heartbeat, Election, Lease time.Duration
}

// Elect joins the election called name among members (peer IDs, this node's
// included), keeping its raft state under dir. A node that has no state yet
// starts the group with members; one that has keeps the membership it knows,
// which SetMembers changes.
func (n *Node) Elect(dir, name string, members []string, settle time.Duration, timing ElectionTiming) (*Election, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l, err := gostream.Listen(n.host, raftProtocol(name))
	if err != nil {
		return nil, err
	}
	sl := &streamLayer{Listener: l, n: n, proto: raftProtocol(name)}
	logger := hclog.New(&hclog.LoggerOptions{Name: "raft " + name, Output: logWriter(n.logf), Level: hclog.Warn})
	tr := raft.NewNetworkTransportWithLogger(sl, 3, 10*time.Second, logger)
	st, err := openRaftStore(filepath.Join(dir, "raft.json"))
	if err != nil {
		tr.Close()
		return nil, err
	}
	snaps, err := raft.NewFileSnapshotStoreWithLogger(dir, 2, logger)
	if err != nil {
		tr.Close()
		return nil, err
	}
	conf := raft.DefaultConfig()
	conf.LocalID = raft.ServerID(n.ID())
	conf.Logger = logger
	if timing.Heartbeat > 0 {
		conf.HeartbeatTimeout = timing.Heartbeat
	}
	if timing.Election > 0 {
		conf.ElectionTimeout = timing.Election
	}
	if timing.Lease > 0 {
		conf.LeaderLeaseTimeout = timing.Lease
	}
	has, err := raft.HasExistingState(st, st, snaps)
	if err != nil {
		tr.Close()
		return nil, err
	}
	r, err := raft.NewRaft(conf, noFSM{}, st, st, snaps, tr)
	if err != nil {
		tr.Close()
		return nil, err
	}
	e := &Election{r: r, tr: tr, st: st, self: conf.LocalID, settle: settle, done: make(chan struct{})}
	if !has {
		if err := r.BootstrapCluster(configuration(members)).Error(); err != nil && !errors.Is(err, raft.ErrCantBootstrap) {
			e.Close()
			return nil, err
		}
	}
	go e.watch()
	return e, nil
}

func raftProtocol(name string) protocol.ID { return protocol.ID("/musql-cloud/raft/" + name + "/1") }

func configuration(members []string) raft.Configuration {
	var c raft.Configuration
	for _, id := range members {
		c.Servers = append(c.Servers, raft.Server{ID: raft.ServerID(id), Address: raft.ServerAddress(id)})
	}
	return c
}

// watch keeps since: when this node became leader.
func (e *Election) watch() {
	ch := e.r.LeaderCh()
	for {
		select {
		case <-e.done:
			return
		case leader := <-ch:
			e.mu.Lock()
			e.since = time.Time{}
			if leader {
				e.since = time.Now()
			}
			e.mu.Unlock()
		}
	}
}

// Writer reports whether this node is the group's master: raft's leader, for
// long enough to have the previous master's last commits.
func (e *Election) Writer() bool {
	e.mu.Lock()
	since := e.since
	e.mu.Unlock()
	return !since.IsZero() && time.Since(since) >= e.settle && e.r.State() == raft.Leader
}

// Leader is the peer ID of the group's leader as this node knows it, "" when
// there is none.
func (e *Election) Leader() string {
	_, id := e.r.LeaderWithID()
	return string(id)
}

// SetMembers makes the group's membership members, when this node leads it:
// a node placed in the group is added, one no longer placed is removed.
func (e *Election) SetMembers(members []string) error {
	if e.r.State() != raft.Leader {
		return nil
	}
	f := e.r.GetConfiguration()
	if err := f.Error(); err != nil {
		return err
	}
	have := map[string]bool{}
	for _, s := range f.Configuration().Servers {
		have[string(s.ID)] = true
	}
	for _, id := range members {
		if !have[id] {
			if err := e.r.AddVoter(raft.ServerID(id), raft.ServerAddress(id), 0, 10*time.Second).Error(); err != nil {
				return fmt.Errorf("add %s: %w", id, err)
			}
		}
	}
	// This node goes last: removing the leader ends its term.
	var removeSelf bool
	for id := range have {
		if slices.Contains(members, id) {
			continue
		}
		if raft.ServerID(id) == e.self {
			removeSelf = true
			continue
		}
		if err := e.r.RemoveServer(raft.ServerID(id), 0, 10*time.Second).Error(); err != nil {
			return fmt.Errorf("remove %s: %w", id, err)
		}
	}
	if removeSelf {
		return e.r.RemoveServer(e.self, 0, 10*time.Second).Error()
	}
	return nil
}

// Close leaves the election.
func (e *Election) Close() error {
	e.once.Do(func() {
		close(e.done)
		e.err = e.r.Shutdown().Error()
		if err := e.tr.Close(); e.err == nil {
			e.err = err
		}
	})
	return e.err
}

// streamLayer is raft's transport over libp2p streams on one protocol: the
// address of a server is its peer ID.
type streamLayer struct {
	net.Listener
	n     *Node
	proto protocol.ID
}

func (s *streamLayer) Dial(addr raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	id, err := peer.Decode(string(addr))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return gostream.Dial(ctx, s.n.host, id, s.proto)
}

// noFSM is the election's state machine: its log carries only raft's own
// entries, so there is nothing to apply or snapshot.
type noFSM struct{}

func (noFSM) Apply(*raft.Log) any                 { return nil }
func (noFSM) Snapshot() (raft.FSMSnapshot, error) { return noSnapshot{}, nil }
func (noFSM) Restore(r io.ReadCloser) error       { return r.Close() }

type noSnapshot struct{}

func (noSnapshot) Persist(sink raft.SnapshotSink) error { return sink.Close() }
func (noSnapshot) Release()                             {}

// logWriter sends raft's warnings to logf, a line each.
type logWriter func(string, ...any)

func (w logWriter) Write(p []byte) (int, error) {
	w("%s", bytes.TrimRight(p, "\r\n"))
	return len(p), nil
}
