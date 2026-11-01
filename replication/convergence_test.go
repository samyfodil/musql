package replication

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	convSeed  = flag.Int64("convergence.seed", 0, "run TestConvergenceRandom on this seed alone")
	convSeeds = flag.Int("convergence.seeds", 0, "seeds TestConvergenceRandom runs (default depends on -short)")
	convSteps = flag.Int("convergence.steps", 0, "steps each seed takes (default depends on -short)")
)

// TestConvergenceRandom: three CRDT nodes write at random while the network
// drops, delays, reorders and repeats messages, partitions come and go, nodes
// restart, and schema changes race their rows. After a final sync every node
// holds the same tables, withheld rows, version vector and clock, the clock is
// the last-writer-wins fold of the log, and a fresh node fed the log in random
// order lands on the same state.
//
// A failing seed replays with -convergence.seed=N (-convergence.steps
// shortens it); each run logs every step. For a long hunt:
// -convergence.seeds=100 -convergence.steps=250.
func TestConvergenceRandom(t *testing.T) {
	seeds, steps := 30, 120
	if testing.Short() {
		seeds, steps = 20, 60
	}
	if *convSeeds > 0 {
		seeds = *convSeeds
	}
	if *convSteps > 0 {
		steps = *convSteps
	}
	first, last := int64(1), int64(seeds)
	if *convSeed != 0 {
		first, last = *convSeed, *convSeed
	}

	logged := &convLog{}
	prev := log.Writer()
	log.SetOutput(logged)
	var mu sync.Mutex
	var total convStats
	t.Cleanup(func() {
		log.SetOutput(prev)
		if s := logged.String(); strings.Contains(s, "replication:") {
			t.Errorf("the replication package logged errors:\n%s", s)
		}
		t.Logf("all seeds: %+v", total)
		if *convSeed != 0 || *convSteps > 0 || t.Failed() {
			return
		}
		for what, n := range map[string]int{
			"committed writes":            total.writes,
			"deletes that hit a row":      total.deletes,
			"resurrected keys":            total.resurrected,
			"rows withheld at the end":    total.withheld,
			"pushes held back":            total.held,
			"pushes dropped":              total.dropped,
			"late tables":                 total.lateTables,
			"late columns":                total.lateColumns,
			"equal HLCs from two sites":   total.ties,
			"clocks that had to catch up": total.skews,
			"values freed in a batch":     total.frees,
		} {
			if n == 0 {
				t.Errorf("the run never produced %s: the generator no longer reaches what this test is for", what)
			}
		}
	})
	for seed := first; seed <= last; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			c := newConvCluster(t, seed)
			t.Cleanup(func() {
				mu.Lock()
				total.add(c.stats)
				mu.Unlock()
				if t.Failed() {
					t.Logf("replay: go test ./replication -run 'TestConvergenceRandom/seed=%d$' -convergence.seed=%d -convergence.steps=%d -v", seed, seed, steps)
				}
			})
			c.setup()
			for i := range steps {
				c.step(i)
				c.checkHeld()
			}
			c.settle()
			c.verify()
			c.replay()
			t.Logf("stats: %+v", c.stats)
		})
	}
}

// convLog collects what the standard logger is handed, which in this package
// is only errors.
type convLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *convLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *convLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type convStats struct {
	writes, refused, deletes, resurrected int
	pushes, held, dropped, partial        int
	lateTables, lateColumns, withheld     int
	ties, skews, frees                    int
}

func (s *convStats) add(o convStats) {
	s.writes += o.writes
	s.refused += o.refused
	s.deletes += o.deletes
	s.resurrected += o.resurrected
	s.pushes += o.pushes
	s.held += o.held
	s.dropped += o.dropped
	s.partial += o.partial
	s.lateTables += o.lateTables
	s.lateColumns += o.lateColumns
	s.withheld += o.withheld
	s.ties += o.ties
	s.skews += o.skews
	s.frees += o.frees
}

// The data model. Keys and UNIQUE values come from small pools, so writes on
// different nodes meet.

type convCol struct {
	name    string
	gen     func(*rand.Rand) any
	notNull bool
	dflt    bool
}

func (c convCol) value(r *rand.Rand) any {
	if !c.notNull && r.IntN(6) == 0 {
		return nil
	}
	return c.gen(r)
}

func (c convCol) must() bool { return c.notNull && !c.dflt }

type convTable struct {
	name string
	ddl  string
	keys []convCol
	cols []convCol
}

func (tb *convTable) with(col convCol) *convTable {
	return &convTable{name: tb.name, keys: tb.keys, cols: append(slices.Clone(tb.cols), col)}
}

func (tb *convTable) randKey(r *rand.Rand) []any {
	key := make([]any, len(tb.keys))
	for i, k := range tb.keys {
		key[i] = k.gen(r)
	}
	return key
}

func (tb *convTable) where() string {
	conds := make([]string, len(tb.keys))
	for i, k := range tb.keys {
		conds[i] = k.name + " = ?"
	}
	return strings.Join(conds, " AND ")
}

