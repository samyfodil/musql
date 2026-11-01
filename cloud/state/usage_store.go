package state

import (
	"context"
	"database/sql"
	"time"
)

// usageSQL is where usage is added: a standalone node's tables, or a cluster
// node's rows of its own (two nodes adding to one replicated row would each
// overwrite the other's count).
type usageSQL struct{ latency, hour, query string }

var standaloneUsage = usageSQL{
	latency: `SELECT latency FROM usage_hours WHERE ?1 = '' AND db_id = ?2 AND hour = ?3`,
	hour: `INSERT INTO usage_hours (db_id, org_id, hour, statements, rows_read, rows_written, errors, bytes_stored, latency)
		SELECT ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10 WHERE ?1 = ''
		ON CONFLICT (db_id, hour) DO UPDATE SET statements = statements + excluded.statements,
			rows_read = rows_read + excluded.rows_read, rows_written = rows_written + excluded.rows_written,
			errors = errors + excluded.errors, bytes_stored = max(bytes_stored, excluded.bytes_stored),
			latency = excluded.latency`,
	query: `INSERT INTO usage_queries (db_id, hour, sql, count, total_us, rows_read, rows_written)
		SELECT ?2, ?3, ?4, ?5, ?6, ?7, ?8 WHERE ?1 = ''
		ON CONFLICT (db_id, hour, sql) DO UPDATE SET count = count + excluded.count, total_us = total_us + excluded.total_us,
			rows_read = rows_read + excluded.rows_read, rows_written = rows_written + excluded.rows_written`,
}

var nodeUsage = usageSQL{
	latency: `SELECT latency FROM node_usage_hours WHERE node = ?1 AND db_id = ?2 AND hour = ?3`,
	hour: `INSERT INTO node_usage_hours (node, db_id, org_id, hour, statements, rows_read, rows_written, errors, bytes_stored, latency)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10)
		ON CONFLICT (node, db_id, hour) DO UPDATE SET statements = statements + excluded.statements,
			rows_read = rows_read + excluded.rows_read, rows_written = rows_written + excluded.rows_written,
			errors = errors + excluded.errors, bytes_stored = max(bytes_stored, excluded.bytes_stored),
			latency = excluded.latency`,
	query: `INSERT INTO node_usage_queries (node, db_id, hour, sql, count, total_us, rows_read, rows_written)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)
		ON CONFLICT (node, db_id, hour, sql) DO UPDATE SET count = count + excluded.count, total_us = total_us + excluded.total_us,
			rows_read = rows_read + excluded.rows_read, rows_written = rows_written + excluded.rows_written`,
}

// AddUsage adds counts to what is kept, in one transaction. Bytes stored is
// the largest size seen in the hour.
func (s *Store) AddUsage(ctx context.Context, hours []*UsageHour, queries []*QueryUsage) error {
	q := standaloneUsage
	if s.node != "" {
		q = nodeUsage
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, h := range hours {
			var lat string
			err := tx.QueryRowContext(ctx, q.latency, s.node, h.DBID, h.Hour.Unix()).Scan(&lat)
			merged := h.Latency
			switch {
			case err == nil:
				if err := merged.unmarshal(lat); err != nil {
					return err
				}
			case err != sql.ErrNoRows:
				return err
			}
			if _, err := tx.ExecContext(ctx, q.hour, s.node, h.DBID, h.OrgID, h.Hour.Unix(), h.Statements, h.RowsRead, h.RowsWritten,
				h.Errors, h.BytesStored, merged.marshal()); err != nil {
				return err
			}
		}
		for _, u := range queries {
			if _, err := tx.ExecContext(ctx, q.query, s.node, u.DBID, u.Hour.Unix(), u.SQL, u.Count, u.TotalUS, u.RowsRead, u.RowsWritten); err != nil {
				return err
			}
		}
		return nil
	})
}

// UsageHours lists the hours in [from, to) of one database, or of every
// database of an org when dbID is empty, by hour: every node's counts summed.
func (s *Store) UsageHours(ctx context.Context, orgID, dbID string, from, to time.Time) ([]*UsageHour, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT db_id, org_id, hour, statements, rows_read, rows_written, errors, bytes_stored, latency
		FROM usage_hours WHERE org_id = ?1 AND (?2 = '' OR db_id = ?2) AND hour >= ?3 AND hour < ?4
		UNION ALL SELECT db_id, org_id, hour, statements, rows_read, rows_written, errors, bytes_stored, latency
		FROM node_usage_hours WHERE org_id = ?1 AND (?2 = '' OR db_id = ?2) AND hour >= ?3 AND hour < ?4
		ORDER BY hour, db_id`,
		orgID, dbID, from.Unix(), to.Unix())
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
		if n := len(out); n > 0 && out[n-1].DBID == h.DBID && out[n-1].Hour.Equal(h.Hour) {
			p := out[n-1]
			p.Statements += h.Statements
			p.RowsRead += h.RowsRead
			p.RowsWritten += h.RowsWritten
			p.Errors += h.Errors
			p.BytesStored = max(p.BytesStored, h.BytesStored)
			p.Latency.Merge(&h.Latency)
			continue
		}
		out = append(out, &h)
	}
	return out, rows.Err()
}

// TopQueries is a database's statements in [from, to), most total time first.
func (s *Store) TopQueries(ctx context.Context, dbID string, from, to time.Time, limit int) ([]*QueryUsage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sql, sum(count), sum(total_us), sum(rows_read), sum(rows_written)
		FROM (SELECT sql, count, total_us, rows_read, rows_written FROM usage_queries WHERE db_id = ?1 AND hour >= ?2 AND hour < ?3
			UNION ALL SELECT sql, count, total_us, rows_read, rows_written FROM node_usage_queries WHERE db_id = ?1 AND hour >= ?2 AND hour < ?3)
		GROUP BY sql ORDER BY sum(total_us) DESC, sql LIMIT ?4`,
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
