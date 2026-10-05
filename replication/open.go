package replication

import (
	"context"
	"crypto/rand"
	"database/sql"
	sqldriver "database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samyfodil/musql/driver"
)

// Open opens the database at path as a replicated database and returns an
// ordinary *sql.DB. Every table in it replicates. A mode is required -- CRDT()
// or Leader(isWriter) -- and every node of one database must run the same one:
// the first recorded wins, and Open refuses another.
//
//	db, err := replication.Open(ctx, "app.db", replication.CRDT(), replication.WithTransport(newTransport))
//	defer db.Close()
//
// The node's site id is generated on first open and kept in the database
// (WithSite overrides it). With a transport, sync runs in the background --
// pushed ops are applied as they arrive and anti-entropy runs every
// WithSyncInterval -- and db.Close stops it. Without one, local writes are
// still captured into the op log, and a peer that later connects picks them up.
//
// A caller that wants to drive the sync loop itself passes WithTransport, which
// hands it the Syncer (Syncer.Attach, Node).
func Open(ctx context.Context, path string, opts ...Option) (*sql.DB, error) {
	_, db, err := openSyncer(ctx, path, opts...)
	return db, err
}

// openSyncer is Open, also returning the Syncer behind the *sql.DB.
func openSyncer(ctx context.Context, path string, opts ...Option) (*Syncer, *sql.DB, error) {
	cfg := config{interval: 2 * time.Second}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.mode == "" {
		return nil, nil, errors.New("replication: Open needs a mode: CRDT() or Leader(isWriter)")
	}
	// Apply/bookkeeping handle: it writes without capturing.
	applyDB, err := openApplyDB(path)
	if err != nil {
		return nil, nil, err
	}
	applyDB.SetMaxOpenConns(1)
	fail := func(err error) (*Syncer, *sql.DB, error) {
		applyDB.Close()
		return nil, nil, err
	}
	if err := InitSchema(ctx, applyDB); err != nil {
		return fail(err)
	}
	if err := setGuard(ctx, applyDB); err != nil {
		return fail(err)
	}
	site, err := siteID(ctx, applyDB, cfg.site, cfg.rowidRange)
	if err != nil {
		return fail(err)
	}
	s, err := newSyncer(ctx, applyDB, site)
	if err != nil {
		return fail(err)
	}
	s.mode, s.writer, s.quorum = cfg.mode, cfg.writer, cfg.quorum
	if err := s.store.recordMode(ctx, cfg.mode); err != nil {
		s.Close()
		return nil, nil, err
	}
	if s.quorum != nil && !s.quorum.s.CompareAndSwap(nil, s) {
		s.Close()
		return nil, nil, errors.New("replication: a Quorum serves one database")
	}

	base, err := (&driver.Driver{}).OpenConnector(path)
	if err != nil {
		s.Close()
		return nil, nil, err
	}
	c := &syncConnector{Connector: base, s: s, maxSize: cfg.maxSize}
	if cfg.transport != nil {
		runCtx, cancel := context.WithCancel(context.Background())
		tr, terr := cfg.transport(runCtx, s)
		if terr != nil {
			cancel()
			s.Close()
			return nil, nil, terr
		}
		node := s.Attach(tr)
		c.tr, c.cancel, c.done = tr, cancel, make(chan struct{})
		go func() {
			defer close(c.done)
			node.Run(runCtx, cfg.interval)
		}()
	}
	db := sql.OpenDB(c)
	c.db = db
	openDBs.Store(db, s)
	return s, db, nil
}

// openDBs maps each *sql.DB Open returned to its Syncer, for the calls that
// take the *sql.DB (Retire).
var openDBs sync.Map

// Option configures Open.
type Option func(*config)

type config struct {
	rowidRange uint32
	mode       string
	writer     func() bool
	quorum     *Quorum
	site       string
	interval   time.Duration
	transport  func(ctx context.Context, s *Syncer) (Transport, error)
	maxSize    int64
}

// WithSite sets this node's site id instead of the one Open generates and
// keeps in the database. It must be stable across restarts and unique across
// every node that syncs this database.
func WithSite(id string) Option { return func(c *config) { c.site = id } }

// The modes, as recorded in the replicated catalog.
const (
	modeCRDT         = "crdt"
	modeLeader       = "leader"
	modeLeaderQuorum = "leader-quorum"
)

// CRDT is the multi-writer mode: every node commits, and nodes are eventually
// consistent. Constraints are checked on a node's own writes; a peer's rows are
// applied without them, so every node converges even where a merge breaks a
// rule each write kept (README.md, "Two modes").
func CRDT() Option { return func(c *config) { c.mode, c.writer = modeCRDT, nil } }

