package driver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// enterAsync starts c waiting on g and returns where its result arrives.
func enterAsync(g *readGate, c *Conn) <-chan bool {
	got := make(chan bool, 1)
	go func() { got <- g.enter(c) }()
	for g.nwait.Load() == 0 {
		runtime.Gosched()
	}
	return got
}

func waitHeld(t *testing.T, got <-chan bool, wantHeld bool, what string) {
	t.Helper()
	select {
	case held := <-got:
		if held != wantHeld {
			t.Fatalf("%s: held=%v, want %v", what, held, wantHeld)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: the waiter never got through", what)
	}
}

func TestReadGateKeepsLeaseBetweenStatements(t *testing.T) {
	g := newReadGate(1)
	c := &Conn{}
	for range 3 {
		if !g.enter(c) {
			t.Fatal("enter without a slot")
		}
		g.leave(c)
	}
	if c.lease.Load() != leaseIdle || g.free != 0 || len(g.holders) != 1 {
		t.Fatalf("lease=%d free=%d holders=%d, want the connection to keep its one slot", c.lease.Load(), g.free, len(g.holders))
	}
	c.closeLeaseOn(g)
	if c.lease.Load() != leaseNone || g.free != 1 || len(g.holders) != 0 {
		t.Fatalf("after close: lease=%d free=%d holders=%d, want the slot back", c.lease.Load(), g.free, len(g.holders))
	}
}

func TestReadGateHandsOverAfterSlice(t *testing.T) {
	g := newReadGate(1)
	holder, waiter := &Conn{}, &Conn{}
	g.enter(holder)
	got := enterAsync(g, waiter)
	holder.leaseSince = nanotime() - int64(2*readSlice)
	g.leave(holder)
	waitHeld(t, got, true, "a holder past its slice")
	if holder.lease.Load() != leaseNone || waiter.lease.Load() != leaseBusy {
		t.Fatalf("holder lease=%d waiter lease=%d, want the slot moved", holder.lease.Load(), waiter.lease.Load())
	}
}

// A lease left on a connection nobody reads with must not keep a waiter out.
func TestReadGateIdleLeaseReachesWaiter(t *testing.T) {
	g := newReadGate(1)
	holder, waiter := &Conn{}, &Conn{}
	g.enter(holder)
	got := enterAsync(g, waiter)
	g.leave(holder) // within its slice: it keeps the lease, then stops reading
	waitHeld(t, got, true, "an idle lease")
}

// A reader with no lease takes one that has sat idle, without waiting.
func TestReadGateStealsIdleLease(t *testing.T) {
	g := newReadGate(1)
	holder, other := &Conn{}, &Conn{}
	g.enter(holder)
	g.leave(holder)
	holder.leaseExit.Store(nanotime() - int64(2*readGateIdle))
	if !g.enter(other) {
		t.Fatal("the idle lease was not taken")
	}
	if holder.lease.Load() != leaseNone {
		t.Fatal("the lease is held twice")
	}
	// The holder comes back: its lease is gone, so it must wait like anyone.
	got := enterAsync(g, holder)
	g.leave(other)
	waitHeld(t, got, true, "the former holder")
}

// A holder that never leaves its statement must not hold the waiters up.
func TestReadGateStuckHolderLetsWaitersThrough(t *testing.T) {
	g := newReadGate(1)
	holder, waiter := &Conn{}, &Conn{}
	g.enter(holder)
	got := enterAsync(g, waiter)
	waitHeld(t, got, false, "a holder that never left")
}

// Shared-pool point reads from more goroutines than processors, each value
// checked, through the gate.
func TestReadGateSharedPoolReads(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "g.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const rows, readers, perReader = 500, 24, 400
	if _, err := db.Exec(`CREATE TABLE kv (k INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	for k := 1; k <= rows; k++ {
		if _, err := db.Exec(`INSERT INTO kv VALUES (?, ?)`, k, fmt.Sprintf("v%d", k)); err != nil {
			t.Fatal(err)
		}
	}
	db.SetMaxOpenConns(readers)
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for r := range readers {
		wg.Go(func() {
			for i := range perReader {
				k := (r*perReader+i)%rows + 1
				var v string
				if err := db.QueryRow(`SELECT v FROM kv WHERE k = ?`, k).Scan(&v); err != nil {
					errs <- err
					return
				}
				if want := fmt.Sprintf("v%d", k); v != want {
					errs <- fmt.Errorf("k=%d: got %q, want %q", k, v, want)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func init() {
	if err := RegisterFunction("read_gate_nested", 1, false, func(args []any) (any, error) {
		db := readGateNestedDB
		var v string
		if err := db.QueryRow(`SELECT v FROM kv WHERE k = ?`, args[0]).Scan(&v); err != nil {
			return nil, err
		}
		return v, nil
	}); err != nil {
		panic(err)
	}
}

var readGateNestedDB *sql.DB

// A user-defined function that queries the same database from inside a gated
// read: with every slot held by such a statement, the inner reads must still
// run, or the gate is a deadlock.
func TestReadGateNestedReadDoesNotDeadlock(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "n.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	readGateNestedDB = db
	if _, err := db.Exec(`CREATE TABLE kv (k INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO kv VALUES (1, 'one')`); err != nil {
		t.Fatal(err)
	}
	outer := 2*runtime.GOMAXPROCS(0) + 2
	done := make(chan error, outer)
	for range outer {
		go func() {
			for range 20 {
				var v string
				if err := db.QueryRow(`SELECT read_gate_nested(1)`).Scan(&v); err != nil {
					done <- err
					return
				}
				if v != "one" {
					done <- fmt.Errorf("got %q, want %q", v, "one")
					return
				}
			}
			done <- nil
		}()
	}
	timeout := time.After(60 * time.Second)
	for range outer {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-timeout:
			t.Fatal("nested reads deadlocked in the read gate")
		}
	}
}
