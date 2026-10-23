// Package state keeps musql cloud's own records in a musql database: users,
// orgs and their members, groups, databases, and the tokens and keys the
// OIDC provider issues and signs with.
package state

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/samyfodil/musql/driver"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("state: not found")

// ErrExists is returned when a record with the same unique key exists.
var ErrExists = errors.New("state: already exists")

// Store is the service's state database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the state database at path and brings its
// schema up to date.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection: every write is serialized in-process, so two requests
	// never race each other for the file's write lock.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

var migrations = []string{
	`CREATE TABLE users (
		id TEXT PRIMARY KEY,
		email TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		password_hash TEXT NOT NULL,
		admin INTEGER NOT NULL DEFAULT 0,
		disabled INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE orgs (
		id TEXT PRIMARY KEY,
		slug TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		personal INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE members (
		org_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		role TEXT NOT NULL,
		PRIMARY KEY (org_id, user_id)
	);
	CREATE TABLE invites (
		id TEXT PRIMARY KEY,
		org_id TEXT NOT NULL,
		email TEXT NOT NULL,
		role TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		UNIQUE (org_id, email)
	);
	CREATE TABLE groups (
		id TEXT PRIMARY KEY,
		org_id TEXT NOT NULL,
		name TEXT NOT NULL,
		primary_location TEXT NOT NULL,
		token_gen INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		UNIQUE (org_id, name)
	);
	CREATE TABLE group_locations (
		group_id TEXT NOT NULL,
		location TEXT NOT NULL,
		PRIMARY KEY (group_id, location)
	);
	CREATE TABLE databases (
		id TEXT PRIMARY KEY,
		org_id TEXT NOT NULL,
		name TEXT NOT NULL,
		group_id TEXT NOT NULL,
		parent TEXT NOT NULL DEFAULT '',
		parent_at INTEGER NOT NULL DEFAULT 0,
		schema_only INTEGER NOT NULL DEFAULT 0,
		token_gen INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		UNIQUE (org_id, name)
	);
	CREATE TABLE platform_tokens (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		name TEXT NOT NULL,
		hash TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL,
		last_used_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE refresh_tokens (
		hash TEXT PRIMARY KEY,
		family TEXT NOT NULL,
		user_id TEXT NOT NULL,
		client_id TEXT NOT NULL,
		scope TEXT NOT NULL,
		used INTEGER NOT NULL DEFAULT 0,
		expires_at INTEGER NOT NULL
	);
	CREATE TABLE signing_keys (
		kid TEXT PRIMARY KEY,
		private_key BLOB NOT NULL,
		created_at INTEGER NOT NULL,
		retired_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE oidc_clients (
		id TEXT PRIMARY KEY,
		redirect_uris TEXT NOT NULL,
		public INTEGER NOT NULL DEFAULT 1,
		secret_hash TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE device_codes (
		device_hash TEXT PRIMARY KEY,
		user_code TEXT NOT NULL UNIQUE,
		client_id TEXT NOT NULL,
		scope TEXT NOT NULL,
		user_id TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'pending',
		expires_at INTEGER NOT NULL
	);
	CREATE TABLE auth_codes (
		hash TEXT PRIMARY KEY,
		client_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		redirect_uri TEXT NOT NULL,
		challenge TEXT NOT NULL,
		scope TEXT NOT NULL,
		nonce TEXT NOT NULL,
		expires_at INTEGER NOT NULL
	);`,
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.Exec(`INSERT INTO schema_version VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	for ; v < len(migrations); v++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("state: migration %d: %w", v+1, err)
		}
		if _, err := tx.Exec(`UPDATE schema_version SET version = ?`, v+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// NewID returns a random identifier.
func NewID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// NewSecret returns a random secret suitable for a bearer token.
func NewSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// HashToken is how secrets are kept: a token is high-entropy, so SHA-256
// suffices, and a leaked state file yields no usable token.
func HashToken(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func now() int64 { return time.Now().Unix() }

func unix(t int64) time.Time {
	if t == 0 {
		return time.Time{}
	}
	return time.Unix(t, 0)
}

// isUnique reports a UNIQUE or PRIMARY KEY violation.
func isUnique(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "PRIMARY KEY"))
}

func one(res sql.Result, err error) error {
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// tx runs fn in a transaction.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