// Leader is the single-writer mode: only the node isWriter reports true on
// commits changes, and the others follow it. Deciding which node that is --
// raft, a lease, configuration -- is the caller's; isWriter is asked at every
// commit, so the answer may change as the caller's consensus moves. A commit
// on any other node fails with ErrNotWriter and rolls back; TEMP tables stay
// writable everywhere.
//
// Without a Quorum, a commit is durable on the writer when it returns and
// reaches the others after it: a writer that dies first takes its last commits
// with it, and a new writer may start before it has them -- primary/replica.
// With one (NewQuorum), a commit returns only once the caller's consensus has
// committed it and this node has applied it -- the raft model -- and every node
// of the database must be opened with one.
func Leader(isWriter func() bool, q ...*Quorum) Option {
	return func(c *config) {
		c.mode, c.writer, c.quorum = modeLeader, isWriter, nil
		if len(q) > 0 && q[0] != nil {
			c.mode, c.quorum = modeLeaderQuorum, q[0]
		}
	}
}

// Quorum hands Leader mode's commits to the caller's consensus log. On the
// writer, a commit runs its statements, turns their changes into one entry,
// discards the transaction, and calls propose with the entry; the commit
// succeeds when propose returns nil and the entry has been applied on this
// node. The caller's log applies each committed entry on every node -- the
// writer included -- by calling Apply, so a node's tables change only through
// the log. Nothing is durable before a quorum has it, and a node that holds an
// entry holds every earlier one.
//
// propose must return only after the entry is committed and Apply has run on
// this node (hashicorp/raft's ApplyFuture.Error does both), and the caller must
// Open the database before its log replays entries into Apply. A node becoming
// the writer must have applied every committed entry first (a raft Barrier).
type Quorum struct {
	propose func(ctx context.Context, entry []byte) error
	s       atomic.Pointer[Syncer]
}

// NewQuorum returns a Quorum that proposes through propose.
func NewQuorum(propose func(ctx context.Context, entry []byte) error) *Quorum {
	return &Quorum{propose: propose}
}

// Apply applies one committed entry to this node's database. Call it from the
// consensus log's apply loop, on every node, in log order. An entry already
// applied (anti-entropy got it first) is a no-op.
func (q *Quorum) Apply(ctx context.Context, entry []byte) error {
	s := q.s.Load()
	if s == nil {
		return errors.New("replication: Quorum.Apply before Open")
	}
	var ops []Op
	if err := json.Unmarshal(entry, &ops); err != nil {
		return fmt.Errorf("replication: a quorum entry does not decode: %w", err)
	}
	for _, op := range ops {
		if err := s.store.Ingest(ctx, op, false); err != nil {
			return err
		}
	}
	return nil
}

// ErrNotWriter is a commit refused because Leader's isWriter says another node
// is the writer.
var ErrNotWriter = errors.New("replication: this node is not the writer")

// WithRowidRange assigns this node's rowid range instead of hashing its site
// id: auto-assigned rowids are n<<32 | counter, n in [1, 2^30). A caller that
// hands each node a distinct n -- it already decides who the nodes are -- makes
// two nodes' ranges collide never, where hashed ones collide with odds about
// sites^2/2^31 (and are then contained: README, "Rowid ranges").
//
// The range becomes part of the site id ("<id>#r<n>"), which is how every other
// node knows it, so it is set on the database's first Open and must match
// after. RowidRange reports a site's range.
func WithRowidRange(n uint32) Option { return func(c *config) { c.rowidRange = n } }

// WithMaxSize caps the database at n bytes: a write through the *sql.DB that
// would leave the file larger fails with "database or disk is full" ("PRAGMA
// max_size", set on each of its connections). Ops applied from peers are never
// refused -- an op refused here would fail on this node forever and it would
// never converge -- so in CRDT mode a node can pass its limit through merges;
// in Leader mode only the writer writes, and the limit is exact. The
// bookkeeping (op log, clock) is in the same file and counts.
func WithMaxSize(n int64) Option { return func(c *config) { c.maxSize = n } }

// WithSyncInterval sets how often a full anti-entropy round runs (default 2s).
func WithSyncInterval(d time.Duration) Option { return func(c *config) { c.interval = d } }

