package driver

import (
	"context"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// The read gate admits at most GOMAXPROCS reads from POOLED connections at
// once, and lets a reader that is running keep running.
//
// Why a driver needs one. Every db.Query/QueryRow on a shared *sql.DB takes
// database/sql's pool lock twice (DB.conn, DB.putConn). With more readers than
// processors, a goroutine that finds that lock held parks, and the Unlock that
// wakes it does not hand it the processor: it waits runnable until the
// goroutine that released the lock next blocks. A musql read is pure Go and
// never blocks, so that wait lasts until the running reader parks on the pool
// lock in turn -- and since that happens every few reads, the waiting is spread
// over a few percent of all reads at hundreds of microseconds each. That was the
// shared pool's p99 (bench/pointread). A cgo read does not have the problem: a
// cgo call that runs long gives its processor away.
//
// The gate keeps the readers that are not running PARKED on the gate instead of
// runnable. With nothing queued behind it, a reader that finds the pool lock
// held spins briefly rather than parking (sync.Mutex spins only while the local
// run queue is empty), so the readers holding a slot run on undisturbed and the
// waiting is paid in rare, long turns -- one per slot every readSlice -- the way
// a retained *sql.Conn per reader already pays it. The mean does not move (with
// a fixed number of readers it is set by the throughput); the median and the
// tail do.
//
// A slot is leased to a CONNECTION and stays with it between statements, so a
// reader that keeps reading touches only its own connection's state, never a
// counter every processor writes. (database/sql hands the most recently
// returned connection out first, so a running reader usually gets its own
// connection back, lease and all.)
//
// The rules that keep it from ever costing more than a delay:
//
//   - Only a read on a connection just checked out of the pool goes through it
//     (ResetSession marks the checkout), with no transaction open and nothing
//     ATTACHed, and it is in the gate only while the statement runs inside the
//     driver -- the rows are materialized -- never across the caller's code. A
//     retained *sql.Conn, a transaction and every write run ungated.
//   - A lease whose connection has not run a gated read for readGateIdle is
//     taken by the next reader that has none, so a lease left on a connection
//     nobody is reading with does not keep anyone out.
//   - A holder that has had its lease for readSlice hands it to the longest
//     waiter at the end of its statement.
//   - A timer runs every readGateTick while anyone waits. It hands idle leases
//     to waiters, and when every lease is held by a statement that has run for
//     a whole tick, it lets every waiter run ungated rather than wait on it. A
//     statement that waits, inside the gate, for another gated read (a
//     user-defined function querying the same database) is that case, so it
//     costs a tick, never a deadlock.
//   - However leases move, no waiter waits more than readGateMaxWait.
//
// The slot count is GOMAXPROCS when the first gated read runs; a later change of
// GOMAXPROCS is not followed. The gate is process-wide, because what it shares
// out is the processors, not a database.
const (
	readSlice       = 8 * time.Millisecond
	readGateIdle    = 50 * time.Microsecond
	readGateTick    = time.Millisecond
	readGateMaxWait = 100 * time.Millisecond
)

// Conn.lease states.
const (
	leaseNone = iota
	leaseIdle // holds a slot, not in a statement
	leaseBusy // holds a slot, in a statement
)

type readGate struct {
	nwait atomic.Int32 // len(waiters), read at the end of every gated read
	_     [60]byte     // keep that read off the cache line the mutex writes

	mu      sync.Mutex
	free    int     // slots no connection holds
	holders []*Conn // connections holding a slot
	waiters []*Conn // FIFO; each parks on its own gateCh
	timer   *time.Timer
}

var (
	theReadGate     *readGate
	theReadGateOnce sync.Once
)

func readGateFor() *readGate {
	theReadGateOnce.Do(func() { theReadGate = newReadGate(runtime.GOMAXPROCS(0)) })
	return theReadGate
}

func newReadGate(slots int) *readGate {
	g := &readGate{free: slots}
	g.timer = time.AfterFunc(time.Hour, g.tick)
	g.timer.Stop()
	return g
}

// ResetSession implements driver.SessionResetter. database/sql calls it when it
// hands out a connection that has been back in the pool, which is how the read
// gate tells a pooled read from one on a retained *sql.Conn.
func (c *Conn) ResetSession(context.Context) error {
	c.pooledCheckout = true
	return nil
}

// enterReadGate enters the gate when this statement is the first on a
// connection fresh from the pool, outside a transaction and with nothing
// ATTACHed. It returns the gate to leave, or nil when the read runs ungated.
func (c *Conn) enterReadGate() *readGate {
	pooled := c.pooledCheckout
	c.pooledCheckout = false
	if !pooled || c.tx != nil || len(c.attached) > 0 || c.temp != nil {
		return nil
	}
	g := readGateFor()
	if !g.enter(c) {
		return nil
	}
	return g
}

// enter makes c a holder in a statement, parking until a lease is handed to it.
// It reports whether c holds one; false means the timer let it run ungated.
func (g *readGate) enter(c *Conn) bool {
	if c.lease.CompareAndSwap(leaseIdle, leaseBusy) {
		return true
	}
	g.mu.Lock()
	now := nanotime()
	if g.free > 0 {
		g.free--
		g.grantLocked(c, now)
		g.mu.Unlock()
		return true
	}
	if g.stealLocked(now, readGateIdle) {
		g.grantLocked(c, now)
		g.mu.Unlock()
		return true
	}
	if c.gateCh == nil {
		c.gateCh = make(chan bool, 1)
	}
	c.waitSince = now
	g.waiters = append(g.waiters, c)
	if g.nwait.Add(1) == 1 {
		g.timer.Reset(readGateTick)
	}
	g.mu.Unlock()
	return <-c.gateCh
}

// leave ends c's gated statement. c keeps its lease unless it has had it for
// readSlice and someone waits.
func (g *readGate) leave(c *Conn) {
	now := nanotime()
	c.leaseExit.Store(now)
	if g.nwait.Load() > 0 && now-c.leaseSince > int64(readSlice) {
		g.mu.Lock()
		if len(g.waiters) > 0 {
			c.lease.Store(leaseNone)
			g.dropHolderLocked(c)
			g.grantLocked(g.popWaiterLocked(), now)
			g.mu.Unlock()
			return
		}
		g.mu.Unlock()
	}
	c.lease.Store(leaseIdle)
}

// closeLease gives up the lease of a connection that is closing.
func (c *Conn) closeLease() {
	if c.lease.Load() != leaseNone {
		c.closeLeaseOn(readGateFor())
	}
}

func (c *Conn) closeLeaseOn(g *readGate) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !c.lease.CompareAndSwap(leaseIdle, leaseNone) {
		return
	}
	g.dropHolderLocked(c)
	if len(g.waiters) > 0 {
		g.grantLocked(g.popWaiterLocked(), nanotime())
		return
	}
	g.free++
}

