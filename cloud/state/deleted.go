package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrGroupDeleted is returned when restoring a database whose group is
// deleted: the group comes back first.
var ErrGroupDeleted = errors.New("state: its group is deleted")

// DeletedDatabase is a deleted database's record, kept until its org's
// retention for deleted databases ends. DeletedWith is the group it was
// deleted with, "" when it was deleted alone.
type DeletedDatabase struct {
	Database
	DeletedAt   time.Time
	DeletedWith string
}

// DeletedGroup is a deleted group's record. Its locations stay in
// group_locations until it is purged.
type DeletedGroup struct {
	Group
	DeletedAt time.Time
}

const deletedDBCols = dbCols + `, deleted_at, deleted_with`

// moveDatabases moves the databases where matches to deleted_databases,
// stamped now and with the group they go with.
func moveDatabases(tx *sql.Tx, with, where string, args ...any) error {
	if _, err := tx.Exec(`INSERT INTO deleted_databases (`+deletedDBCols+`) SELECT `+dbCols+`, ?, ? FROM databases WHERE `+where,
		append([]any{now(), with}, args...)...); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM databases WHERE `+where, args...)
	return err
}

// SoftDeleteDatabase deletes a database's record and keeps it as deleted:
// its name is free, no token reaches it, and RestoreDatabase brings it back.
func (s *Store) SoftDeleteDatabase(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var protected bool
		if err := tx.QueryRow(`SELECT delete_protection FROM databases WHERE id = ?`, id).Scan(&protected); err != nil {
			return notFound(err)
		}
		if protected {
			return ErrProtected
		}
		return moveDatabases(tx, "", `id = ?`, id)
	})
}

// DeleteGroup deletes a group and its databases, kept as SoftDeleteDatabase
// keeps one, and returns the databases. Refused while the group or one of its
// databases is protected.
func (s *Store) DeleteGroup(ctx context.Context, id string) ([]*Database, error) {
	var dbs []*Database
	err := s.tx(ctx, func(tx *sql.Tx) error {
		dbs = nil
		g, err := scanGroup(tx.QueryRow(`SELECT `+groupCols+` FROM groups WHERE id = ?`, id))
		if err != nil {
			return err
		}
		if g.DeleteProtection {
			return ErrProtected
		}
		rows, err := tx.Query(`SELECT `+dbCols+` FROM databases WHERE group_id = ?`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			d, err := scanDatabase(rows)
			if err != nil {
				rows.Close()
				return err
			}
			dbs = append(dbs, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range dbs {
			if d.DeleteProtection {
				return ErrProtected
			}
		}
		if err := moveDatabases(tx, id, `group_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO deleted_groups (`+groupCols+`, deleted_at) SELECT `+groupCols+`, ? FROM groups WHERE id = ?`, now(), id); err != nil {
			return err
		}
		return one(tx.Exec(`DELETE FROM groups WHERE id = ?`, id))
	})
	return dbs, err
}

func scanDeletedDatabase(r scanner) (*DeletedDatabase, error) {
	var d DeletedDatabase
	var parentAt, created, deleted int64
	if err := r.Scan(&d.ID, &d.OrgID, &d.Name, &d.GroupID, &d.Parent, &parentAt, &d.SchemaOnly, &d.TokenGen, &created, &d.File, &d.Type, &d.Durability,
		&d.DeleteProtection, &d.BlockReads, &d.BlockWrites, &deleted, &d.DeletedWith); err != nil {
		return nil, notFound(err)
	}
	d.ParentAt, d.CreatedAt, d.DeletedAt = unix(parentAt), unix(created), unix(deleted)
	return &d, nil
}

// DeletedDatabases lists an org's deleted databases, or every org's when
// orgID is "".
func (s *Store) DeletedDatabases(ctx context.Context, orgID string) ([]*DeletedDatabase, error) {
	rows, err := s.q().QueryContext(ctx, `SELECT `+deletedDBCols+` FROM deleted_databases WHERE ? = '' OR org_id = ? ORDER BY deleted_at DESC, name`, orgID, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DeletedDatabase
	for rows.Next() {
		d, err := scanDeletedDatabase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeletedDatabaseByID returns a deleted database.
func (s *Store) DeletedDatabaseByID(ctx context.Context, id string) (*DeletedDatabase, error) {
	return scanDeletedDatabase(s.q().QueryRowContext(ctx, `SELECT `+deletedDBCols+` FROM deleted_databases WHERE id = ?`, id))
}

func scanDeletedGroup(r scanner) (*DeletedGroup, error) {
	var g DeletedGroup
	var created, deleted int64
	if err := r.Scan(&g.ID, &g.OrgID, &g.Name, &g.Primary, &g.TokenGen, &created, &g.Type, &g.DeleteProtection, &deleted); err != nil {
		return nil, notFound(err)
	}
	g.CreatedAt, g.DeletedAt = unix(created), unix(deleted)
	return &g, nil
}

// DeletedGroups lists an org's deleted groups, or every org's when orgID is
// "".
func (s *Store) DeletedGroups(ctx context.Context, orgID string) ([]*DeletedGroup, error) {
	rows, err := s.q().QueryContext(ctx, `SELECT `+groupCols+`, deleted_at FROM deleted_groups WHERE ? = '' OR org_id = ? ORDER BY deleted_at DESC, name`, orgID, orgID)
	if err != nil {
		return nil, err
	}
	var out []*DeletedGroup
	for rows.Next() {
		g, err := scanDeletedGroup(rows)
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
		if err := s.loadLocations(ctx, &g.Group); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeletedGroupByID returns a deleted group.
func (s *Store) DeletedGroupByID(ctx context.Context, id string) (*DeletedGroup, error) {
	g, err := scanDeletedGroup(s.q().QueryRowContext(ctx, `SELECT `+groupCols+`, deleted_at FROM deleted_groups WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return g, s.loadLocations(ctx, &g.Group)
}