// WithTransport syncs over the Transport fn builds. fn is called once, with a
// context that db.Close cancels and the Syncer whose Store the transport serves
// peers from; a Transport that is an io.Closer is closed by db.Close, after the
// sync loop has stopped. examples/libp2p is one over libp2p.
func WithTransport(fn func(ctx context.Context, s *Syncer) (Transport, error)) Option {
	return func(c *config) { c.transport = fn }
}

// siteID is want if given, else the id kept in _repl_meta, else a fresh random
// one, which it keeps -- carrying rng, when set, as WithRowidRange does. A want
// or rng that differs from the kept id is an error: the op log's sequence
// numbers, and the rowids handed out, belong to the kept one.
func siteID(ctx context.Context, db *sql.DB, want string, rng uint32) (string, error) {
	if rng != 0 && rng >= 1<<30 {
		return "", fmt.Errorf("replication: rowid range %d is not in [1, 2^30)", rng)
	}
	var kept []byte
	err := db.QueryRowContext(ctx, `SELECT v FROM main._repl_meta WHERE k='site'`).Scan(&kept)
	switch {
	case err == nil:
		base, keptRng := splitSite(string(kept))
		if want != "" && want != base && want != string(kept) {
			return "", fmt.Errorf("replication: this database's site id is %q, not %q", kept, want)
		}
		if rng != 0 && rng != keptRng {
			return "", fmt.Errorf("replication: this database's site %q has rowid range %d, not %d", kept, keptRng, rng)
		}
		return string(kept), nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	}
	if want == "" {
		var b [16]byte
		if _, rerr := rand.Read(b[:]); rerr != nil {
			return "", rerr
		}
		want = hex.EncodeToString(b[:])
	}
	if rng != 0 {
		want = fmt.Sprintf("%s#r%d", want, rng)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO main._repl_meta(k, v) VALUES('site', ?)`, []byte(want)); err != nil {
		return "", err
	}
	return want, nil
}

// openApplyDB opens dsn through applyConnector: a handle that writes a
// replicated database without capturing -- what a Store runs on.
func openApplyDB(dsn string) (*sql.DB, error) {
	base, err := (&driver.Driver{}).OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(applyConnector{base}), nil
}

// guardReason is the capture guard a replicated database carries
// (driver.Conn.SetCaptureGuard): every connection but Open's own is refused
// writes to it, since a write it made would never be captured -- no peer would
// get it, and a schema change would leave the replicated catalog behind.
const guardReason = "replication: this database is replicated; write it through the *sql.DB replication.Open returned (writes through any other connection would never reach a peer)"

// setGuard records guardReason in db's file. Open calls it every time rather
// than once at creation, so a database replicated before the guard lived in the
// file gets one too.
func setGuard(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(dc any) error { return dc.(*driver.Conn).SetCaptureGuard(guardReason) })
}

// applyConnector is the apply handle's connector. Its connections write peers'
// rows without capturing them (driver.Conn.AllowUncapturedWrites) and without
// CHECK constraints: each write was checked where it was made, and a row merged
// from two of them may break a CHECK neither did -- applied, every node holds
// it; refused, the op would fail on every node forever. Foreign keys are off on
// it as on any connection that does not turn them on.
type applyConnector struct{ sqldriver.Connector }

func (c applyConnector) Connect(ctx context.Context) (sqldriver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	conn.(*driver.Conn).AllowUncapturedWrites()
	if err := conn.(*driver.Conn).Exec(ctx, `PRAGMA ignore_check_constraints = ON`); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// syncConnector is the *sql.DB's connector: every connection it hands out
// captures into the Syncer, and closing it -- which db.Close does, as a
// Connector that is an io.Closer -- stops background sync and closes the Syncer.
type syncConnector struct {
	sqldriver.Connector
	db      *sql.DB
	s       *Syncer
	tr      Transport
	cancel  context.CancelFunc
	done    chan struct{}
	maxSize int64 // WithMaxSize
	once    sync.Once
}

// Connect hands out a driver connection with capture attached before it is
// ever used.
func (c *syncConnector) Connect(ctx context.Context) (sqldriver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	c.s.attach(conn.(*driver.Conn))
	if c.maxSize > 0 {
		if err := conn.(*driver.Conn).Exec(ctx, fmt.Sprintf("PRAGMA max_size = %d", c.maxSize)); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func (c *syncConnector) Close() error {
	var err error
	c.once.Do(func() {
		openDBs.Delete(c.db)
		if c.cancel != nil {
			c.cancel()
			<-c.done
			if cl, ok := c.tr.(io.Closer); ok {
				err = cl.Close()
			}
		}
		err = errors.Join(err, c.s.Close())
	})
	return err
}
