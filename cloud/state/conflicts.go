package state

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/samyfodil/musql/replication"
)

// heldNames is, per table, the column whose UNIQUE value two cluster nodes
// can both claim: the row replication holds out is written back renamed.
var heldNames = map[string]string{
	"users":     "email",
	"orgs":      "slug",
	"groups":    "name",
	"databases": "name",
}

// heldName is the name a row comes back under when another holds its own:
// suffixed with its ID's first 6 characters, "shop-1a2b3c", or for an email
// "a+1a2b3c@example.com". A name that has the suffix already keeps it.
func heldName(col, name, id string) string {
	if strings.HasSuffix(name, "-"+id[:6]) || strings.Contains(name, "+"+id[:6]+"@") {
		return name
	}
	if col == "email" {
		local, domain, _ := strings.Cut(name, "@")
		return local + "+" + id[:6] + "@" + domain
	}
	return name + "-" + id[:6]
}

// Renamed is a row ResolveHeld wrote back under a new name.
type Renamed struct {
	Table, ID, From, To string
}

// ResolveHeld renames the rows a cluster's state holds out because another
// node claimed the same name: each comes back under its name suffixed with
// its ID's first 6 characters. Every node computes the same row, so the
// writes merge to one value.
func (s *Store) ResolveHeld(ctx context.Context) ([]Renamed, error) {
	if s.node == "" {
		return nil, nil
	}
	held, err := replication.Withheld(ctx, s.db)
	if err != nil {
		return nil, err
	}
	var out []Renamed
	for _, h := range held {
		col, ok := heldNames[h.Table]
		if !ok {
			continue
		}
		id, _ := h.Row["id"].(string)
		name, _ := h.Row[col].(string)
		if len(id) < 6 || name == "" {
			continue
		}
		to := heldName(col, name, id)
		if to == name {
			continue
		}
		h.Row[col] = to
		cols := make([]string, 0, len(h.Row))
		for c := range h.Row {
			cols = append(cols, c)
		}
		slices.Sort(cols)
		args := make([]any, len(cols))
		for i, c := range cols {
			args[i] = h.Row[c]
			cols[i] = `"` + c + `"`
		}
		q := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, h.Table, strings.Join(cols, ", "), strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", "))
		if _, err := s.exec(ctx, q, args...); err != nil {
			// Another node's rename of this row may have reached this one
			// since Withheld read it: every node writes the same row.
			var got string
			if s.q().QueryRowContext(ctx, fmt.Sprintf(`SELECT "%s" FROM %s WHERE id = ?`, col, h.Table), id).Scan(&got) == nil && got == to {
				continue
			}
			return out, fmt.Errorf("state: rename held %s %s: %w", h.Table, id, err)
		}
		out = append(out, Renamed{Table: h.Table, ID: id, From: name, To: to})
	}
	return out, nil
}