// insert is an INSERT of key with the columns that must be there, some of the
// others, and force.
func (tb *convTable) insert(r *rand.Rand, key []any, force string) convStmt {
	names := make([]string, 0, len(tb.keys)+len(tb.cols))
	for _, k := range tb.keys {
		names = append(names, k.name)
	}
	args := slices.Clone(key)
	for _, c := range tb.cols {
		if c.must() || c.name == force || r.IntN(10) < 6 {
			names = append(names, c.name)
			args = append(args, c.value(r))
		}
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ")
	return convStmt{q: fmt.Sprintf("INSERT INTO %s(%s) VALUES(%s)", tb.name, strings.Join(names, ", "), marks), args: args}
}

func (tb *convTable) update(r *rand.Rand, key []any) convStmt {
	var sets []string
	var args []any
	for _, i := range r.Perm(len(tb.cols))[:1+r.IntN(min(3, len(tb.cols)))] {
		sets = append(sets, tb.cols[i].name+" = ?")
		args = append(args, tb.cols[i].value(r))
	}
	return convStmt{q: "UPDATE " + tb.name + " SET " + strings.Join(sets, ", ") + " WHERE " + tb.where(), args: append(args, key...)}
}

func (tb *convTable) remove(key []any) convStmt {
	return convStmt{q: "DELETE FROM " + tb.name + " WHERE " + tb.where(), args: key, table: tb.name, key: key}
}

type convStmt struct {
	q     string
	args  []any
	table string // set on a DELETE, with the key it deletes
	key   []any
}

func convSQL(q string, args ...any) convStmt { return convStmt{q: q, args: args} }

var convWords = []string{"", "a", "b", "it's", "ünï", "x y", "10", "zz", "two\nlines"}

func convText(r *rand.Rand) any { return convWords[r.IntN(len(convWords))] }

func convInt(r *rand.Rand) any {
	if r.IntN(6) == 0 {
		return []int64{0, -1, 1 << 31, -(1 << 40), 1 << 62}[r.IntN(5)]
	}
	return int64(r.IntN(30) - 5)
}

func convReal(r *rand.Rand) any { return float64(r.IntN(400)-100) / 8 }

func convBlob(r *rand.Rand) any {
	b := make([]byte, r.IntN(5))
	for i := range b {
		b[i] = byte(r.IntN(256))
	}
	return b
}

func convAny(r *rand.Rand) any {
	switch r.IntN(4) {
	case 0:
		return convInt(r)
	case 1:
		return convReal(r)
	case 2:
		return convText(r)
	}
	return convBlob(r)
}

func convIntPool(n int) func(*rand.Rand) any {
	return func(r *rand.Rand) any { return int64(1 + r.IntN(n)) }
}

func convTextPool(prefix string, n int) func(*rand.Rand) any {
	return func(r *rand.Rand) any { return prefix + strconv.Itoa(r.IntN(n)) }
}

// The base tables span the shapes the apply path tells apart: rowid and
// composite TEXT keys with UNIQUE columns, a TEXT key, and an unindexed rowid
// table written in batches.
var (
	convItems = &convTable{
		name: "items",
		ddl:  `CREATE TABLE items(id INTEGER PRIMARY KEY, name TEXT NOT NULL, code TEXT UNIQUE, qty INTEGER, price REAL, tag TEXT, bin BLOB, misc)`,
		keys: []convCol{{name: "id", gen: convIntPool(10)}},
		cols: []convCol{
			{name: "name", gen: convText, notNull: true},
			{name: "code", gen: convTextPool("c", 12)},
			{name: "qty", gen: convInt},
			{name: "price", gen: convReal},
			{name: "tag", gen: convText},
			{name: "bin", gen: convBlob},
			{name: "misc", gen: convAny},
		},
	}
	convTags = &convTable{
		name: "tags",
		ddl:  `CREATE TABLE tags(owner TEXT NOT NULL, label TEXT NOT NULL, pos INTEGER NOT NULL, weight REAL, note TEXT, PRIMARY KEY(owner, label), UNIQUE(owner, pos))`,
		keys: []convCol{{name: "owner", gen: convTextPool("o", 3)}, {name: "label", gen: convTextPool("l", 4)}},
		cols: []convCol{
			{name: "pos", gen: convIntPool(8), notNull: true},
			{name: "weight", gen: convReal},
			{name: "note", gen: convText},
		},
	}
	convNotes = &convTable{
		name: "notes",
		ddl:  `CREATE TABLE notes(slug TEXT PRIMARY KEY, body TEXT NOT NULL, hits INTEGER)`,
		keys: []convCol{{name: "slug", gen: convTextPool("s", 8)}},
		cols: []convCol{
			{name: "body", gen: convText, notNull: true},
			{name: "hits", gen: convInt},
		},
	}
	convPlain = &convTable{
		name: "plain",
		ddl:  `CREATE TABLE plain(id INTEGER PRIMARY KEY, v TEXT NOT NULL, n INTEGER, b)`,
		keys: []convCol{{name: "id", gen: convIntPool(40)}},
		cols: []convCol{
			{name: "v", gen: convText, notNull: true},
			{name: "n", gen: convInt},
			{name: "b", gen: convAny},
		},
	}
	convBase = []*convTable{convItems, convTags, convNotes, convPlain}

	// convLate is created mid-run, with or without a UNIQUE column.
	convLate = &convTable{
		name: "late",
		keys: []convCol{{name: "id", gen: convIntPool(6)}},
		cols: []convCol{
			{name: "v", gen: convText, notNull: true},
			{name: "w", gen: convIntPool(8)},
			{name: "x", gen: convAny},
		},
	}
)

// convTiesDDL is the table the clock races write: no other statement touches
// it, so a race's cells stay what the race left them.
const convTiesDDL = `CREATE TABLE ties(id INTEGER PRIMARY KEY, v TEXT NOT NULL, n INTEGER)`

func convLateDDL(unique bool) string {
	if unique {
		return `CREATE TABLE late(id INTEGER PRIMARY KEY, v TEXT NOT NULL, w INTEGER UNIQUE, x)`
	}
	return `CREATE TABLE late(id INTEGER PRIMARY KEY, v TEXT NOT NULL, w INTEGER, x)`
}

// convExtras are the columns ALTER TABLE adds mid-run.
var convExtras = []struct {
	def string
	col convCol
}{
	{`extra INTEGER`, convCol{name: "extra", gen: convInt}},
	{`extra TEXT DEFAULT 'd'`, convCol{name: "extra", gen: convText}},
	{`extra INTEGER NOT NULL DEFAULT 7`, convCol{name: "extra", gen: convInt, notNull: true, dflt: true}},
}

// The cluster.

type convNode struct {
	site string
	id   PeerID
	db   *sql.DB
	syn  *Syncer
	node *Node
	tr   *convTransport
}

type convStage int

const (
	convNormal convStage = iota
	convHold             // every push waits in the queue
	convSend             // every push goes through at once
)

// convWeather is how rough one seed's network is: the odds that a push waits
// in the queue, that it is lost, and that an anti-entropy batch arrives
// shuffled and with repeats.
type convWeather struct {
	hold, drop, shuffle float64
}

type convPush struct {
	from, to *convNode
	op       Op
}

type convCluster struct {
	t    *testing.T
	ctx  context.Context
	seed int64
	rng  *rand.Rand
	w    convWeather
	net  *MemNetwork
	dir  string

	nodes []*convNode
	cut   map[[2]PeerID]bool
	held  []convPush
	stage convStage
	calm  bool
	dup   bool
	at    int

	future  uint64 // the physical part of the last HLC warp handed out
	deleted map[string][][]any
	late    *convTable
	wide    map[string]*convTable // a table that got the extra column, with it
	lateAt  int
	colAt   int
	tieAt   int
	skewAt  int
	freeAt  int
	fresh   int // how many ids and values freeRace has used up
	rows    int // how many rows of ties the races have used up
	serial  int64
	stats   convStats
}

func newConvCluster(t *testing.T, seed int64) *convCluster {
	r := rand.New(rand.NewPCG(uint64(seed), 0x9e3779b97f4a7c15))
	pick := func(odds ...float64) float64 { return odds[r.IntN(len(odds))] }
	c := &convCluster{
		t:       t,
		ctx:     t.Context(),
		seed:    seed,
		rng:     r,
		w:       convWeather{hold: pick(0, 0.1, 0.3, 0.6), drop: pick(0, 0.05, 0.2), shuffle: pick(0, 0.5, 1)},
		net:     NewMemNetwork(),
		dir:     t.TempDir(),
		cut:     map[[2]PeerID]bool{},
		future:  uint64(time.Now().Add(time.Hour).UnixMilli()),
		deleted: map[string][][]any{},
		wide:    map[string]*convTable{},
		calm:    true,
	}
	at := r.Perm(30)
	c.lateAt, c.colAt, c.tieAt, c.skewAt, c.freeAt = 6+at[0], 6+at[1], 6+at[2], 6+at[3], 6+at[4]
	t.Logf("weather: %+v, late table at step %d, late column at %d, clock tie at %d, clock skew at %d, freed value at %d", c.w, c.lateAt, c.colAt, c.tieAt, c.skewAt, c.freeAt)
	return c
}

func (c *convCluster) logf(format string, args ...any) {
	c.t.Logf("%03d "+format, append([]any{c.at}, args...)...)
}

func (c *convCluster) open(site string) *convNode {
	n := &convNode{site: site, id: PeerID(site)}
	c.attach(n)
	return n
}

// attach opens n's database and joins it to the network. The node loop Open
// starts ends at once (see SubscribePush); this test calls Sync itself.
func (c *convCluster) attach(n *convNode) {
	db, err := Open(c.ctx, filepath.Join(c.dir, n.site+".db"), CRDT(), WithSite(n.site), WithSyncInterval(time.Hour),
		WithTransport(func(ctx context.Context, s *Syncer) (Transport, error) {
			n.syn = s
			n.tr = &convTransport{Transport: c.net.AddNode(n.id, convSource{s.OpSource(), c, n}), c: c, from: n, budget: -1}
			return n.tr, nil
		}))
	if err != nil {
		c.t.Fatalf("open %s: %v", n.site, err)
	}
	c.t.Cleanup(func() { db.Close() })
	n.db = db
	n.node = NewNode(n.syn.Store(), n.tr)
}

// restart closes n's database and opens it again, which has the store load
// from the file what it kept in memory.
func (c *convCluster) restart(n *convNode) {
	c.logf("%s restarts", n.site)
	if err := n.db.Close(); err != nil {
		c.t.Fatalf("%s: close: %v", n.site, err)
	}
	c.attach(n)
}

func (c *convCluster) setup() {
	for _, site := range []string{"a", "b", "c"} {
		c.nodes = append(c.nodes, c.open(fmt.Sprintf("s%d%s", c.seed, site)))
	}
	for _, tb := range convBase {
		c.mustExec(c.nodes[0], tb.ddl)
	}
	c.mustExec(c.nodes[0], convTiesDDL)
	c.syncRounds()
	c.calm = false
}

func (c *convCluster) mustExec(n *convNode, q string) {
	c.logf("%s: %s", n.site, q)
	if _, err := n.db.ExecContext(c.ctx, q); err != nil {
		c.t.Fatalf("%s: %s: %v", n.site, q, err)
	}
}

func (c *convCluster) pickNode() *convNode { return c.nodes[c.rng.IntN(len(c.nodes))] }

func (c *convCluster) pair() (*convNode, *convNode) {
	i := c.rng.IntN(len(c.nodes))
	return c.nodes[i], c.nodes[(i+1+c.rng.IntN(len(c.nodes)-1))%len(c.nodes)]
}

func convCutKey(a, b *convNode) [2]PeerID {
	if a.id > b.id {
		a, b = b, a
	}
	return [2]PeerID{a.id, b.id}
}

func (c *convCluster) setCut(a, b *convNode, cut bool) {
	c.net.Partition(a.id, b.id, cut)
	if cut {
		c.cut[convCutKey(a, b)] = true
	} else {
		delete(c.cut, convCutKey(a, b))
	}
}

// The network: what happens to a push, a pull, and a queued push.

type convTransport struct {
	Transport
	c      *convCluster
	from   *convNode
	only   PeerID // set during a pair sync: the one peer Peers reports
	budget int    // ops a pull may still get, or -1 for any
}

func (t *convTransport) Peers() []PeerID {
	peers := t.Transport.Peers()
	slices.Sort(peers)
	if t.only != "" {
		peers = slices.DeleteFunc(peers, func(p PeerID) bool { return p != t.only })
	}
	return peers
}

func (t *convTransport) PushOp(ctx context.Context, peer PeerID, op Op) error {
	t.c.stats.pushes++
	switch t.c.stage {
	case convHold:
		return t.c.hold(t, peer, op)
	case convSend:
		return t.Transport.PushOp(ctx, peer, op)
	}
	if !t.c.calm {
		switch x := t.c.rng.Float64(); {
		case x < t.c.w.drop:
			t.c.stats.dropped++
			return nil
		case x < t.c.w.drop+t.c.w.hold:
			return t.c.hold(t, peer, op)
		}
	}
	return t.Transport.PushOp(ctx, peer, op)
}

func (c *convCluster) hold(t *convTransport, peer PeerID, op Op) error {
	c.stats.held++
	c.held = append(c.held, convPush{from: t.from, to: c.byID(peer), op: op})
	return nil
}

func (c *convCluster) byID(id PeerID) *convNode {
	for _, n := range c.nodes {
		if n.id == id {
			return n
		}
	}
	c.t.Fatalf("no node %s", id)
	return nil
}

var errConvLinkLost = errors.New("convergence: the link dropped mid-sync")

func (t *convTransport) GetOps(ctx context.Context, peer PeerID, site string, fromSeq uint64, limit int) ([]Op, error) {
	if t.budget == 0 {
		t.c.stats.partial++
		return nil, errConvLinkLost
	}
	ops, err := t.Transport.GetOps(ctx, peer, site, fromSeq, limit)
	if err != nil {
		return nil, err
	}
	if t.budget > 0 {
		ops = ops[:min(len(ops), t.budget)]
		t.budget -= len(ops)
	}
	return t.c.scramble(ops), nil
}

// scramble shuffles a pulled batch and repeats some of its ops.
func (c *convCluster) scramble(ops []Op) []Op {
	if c.calm || len(ops) == 0 || c.rng.Float64() >= c.w.shuffle {
		return ops
	}
	ops = slices.Clone(ops)
	c.rng.Shuffle(len(ops), func(i, j int) { ops[i], ops[j] = ops[j], ops[i] })
	for range c.rng.IntN(3) {
		ops = append(ops, ops[c.rng.IntN(len(ops))])
	}
	return ops
}

// SubscribePush refuses, so Run ends at once: this test drives pushes and
// anti-entropy itself, and a commit's Broadcast pushes inside its write.
func (t *convTransport) SubscribePush(ctx context.Context) (<-chan PeerOp, error) {
	return nil, errConvNoRun
}

var errConvNoRun = errors.New("convergence: the test drives every node itself")

// convSource applies a push as ReceiveOp does, but a refused op fails the test.
type convSource struct {
	OpSource
	c *convCluster
	n *convNode
}

func (s convSource) ReceiveOp(from PeerID, op Op) {
	if err := s.n.syn.Store().Ingest(s.c.ctx, op, false); err != nil {
		s.c.t.Errorf("%s: applying a pushed op %s/%d from %s: %v", s.n.site, op.Site, op.Seq, from, err)
	}
}

// flush delivers some (or all) of the queued pushes, in random order and
// sometimes twice. A push whose link is cut stays queued.
func (c *convCluster) flush(all bool) {
	if len(c.held) == 0 {
		return
	}
	c.rng.Shuffle(len(c.held), func(i, j int) { c.held[i], c.held[j] = c.held[j], c.held[i] })
	n := len(c.held)
	if !all {
		n = 1 + c.rng.IntN(n)
	}
	var rest []convPush
	sent := 0
	for i, p := range c.held {
		if i >= n || c.cut[convCutKey(p.from, p.to)] {
			rest = append(rest, p)
			continue
		}
		p.from.tr.Transport.PushOp(c.ctx, p.to.id, p.op)
		if c.rng.IntN(7) == 0 {
			p.from.tr.Transport.PushOp(c.ctx, p.to.id, p.op)
		}
		sent++
	}
	c.logf("flushed %d of %d queued pushes", sent, len(c.held))
	c.held = rest
}

func (c *convCluster) syncPair() {
	a, b := c.pair()
	budget := -1
	if c.rng.IntN(2) == 0 {
		budget = c.rng.IntN(12)
	}
	c.logf("%s pulls from %s, budget %d", a.site, b.site, budget)
	a.tr.only, a.tr.budget = b.id, budget
	a.node.Sync(c.ctx)
	a.tr.only, a.tr.budget = "", -1
}

func (c *convCluster) syncNode() {
	n := c.pickNode()
	c.logf("%s pulls from every reachable peer", n.site)
	n.node.Sync(c.ctx)
}

func (c *convCluster) togglePartition() {
	if c.rng.IntN(7) == 0 {
		for _, a := range c.nodes {
			for _, b := range c.nodes {
				if a.id < b.id {
					c.setCut(a, b, false)
				}
			}
		}
		c.logf("every link healed")
		return
	}
	a, b := c.pair()
	cut := !c.cut[convCutKey(a, b)]
	c.setCut(a, b, cut)
	c.logf("link %s-%s cut=%v", a.site, b.site, cut)
}

func (c *convCluster) toggleDup() {
	c.dup = !c.dup
	c.net.SetDuplicate(c.dup)
	c.logf("network duplicates pushes: %v", c.dup)
}

// The writes.

func (c *convCluster) step(i int) {
	c.at = i
	switch i {
	case c.lateAt:
		c.createLate()
		return
	case c.colAt:
		c.addColumn()
		return
	case c.tieAt:
		c.tieRace()
		return
	case c.skewAt:
		c.skewRace()
		return
	case c.freeAt:
		c.freeRace()
		return
	}
	switch x := c.rng.IntN(100); {
	case x < 56:
		c.write()
	case x < 64:
		c.syncPair()
	case x < 68:
		c.syncNode()
	case x < 76:
		c.flush(false)
	case x < 83:
		c.togglePartition()
	case x < 88:
		c.uniqueRace()
	case x < 91:
		c.toggleDup()
	case x < 93:
		c.restart(c.pickNode())
	case x < 94:
		c.bulk()
	case x < 95:
		c.tieRace()
	case x < 96:
		c.skewRace()
	case x < 98:
		c.freeRace()
	default:
		c.write()
	}
}

// warp moves the clocks of ns to the same HLC, an hour or more ahead of the
// wall clock. Left to the wall clock, nodes stamp writes a millisecond or more
// apart and the later write is always the newer.
func (c *convCluster) warp(ns ...*convNode) {
	c.future++
	for _, n := range ns {
		n.syn.Store().Clock().Merge(HLC(c.future << 16))
	}
}

// upsert sets column n of row id of ties to a value no write had before, so
// the statement is never one that changes nothing.
func (c *convCluster) upsert(n *convNode, id int) {
	c.serial++
	c.exec(n, convSQL(`INSERT INTO ties(id, v, n) VALUES(?, ?, ?) ON CONFLICT(id) DO UPDATE SET n = excluded.n`, id, convText(c.rng), c.serial))
}

// tieRace has two nodes, apart and on one clock, write the same row of ties,
// new to both or held by both: the writes carry the same HLC, and the site id
// decides.
func (c *convCluster) tieRace() {
	a, b := c.pair()
	c.rows++
	id := c.rows
	c.setCut(a, b, false)
	if c.rng.IntN(2) == 0 {
		c.upsert(a, id)
		c.pull(b, a)
	}
	c.setCut(a, b, true)
	c.warp(a, b)
	c.logf("%s and %s are apart on one clock", a.site, b.site)
	c.upsert(a, id)
	c.upsert(b, id)
	if c.lastOp(a).HLC == c.lastOp(b).HLC {
		c.stats.ties++
	}
}

// lastOp is the newest op n wrote itself.
func (c *convCluster) lastOp(n *convNode) Op {
	var last Op
	for _, op := range c.opLog(n) {
		if op.Site == n.site && op.Seq > last.Seq {
			last = op
		}
	}
	return last
}

// skewRace has a node whose clock runs ahead write a row, and another take
// the write in and write the row again: only a clock that took in the first
// write's HLC stamps the second after it.
func (c *convCluster) skewRace() {
	a, b := c.pair()
	c.setCut(a, b, false)
	c.warp(a)
	c.logf("%s's clock jumps ahead", a.site)
	c.rows++
	id := c.rows
	c.upsert(a, id)
	c.pull(b, a)
	if ahead := c.lastOp(a); b.syn.Store().LocalVV()[a.site] >= ahead.Seq {
		if now := b.syn.Store().Clock().Now(); now <= ahead.HLC {
			c.t.Errorf("%s took in an op of %s stamped %d, and its clock says %d", b.site, a.site, ahead.HLC, now)
		}
		c.stats.skews++
	}
	c.upsert(b, id)
}

// pull has n take in everything peer has, in one batch per site and as it is.
func (c *convCluster) pull(n, peer *convNode) {
	calm := c.calm
	c.calm = true
	n.tr.only, n.tr.budget = peer.id, -1
	n.node.Sync(c.ctx)
	n.tr.only = ""
	c.calm = calm
}

// freeRace has a take in, as one batch, an op of b's wanting a UNIQUE value a
// row holds and one freeing it: the waiting row must end in the table, as op
// by op. On the way b takes the newer write that gave the value to the holder,
// then frees it for its own waiting row.
func (c *convCluster) freeRace() {
	a, b := c.pair()
	c.fresh++
	holder, waiter := 50+2*c.fresh, 51+2*c.fresh
	home, want, away := "h"+strconv.Itoa(c.fresh), "v"+strconv.Itoa(c.fresh), "z"+strconv.Itoa(c.fresh)
	c.setCut(a, b, false)
	c.exec(a, convSQL(`INSERT INTO items(id, name, code) VALUES(?, 'holder', ?)`, holder, home))
	c.pull(b, a)
	if _, ok := c.codeOf(b, holder); !ok {
		c.logf("%s did not get row %d from %s: no race", b.site, holder, a.site)
		return
	}
	c.setCut(a, b, true)
	c.logf("%s and %s are apart; %s waits for %q, which %s gives to row %d; %s frees it", a.site, b.site, b.site, want, a.site, holder, b.site)
	c.warp(b)
	c.exec(b, convSQL(`INSERT INTO items(id, name, code) VALUES(?, 'waiter', ?)`, waiter, want))
	c.warp(a)
	c.exec(a, convSQL(`UPDATE items SET code = ? WHERE id = ?`, want, holder))
	c.setCut(a, b, false)
	c.pull(b, a)
	if code, _ := c.codeOf(b, holder); code != want {
		c.t.Errorf("%s took in the newer write of %s, and row %d of items should hold %q: it reads %q", b.site, a.site, holder, want, code)
	}
	if _, ok := c.codeOf(b, waiter); ok {
		c.t.Errorf("%s took in the newer write of %s, and row %d of items should wait for %q: it is in the table", b.site, a.site, waiter, want)
	}
	// Cut again, so a gets b's two ops only by the pull below, together.
	c.setCut(a, b, true)
	c.warp(b)
	if c.rng.IntN(2) == 0 {
		c.exec(b, convSQL(`DELETE FROM items WHERE id = ?`, holder))
	} else {
		c.exec(b, convSQL(`UPDATE items SET code = ? WHERE id = ?`, away, holder))
	}
	if code, _ := c.codeOf(b, waiter); code != want {
		c.t.Errorf("%s took %q off row %d, and row %d of items should hold it: it reads %q", b.site, want, holder, waiter, code)
	}
	c.setCut(a, b, false)
	c.pull(a, b)
	if code, _ := c.codeOf(a, waiter); code != want {
		c.t.Errorf("%s took in the ops of %s together, and row %d of items should hold %q: it reads %q", a.site, b.site, waiter, want, code)
	}
	c.stats.frees++
}

// codeOf reads row id of items on n: its code, and whether the table has the
// row at all.
func (c *convCluster) codeOf(n *convNode, id int) (string, bool) {
	var code sql.NullString
	err := n.db.QueryRowContext(c.ctx, `SELECT code FROM items WHERE id = ?`, id).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		c.t.Fatalf("%s: reading items: %v", n.site, err)
	}
	return code.String, true
}

