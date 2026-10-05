// Package driver is the database/sql driver over the pure-Go engine in
// github.com/samyfodil/musql/engine. It registers driver name "sqlite" (and
// "musql" as an alias):
//
//	db, err := sql.Open("sqlite", "/path/to/file.musq")
//
// The file is musql's own format, and a file of any other is an error. A C
// SQLite database is converted with cmd/musql-convert.
//
// See conn.go for the autocommit-vs-transaction execution model (each
// connection holds one engine session across its statements, and an open
// transaction reads its own writes from that session -- see Conn's doc comment
// and engine.DB.SnapshotPager), and capture.go for the change-capture seam the
// replication layer builds on.
//
// Not supported: multi-statement Exec (each Exec/Query call is exactly one SQL
// statement -- the engine's own parsers reject trailing garbage after the
// first statement), and driver.TxOptions isolation levels and read-only
// transactions (accepted but ignored: every transaction is a plain read-write
// session). Concurrent writers on the same file are serialized: a second
// writer's commit fails with engine.ErrBusy (SQLITE_BUSY) rather than
// corrupting the first's work, and autocommit statements retry it
// automatically (see busyRetry in conn.go), but an explicit transaction
// surfaces it to the caller.
package driver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/samyfodil/musql/engine"
)

// DriverName is the name this package registers with database/sql (see
// init below): sql.Open(DriverName, path). "sqlite" lets code written for
// another SQLite driver switch by changing only its import; AliasDriverName
// names musql explicitly.
const (
	DriverName      = "sqlite"
	AliasDriverName = "musql"
)

func init() {
	sql.Register(DriverName, &Driver{})
	sql.Register(AliasDriverName, &Driver{})
}

// Driver implements driver.Driver and driver.DriverContext. It carries no
// state of its own -- every Open/OpenConnector call is independent, keyed
// only by the DSN (a file path, see splitDSN).
type Driver struct{}

var (
	_ driver.Driver        = (*Driver)(nil)
	_ driver.DriverContext = (*Driver)(nil)
)

// Open implements driver.Driver (the legacy, non-context path: database/sql
// falls back to this only when the driver does NOT implement
// driver.DriverContext's OpenConnector, which Driver does -- so in practice
// database/sql calls OpenConnector/Connect instead, and this method exists
// only to satisfy the driver.Driver interface and for callers that invoke it
// directly).
func (d *Driver) Open(dsn string) (driver.Conn, error) {
	return newConn(dsn)
}

// OpenConnector implements driver.DriverContext.
func (d *Driver) OpenConnector(dsn string) (driver.Connector, error) {
	return &connector{path: splitDSN(dsn), dsn: dsn, driver: d}, nil
}

// connector implements driver.Connector: every Connect call returns a fresh
// *Conn bound to the same file path. There is no connection pooling of any
// engine-side resource here (each autocommit Exec/Query opens and closes its
// own engine.DB/ReadOnlyPager -- see Conn's doc comment), so Connect itself
// does no I/O.
type connector struct {
	path   string
	dsn    string
	driver *Driver
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	return newConn(c.dsn)
}

