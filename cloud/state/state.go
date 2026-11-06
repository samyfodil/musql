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
	"github.com/samyfodil/musql/engine"
	"github.com/samyfodil/musql/replication"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("state: not found")

// ErrExists is returned when a record with the same unique key exists.
var ErrExists = errors.New("state: already exists")

// Store is the service's state database.
type Store struct {
	db *sql.DB
	// master and remote reach the state on the master, from a cluster
	// follower; nil standalone.
	master *Master
	remote *sql.DB
	// node is this node's name in a cluster, "" standalone: usage is kept
	// per node there.
	node string
	// dbType is what a new group's databases default to.
	dbType string
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
	s := &Store{db: db, dbType: TypePlain}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenReplicated takes the state of a cluster node: db is the *sql.DB
// replication.Open returned for it, node this node's name, m how a follower
// reaches the master's (nil: every call runs here). Every node runs the same
// schema statements on its own copy, each idempotent, so nodes that start
// apart create the same tables and merge; the caller lets this node write
// while they run.
func OpenReplicated(db *sql.DB, node string, m *Master) (*Store, error) {
	s := &Store{db: db, master: m, node: node, dbType: TypePlain}
	if m != nil {
		s.remote = remoteDB(m, db)
	}
	for _, m := range migrations {
		for stmt := range strings.SplitSeq(m, ";") {
			if stmt = strings.TrimSpace(stmt); stmt == "" {
				continue
			}
			if err := s.execIdempotent(stmt); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

// execIdempotent runs a schema statement unless what it makes exists.
func (s *Store) execIdempotent(stmt string) error {
	for _, kw := range []string{"CREATE TABLE ", "CREATE INDEX "} {
		if rest, ok := strings.CutPrefix(stmt, kw); ok {
			stmt = kw + "IF NOT EXISTS " + rest
		}
	}
	if rest, ok := strings.CutPrefix(stmt, "ALTER TABLE "); ok {
		f := strings.Fields(rest)
		if len(f) < 4 || f[1] != "ADD" || f[2] != "COLUMN" {
			return fmt.Errorf("state: not an ADD COLUMN: %s", stmt)
		}
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, f[0], f[3]).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
	}
	// A peer's same statement can land between the check and this one.
	if _, err := s.db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("state: %s: %w", stmt, err)
	}
	return nil
}

// Database types.
const (
	TypePlain = "plain"
	TypeCRDT  = "crdt"
	TypeRaft  = "raft"
)

// How a regular database's commits are kept in a cluster: durable once a
// majority of its group's nodes hold them (its group's raft log carries
// them), or fast, once its master does.
const (
	DurabilityDurable = "durable"
	DurabilityFast    = "fast"
)

// DefaultType is the type a new group's databases default to: plain, which
// is a regular database on its own node and in a cluster alike.
func (s *Store) DefaultType() string { return s.dbType }

// Clustered reports a cluster node's state.
func (s *Store) Clustered() bool { return s.node != "" }

// derivedID is an ID made from what names a record, so every node that
// creates it creates the same row.
func derivedID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:12])
}

// q is the handle a call runs on: this node's state, or the master's on a
// cluster follower, so a request that writes and then reads sees its write.
// With no master to reach, this node's copy still answers reads, once it has
// synced with a peer (Master.Local): before, the call goes to the master and
// fails with ErrNoMaster. A write on this copy fails as not the writer; tx
// waits either out.
func (s *Store) q() *sql.DB {
	if s.master == nil || s.master.Here() {
		return s.db
	}
	if _, err := s.master.URL(); err != nil && s.master.local() {
		return s.db
	}
	return s.remote
}

