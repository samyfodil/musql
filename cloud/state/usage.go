package state

import (
	"context"
	"database/sql"
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

// AddUsage adds counts to what is kept, in one transaction. Bytes stored is
// the largest size seen in the hour.
func (s *Store) AddUsage(ctx context.Context, hours []*UsageHour, queries []*QueryUsage) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, h := range hours {
			var lat string
			err := tx.QueryRowContext(ctx, `SELECT latency FROM usage_hours WHERE db_id = ? AND hour = ?`, h.DBID, h.Hour.Unix()).Scan(&lat)
			merged := h.Latency
			switch {
			case err == nil:
				if err := merged.unmarshal(lat); err != nil {
					return err
				}
			case err != sql.ErrNoRows:
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO usage_hours (db_id, org_id, hour, statements, rows_read, rows_written, errors, bytes_stored, latency)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (db_id, hour) DO UPDATE SET statements = statements + excluded.statements,
					rows_read = rows_read + excluded.rows_read, rows_written = rows_written + excluded.rows_written,
					errors = errors + excluded.errors, bytes_stored = max(bytes_stored, excluded.bytes_stored),
					latency = excluded.latency`,
				h.DBID, h.OrgID, h.Hour.Unix(), h.Statements, h.RowsRead, h.RowsWritten, h.Errors, h.BytesStored, merged.marshal()); err != nil {
				return err
			}
		}
		for _, q := range queries {
			if _, err := tx.ExecContext(ctx, `INSERT INTO usage_queries (db_id, hour, sql, count, total_us, rows_read, rows_written)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (db_id, hour, sql) DO UPDATE SET count = count + excluded.count, total_us = total_us + excluded.total_us,
					rows_read = rows_read + excluded.rows_read, rows_written = rows_written + excluded.rows_written`,
				q.DBID, q.Hour.Unix(), q.SQL, q.Count, q.TotalUS, q.RowsRead, q.RowsWritten); err != nil {
				return err
			}
		}
		return nil
	})
}

// UsageHours lists the hours in [from, to) of one database, or of every
// database of an org when dbID is empty, by hour.
func (s *Store) UsageHours(ctx context.Context, orgID, dbID string, from, to time.Time) ([]*UsageHour, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT db_id, org_id, hour, statements, rows_read, rows_written, errors, bytes_stored, latency
		FROM usage_hours WHERE org_id = ? AND (? = '' OR db_id = ?) AND hour >= ? AND hour < ? ORDER BY hour, db_id`,
		orgID, dbID, dbID, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UsageHour
	for rows.Next() {
		var h UsageHour
		var hour int64
		var lat string
		if err := rows.Scan(&h.DBID, &h.OrgID, &hour, &h.Statements, &h.RowsRead, &h.RowsWritten, &h.Errors, &h.BytesStored, &lat); err != nil {
			return nil, err
		}
		if err := h.Latency.unmarshal(lat); err != nil {
			return nil, err
		}
		h.Hour = time.Unix(hour, 0).UTC()
		out = append(out, &h)
	}
	return out, rows.Err()
}

// TopQueries is a database's statements in [from, to), most total time first.
func (s *Store) TopQueries(ctx context.Context, dbID string, from, to time.Time, limit int) ([]*QueryUsage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sql, sum(count), sum(total_us), sum(rows_read), sum(rows_written)
		FROM usage_queries WHERE db_id = ? AND hour >= ? AND hour < ? GROUP BY sql ORDER BY sum(total_us) DESC, sql LIMIT ?`,
		dbID, from.Unix(), to.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*QueryUsage
	for rows.Next() {
		q := QueryUsage{DBID: dbID}
		if err := rows.Scan(&q.SQL, &q.Count, &q.TotalUS, &q.RowsRead, &q.RowsWritten); err != nil {
			return nil, err
		}
		out = append(out, &q)
	}
	return out, rows.Err()
}
