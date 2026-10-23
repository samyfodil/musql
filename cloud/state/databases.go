package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrInUse is returned when deleting a group that still holds databases.
var ErrInUse = errors.New("state: in use")

// Group is a set of locations databases are placed in.
type Group struct {
	ID        string
	OrgID     string
	Name      string
	Primary   string
	Locations []string
	TokenGen  int64
	CreatedAt time.Time
}

func createGroupTx(tx *sql.Tx, orgID, name, location string) error {
	id := NewID()
	if _, err := tx.Exec(`INSERT INTO groups (id, org_id, name, primary_location, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, orgID, name, location, now()); err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	if location == "" {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO group_locations (group_id, location) VALUES (?, ?)`, id, location)
	return err
}

// CreateGroup creates a group whose primary is location.
func (s *Store) CreateGroup(ctx context.Context, orgID, name, location string) (*Group, error) {
	if err := s.tx(ctx, func(tx *sql.Tx) error { return createGroupTx(tx, orgID, name, location) }); err != nil {
		return nil, err
	}
	return s.Group(ctx, orgID, name)
}

// EnsureGroupLocation gives a group with no location the given one: a
// personal org is created before the node's location is known to the store.
func (s *Store) EnsureGroupLocation(ctx context.Context, location string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT id FROM groups WHERE primary_location = ''`)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE groups SET primary_location = ? WHERE id = ?`, location, id); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO group_locations (group_id, location) VALUES (?, ?)`, id, location); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) loadLocations(ctx context.Context, g *Group) error {
	rows, err := s.db.QueryContext(ctx, `SELECT location FROM group_locations WHERE group_id = ? ORDER BY location`, g.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	g.Locations = nil
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return err
		}
		g.Locations = append(g.Locations, l)
	}
	return rows.Err()
}

const groupCols = `id, org_id, name, primary_location, token_gen, created_at`

func scanGroup(r scanner) (*Group, error) {
	var g Group
	var created int64
	if err := r.Scan(&g.ID, &g.OrgID, &g.Name, &g.Primary, &g.TokenGen, &created); err != nil {
		return nil, notFound(err)
	}
	g.CreatedAt = unix(created)
	return &g, nil
}

// Group returns an org's group by name.
func (s *Store) Group(ctx context.Context, orgID, name string) (*Group, error) {
	g, err := scanGroup(s.db.QueryRowContext(ctx, `SELECT `+groupCols+` FROM groups WHERE org_id = ? AND name = ?`, orgID, name))
	if err != nil {
		return nil, err
	}
	return g, s.loadLocations(ctx, g)
}

// GroupByID returns a group.
func (s *Store) GroupByID(ctx context.Context, id string) (*Group, error) {
	g, err := scanGroup(s.db.QueryRowContext(ctx, `SELECT `+groupCols+` FROM groups WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return g, s.loadLocations(ctx, g)
}

// Groups lists an org's groups.
func (s *Store) Groups(ctx context.Context, orgID string) ([]*Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+groupCols+` FROM groups WHERE org_id = ? ORDER BY name`, orgID)
	if err != nil {
		return nil, err
	}
	var out []*Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, g := range out {
		if err := s.loadLocations(ctx, g); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteGroup removes an empty group.
func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM databases WHERE group_id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrInUse
		}
		if err := one(tx.Exec(`DELETE FROM groups WHERE id = ?`, id)); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM group_locations WHERE group_id = ?`, id)
		return err
	})
}

// AddLocation adds a location to a group.
func (s *Store) AddLocation(ctx context.Context, groupID, location string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO group_locations (group_id, location) VALUES (?, ?)`, groupID, location)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

// RemoveLocation removes a non-primary location from a group.
func (s *Store) RemoveLocation(ctx context.Context, groupID, location string) error {
	return one(s.db.ExecContext(ctx, `DELETE FROM group_locations WHERE group_id = ? AND location = ?
		AND location <> (SELECT primary_location FROM groups WHERE id = ?)`, groupID, location, groupID))
}

// BumpGroupTokens invalidates every group token issued so far.
func (s *Store) BumpGroupTokens(ctx context.Context, groupID string) error {
	return one(s.db.ExecContext(ctx, `UPDATE groups SET token_gen = token_gen + 1 WHERE id = ?`, groupID))
}

// Database is a hosted database's record. Its file is named by ID, so a
// rename touches only this record.
type Database struct {
	ID         string
	OrgID      string
	Name       string
	GroupID    string
	Parent     string
	ParentAt   time.Time
	SchemaOnly bool
	TokenGen   int64
	CreatedAt  time.Time
	// File is the database file's name in its org's directory; empty means
	// "<ID>.musq".
	File string
}

const dbCols = `id, org_id, name, group_id, parent, parent_at, schema_only, token_gen, created_at, file`

func scanDatabase(r scanner) (*Database, error) {
	var d Database
	var parentAt, created int64
	if err := r.Scan(&d.ID, &d.OrgID, &d.Name, &d.GroupID, &d.Parent, &parentAt, &d.SchemaOnly, &d.TokenGen, &created, &d.File); err != nil {
		return nil, notFound(err)
	}
	d.ParentAt, d.CreatedAt = unix(parentAt), unix(created)
	return &d, nil
}

// CreateDatabase records a new database; d.ID is assigned when empty.
func (s *Store) CreateDatabase(ctx context.Context, d *Database) error {
	if d.ID == "" {
		d.ID = NewID()
	}
	d.CreatedAt = unix(now())
	var parentAt int64
	if !d.ParentAt.IsZero() {
		parentAt = d.ParentAt.Unix()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO databases (`+dbCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		d.ID, d.OrgID, d.Name, d.GroupID, d.Parent, parentAt, d.SchemaOnly, d.CreatedAt.Unix(), d.File)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

// Database returns an org's database by name.
func (s *Store) Database(ctx context.Context, orgID, name string) (*Database, error) {
	return scanDatabase(s.db.QueryRowContext(ctx, `SELECT `+dbCols+` FROM databases WHERE org_id = ? AND name = ?`, orgID, name))
}

// DatabaseByID returns a database.
func (s *Store) DatabaseByID(ctx context.Context, id string) (*Database, error) {
	return scanDatabase(s.db.QueryRowContext(ctx, `SELECT `+dbCols+` FROM databases WHERE id = ?`, id))
}

// Databases lists an org's databases, optionally of one group.
func (s *Store) Databases(ctx context.Context, orgID, groupID string) ([]*Database, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+dbCols+` FROM databases WHERE org_id = ? AND (? = '' OR group_id = ?) ORDER BY name`, orgID, groupID, groupID)
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

// RenameDatabase renames a database within its org.
func (s *Store) RenameDatabase(ctx context.Context, id, name string) error {
	return one(s.db.ExecContext(ctx, `UPDATE databases SET name = ? WHERE id = ?`, name, id))
}

// DeleteDatabase removes a database's record. Its usage stays: it is part of
// the org's history.
func (s *Store) DeleteDatabase(ctx context.Context, id string) error {
	return one(s.db.ExecContext(ctx, `DELETE FROM databases WHERE id = ?`, id))
}

// SetDatabaseFile points a database's record at another file.
func (s *Store) SetDatabaseFile(ctx context.Context, id, file string) error {
	return one(s.db.ExecContext(ctx, `UPDATE databases SET file = ? WHERE id = ?`, file, id))
}

// BumpDatabaseTokens invalidates every token issued so far for a database.
func (s *Store) BumpDatabaseTokens(ctx context.Context, id string) error {
	return one(s.db.ExecContext(ctx, `UPDATE databases SET token_gen = token_gen + 1 WHERE id = ?`, id))
}