func (c *convCluster) hasTable(n *convNode, name string) bool {
	var k int
	if err := n.db.QueryRowContext(c.ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&k); err != nil {
		c.t.Fatalf("%s: looking for table %s: %v", n.site, name, err)
	}
	return k > 0
}

func (c *convCluster) hasColumn(n *convNode, table, col string) bool {
	var k int
	if err := n.db.QueryRowContext(c.ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&k); err != nil {
		c.t.Fatalf("%s: looking for column %s.%s: %v", n.site, table, col, err)
	}
	return k > 0
}

// tablesFor is the tables n can write to now.
func (c *convCluster) tablesFor(n *convNode) []*convTable {
	var tbs []*convTable
	for _, tb := range convBase {
		if w := c.wide[tb.name]; w != nil && c.hasColumn(n, tb.name, "extra") {
			tb = w
		}
		tbs = append(tbs, tb)
	}
	if c.late != nil && c.hasTable(n, "late") {
		tbs = append(tbs, c.late)
	}
	return tbs
}

func (c *convCluster) genStmt(n *convNode, tb *convTable) convStmt {
	key := tb.randKey(c.rng)
	switch x := c.rng.IntN(100); {
	case x < 38:
		if list := c.deleted[tb.name]; len(list) > 0 && c.rng.IntN(100) < 45 {
			i := c.rng.IntN(len(list))
			key = list[i]
			c.deleted[tb.name] = slices.Delete(slices.Clone(list), i, i+1)
			c.stats.resurrected++
		}
		return tb.insert(c.rng, key, "")
	case x < 75:
		return tb.update(c.rng, c.heldKey(n, tb, key))
	}
	return tb.remove(c.heldKey(n, tb, key))
}