// newConn builds a connection for dsn and runs every registered connection
// hook over it (capture.go), so a hook can install change capture before the
// connection is ever used.
func newConn(dsn string) (driver.Conn, error) {
	c := &Conn{path: splitDSN(dsn), rtreeConns: engine.NewRtreeConnections()}
	if path, private, shared, ok := memBacking(dsn); ok {
		c.path = path
		c.memPrivate = private // removed by Conn.Close (memdb.go)
		c.memShared = shared
	}
	if err := c.applyDSNParams(dsn); err != nil {
		return nil, err
	}
	if err := runConnectionHooks(c, dsn); err != nil {
		return nil, err
	}
	if err := c.applyDSNPragmas(dsn); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// applyDSNPragmas runs each "_pragma=<p>" query parameter as "PRAGMA <p>", in
// order, on the new connection -- modernc.org/sqlite's spelling ("_pragma=
// foreign_keys(1)"), which callers carry over. It runs last, on a connection
// the hooks have finished, through the same path as any PRAGMA, so a setting
// it makes is the connection's from its first statement.
//
// An error fails the open. This driver used to ignore _pragma, which is the
// failure this repo fears most: "?_pragma=foreign_keys(1)" opened with foreign
// keys OFF, and "_pragma=busy_timeout(5000)" with no timeout, and nothing said
// so. Each value must be one PRAGMA statement (ParsePragma refuses trailing
// input), so a DSN cannot smuggle in another statement.
func (c *Conn) applyDSNPragmas(dsn string) error {
	i := strings.IndexByte(dsn, '?')
	if i < 0 {
		return nil
	}
	v, err := url.ParseQuery(dsn[i+1:])
	if err != nil {
		// ParseQuery drops a pair it cannot read (one holding a ";") and keeps
		// the rest, so a pragma could vanish here without a word.
		if strings.Contains(dsn[i+1:], "_pragma") {
			return fmt.Errorf("driver: DSN query: %w", err)
		}
		return nil // an unparseable query string opens as before, path only
	}
	for _, p := range v["_pragma"] {
		text := "PRAGMA " + p
		if _, err := engine.ParsePragma(text); err != nil {
			return fmt.Errorf("driver: _pragma=%s: %w", p, err)
		}
		if _, err := c.execArgs(text, nil); err != nil {
			return fmt.Errorf("driver: _pragma=%s: %w", p, err)
		}
	}
	return nil
}

func (c *connector) Driver() driver.Driver { return c.driver }

// applyDSNParams reads the query parameters that configure the CONNECTION
// rather than which file it opens. Two today, both mattn-compatible spellings
// and both off by default:
//
//	_time_decltype=1   coerce a "timestamp"/"datetime"/"date" column to a Go
//	                   time.Time, exactly as mattn/go-sqlite3 does -- see
//	                   decltype_time.go, which owns the rule and the reason it
//	                   is opt-in
//	_loc=<name>        the location such a value is placed in; "auto" is
//	                   time.Local (mattn's own spelling, sqlite3.go)
//
// An unparseable _loc is an ERROR from Open, like mattn's, rather than a
// silently ignored parameter: a connection that quietly used UTC where the
// caller asked for a zone would report wrong times rather than fail.
func (c *Conn) applyDSNParams(dsn string) error {
	i := strings.IndexByte(dsn, '?')
	if i < 0 {
		return nil
	}
	v, err := url.ParseQuery(dsn[i+1:])
	if err != nil {
		return nil // an unparseable query string opens as before, path only
	}
	if s := v.Get("_time_decltype"); s != "" {
		switch strings.ToLower(s) {
		case "1", "true", "yes", "on":
			c.timeDeclType = true
		}
	}
	// _busy_timeout is mattn's spelling of sqlite3_busy_timeout: how long a
	// statement waits on another connection's lock before answering
	// SQLITE_BUSY, and what "PRAGMA busy_timeout" then reports. Milliseconds, as
	// mattn takes it; a Go duration ("50ms") is accepted too.
	if s := v.Get("_busy_timeout"); s != "" {
		ms, perr := strconv.ParseInt(s, 10, 64)
		if perr != nil {
			d, derr := time.ParseDuration(s)
			if derr != nil {
				return fmt.Errorf("driver: invalid _busy_timeout: %v", s)
			}
			ms = d.Milliseconds()
		}
		if c.pragmaState == nil {
			c.pragmaState = &engine.PragmaConnState{}
		}
		if c.pragmaState.Values == nil {
			c.pragmaState.Values = map[string]int64{}
		}
		c.pragmaState.Values["busy_timeout"] = max(ms, 0)
	}
	if s := v.Get("_loc"); s != "" {
		if strings.EqualFold(s, "auto") {
			c.loc = time.Local
			return nil
		}
		loc, lerr := time.LoadLocation(s)
		if lerr != nil {
			return fmt.Errorf("driver: invalid _loc: %v: %v", s, lerr)
		}
		c.loc = loc
	}
	return nil
}

// splitDSN extracts the file path from a DSN, discarding everything from
// the first "?" onward. The query parameters this driver reads at all are
// mode=memory/cache=shared (memdb.go), the connection ones applyDSNParams
// takes just above, and _pragma (applyDSNPragmas); anything else (_txlock,
// ...) is ignored rather than rejected, so an older DSN still opens.
func splitDSN(dsn string) string {
	if i := strings.IndexByte(dsn, '?'); i >= 0 {
		dsn = dsn[:i]
	}
	// A "file:" URI DSN names the same file its bare-path spelling does --
	// C SQLite accepts both, and DSNs are commonly written in the URI form. ponytail: the scheme is stripped, not parsed;
	// no host/authority or percent-decoding (nobody passes those here).
	return strings.TrimPrefix(dsn, "file:")
}

// defaultPageSize is the page size used when this driver has to create a
// brand-new database file from scratch (see Conn.openWriteOrCreate): 4096 is
// both SQLite's own modern default and the size engine/write_verify_test.go
// exercises most, so a fresh musql-created file matches what C
// SQLite would choose unprompted.
const defaultPageSize = 4096
