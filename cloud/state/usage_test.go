package state

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestLatencyQuantiles(t *testing.T) {
	var l Latency
	for us := int64(1); us <= 10000; us++ {
		l.Add(us)
	}
	for _, c := range []struct{ q, want float64 }{{0.5, 5}, {0.95, 9.5}, {0.99, 9.9}} {
		got := l.Quantile(c.q)
		if math.Abs(got-c.want)/c.want > 0.125 {
			t.Errorf("q%.2f = %.3fms, want about %.3fms", c.q, got, c.want)
		}
	}
	for us := int64(0); us < 1<<20; us = us*5/4 + 1 {
		i := LatencyBucket(us)
		if lo := bucketLow(i); float64(us) < lo || (i+1 < LatencyBuckets && float64(us) >= bucketLow(i+1)) {
			t.Fatalf("%dus in bucket %d [%v, %v)", us, i, lo, bucketLow(i+1))
		}
	}
}

func TestUsageAndSnapshots(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	hour := time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC)
	add := func(stmts int64, us int64) {
		t.Helper()
		h := &UsageHour{DBID: "d1", OrgID: "o1", Hour: hour, Statements: stmts, RowsRead: 2 * stmts, BytesStored: stmts * 100}
		for range stmts {
			h.Latency.Add(us)
		}
		q := &QueryUsage{DBID: "d1", Hour: hour, SQL: "SELECT ?", Count: stmts, TotalUS: stmts * us}
		if err := s.AddUsage(ctx, []*UsageHour{h}, []*QueryUsage{q}); err != nil {
			t.Fatal(err)
		}
	}
	add(3, 1000)
	add(1, 100000)
	hs, err := s.UsageHours(ctx, "o1", "", hour, hour.Add(time.Hour))
	if err != nil || len(hs) != 1 {
		t.Fatalf("%v %v", hs, err)
	}
	h := hs[0]
	if h.Statements != 4 || h.RowsRead != 8 || h.BytesStored != 300 || !h.Hour.Equal(hour) {
		t.Fatalf("%+v", h)
	}
	if p50, p95 := h.Latency.Quantile(0.5), h.Latency.Quantile(0.95); p50 > 1.2 || p95 < 90 {
		t.Fatalf("p50 %v p95 %v", p50, p95)
	}
	qs, err := s.TopQueries(ctx, "d1", hour, hour.Add(time.Hour), 10)
	if err != nil || len(qs) != 1 || qs[0].Count != 4 || qs[0].TotalUS != 103000 {
		t.Fatalf("%v %v", qs, err)
	}

}