// heldKey is the key of a row n holds in tb, mostly: an update or a delete of
// a key nobody holds changes nothing. It is key when n has no row, or the
// draw says so.
func (c *convCluster) heldKey(n *convNode, tb *convTable, key []any) []any {
	if c.rng.IntN(100) >= 85 {
		return key
	}
	var count int
	if err := n.db.QueryRowContext(c.ctx, `SELECT count(*) FROM `+tb.name).Scan(&count); err != nil {
		c.t.Fatalf("%s: counting %s: %v", n.site, tb.name, err)
	}
	if count == 0 {
		return key
	}
	names := make([]string, len(tb.keys))
	for i, k := range tb.keys {
		names[i] = k.name
	}
	list := strings.Join(names, ", ")
	held := make([]any, len(names))
	ptrs := make([]any, len(names))
	for i := range held {
		ptrs[i] = &held[i]
	}
	err := n.db.QueryRowContext(c.ctx, fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s LIMIT 1 OFFSET ?`, list, tb.name, list), c.rng.IntN(count)).Scan(ptrs...)
	if err != nil {
		c.t.Fatalf("%s: picking a row of %s: %v", n.site, tb.name, err)
	}
	return held
}

func (c *convCluster) write() {
	n := c.pickNode()
	tbs := c.tablesFor(n)
	if c.rng.IntN(5) > 0 {
		c.exec(n, c.genStmt(n, tbs[c.rng.IntN(len(tbs))]))
		return
	}
	stmts := make([]convStmt, 2+c.rng.IntN(3))
	for i := range stmts {
		stmts[i] = c.genStmt(n, tbs[c.rng.IntN(len(tbs))])
	}
	c.txn(n, stmts, c.rng.IntN(5) == 0)
}

func convConstraint(err error) bool { return strings.Contains(err.Error(), "constraint failed") }

// run executes s on n and reports the rows it changed, or false when a
// constraint the node itself enforces refused it; any other error fails the
// test.
func (c *convCluster) run(n *convNode, s convStmt) (int64, bool) {
	res, err := n.db.ExecContext(c.ctx, s.q, s.args...)
	if err != nil {
		if !convConstraint(err) {
			c.t.Fatalf("%s: %s %v: %v", n.site, s.q, s.args, err)
		}
		c.stats.refused++
		c.logf("%s: %s %v: refused: %v", n.site, s.q, s.args, err)
		return 0, false
	}
	c.committed(s, res)
	changed, err := res.RowsAffected()
	if err != nil {
		c.t.Fatalf("%s: %s %v: %v", n.site, s.q, s.args, err)
	}
	c.logf("%s: %s %v: %d rows", n.site, s.q, s.args, changed)
	return changed, true
}

func (c *convCluster) exec(n *convNode, s convStmt) bool {
	_, ok := c.run(n, s)
	return ok
}

func (c *convCluster) committed(s convStmt, res sql.Result) {
	c.stats.writes++
	if s.table == "" {
		return
	}
	if k, err := res.RowsAffected(); err == nil && k > 0 {
		c.stats.deletes++
		c.deleted[s.table] = append(c.deleted[s.table], s.key)
	}
}

// txn runs stmts as one transaction, and rolls it back if asked to.
func (c *convCluster) txn(n *convNode, stmts []convStmt, rollback bool) {
	tx, err := n.db.BeginTx(c.ctx, nil)
	if err != nil {
		c.t.Fatalf("%s: begin: %v", n.site, err)
	}
	var done []convStmt
	var results []sql.Result
	for _, s := range stmts {
		res, err := tx.ExecContext(c.ctx, s.q, s.args...)
		if err != nil {
			if !convConstraint(err) {
				c.t.Fatalf("%s: %s %v: %v", n.site, s.q, s.args, err)
			}
			c.stats.refused++
			c.logf("%s: (txn) %s %v: refused: %v", n.site, s.q, s.args, err)
			continue
		}
		if len(stmts) < 10 {
			c.logf("%s: (txn) %s %v", n.site, s.q, s.args)
		}
		done = append(done, s)
		results = append(results, res)
	}
	if rollback {
		c.logf("%s: rollback", n.site)
		if err := tx.Rollback(); err != nil {
			c.t.Fatalf("%s: rollback: %v", n.site, err)
		}
		return
	}
	if err := tx.Commit(); err != nil {
		c.t.Fatalf("%s: commit: %v", n.site, err)
	}
	c.logf("%s: commit of %d statements", n.site, len(done))
	for i, s := range done {
		c.committed(s, results[i])
	}
}

// uniqueRace cuts two nodes apart and has each write the same UNIQUE value.
func (c *convCluster) uniqueRace() {
	a, b := c.pair()
	c.setCut(a, b, true)
	c.logf("link %s-%s cut for a UNIQUE race", a.site, b.site)
	if c.rng.IntN(2) == 0 {
		code := "c" + strconv.Itoa(c.rng.IntN(12))
		for _, n := range []*convNode{a, b} {
			id := 1 + c.rng.IntN(10)
			if !c.exec(n, convSQL(`INSERT INTO items(id, name, code) VALUES(?, 'race', ?)`, id, code)) {
				c.exec(n, convSQL(`UPDATE items SET code = ? WHERE id = ?`, code, id))
			}
		}
		return
	}
	owner, pos := "o"+strconv.Itoa(c.rng.IntN(3)), 1+c.rng.IntN(8)
	for _, n := range []*convNode{a, b} {
		label := "l" + strconv.Itoa(c.rng.IntN(4))
		if !c.exec(n, convSQL(`INSERT INTO tags(owner, label, pos) VALUES(?, ?, ?)`, owner, label, pos)) {
			c.exec(n, convSQL(`UPDATE tags SET pos = ? WHERE owner = ? AND label = ?`, pos, owner, label))
		}
	}
}

// createLate creates a table on one node with the schema op held back and the
// rows pushed at once, so the others get rows for a table they lack.
func (c *convCluster) createLate() {
	n := c.pickNode()
	unique := c.rng.IntN(2) == 0
	c.stage = convHold
	c.mustExec(n, convLateDDL(unique))
	c.late = convLate
	c.stats.lateTables++
	c.stage = convSend
	if c.rng.IntN(2) == 0 {
		c.bulkInsert(n, "late", "w", 1000)
	}
	for wrote, tries := 0, 0; wrote < 3 && tries < 30; tries++ {
		if c.exec(n, convLate.insert(c.rng, convLate.randKey(c.rng), "")) {
			wrote++
		}
	}
	c.stage = convNormal
}

// addColumn adds a column to a table on one node, holds the schema op back,
// and sets the new column on rows, which are pushed at once.
func (c *convCluster) addColumn() {
	n := c.pickNode()
	tb := []*convTable{convItems, convPlain}[c.rng.IntN(2)]
	extra := convExtras[c.rng.IntN(len(convExtras))]
	wide := tb.with(extra.col)
	c.stage = convHold
	c.mustExec(n, "ALTER TABLE "+tb.name+" ADD COLUMN "+extra.def)
	c.wide[tb.name] = wide
	c.stats.lateColumns++
	c.stage = convSend
	for wrote, tries := 0, 0; wrote < 3 && tries < 30; tries++ {
		key := tb.randKey(c.rng)
		set := convSQL("UPDATE "+tb.name+" SET extra = ? WHERE "+tb.where(), append([]any{extra.col.gen(c.rng)}, key...)...)
		if changed, ok := c.run(n, set); ok && changed > 0 || c.exec(n, wide.insert(c.rng, key, "extra")) {
			wrote++
		}
	}
	c.stage = convNormal
}

// bulkInsert puts a few more rows than one INSERT of the apply path holds in
// one transaction; col gets a value (a distinct one, for a UNIQUE column) in
// every other row.
func (c *convCluster) bulkInsert(n *convNode, table, col string, first int) {
	stmts := make([]convStmt, 66+c.rng.IntN(20))
	for i := range stmts {
		var val any
		if i%2 == 0 {
			val = int64(first + i)
		}
		stmts[i] = convSQL(fmt.Sprintf(`INSERT INTO %s(id, v, %s) VALUES(?, ?, ?)`, table, col), first+i, convText(c.rng), val)
	}
	c.txn(n, stmts, false)
}

// bulk changes many rows of plain at once.
func (c *convCluster) bulk() {
	n := c.pickNode()
	switch c.rng.IntN(3) {
	case 0:
		c.bulkInsert(n, "plain", "n", 1000+c.rng.IntN(300))
	case 1:
		c.exec(n, convSQL(`UPDATE plain SET n = ? WHERE id >= 1000`, convInt(c.rng)))
	default:
		c.exec(n, convSQL(`DELETE FROM plain WHERE id >= 1000 AND id % 2 = ?`, c.rng.IntN(2)))
	}
}

// Convergence.

// tableOf is the model of the table called name.
func (c *convCluster) tableOf(name string) *convTable {
	if w := c.wide[name]; w != nil {
		return w
	}
	for _, tb := range append([]*convTable{c.late}, convBase...) {
		if tb != nil && tb.name == name {
			return tb
		}
	}
	return nil
}

// checkHeld fails when a node holds out a row missing a NOT NULL value: only
// a live, complete row is held out, for a UNIQUE value another row took.
func (c *convCluster) checkHeld() {
	for _, n := range c.nodes {
		held, err := Withheld(c.ctx, n.db)
		if err != nil {
			c.t.Fatalf("%s: reading the withheld rows: %v", n.site, err)
		}
		for _, w := range held {
			tb := c.tableOf(w.Table)
			if tb == nil {
				c.t.Errorf("%s withholds %s %v, a table with no UNIQUE index to hold a row out for", n.site, w.Table, w.Key)
				continue
			}
			for _, col := range tb.cols {
				if v, ok := w.Row[col.name]; col.must() && (!ok || v == nil) {
					c.t.Errorf("%s withholds %s %v, which has no value for %s: %v", n.site, w.Table, w.Key, col.name, w.Row)
				}
			}
		}
	}
}

// syncRounds has every node pull from every peer until the version vectors
// agree.
func (c *convCluster) syncRounds() {
	for range 8 {
		for _, i := range c.rng.Perm(len(c.nodes)) {
			c.nodes[i].node.Sync(c.ctx)
		}
		if c.sameVV() {
			return
		}
	}
	var b strings.Builder
	for _, n := range c.nodes {
		fmt.Fprintf(&b, "%s: %v\n", n.site, n.syn.Store().LocalVV())
	}
	c.t.Fatalf("the version vectors still differ after 8 rounds of full sync:\n%s", b.String())
}

func (c *convCluster) sameVV() bool {
	first := c.nodes[0].syn.Store().LocalVV()
	for _, n := range c.nodes[1:] {
		vv := n.syn.Store().LocalVV()
		if len(vv) != len(first) {
			return false
		}
		for site, seq := range first {
			if vv[site] != seq {
				return false
			}
		}
	}
	return true
}

// settle ends every fault: links heal, the queued pushes arrive late and in
// random order, and the nodes sync fully.
func (c *convCluster) settle() {
	c.at = -1
	c.calm = true
	c.stage = convNormal
	c.net.SetDuplicate(false)
	for _, a := range c.nodes {
		for _, b := range c.nodes {
			if a.id < b.id {
				c.setCut(a, b, false)
			}
		}
	}
	c.flush(true)
	c.syncRounds()
}

// convView is everything one node holds that must be the same on every node.
type convView struct {
	vv     []string
	schema []string
	tables []string
	clock  []string
	held   []string
}

func (c *convCluster) view(n *convNode, skipSite string) convView {
	var v convView
	for site, seq := range n.syn.Store().LocalVV() {
		if site != skipSite {
			v.vv = append(v.vv, fmt.Sprintf("%s=%d", site, seq))
		}
	}
	slices.Sort(v.vv)

	rows, err := n.db.QueryContext(c.ctx, `SELECT type, name, tbl_name, sql FROM sqlite_master WHERE name NOT LIKE '\_repl\_%' ESCAPE '\' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		c.t.Fatalf("%s: reading the schema: %v", n.site, err)
	}
	var tables []string
	for rows.Next() {
		var typ, name, tbl string
		var text sql.NullString
		if err := rows.Scan(&typ, &name, &tbl, &text); err != nil {
			c.t.Fatalf("%s: reading the schema: %v", n.site, err)
		}
		v.schema = append(v.schema, fmt.Sprintf("%s %s on %s: %s", typ, name, tbl, text.String))
		if typ == "table" {
			tables = append(tables, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		c.t.Fatalf("%s: reading the schema: %v", n.site, err)
	}
	for _, name := range tables {
		v.tables = append(v.tables, c.dumpTable(n, name)...)
	}

	v.clock = convClockLines(c.clock(n))

	held, err := Withheld(c.ctx, n.db)
	if err != nil {
		c.t.Fatalf("%s: reading the withheld rows: %v", n.site, err)
	}
	for _, w := range held {
		cols := make([]string, 0, len(w.Row))
		for col, val := range w.Row {
			cols = append(cols, col+"="+convValue(val))
		}
		slices.Sort(cols)
		v.held = append(v.held, fmt.Sprintf("%s %v: %s", w.Table, w.Key, strings.Join(cols, " ")))
	}
	return v
}

// dumpTable is the rows of table, in key order, one line each.
func (c *convCluster) dumpTable(n *convNode, table string) []string {
	rows, err := n.db.QueryContext(c.ctx, `SELECT name, pk FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		c.t.Fatalf("%s: describing %s: %v", n.site, table, err)
	}
	var cols []string
	keys := map[int]string{}
	for rows.Next() {
		var name string
		var pk int
		if err := rows.Scan(&name, &pk); err != nil {
			c.t.Fatalf("%s: describing %s: %v", n.site, table, err)
		}
		cols = append(cols, name)
		if pk > 0 {
			keys[pk] = name
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		c.t.Fatalf("%s: describing %s: %v", n.site, table, err)
	}
	order := make([]string, len(keys))
	for pk, name := range keys {
		order[pk-1] = name
	}
	out := []string{fmt.Sprintf("%s: columns %s", table, strings.Join(cols, ","))}
	data, err := n.db.QueryContext(c.ctx, fmt.Sprintf(`SELECT * FROM %s ORDER BY %s`, table, strings.Join(order, ", ")))
	if err != nil {
		c.t.Fatalf("%s: reading %s: %v", n.site, table, err)
	}
	defer data.Close()
	for data.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := data.Scan(ptrs...); err != nil {
			c.t.Fatalf("%s: reading %s: %v", n.site, table, err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = convValue(v)
		}
		out = append(out, table+": "+strings.Join(parts, " | "))
	}
	if err := data.Err(); err != nil {
		c.t.Fatalf("%s: reading %s: %v", n.site, table, err)
	}
	return out
}

func convValue(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case int64:
		return "i" + strconv.FormatInt(v, 10)
	case float64:
		return "f" + strconv.FormatFloat(v, 'g', -1, 64)
	case string:
		return "t" + strconv.Quote(v)
	case []byte:
		return fmt.Sprintf("b%x", v)
	}
	return fmt.Sprintf("%T(%v)", v, v)
}

// pages hands take every page of one part of n's snapshot.
func (c *convCluster) pages(n *convNode, part int, take func(SnapshotPage)) {
	var cursor []byte
	for {
		page, err := n.syn.Store().SnapshotPage(c.ctx, part, cursor)
		if err != nil {
			c.t.Fatalf("%s: snapshot part %d: %v", n.site, part, err)
		}
		take(page)
		if page.Next == nil {
			return
		}
		cursor = page.Next
	}
}

func (c *convCluster) opLog(n *convNode) []Op {
	var ops []Op
	c.pages(n, SnapshotOps, func(p SnapshotPage) { ops = append(ops, p.Ops...) })
	return ops
}

func (c *convCluster) clock(n *convNode) []ClockCell {
	var cells []ClockCell
	c.pages(n, SnapshotClock, func(p SnapshotPage) { cells = append(cells, p.Cells...) })
	return cells
}

func convClockLines(cells []ClockCell) []string {
	cells = slices.Clone(cells)
	slices.SortFunc(cells, func(a, b ClockCell) int {
		return cmp.Or(cmp.Compare(a.Tbl, b.Tbl), bytes.Compare(a.PK, b.PK), cmp.Compare(a.Col, b.Col))
	})
	lines := make([]string, len(cells))
	for i, cell := range cells {
		lines[i] = fmt.Sprintf("%s %x %s hlc=%d site=%s val=%x", cell.Tbl, cell.PK, cell.Col, cell.HLC, cell.Site, cell.Val)
	}
	return lines
}

// convFoldClock is the clock the op log says: per cell, the newest writer. The
// order is spelled out here, not taken from TagLess, so the merge under test
// cannot vouch for itself.
func convFoldClock(ops []Op) []ClockCell {
	newer := func(a, b ClockCell) bool { return a.HLC > b.HLC || a.HLC == b.HLC && a.Site > b.Site }
	best := map[string]ClockCell{}
	put := func(op Op, col string, val []byte) {
		cell := ClockCell{Tbl: op.Tbl, PK: op.PK, Col: col, HLC: op.HLC, Site: op.Site, Val: val}
		k := op.Tbl + "\x00" + string(op.PK) + "\x00" + col
		if cur, ok := best[k]; !ok || newer(cell, cur) {
			best[k] = cell
		}
	}
	for _, op := range ops {
		if op.Kind == OpSchema {
			continue
		}
		alive := byte(1)
		if op.Kind == OpDelete {
			alive = 0
		}
		put(op, presenceCol, []byte{alive})
		for _, cell := range op.Cells {
			put(op, cell.Col, packVal(cell.Type, cell.Val))
		}
	}
	cells := make([]ClockCell, 0, len(best))
	for _, cell := range best {
		cells = append(cells, cell)
	}
	return cells
}

func convDiff(a, b []string) string {
	inA, inB := map[string]bool{}, map[string]bool{}
	for _, s := range a {
		inA[s] = true
	}
	for _, s := range b {
		inB[s] = true
	}
	var out []string
	add := func(sign string, from []string, not map[string]bool) {
		shown := 0
		for _, s := range from {
			if !not[s] {
				if shown++; shown > 8 {
					out = append(out, sign+" ...")
					return
				}
				out = append(out, sign+" "+s)
			}
		}
	}
	add("-", a, inB)
	add("+", b, inA)
	return strings.Join(out, "\n")
}

func (c *convCluster) compare(what string, want, got convView, wantSite, gotSite string) {
	rows := false
	for _, f := range []struct {
		name      string
		want, got []string
		rows      bool
	}{
		{"version vector", want.vv, got.vv, false},
		{"schema", want.schema, got.schema, false},
		{"user tables", want.tables, got.tables, true},
		{"withheld rows", want.held, got.held, true},
		{"clock cells", want.clock, got.clock, false},
	} {
		if !slices.Equal(f.want, f.got) {
			c.t.Errorf("%s: %s differs between %s (-) and %s (+):\n%s", what, f.name, wantSite, gotSite, convDiff(f.want, f.got))
			rows = rows || f.rows
		}
	}
	if rows {
		c.t.Logf("%s holds:\n%s\n%s", gotSite, strings.Join(got.tables, "\n"), strings.Join(got.held, "\n"))
	}
}

func (c *convCluster) verify() {
	ref := c.nodes[0]
	want := c.view(ref, "")
	for _, n := range c.nodes[1:] {
		c.compare("after the final sync", want, c.view(n, ""), ref.site, n.site)
	}
	if folded := convClockLines(convFoldClock(c.opLog(ref))); !slices.Equal(folded, want.clock) {
		c.t.Errorf("the clock of %s is not the last-writer-wins fold of its op log (-):\n%s", ref.site, convDiff(folded, want.clock))
	}
	for _, line := range want.held {
		c.logf("withheld at the end: %s", line)
	}
	c.stats.withheld += len(want.held)
	for _, line := range want.tables {
		c.logf("final: %s", line)
	}
	n := c.pickNode()
	before := c.view(n, "")
	c.restart(n)
	c.compare("after a restart", before, c.view(n, ""), n.site, n.site)
}

// replay feeds a fresh node the whole op log in random order, which has to
// land it on the converged state too.
func (c *convCluster) replay() {
	ref := c.nodes[0]
	ops := c.opLog(ref)
	d := c.open(fmt.Sprintf("s%dd", c.seed))
	c.rng.Shuffle(len(ops), func(i, j int) { ops[i], ops[j] = ops[j], ops[i] })
	for _, op := range ops {
		if err := d.syn.Store().Ingest(c.ctx, op, false); err != nil {
			c.t.Fatalf("%s: replaying %s/%d (%d ops in random order): %v", d.site, op.Site, op.Seq, len(ops), err)
		}
	}
	c.compare("after replaying the log in random order", c.view(ref, d.site), c.view(d, d.site), ref.site, d.site)
}

// TestConvergenceFreedValueInOneBatch is the smallest divergence
// TestConvergenceRandom found: the op freeing a UNIQUE value arrives in the
// same batch as the op that held row 5 out, and row 5 must end in the table.
func TestConvergenceFreedValueInOneBatch(t *testing.T) {
	c := newConvCluster(t, 1000)
	c.setup()
	a, b := c.nodes[0], c.nodes[1]
	c.exec(a, convSQL(`INSERT INTO items(id, name, code) VALUES(9, 'nine', 'k')`))
	c.syncRounds()
	for _, x := range c.nodes {
		for _, y := range c.nodes {
			if x.id < y.id {
				c.setCut(x, y, true)
			}
		}
	}
	// A warp puts a node's next write after every write so far.
	c.warp(b)
	c.exec(b, convSQL(`INSERT INTO items(id, name, code) VALUES(5, 'five', 'x')`))
	c.warp(a)
	c.exec(a, convSQL(`UPDATE items SET code = 'x' WHERE id = 9`))
	c.warp(b)
	c.exec(b, convSQL(`DELETE FROM items WHERE id = 9`))
	c.settle()
	c.verify()
	for _, n := range c.nodes {
		v := c.view(n, "")
		if !slices.ContainsFunc(v.tables, func(line string) bool { return strings.HasPrefix(line, `items: i5 | t"five" | t"x" | `) }) || len(v.held) > 0 {
			t.Errorf("%s: row 5 should be in items and nothing withheld:\n%s\n%s", n.site, strings.Join(v.tables, "\n"), strings.Join(v.held, "\n"))
		}
	}
}
