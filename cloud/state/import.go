package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// importTables are the tables Import copies, parents first. renamed is the
// column whose value another record may hold already: the row comes in under
// its value suffixed with its ID's first 6 characters, as ResolveHeld does.
// Authorization and device codes live minutes, and are left behind.
var importTables = []struct{ table, renamed string }{
	{"users", "email"},
	{"orgs", "slug"},
	{"members", ""},
	{"invites", ""},
	{"groups", "name"},
	{"group_locations", ""},
	{"databases", "name"},
	{"platform_tokens", ""},
	{"refresh_tokens", ""},
	{"signing_keys", ""},
	{"oidc_clients", ""},
	{"usage_hours", ""},
	{"usage_queries", ""},
	{"node_usage_hours", ""},
	{"node_usage_queries", ""},
}

// Import writes the records of src, a state database this store did not
// grow from (a standalone node's, or one kept in another replication mode),
// into this one, in one transaction: a record already here by its key stays
// as it is, and one whose name another record holds comes in renamed. A
// cluster's nodes all build their state with the same statements, whose
// replicated schema is the same everywhere; another file's rows join it this
// way, never as a file. It reports how many rows it wrote.
func (s *Store) Import(ctx context.Context, src *sql.DB) (int, error) {
	var n int
	err := s.tx(ctx, func(tx *sql.Tx) error {
		n = 0
		for _, t := range importTables {
			cols, err := sharedCols(ctx, src, tx, t.table)
			if err != nil {
				return err
			}
			if len(cols) == 0 {
				continue
			}
			rows, err := readRows(ctx, src, t.table, cols)
			if err != nil {
				return err
			}
			named := strings.Join(cols, ", ")
			marks := strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")
			insert := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT DO NOTHING`, t.table, named, marks)
			if t.renamed != "" {
				insert = fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (id) DO NOTHING`, t.table, named, marks)
			}
			ren := -1
			for i, c := range cols {
				if c == t.renamed {
					ren = i
				}
			}
			for _, row := range rows {
				res, err := tx.ExecContext(ctx, insert, row...)
				if isUnique(err) && ren >= 0 {
					id, _ := row[0].(string)
					name, _ := row[ren].(string)
					if len(id) >= 6 {
						row[ren] = heldName(t.renamed, name, id)
						res, err = tx.ExecContext(ctx, insert, row...)
					}
				}
				if err != nil {
					return fmt.Errorf("state: import %s: %w", t.table, err)
				}
				k, err := res.RowsAffected()
				if err != nil {
					return err
				}
				n += int(k)
			}
		}
		return nil
	})
	return n, err
}

// sharedCols are the columns of table in both src and tx, in src's order,
// the ID first when there is one; none when src has no such table.
func sharedCols(ctx context.Context, src *sql.DB, tx *sql.Tx, table string) ([]string, error) {
	names := func(q interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	}) ([]string, error) {
		rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, rows.Err()
	}
	from, err := names(src)
	if err != nil {
		return nil, err
	}
	to, err := names(tx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range from {
		for _, d := range to {
			if c == d {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

func readRows(ctx context.Context, src *sql.DB, table string, cols []string) ([][]any, error) {
	rows, err := src.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM %s`, strings.Join(cols, ", "), table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		row := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
