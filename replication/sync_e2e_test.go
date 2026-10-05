package replication

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestEndToEndSync wires two opened databases onto a MemNetwork by hand and drives
// them purely through Node.Run, proving the whole capture -> propagate ->
// apply loop end to end without libp2p: a preupdate-hook capture on one site's
// *sql.DB must reach the other site's user table via Syncer.Attach /
// Syncer.OpSource / Node.Broadcast / Node.Sync, with no direct calls into the
// store on the writer's behalf.
func TestEndToEndSync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	net := NewMemNetwork()

	sA, dbA := wrapUsers(t, "e2eA")
	sB, dbB := wrapUsers(t, "e2eB")

	idA, idB := PeerID(sA.Site()), PeerID(sB.Site())
	trA := net.AddNode(idA, sA.OpSource())
	trB := net.AddNode(idB, sB.OpSource())

	nodeA := sA.Attach(trA)
	nodeB := sB.Attach(trB)
	if nodeA.Transport() != trA {
		t.Fatalf("nodeA.Transport() = %v, want the transport passed to Attach", nodeA.Transport())
	}
	if nodeB.Transport() != trB {
		t.Fatalf("nodeB.Transport() = %v, want the transport passed to Attach", nodeB.Transport())
	}

	// Partition before either side writes, so the two sites can only
	// converge once healed via Node.Run's periodic anti-entropy Sync (rather
	// than the synchronous Broadcast push reaching a live peer), exercising
	// Syncer.OpSource's LocalVV/OpsSince/ReceiveOp through the full Sync path.
	net.Partition(idA, idB, true)

	// Each Run writes its database until it returns; the cleanup that closes
	// and removes them must wait for that.
	var runs sync.WaitGroup
	t.Cleanup(runs.Wait)
	t.Cleanup(cancel)
	for _, n := range []*Node{nodeA, nodeB} {
		runs.Add(1)
		go func() {
			defer runs.Done()
			n.Run(ctx, 20*time.Millisecond)
		}()
	}

	if _, err := dbA.ExecContext(ctx, `INSERT INTO users(id,name,age) VALUES('a1','alice',30)`); err != nil {
		t.Fatal(err)
	}
	if _, err := dbB.ExecContext(ctx, `INSERT INTO users(id,name,age) VALUES('b1','bob',25)`); err != nil {
		t.Fatal(err)
	}

	// Each side only knows about its own write while partitioned.
	if got, want := userDump(t, dbA), "a1|alice|30\n"; got != want {
		t.Fatalf("A while partitioned = %q, want %q", got, want)
	}
	if got, want := userDump(t, dbB), "b1|bob|25\n"; got != want {
		t.Fatalf("B while partitioned = %q, want %q", got, want)
	}

	net.Partition(idA, idB, false)

	const want = "a1|alice|30\nb1|bob|25\n"
	deadline := time.Now().Add(10 * time.Second)
	var gotA, gotB string
	for time.Now().Before(deadline) {
		gotA = userDump(t, dbA)
		gotB = userDump(t, dbB)
		if gotA == want && gotB == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("did not converge within timeout: A=%q B=%q", gotA, gotB)
}