// Close closes the database.
func (s *Store) Close() error {
	if s.remote != nil {
		s.remote.Close()
	}
	return s.db.Close()
}

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
	// Keys that existed before this column are -1: what they signed is not
	// known, so they are never dropped.
	`ALTER TABLE signing_keys ADD COLUMN max_exp INTEGER NOT NULL DEFAULT -1`,
	// Usage (stage D), and a database's file name as a column so an in-place
	// restore lands by updating one row: the new file is complete before the
	// record points at it.
	`ALTER TABLE databases ADD COLUMN file TEXT NOT NULL DEFAULT '';
	CREATE TABLE usage_hours (
		db_id TEXT NOT NULL,
		org_id TEXT NOT NULL,
		hour INTEGER NOT NULL,
		statements INTEGER NOT NULL,
		rows_read INTEGER NOT NULL,
		rows_written INTEGER NOT NULL,
		errors INTEGER NOT NULL,
		bytes_stored INTEGER NOT NULL,
		latency TEXT NOT NULL,
		PRIMARY KEY (db_id, hour)
	);
	CREATE INDEX usage_hours_org ON usage_hours (org_id, hour);
	CREATE TABLE usage_queries (
		db_id TEXT NOT NULL,
		hour INTEGER NOT NULL,
		sql TEXT NOT NULL,
		count INTEGER NOT NULL,
		total_us INTEGER NOT NULL,
		rows_read INTEGER NOT NULL,
		rows_written INTEGER NOT NULL,
		PRIMARY KEY (db_id, hour, sql)
	);`,
	// Database types, and usage kept per node in a cluster, where two nodes
	// adding to one row would each overwrite the other's count.
	`ALTER TABLE groups ADD COLUMN type TEXT NOT NULL DEFAULT 'plain';
	ALTER TABLE databases ADD COLUMN type TEXT NOT NULL DEFAULT 'plain';
	CREATE TABLE node_usage_hours (
		node TEXT NOT NULL,
		db_id TEXT NOT NULL,
		org_id TEXT NOT NULL,
		hour INTEGER NOT NULL,
		statements INTEGER NOT NULL,
		rows_read INTEGER NOT NULL,
		rows_written INTEGER NOT NULL,
		errors INTEGER NOT NULL,
		bytes_stored INTEGER NOT NULL,
		latency TEXT NOT NULL,
		PRIMARY KEY (node, db_id, hour)
	);
	CREATE INDEX node_usage_hours_org ON node_usage_hours (org_id, hour);
	CREATE TABLE node_usage_queries (
		node TEXT NOT NULL,
		db_id TEXT NOT NULL,
		hour INTEGER NOT NULL,
		sql TEXT NOT NULL,
		count INTEGER NOT NULL,
		total_us INTEGER NOT NULL,
		rows_read INTEGER NOT NULL,
		rows_written INTEGER NOT NULL,
		PRIMARY KEY (node, db_id, hour, sql)
	);`,
	// A regular database's durability in a cluster. Those from before were
	// opened without a quorum, which their replication catalog keeps.
	`ALTER TABLE databases ADD COLUMN durability TEXT NOT NULL DEFAULT 'fast'`,
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

// tx runs fn in a transaction. On a cluster node a change from elsewhere can
// commit while it is open, which fails it with SQLITE_BUSY, and the master can
// change under it; it is run again, for up to retryFor.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	deadline := time.Now().Add(retryFor)
	for try := 0; ; try++ {
		err := s.tx1(ctx, fn)
		if !retryable(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(min(time.Duration(try+1)*5*time.Millisecond, 200*time.Millisecond))
	}
}

// retryFor is how long tx retries: longer than an election takes.
const retryFor = 15 * time.Second

// retryable reports a failure that running the transaction again may not
// meet: a busy file, or no master (being elected, unreachable, or just
// elected and still catching up).
func retryable(err error) bool {
	var r *errRemote
	switch {
	case err == nil:
		return false
	case errors.Is(err, engine.ErrBusy), errors.Is(err, ErrNoMaster), errors.Is(err, replication.ErrNotWriter), errors.Is(err, errUnreachable):
		return true
	case errors.As(err, &r):
		// The master's own error, as text.
		return strings.Contains(r.msg, engine.ErrBusy.Error()) || strings.Contains(r.msg, replication.ErrNotWriter.Error())
	}
	return false
}

// ErrNoMaster is a cluster's state with no master to write to yet.
var ErrNoMaster = errors.New("state: no master yet: an election is under way")

// exec runs one write, again while it meets what tx retries.
func (s *Store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	deadline := time.Now().Add(retryFor)
	for try := 0; ; try++ {
		res, err := s.q().ExecContext(ctx, query, args...)
		if !retryable(err) || time.Now().After(deadline) {
			return res, err
		}
		time.Sleep(min(time.Duration(try+1)*5*time.Millisecond, 200*time.Millisecond))
	}
}

func (s *Store) tx1(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.q().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
