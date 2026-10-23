package server

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samyfodil/musql/hrana"

	"github.com/samyfodil/musql/cloud/state"
)

// meter counts Hrana statements per database per hour in memory and writes
// them to the state in batches (flush), so a statement costs a few counter
// bumps under one database's lock, never a write.
type meter struct {
	store *state.Store
	size  func(dbID string) int64

	mu  sync.Mutex
	dbs map[string]*dbMeter

	norm      sync.Map // raw SQL -> normalized
	normCount atomic.Int64
}

// maxQueries bounds the distinct statements kept per database per hour; the
// rest are counted under otherQueries.
const (
	maxQueries   = 500
	otherQueries = "(other statements)"
	maxNormCache = 8192
	maxSQLLen    = 1000
)

type dbMeter struct {
	dbID, orgID string
	mu          sync.Mutex
	hours       map[int64]*hourMeter
}

type hourMeter struct {
	h       state.UsageHour
	queries map[string]*state.QueryUsage
}

func newMeter(store *state.Store, size func(dbID string) int64) *meter {
	return &meter{store: store, size: size, dbs: map[string]*dbMeter{}}
}

// observer is the hrana.WithObserver callback for one database.
func (m *meter) observer(dbID, orgID string) func(hrana.Stmt) {
	m.mu.Lock()
	dm, ok := m.dbs[dbID]
	if !ok {
		dm = &dbMeter{dbID: dbID, orgID: orgID, hours: map[int64]*hourMeter{}}
		m.dbs[dbID] = dm
	}
	m.mu.Unlock()
	return func(st hrana.Stmt) { dm.add(m.normalize(st.SQL), st) }
}

func (dm *dbMeter) add(q string, st hrana.Stmt) {
	hour := time.Now().Unix() / 3600 * 3600
	us := st.Duration.Microseconds()
	dm.mu.Lock()
	defer dm.mu.Unlock()
	hm, ok := dm.hours[hour]
	if !ok {
		hm = &hourMeter{h: state.UsageHour{DBID: dm.dbID, OrgID: dm.orgID, Hour: time.Unix(hour, 0).UTC()}, queries: map[string]*state.QueryUsage{}}
		dm.hours[hour] = hm
	}
	hm.h.Statements++
	hm.h.RowsRead += st.RowsRead
	hm.h.RowsWritten += st.RowsWritten
	if st.Err != nil {
		hm.h.Errors++
	}
	hm.h.Latency.Add(us)
	qu, ok := hm.queries[q]
	if !ok {
		if len(hm.queries) >= maxQueries {
			q = otherQueries
			qu = hm.queries[q]
		}
		if qu == nil {
			qu = &state.QueryUsage{DBID: dm.dbID, Hour: hm.h.Hour, SQL: q}
			hm.queries[q] = qu
		}
	}
	qu.Count++
	qu.TotalUS += us
	qu.RowsRead += st.RowsRead
	qu.RowsWritten += st.RowsWritten
}

// normalize is a statement's text with every literal replaced by "?" and
// runs of whitespace by one space, so statements that differ only in their
// values count as one. Results are cached by the raw text: clients repeat it.
func (m *meter) normalize(q string) string {
	if v, ok := m.norm.Load(q); ok {
		return v.(string)
	}
	n := normalizeSQL(q)
	if m.normCount.Add(1) > maxNormCache {
		m.norm.Clear()
		m.normCount.Store(1)
	}
	m.norm.Store(q, n)
	return n
}

func normalizeSQL(q string) string {
	var b strings.Builder
	b.Grow(len(q))
	space := false
	identChar := func(c byte) bool {
		return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
	}
	for i := 0; i < len(q) && b.Len() < maxSQLLen; {
		c := q[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			space = true
			i++
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		switch {
		case c == '\'' || (c == 'x' || c == 'X') && i+1 < len(q) && q[i+1] == '\'' && (i == 0 || !identChar(q[i-1])):
			if c != '\'' {
				i++
			}
			i++
			for i < len(q) {
				if q[i] == '\'' {
					if i+1 < len(q) && q[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteByte('?')
		case c == '"' || c == '`' || c == '[':
			end := c
			if c == '[' {
				end = ']'
			}
			j := i + 1
			for j < len(q) && q[j] != end {
				j++
			}
			if j < len(q) {
				j++
			}
			b.WriteString(q[i:j])
			i = j
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(q) && q[i+1] >= '0' && q[i+1] <= '9':
			if i > 0 && (identChar(q[i-1]) || q[i-1] == '?') {
				b.WriteByte(c)
				i++
				continue
			}
			for i < len(q) && (identChar(q[i]) || q[i] == '.' || (q[i] == '+' || q[i] == '-') && (q[i-1] == 'e' || q[i-1] == 'E')) {
				i++
			}
			b.WriteByte('?')
		default:
			b.WriteByte(c)
			i++
		}
	}
	return strings.TrimRight(b.String(), "; ")
}

// flush writes what has been counted and forgets it.
func (m *meter) flush(ctx context.Context) error {
	m.mu.Lock()
	dms := make([]*dbMeter, 0, len(m.dbs))
	for _, dm := range m.dbs {
		dms = append(dms, dm)
	}
	m.mu.Unlock()
	var hours []*state.UsageHour
	var queries []*state.QueryUsage
	for _, dm := range dms {
		dm.mu.Lock()
		taken := dm.hours
		dm.hours = map[int64]*hourMeter{}
		dm.mu.Unlock()
		if len(taken) == 0 {
			continue
		}
		size := m.size(dm.dbID)
		for _, hm := range taken {
			hm.h.BytesStored = size
			h := hm.h
			hours = append(hours, &h)
			for _, q := range hm.queries {
				queries = append(queries, q)
			}
		}
	}
	if len(hours) == 0 {
		return nil
	}
	return m.store.AddUsage(ctx, hours, queries)
}

// fileSize is a database's bytes on disk: its file and the delta beside it.
func fileSize(path string) int64 {
	var n int64
	for _, p := range []string{path, path + ".delta"} {
		if st, err := os.Stat(p); err == nil {
			n += st.Size()
		}
	}
	return n
}