// restoreDatabaseTx moves a deleted database back under name, with a new
// token generation so no token from before reaches it.
func restoreDatabaseTx(tx *sql.Tx, id, name string) error {
	if _, err := tx.Exec(`INSERT INTO databases (`+dbCols+`) SELECT id, org_id, ?, group_id, parent, parent_at, schema_only, token_gen + 1, created_at,
		file, type, durability, delete_protection, block_reads, block_writes FROM deleted_databases WHERE id = ?`, name, id); err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	return one(tx.Exec(`DELETE FROM deleted_databases WHERE id = ?`, id))
}

// RestoreDatabase brings a deleted database back under name (its own when
// empty). Its group must exist.
func (s *Store) RestoreDatabase(ctx context.Context, id, name string) (*Database, error) {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		d, err := scanDeletedDatabase(tx.QueryRow(`SELECT `+deletedDBCols+` FROM deleted_databases WHERE id = ?`, id))
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM groups WHERE id = ?`, d.GroupID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrGroupDeleted
		}
		if name == "" {
			name = d.Name
		}
		return restoreDatabaseTx(tx, id, name)
	})
	if err != nil {
		return nil, err
	}
	return s.DatabaseByID(ctx, id)
}

// RestoreGroup brings a deleted group back under name (its own when empty)
// with the databases deleted with it, each under its name or, when that is
// taken, its name suffixed with its ID's first 6 characters.
func (s *Store) RestoreGroup(ctx context.Context, id, name string) (*Group, []*Database, error) {
	var ids []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		ids = nil
		g, err := scanDeletedGroup(tx.QueryRow(`SELECT `+groupCols+`, deleted_at FROM deleted_groups WHERE id = ?`, id))
		if err != nil {
			return err
		}
		if name == "" {
			name = g.Name
		}
		if _, err := tx.Exec(`INSERT INTO groups (`+groupCols+`) SELECT id, org_id, ?, primary_location, token_gen + 1, created_at, type, delete_protection
			FROM deleted_groups WHERE id = ?`, name, id); err != nil {
			if isUnique(err) {
				return ErrExists
			}
			return err
		}
		if err := one(tx.Exec(`DELETE FROM deleted_groups WHERE id = ?`, id)); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT id, name FROM deleted_databases WHERE deleted_with = ?`, id)
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var dbID, dbName string
			if err := rows.Scan(&dbID, &dbName); err != nil {
				rows.Close()
				return err
			}
			ids, names = append(ids, dbID), append(names, dbName)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i, dbID := range ids {
			err := restoreDatabaseTx(tx, dbID, names[i])
			if errors.Is(err, ErrExists) {
				err = restoreDatabaseTx(tx, dbID, heldName("name", names[i], dbID))
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	g, err := s.GroupByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	var dbs []*Database
	for _, dbID := range ids {
		d, err := s.DatabaseByID(ctx, dbID)
		if err != nil {
			return nil, nil, err
		}
		dbs = append(dbs, d)
	}
	return g, dbs, nil
}

// PurgeDeletedDatabase forgets a deleted database for good.
func (s *Store) PurgeDeletedDatabase(ctx context.Context, id string) error {
	return one(s.exec(ctx, `DELETE FROM deleted_databases WHERE id = ?`, id))
}

// PurgeDeletedGroup forgets a deleted group and its locations for good.
func (s *Store) PurgeDeletedGroup(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := one(tx.Exec(`DELETE FROM deleted_groups WHERE id = ?`, id)); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM group_locations WHERE group_id = ?`, id)
		return err
	})
}

// UpdateDatabase sets a database's delete protection and blocks; a nil
// field is left as it is.
func (s *Store) UpdateDatabase(ctx context.Context, id string, protect, blockReads, blockWrites *bool) error {
	return one(s.exec(ctx, `UPDATE databases SET delete_protection = coalesce(?, delete_protection),
		block_reads = coalesce(?, block_reads), block_writes = coalesce(?, block_writes) WHERE id = ?`, protect, blockReads, blockWrites, id))
}

// SetGroupProtection sets a group's delete protection.
func (s *Store) SetGroupProtection(ctx context.Context, id string, on bool) error {
	return one(s.exec(ctx, `UPDATE groups SET delete_protection = ? WHERE id = ?`, on, id))
}
