package state

import "context"

// AllDatabases lists every database of every org.
func (s *Store) AllDatabases(ctx context.Context) ([]*Database, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+dbCols+` FROM databases ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Database
	for rows.Next() {
		d, err := scanDatabase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
