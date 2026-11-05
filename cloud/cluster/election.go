package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	gostream "github.com/libp2p/go-libp2p-gostream"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// Election picks one node of a group to write: a raft group over libp2p. The
// leader is the master. Without a StateMachine its log carries nothing but
// raft's own entries, and the data replicates by replication's Leader mode
// alone; with one, the log also carries what the master proposes, applied on
// every member in order.
type Election struct {
	r    *raft.Raft
	tr   *raft.NetworkTransport
	st   *raftStore
	sm   StateMachine
	self raft.ServerID
	// settle is how long a new leader waits before it writes, so the commits
	// the previous master made reach it first (replication's anti-entropy).
	settle time.Duration

	mu    sync.Mutex
	since time.Time // when this node became leader; zero when it is not
	// caughtUp is set once a leader has applied every entry before its term.
	caughtUp bool
	done     chan struct{}
	once     sync.Once
	err      error
}

// StateMachine applies what a group's log carries. Its state outlives a
// restart on its own (the databases' files), so raft does not restore a
// snapshot into it at start.
type StateMachine interface {
	// Apply applies an entry Propose made under key, on every member, in log
	// order; what it returns is what Propose returns on the master.
	Apply(key string, data []byte) error
	// Snapshot describes the state, for a member too far behind for the log.
	Snapshot() ([]byte, error)
	// Restore makes the state at least what a Snapshot described.
	Restore(snapshot []byte) error
	// Stop ends an Apply or Restore that is waiting, as the election closes.
	Stop()
}

// ElectionTiming sets raft's timeouts, and how often it compacts its log
// into a snapshot (every Snapshot entries, keeping the last Trailing); zero
// fields keep raft's defaults.
type ElectionTiming struct {
	Heartbeat, Election, Lease time.Duration
	Snapshot, Trailing         uint64
}

// Elect joins the election called name among members (peer IDs, this node's
// included), keeping its raft state under dir. A node that has no state yet
// starts the group with members; one that has keeps the membership it knows,
// which SetMembers changes. sm, when not nil, applies the log's entries.
func (n *Node) Elect(dir, name string, members []string, settle time.Duration, timing ElectionTiming, sm StateMachine) (*Election, error) {
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
	st, err := openRaftStore(dir)
	if err != nil {
		tr.Close()
		return nil, err
	}
	fail := func(err error) (*Election, error) {
		tr.Close()
		st.Close()
		return nil, err
	}
	snaps, err := raft.NewFileSnapshotStoreWithLogger(dir, 2, logger)
	if err != nil {
		return fail(err)
	}
	conf := raft.DefaultConfig()
	conf.LocalID = raft.ServerID(n.ID())
	conf.Logger = logger
	conf.NoSnapshotRestoreOnStart = true
	if timing.Heartbeat > 0 {
		conf.HeartbeatTimeout = timing.Heartbeat
	}
	if timing.Election > 0 {
		conf.ElectionTimeout = timing.Election
	}
	if timing.Lease > 0 {
		conf.LeaderLeaseTimeout = timing.Lease
	}
	if timing.Snapshot > 0 {
		conf.SnapshotThreshold, conf.SnapshotInterval = timing.Snapshot, 100*time.Millisecond
	}
	if timing.Trailing > 0 {
		conf.TrailingLogs = timing.Trailing
	}
	has, err := raft.HasExistingState(st, st, snaps)
	if err != nil {
		return fail(err)
	}
	var fsm raft.FSM = noFSM{}
	if sm != nil {
		fsm = smFSM{sm}
	}
	r, err := raft.NewRaft(conf, fsm, st, st, snaps, tr)
	if err != nil {
		return fail(err)
	}
	e := &Election{r: r, tr: tr, st: st, sm: sm, self: conf.LocalID, settle: settle, done: make(chan struct{})}
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

// watch keeps since, when this node became leader, and caughtUp.
func (e *Election) watch() {
	ch := e.r.LeaderCh()
	for {
		select {
		case <-e.done:
			return
		case leader := <-ch:
			e.mu.Lock()
			e.since, e.caughtUp = time.Time{}, false
			if leader {
				e.since = time.Now()
				go e.catchUp(e.since)
			}
			e.mu.Unlock()
		}
	}
}

// catchUp sets caughtUp once this node, leader since since, has applied
// every entry the log held when it was elected (a raft Barrier).
func (e *Election) catchUp(since time.Time) {
	for {
		err := e.r.Barrier(10 * time.Second).Error()
		e.mu.Lock()
		current := e.since.Equal(since)
		if err == nil && current {
			e.caughtUp = true
		}
		e.mu.Unlock()
		if err == nil || !current {
			return
		}
		select {
		case <-e.done:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Writer reports whether this node is the group's master: raft's leader, with
// every entry before its term applied, for long enough to have the previous
// master's last commits.
func (e *Election) Writer() bool {
	e.mu.Lock()
	since, caughtUp := e.since, e.caughtUp
	e.mu.Unlock()
	return caughtUp && time.Since(since) >= e.settle && e.r.State() == raft.Leader
}

// ErrNotLeader is a proposal on a node that is not, or stopped being, the
// group's leader: the entry may or may not be in the log.
var ErrNotLeader = errors.New("cluster: this node is not the group's leader")

// Propose appends data, under key, to the group's log on its leader, and
// returns once a majority of the members hold it and this node applied it,
// with what the StateMachine's Apply returned.
func (e *Election) Propose(key string, data []byte) error {
	f := e.r.ApplyLog(raft.Log{Data: data, Extensions: []byte(key)}, 10*time.Second)
	if err := f.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) || errors.Is(err, raft.ErrLeadershipTransferInProgress) {
			return fmt.Errorf("%w: %v", ErrNotLeader, err)
		}
		return err
	}
	if err, ok := f.Response().(error); ok {
		return err
	}
	return nil
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
		if e.sm != nil {
			e.sm.Stop()
		}
		e.err = e.r.Shutdown().Error()
		if err := e.tr.Close(); e.err == nil {
			e.err = err
		}
		if err := e.st.Close(); e.err == nil {
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

// smFSM is raft's FSM over a StateMachine.
type smFSM struct{ sm StateMachine }

func (f smFSM) Apply(l *raft.Log) any { return f.sm.Apply(string(l.Extensions), l.Data) }

func (f smFSM) Snapshot() (raft.FSMSnapshot, error) {
	b, err := f.sm.Snapshot()
	return bytesSnapshot(b), err
}

func (f smFSM) Restore(r io.ReadCloser) error {
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return f.sm.Restore(b)
}

type bytesSnapshot []byte

func (b bytesSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(b); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (bytesSnapshot) Release() {}

func (noSnapshot) Persist(sink raft.SnapshotSink) error { return sink.Close() }
func (noSnapshot) Release()                             {}

// logWriter sends raft's warnings to logf, a line each.
type logWriter func(string, ...any)

func (w logWriter) Write(p []byte) (int, error) {
	w("%s", bytes.TrimRight(p, "\r\n"))
	return len(p), nil
}