// grantLocked makes c a holder in a statement, and wakes it if it waits.
func (g *readGate) grantLocked(c *Conn, now int64) {
	c.lease.Store(leaseBusy)
	c.leaseSince = now
	c.leaseExit.Store(now)
	g.holders = append(g.holders, c)
	if c.waitSince != 0 {
		c.waitSince = 0
		c.gateCh <- true
	}
}

// stealLocked takes back one lease that has been idle for at least idle, and
// reports whether it did.
func (g *readGate) stealLocked(now int64, idle time.Duration) bool {
	for _, h := range g.holders {
		if now-h.leaseExit.Load() >= int64(idle) && h.lease.CompareAndSwap(leaseIdle, leaseNone) {
			g.dropHolderLocked(h)
			return true
		}
	}
	return false
}

func (g *readGate) dropHolderLocked(c *Conn) {
	if i := slices.Index(g.holders, c); i >= 0 {
		g.holders = slices.Delete(g.holders, i, i+1)
	}
}

func (g *readGate) popWaiterLocked() *Conn {
	w := g.waiters[0]
	g.waiters[0] = nil
	g.waiters = g.waiters[1:]
	g.nwait.Add(-1)
	return w
}

// tick runs every readGateTick while anyone waits; see the rules above.
func (g *readGate) tick() {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := nanotime()
	for len(g.waiters) > 0 && g.stealLocked(now, readGateTick) {
		g.grantLocked(g.popWaiterLocked(), now)
	}
	stuck := len(g.waiters) > 0 && len(g.holders) > 0
	for _, h := range g.holders {
		if h.lease.Load() != leaseBusy || now-h.leaseExit.Load() < int64(readGateTick) {
			stuck = false
			break
		}
	}
	for len(g.waiters) > 0 && (stuck || now-g.waiters[0].waitSince > int64(readGateMaxWait)) {
		w := g.popWaiterLocked()
		w.waitSince = 0
		w.gateCh <- false
	}
	if len(g.waiters) > 0 {
		g.timer.Reset(readGateTick)
	}
}

var nanotimeBase = time.Now()

// nanotime is monotonic nanoseconds since the package was initialized.
func nanotime() int64 { return int64(time.Since(nanotimeBase)) }
