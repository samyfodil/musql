package replication

import "sync"

// Changes is told what changed in this node's tables (WithChanges): a
// local commit, an op ingested from a peer, pushed or by anti-entropy, a
// Quorum entry applied, a snapshot merged or an image installed.
type Changes interface {
	// Changed is called after rows of table change on this node, once the
	// change is visible to readers, with each changed row's encoded primary
	// key (Op.PK); nil pks means any row of the table may have changed.
	//
	// It runs on the goroutine that committed the change, which waits for
	// it: it must return quickly and must not write to the database.
	Changed(table string, pks [][]byte)
}

// WithChanges has Open tell c what changes in the database's tables, on
// every path that changes them.
func WithChanges(c Changes) Option { return func(cfg *config) { cfg.changes = c } }

// changeSet is what one transaction changed: rows by table ID, tables any of
// whose rows may have changed, or every table.
type changeSet struct {
	mu    sync.Mutex
	rows  map[string][][]byte
	whole map[string]bool
	all   bool
}

func (c *changeSet) row(tbl string, pk []byte) {
	c.mu.Lock()
	if c.rows == nil {
		c.rows = map[string][][]byte{}
	}
	c.rows[tbl] = append(c.rows[tbl], pk)
	c.mu.Unlock()
}

func (c *changeSet) table(tbl string) {
	c.mu.Lock()
	if c.whole == nil {
		c.whole = map[string]bool{}
	}
	c.whole[tbl] = true
	c.mu.Unlock()
}

func (c *changeSet) everything() {
	c.mu.Lock()
	c.all = true
	c.mu.Unlock()
}

// take empties c, returning what it held.
func (c *changeSet) take() (rows map[string][][]byte, whole map[string]bool, all bool) {
	c.mu.Lock()
	rows, whole, all = c.rows, c.whole, c.all
	c.rows, c.whole, c.all = nil, nil, false
	c.mu.Unlock()
	return rows, whole, all
}

// tell empties c into obs, naming each table as cat does; one cat does not
// know is dropped, as nothing reads it now.
func (c *changeSet) tell(obs Changes, cat *catalog) {
	rows, whole, all := c.take()
	if obs == nil || cat == nil {
		return
	}
	for _, o := range cat.Objs {
		if o.Type != "table" {
			continue
		}
		switch {
		case all || whole[o.ID]:
			obs.Changed(o.Name, nil)
		case len(rows[o.ID]) > 0:
			obs.Changed(o.Name, rows[o.ID])
		}
	}
}

// opChanges is what ops change: their rows, or every table for a schema op.
func opChanges(ops []Op) *changeSet {
	cs := &changeSet{}
	for _, op := range ops {
		if op.Kind == OpSchema {
			cs.everything()
			continue
		}
		cs.row(op.Tbl, op.PK)
	}
	return cs
}
