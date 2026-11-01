package state

import (
	"encoding/json"
	"math"
	"math/bits"
	"strconv"
	"time"
)

// LatencyBuckets is how many buckets a Latency has: four per power of two of
// microseconds, so a quantile read from it is within 12.5% of the true one.
const LatencyBuckets = 4 * 42

// Latency is a histogram of statement times.
type Latency [LatencyBuckets]uint64

// LatencyBucket is the bucket a time of us microseconds falls in.
func LatencyBucket(us int64) int {
	if us < 4 {
		if us < 0 {
			return 0
		}
		return int(us)
	}
	n := bits.Len64(uint64(us)) // us in [2^(n-1), 2^n)
	sub := int(uint64(us)>>(n-3)) & 3
	i := 4*(n-2) + sub
	if i >= LatencyBuckets {
		return LatencyBuckets - 1
	}
	return i
}

// bucketLow is the smallest time, in microseconds, bucket i holds.
func bucketLow(i int) float64 {
	if i < 4 {
		return float64(i)
	}
	n := i/4 + 2
	return float64(uint64(4+i%4) << (n - 3))
}

// Add counts one time of us microseconds.
func (l *Latency) Add(us int64) { l[LatencyBucket(us)]++ }

// Merge adds o's counts to l's.
func (l *Latency) Merge(o *Latency) {
	for i, n := range o {
		l[i] += n
	}
}

// Quantile is the q-th quantile (0..1) in milliseconds, 0 when empty: the
// middle of the bucket it falls in.
func (l *Latency) Quantile(q float64) float64 {
	var total uint64
	for _, n := range l {
		total += n
	}
	if total == 0 {
		return 0
	}
	rank := max(uint64(math.Ceil(q*float64(total))), 1)
	var seen uint64
	for i, n := range l {
		seen += n
		if seen >= rank {
			lo := bucketLow(i)
			hi := lo + 1
			if i+1 < LatencyBuckets {
				hi = bucketLow(i + 1)
			}
			return (lo + hi) / 2 / 1000
		}
	}
	return 0
}

func (l *Latency) marshal() string {
	b := []byte{'{'}
	for i, n := range l {
		if n > 0 {
			if len(b) > 1 {
				b = append(b, ',')
			}
			b = append(b, '"')
			b = strconv.AppendInt(b, int64(i), 10)
			b = append(b, '"', ':')
			b = strconv.AppendUint(b, n, 10)
		}
	}
	return string(append(b, '}'))
}

func (l *Latency) unmarshal(s string) error {
	m := map[int]uint64{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return err
	}
	for i, n := range m {
		if i >= 0 && i < LatencyBuckets {
			l[i] += n
		}
	}
	return nil
}

// UsageHour is one database's usage in one hour.
type UsageHour struct {
	DBID, OrgID                               string
	Hour                                      time.Time
	Statements, RowsRead, RowsWritten, Errors int64
	BytesStored                               int64
	Latency                                   Latency
}

// QueryUsage is one normalized statement's usage by one database in one hour.
type QueryUsage struct {
	DBID                  string
	Hour                  time.Time
	SQL                   string
	Count, TotalUS        int64
	RowsRead, RowsWritten int64
}
