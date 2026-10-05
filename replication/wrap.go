package replication

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/samyfodil/musql/driver"
)

// Syncer is the shared CRDT state for one database. Open creates it, and every
// connection of the *sql.DB Open returns captures into it.
type Syncer struct {
	site  string
	store *Store // on a hookless apply connection: remote apply + reads
	clock *Clock // shared with store

	// mode is the mode this node was opened in; writer is Leader's isWriter,
	// nil in CRDT mode, where every node writes.
	mode   string
	writer func() bool
	// quorum is Leader's Quorum, nil without one.
	quorum *Quorum

	// rowidLo, rowidHi are this site's rowid range; see RowidRange.
	rowidLo, rowidHi uint64
	// high is the largest rowid in that range each table (by ID) has ever had
	// -- the floor the engine allocates above (rowidFloor).
	highMu sync.Mutex
	high   map[string]uint64

	mu   sync.Mutex
	node *Node // set in Phase 3 for propagation; nil = anti-entropy only

	// cat is the last installed catalog a capture read, under its version: a
	// capture reads the version inside its transaction and reuses cat only when
	// it matches (catalogIn).
	catMu  sync.Mutex
	cat    *catalog
	catVer string
}

// catalogIn is the installed catalog as conn's transaction sees it.
func (s *Syncer) catalogIn(ctx context.Context, conn *driver.Conn) (*catalog, string, error) {
	rows, err := conn.Query(ctx, `SELECT v FROM main._repl_meta WHERE k=?`, metaSchemaVer)
	if err != nil {
		return nil, "", err
	}
	ver := ""
	if len(rows) > 0 {
		ver = asString(rows[0][0])
	}
	s.catMu.Lock()
	cat, cached := s.cat, s.catVer
	s.catMu.Unlock()
	if cat != nil && cached == ver {
		return cat, ver, nil
	}
	cat = newCatalog()
	if ver != "" {
		rows, err := conn.Query(ctx, `SELECT v FROM main._repl_meta WHERE k=?`, metaCatalog)
		if err != nil {
			return nil, "", err
		}
		if cat, err = decodeCatalog(asBytes(rows[0][0])); err != nil {
			return nil, "", err
		}
	}
	s.setCatalog(cat, ver)
	return cat, ver, nil
}

func (s *Syncer) setCatalog(cat *catalog, ver string) {
	s.catMu.Lock()
	s.cat, s.catVer = cat, ver
	s.catMu.Unlock()
}

// propagate sends local ops to the transport if wired.
func (s *Syncer) propagate(ops []Op) {
	s.mu.Lock()
	n := s.node
	s.mu.Unlock()
	if n == nil {
		return
	}
	n.Broadcast(ops)
}

// Attach wires a Transport to this syncer and returns a Node driving it.
func (s *Syncer) Attach(tr Transport) *Node {
	n := NewNode(s.store, tr)
	s.mu.Lock()
	s.node = n
	s.mu.Unlock()
	return n
}

// Store returns the syncer's Store (apply connection).
func (s *Syncer) Store() *Store { return s.store }

// OpSource returns an OpSource view of this syncer's store.
func (s *Syncer) OpSource() OpSource { return syncerOpSource{s} }

type syncerOpSource struct{ s *Syncer }

func (o syncerOpSource) LocalVV() VersionVector { return o.s.store.LocalVV() }

func (o syncerOpSource) OpsSince(site string, fromSeq uint64, limit int) ([]Op, error) {
	return o.s.store.OpsSince(context.Background(), site, fromSeq, limit)
}

func (o syncerOpSource) SnapshotPage(part int, cursor []byte) (SnapshotPage, error) {
	return o.s.store.SnapshotPage(context.Background(), part, cursor)
}

func (o syncerOpSource) ExchangeAcks(acks Acks) Acks {
	o.s.store.MergeAcks(acks)
	return o.s.store.Acks()
}

func (o syncerOpSource) ReceiveOp(from PeerID, op Op) {
	if err := o.s.store.Ingest(context.Background(), op, false); err != nil {
		log.Printf("replication: ReceiveOp from %s ingest: %v", from, err)
	}
}

// Site returns the syncer's site id.
func (s *Syncer) Site() string { return s.site }

// openSites tracks every Syncer open in this process (prevents duplicate site ids).
var openSites = struct {
	mu sync.Mutex
	m  map[string]bool
}{m: map[string]bool{}}

// newSyncer builds the Syncer for site, which owns applyDB.
func newSyncer(ctx context.Context, applyDB *sql.DB, site string) (s *Syncer, err error) {
	openSites.mu.Lock()
	dup := openSites.m[site]
	openSites.m[site] = true
	openSites.mu.Unlock()
	if dup {
		return nil, fmt.Errorf("replication: a database with site %q is already open in this process", site)
	}
	defer func() {
		if err != nil {
			releaseSite(site)
		}
	}()
	store, err := OpenStore(ctx, applyDB, site)
	if err != nil {
		return nil, err
	}
	s = &Syncer{site: site, store: store, clock: store.Clock()}
	s.rowidLo, s.rowidHi = RowidRange(site)
	if err := store.genesis(ctx, s.rowidLo); err != nil {
		return nil, err
	}
	if err := store.resync(ctx); err != nil {
		return nil, err
	}
	if s.high, err = store.rowidHighs(ctx, s.rowidLo, s.rowidHi); err != nil {
		return nil, err
	}
	s.setCatalog(store.cat, store.ver) // rowidFloor needs it before any capture has run
	return s, nil
}

// rowidFloor is the per-table floor for this site's range (largest rowid ever had)
// so deleted row ids are never reused and never become new rows.
func (s *Syncer) rowidFloor(table string) uint64 {
	s.catMu.Lock()
	cat := s.cat
	s.catMu.Unlock()
	if cat == nil {
		return 0
	}
	t := cat.byName(table)
	if t == nil {
		return 0
	}
	s.highMu.Lock()
	defer s.highMu.Unlock()
	return s.high[t.ID]
}

// noteRowid raises tbl's floor to rowid, when rowid is in this site's range.
func (s *Syncer) noteRowid(tblID string, rowid int64) {
	if r := uint64(rowid); rowid > 0 && r >= s.rowidLo && r < s.rowidHi {
		s.highMu.Lock()
		if r > s.high[tblID] {
			s.high[tblID] = r
		}
		s.highMu.Unlock()
	}
}

func releaseSite(site string) {
	openSites.mu.Lock()
	delete(openSites.m, site)
	openSites.mu.Unlock()
}

// Close closes the syncer's apply handle and frees its site for another Open.
func (s *Syncer) Close() error {
	defer releaseSite(s.site)
	return s.store.db.Close()
}

// RowidRange is the rowid range for this site's auto-assigned ids: rowid = h<<32 | counter,
// where h is a hash of the site id (or explicit for "#r<n>" ids).
func RowidRange(site string) (lo, hi uint64) {
	h := uint64(0)
	if _, n := splitSite(site); n != 0 {
		h = uint64(n)
	} else {
		f := fnv.New32a()
		f.Write([]byte(site))
		h = uint64(f.Sum32())%(1<<30-1) + 1
	}
	return h << 32, (h + 1) << 32
}

// splitSite splits a site id "<base>#r<n>" into base and n; any other id returns itself and 0.
func splitSite(site string) (string, uint32) {
	i := strings.LastIndex(site, "#r")
	if i < 0 {
		return site, 0
	}
	n, err := strconv.ParseUint(site[i+2:], 10, 32)
	if err != nil || n == 0 || n >= 1<<30 {
		return site, 0
	}
	return site[:i], uint32(n)
}
